package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

type running struct {
	base   string
	stdout *syncBuffer
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func start(t *testing.T, cfg Config) *running {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.MaxMessages == 0 {
		cfg.MaxMessages = 10000
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stdout: &syncBuffer{}, cancel: cancel, done: make(chan error, 1)}
	ready := make(chan string, 1)
	go func() { r.done <- Run(ctx, cfg, "v-test", r.stdout, func(a string) { ready <- a }) }()
	select {
	case addr := <-ready:
		r.base = "http://" + addr
	case err := <-r.done:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Fatalf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("server did not stop within 5s")
		}
	})
}

func post(t *testing.T, base, channel, from, text string) {
	t.Helper()
	body := `{"from":"` + from + `","text":"` + text + `"}`
	res, err := http.Post(base+"/api/v1/channels/"+channel+"/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("post = %d", res.StatusCode)
	}
}

// spec: CLI-1
func TestNoDependencies(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "require") {
			t.Fatalf("go.mod must not have dependencies: %q", line)
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "go.sum")); err == nil {
		t.Fatal("go.sum must not exist (no dependencies)")
	}
}

// spec: CLI-2
func TestAddrPrecedence(t *testing.T) {
	cases := []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, nil, "127.0.0.1:7878"},
		{nil, map[string]string{"BOTCHAT_ADDR": ":9000"}, ":9000"},
		{[]string{"--addr", ":9001"}, map[string]string{"BOTCHAT_ADDR": ":9000"}, ":9001"},
		{[]string{"-addr=:9002"}, nil, ":9002"},
	}
	for _, c := range cases {
		cfg, err := ParseConfig(c.args, env(c.env), io.Discard)
		if err != nil || cfg.Addr != c.want {
			t.Errorf("%v %v: addr %q (%v), want %q", c.args, c.env, cfg.Addr, err, c.want)
		}
	}
}

// spec: CLI-3
func TestTokenPrecedence(t *testing.T) {
	cfg, _ := ParseConfig(nil, env(map[string]string{"BOTCHAT_TOKEN": "env"}), io.Discard)
	if cfg.Token != "env" {
		t.Fatalf("token %q", cfg.Token)
	}
	cfg, _ = ParseConfig([]string{"--token", "flag"}, env(map[string]string{"BOTCHAT_TOKEN": "env"}), io.Discard)
	if cfg.Token != "flag" {
		t.Fatalf("token %q", cfg.Token)
	}
	cfg, _ = ParseConfig(nil, env(nil), io.Discard)
	if cfg.Token != "" {
		t.Fatal("no token by default")
	}

	r := start(t, Config{Token: "sekrit"})
	res, _ := http.Get(r.base + "/api/v1/agents")
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated = %d", res.StatusCode)
	}
	if !strings.Contains(r.stdout.String(), "Authorization: Bearer") {
		t.Fatal("banner should show how to pass the token")
	}
}

// spec: CLI-4
func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(context.Background(), []string{"--version"}, env(nil), &out, &errb, "1.2.3"); code != 0 || out.String() != "botchat 1.2.3\n" {
		t.Fatalf("--version: %d %q", code, out.String())
	}
	out.Reset()
	if code := Main(context.Background(), []string{"--bogus"}, env(nil), &out, &errb, "x"); code != 2 || !strings.Contains(errb.String(), "Usage") {
		t.Fatalf("unknown flag: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := Main(context.Background(), []string{"extra"}, env(nil), &out, &errb, "x"); code != 2 {
		t.Fatalf("positional arg: %d", code)
	}
	if code := Main(context.Background(), []string{"--max-messages", "0"}, env(nil), &out, &errb, "x"); code != 2 {
		t.Fatalf("bad max-messages: %d", code)
	}
	if code := Main(context.Background(), []string{"-h"}, env(nil), &out, &errb, "x"); code != 0 {
		t.Fatalf("-h: %d", code)
	}
	if code := Main(context.Background(), []string{"--addr", "256.0.0.1:1"}, env(nil), &out, &errb, "x"); code != 1 {
		t.Fatalf("bad listen address: %d", code)
	}
}

// spec: CLI-5
func TestBanner(t *testing.T) {
	r := start(t, Config{})
	out := r.stdout.String()
	for _, want := range []string{"v-test", r.base + "/", r.base + "/mcp", r.base + "/llms.txt", "claude mcp add"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}
	var b bytes.Buffer
	printBanner(&b, "0.0.0.0:7878", "v", false)
	if !strings.Contains(b.String(), "http://localhost:7878/mcp") {
		t.Errorf("unspecified address should display as localhost:\n%s", b.String())
	}
}

// spec: CLI-6
func TestGracefulShutdown(t *testing.T) {
	r := start(t, Config{})
	res, err := http.Get(r.base + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitDone := make(chan int, 1)
	go func() {
		res, err := http.Get(r.base + "/api/v1/wait?timeout=60")
		if err != nil {
			waitDone <- -1
			return
		}
		res.Body.Close()
		waitDone <- res.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)

	begin := time.Now()
	r.stop(t)
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	streamEnded := make(chan struct{})
	go func() { io.Copy(io.Discard, res.Body); close(streamEnded) }()
	select {
	case <-streamEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream not closed on shutdown")
	}
	select {
	case code := <-waitDone:
		if code != 200 && code != -1 {
			t.Fatalf("pending wait ended with %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending wait not ended on shutdown")
	}
}

// spec: CLI-7
func TestEventLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	r := start(t, Config{LogPath: path})
	post(t, r.base, "general", "alice", "logged")
	r.stop(t)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var types []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Seq  int    `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "agent.joined,message.created" {
		t.Fatalf("logged %v", types)
	}

	// Appends across runs.
	r2 := start(t, Config{LogPath: path})
	post(t, r2.base, "general", "bob", "again")
	r2.stop(t)
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 4 {
		t.Fatalf("expected 4 lines after second run, got %d", n)
	}

	if err := Run(context.Background(), Config{Addr: "127.0.0.1:0", LogPath: filepath.Join(dir, "missing", "x.log"), MaxMessages: 1}, "v", io.Discard, nil); err == nil {
		t.Fatal("unwritable log path must fail")
	}
}

// spec: CLI-7
func TestNoFilesWithoutLog(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	r := start(t, Config{})
	post(t, r.base, "general", "alice", "x")
	r.stop(t)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("botchat wrote files: %v", entries)
	}
}

// spec: CLI-8
func TestMaxMessages(t *testing.T) {
	cfg, err := ParseConfig([]string{"--max-messages", "2"}, env(nil), io.Discard)
	if err != nil || cfg.MaxMessages != 2 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if def, _ := ParseConfig(nil, env(nil), io.Discard); def.MaxMessages != 10000 {
		t.Fatalf("default %d", def.MaxMessages)
	}
	cfg.Addr = "127.0.0.1:0" // don't collide with a real botchat on the default port
	r := start(t, cfg)
	for range 3 {
		post(t, r.base, "general", "a", "x")
	}
	res, _ := http.Get(r.base + "/api/v1/channels/general")
	var ch struct {
		MessageCount int `json:"message_count"`
	}
	json.NewDecoder(res.Body).Decode(&ch)
	res.Body.Close()
	if ch.MessageCount != 2 {
		t.Fatalf("retained %d", ch.MessageCount)
	}
}

func TestEndToEnd(t *testing.T) {
	r := start(t, Config{})
	for _, p := range []string{"/", "/app.js", "/healthz"} {
		res, err := http.Get(r.base + p)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", p, res, err)
		}
		res.Body.Close()
	}
	req, _ := http.NewRequest("POST", r.base+"/mcp?agent=e2e", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 || res.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("mcp initialize: %v %v", res, err)
	}
	res.Body.Close()
}
