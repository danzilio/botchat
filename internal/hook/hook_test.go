package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danzilio/botchat/internal/hub"
	"github.com/danzilio/botchat/internal/server"
)

type env struct {
	t   *testing.T
	hub *hub.Hub
	url string
	dir string
}

func newEnv(t *testing.T, token string) *env {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := httptest.NewServer(server.New(server.Config{Hub: h, Token: token}))
	t.Cleanup(func() { h.Close(); srv.Close() })
	return &env{t: t, hub: h, url: srv.URL, dir: t.TempDir()}
}

func (e *env) post(from, text string, thread uint64) hub.Message {
	e.t.Helper()
	ch := "general"
	if thread != 0 {
		ch = ""
	}
	m, err := e.hub.Post(hub.PostInput{Channel: ch, ThreadID: thread, From: from, Text: text})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

type result struct {
	code           int
	stdout, stderr string
	decision       string
	reason         string
}

func (e *env) run(stdin string, args ...string) result {
	e.t.Helper()
	return e.runEnv(stdin, map[string]string{}, args...)
}

func (e *env) runEnv(stdin string, vars map[string]string, args ...string) result {
	e.t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"stop", "--url", e.url}, args...)
	getenv := func(k string) string { return vars[k] }
	// StateDir isn't a flag; route it through a package-level override.
	stateDirOverride = e.dir
	defer func() { stateDirOverride = "" }()
	code := Main(context.Background(), full, getenv, strings.NewReader(stdin), &out, &errb)
	r := result{code: code, stdout: out.String(), stderr: errb.String()}
	if code == 0 && strings.TrimSpace(r.stdout) != "{}" {
		var v map[string]string
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			e.t.Fatalf("stdout is not JSON: %q", r.stdout)
		}
		r.decision, r.reason = v["decision"], v["reason"]
	}
	return r
}

const session = `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":false}`

// spec: HOOK-1
func TestFlagsAndEnv(t *testing.T) {
	cfg, err := parse(nil, func(k string) string {
		return map[string]string{"BOTCHAT_URL": "http://x:1/", "BOTCHAT_AGENT": "a", "BOTCHAT_TOKEN": "t"}[k]
	}, &bytes.Buffer{})
	if err != nil || cfg.URL != "http://x:1" || cfg.Agent != "a" || cfg.Token != "t" || cfg.Wait != 0 || cfg.All {
		t.Fatalf("%+v %v", cfg, err)
	}
	cfg, err = parse([]string{"--url", "http://y:2", "--agent", "b", "--token", "u", "--wait", "90s", "--all"},
		func(k string) string { return map[string]string{"BOTCHAT_AGENT": "a"}[k] }, &bytes.Buffer{})
	if err != nil || cfg.URL != "http://y:2" || cfg.Agent != "b" || cfg.Token != "u" || cfg.Wait != 90*time.Second || !cfg.All {
		t.Fatalf("flags must win: %+v %v", cfg, err)
	}
	if cfg, _ := parse([]string{"--agent", "a"}, func(string) string { return "" }, &bytes.Buffer{}); cfg.URL != DefaultURL {
		t.Fatalf("default url %q", cfg.URL)
	}
	for _, args := range [][]string{{}, {"--agent", "a", "--wait", "-1s"}, {"--agent", "a", "extra"}, {"--bogus"}} {
		var errb bytes.Buffer
		if code := Main(context.Background(), append([]string{"stop"}, args...), func(string) string { return "" }, strings.NewReader(session), &bytes.Buffer{}, &errb); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := Main(context.Background(), []string{"nope"}, func(string) string { return "" }, nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Error("unknown hook must exit 2")
	}
	if code := Main(context.Background(), []string{"stop", "-h"}, func(string) string { return "" }, nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Error("-h must exit 0")
	}
}

// spec: HOOK-2
// spec: HOOK-3
func TestDeliversMentionsSinceLastPost(t *testing.T) {
	e2 := newEnv(t, "")
	e2.post("alice", "@bot old mention", 0)
	e2.post("bot", "hello", 0)
	e2.post("alice", "chatter", 0)
	want1 := e2.post("alice", "@bot can you look at this?", 0)
	e2.post("carol", "@all standup now", want1.ID)

	r := e2.run(session, "--agent", "bot")
	if r.code != 0 || r.decision != "block" {
		t.Fatalf("%+v", r)
	}
	for _, s := range []string{"can you look at this?", "standup now", "#general · message 4 · from alice", "in thread 4", "from carol", "@bot", e2.url, "keep listening"} {
		if !strings.Contains(r.reason, s) {
			t.Errorf("reason missing %q:\n%s", s, r.reason)
		}
	}
	for _, s := range []string{"old mention", "chatter"} {
		if strings.Contains(r.reason, s) {
			t.Errorf("reason must not include %q", s)
		}
	}

	// Same session: already delivered, so nothing new.
	if r := e2.run(session, "--agent", "bot"); r.code != 0 || strings.TrimSpace(r.stdout) != "{}" {
		t.Fatalf("second run: %+v", r)
	}
	// A different session starts from the agent's last post again.
	if r := e2.run(`{"session_id":"s2"}`, "--agent", "bot"); r.decision != "block" {
		t.Fatalf("new session: %+v", r)
	}
	// Once the agent posts, earlier mentions count as handled.
	e2.post("bot", "on it", 0)
	if r := e2.run(`{"session_id":"s3"}`, "--agent", "bot"); strings.TrimSpace(r.stdout) != "{}" {
		t.Fatalf("after replying: %+v", r)
	}
}

// spec: HOOK-2
func TestAllAndUnknownAgent(t *testing.T) {
	e := newEnv(t, "")
	e.post("alice", "no mention here", 0)
	if r := e.run(session, "--agent", "newbie"); strings.TrimSpace(r.stdout) != "{}" {
		t.Fatalf("mentions only by default: %+v", r)
	}
	r := e.run(`{"session_id":"x"}`, "--agent", "newbie", "--all")
	if r.decision != "block" || !strings.Contains(r.reason, "no mention here") {
		t.Fatalf("--all: %+v", r)
	}
	// No session id: works, but nothing is remembered.
	if r := e.run(`{}`, "--agent", "newbie", "--all"); r.decision != "block" {
		t.Fatalf("no session: %+v", r)
	}
	if r := e.run(`{}`, "--agent", "newbie", "--all"); r.decision != "block" {
		t.Fatalf("no session is stateless: %+v", r)
	}
}

// spec: HOOK-2
func TestWaitsForMessages(t *testing.T) {
	e := newEnv(t, "")
	go func() {
		time.Sleep(150 * time.Millisecond)
		if a, err := e.hub.Agent("bot"); err != nil || !a.Online {
			t.Errorf("waiting agent should be online: %+v %v", a, err)
		}
		e.post("alice", "@bot ping", 0)
	}()
	start := time.Now()
	r := e.run(session, "--agent", "bot", "--wait", "5s")
	if r.decision != "block" || !strings.Contains(r.reason, "ping") || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}

	start = time.Now()
	r = e.run(session, "--agent", "bot", "--wait", "200ms") // ping already delivered
	if strings.TrimSpace(r.stdout) != "{}" || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("timeout: %+v after %v", r, time.Since(start))
	}
}

// spec: HOOK-3
func TestCursorStoredInStateDir(t *testing.T) {
	e := newEnv(t, "")
	e.post("alice", "@bot one", 0)
	e.run(session, "--agent", "bot")
	files, _ := filepath.Glob(filepath.Join(e.dir, "*.cursor"))
	if len(files) != 1 {
		t.Fatalf("state files: %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("cursor %q", b)
	}
}

// spec: HOOK-3
func TestReasonTruncation(t *testing.T) {
	e := newEnv(t, "")
	e.post("alice", "@bot "+strings.Repeat("x", 5000), 0)
	for range 20 {
		e.post("alice", "@bot "+strings.Repeat("y", 1500), 0)
	}
	r := e.run(session, "--agent", "bot")
	if r.decision != "block" || len(r.reason) > maxReasonChars+500 || !strings.Contains(r.reason, "truncated") || !strings.Contains(r.reason, "more; read them in botchat") {
		t.Fatalf("len %d:\n%.300s", len(r.reason), r.reason)
	}
}

// spec: HOOK-4
func TestFailuresLetTheAgentStop(t *testing.T) {
	e := newEnv(t, "s3cret")
	cases := []struct {
		name  string
		stdin string
		args  []string
		vars  map[string]string
	}{
		{"bad stdin", "not json", []string{"--agent", "bot"}, nil},
		{"empty stdin", "", []string{"--agent", "bot"}, nil},
		{"missing token", session, []string{"--agent", "bot"}, nil},
		{"wrong token", session, []string{"--agent", "bot", "--token", "nope"}, nil},
	}
	for _, c := range cases {
		r := e.runEnv(c.stdin, map[string]string{}, c.args...)
		if r.code != 1 || r.stdout != "" || r.stderr == "" {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	// Right token works.
	e.post("alice", "@bot hi", 0)
	if r := e.runEnv(session, map[string]string{"BOTCHAT_TOKEN": "s3cret"}, "--agent", "bot"); r.decision != "block" {
		t.Errorf("token via env: %+v", r)
	}

	// Unreachable server.
	var out, errb bytes.Buffer
	code := Main(context.Background(), []string{"stop", "--url", "http://127.0.0.1:1", "--agent", "bot"},
		func(string) string { return "" }, strings.NewReader(session), &out, &errb)
	if code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "cannot reach botchat") {
		t.Errorf("unreachable: %d %q %q", code, out.String(), errb.String())
	}
}
