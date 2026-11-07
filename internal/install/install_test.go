package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeMachine is a temporary home directory plus fake agent CLIs.
type fakeMachine struct {
	t         *testing.T
	home, cfg string
	bins      map[string]bool
	servers   map[string]bool // CLI agents that already have botchat at user level
	// CLI agents with a project- or local-scoped botchat entry only
	projectOnly map[string]bool
	failAdd     map[string]bool
	calls       []string
}

func newMachine(t *testing.T, bins ...string) *fakeMachine {
	t.Helper()
	root := t.TempDir()
	m := &fakeMachine{
		t:           t,
		home:        filepath.Join(root, "home"),
		cfg:         filepath.Join(root, "config"),
		bins:        map[string]bool{},
		servers:     map[string]bool{},
		projectOnly: map[string]bool{},
		failAdd:     map[string]bool{},
	}
	for _, b := range bins {
		m.bins[b] = true
	}
	return m
}

func (m *fakeMachine) env(stdin string, stdout *bytes.Buffer) Env {
	return Env{
		Home:      m.home,
		ConfigDir: m.cfg,
		LookPath: func(f string) (string, error) {
			if m.bins[f] {
				return "/bin/" + f, nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, dir, path string, args ...string) (string, error) {
			bin := filepath.Base(path)
			m.calls = append(m.calls, cmdline(bin, args))
			switch args[1] {
			case "get":
				if m.servers[bin] {
					return "botchat", nil
				}
				// Project/local entries are only visible from a project dir.
				if entries, err := os.ReadDir(dir); m.projectOnly[bin] && (dir == "" || err != nil || len(entries) > 0) {
					return "botchat (local)", nil
				}
				return "not found", errors.New("exit status 1")
			case "remove":
				delete(m.servers, bin)
			case "add":
				if m.failAdd[bin] {
					return "boom", errors.New("exit status 1")
				}
				m.servers[bin] = true
			}
			return "", nil
		},
		Stdin:  strings.NewReader(stdin),
		Stdout: stdout,
		Stderr: stdout,
	}
}

func (m *fakeMachine) run(stdin string, env map[string]string, args ...string) (int, string) {
	m.t.Helper()
	var out bytes.Buffer
	getenv := func(k string) string { return env[k] }
	code := MainEnv(context.Background(), args, getenv, m.env(stdin, &out))
	return code, out.String()
}

func (m *fakeMachine) write(rel, content string) string {
	m.t.Helper()
	p := filepath.Join(m.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
	return p
}

func (m *fakeMachine) read(path string) string {
	m.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		m.t.Fatal(err)
	}
	return string(b)
}

func (m *fakeMachine) mkdir(rel string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Join(m.home, rel), 0o755); err != nil {
		m.t.Fatal(err)
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
	return v
}

// spec: INS-1
func TestDetection(t *testing.T) {
	m := newMachine(t, "claude", "opencode")
	m.mkdir(".cursor")
	m.mkdir(".gemini") // without the gemini binary, not Gemini CLI
	m.servers["claude"] = true

	code, out := m.run("", nil)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"Found 3 coding agents", "1. Claude Code", "2. Cursor", "3. opencode", "http://127.0.0.1:7878/mcp"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Gemini") {
		t.Errorf("~/.gemini alone should not detect Gemini CLI:\n%s", out)
	}
	claudeLine := strings.SplitN(out[strings.Index(out, "1. Claude Code"):], "\n", 2)[0]
	if !strings.Contains(claudeLine, "already installed") {
		t.Errorf("claude should be marked installed: %q", claudeLine)
	}
	if strings.Contains(strings.SplitN(out[strings.Index(out, "2. Cursor"):], "\n", 2)[0], "already") {
		t.Error("cursor should not be marked installed")
	}

	// An existing file entry is detected too.
	m.write(".cursor/mcp.json", `{"mcpServers":{"botchat":{"url":"x"}}}`)
	_, out = m.run("", nil)
	if !strings.Contains(strings.SplitN(out[strings.Index(out, "2. Cursor"):], "\n", 2)[0], "already installed") {
		t.Errorf("cursor should be marked installed:\n%s", out)
	}
}

// spec: INS-1
func TestNothingDetected(t *testing.T) {
	m := newMachine(t)
	code, out := m.run("all\n", nil)
	if code != 0 || !strings.Contains(out, "No supported coding agents found") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// spec: INS-2
func TestSelection(t *testing.T) {
	cases := []struct {
		name, stdin string
		args        []string
		want        []string // CLI agents that end up with botchat
		code        int
	}{
		{"numbers", "1\n", nil, []string{"claude"}, 0},
		{"ids and numbers", "codex, 1\n", nil, []string{"claude", "codex"}, 0},
		{"all", "ALL\n", nil, []string{"claude", "codex"}, 0},
		{"empty line", "\n", nil, nil, 0},
		{"end of input", "", nil, nil, 0},
		{"flag", "", []string{"--agents", "codex"}, []string{"codex"}, 0},
		{"flag all", "", []string{"--agents=all"}, []string{"claude", "codex"}, 0},
		{"unknown", "1 nope\n", nil, nil, 2},
		{"out of range", "3\n", nil, nil, 2},
		{"not detected", "", []string{"--agents", "claude,cursor"}, nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, "claude", "codex")
			code, out := m.run(tc.stdin, nil, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, out)
			}
			var got []string
			for b := range m.servers {
				got = append(got, b)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("installed %v, want %v\n%s", got, tc.want, out)
			}
			prompted := strings.Contains(out, "which agents?")
			if prompted != (len(tc.args) == 0) {
				t.Errorf("prompted = %v with args %v", prompted, tc.args)
			}
		})
	}
}

// spec: INS-3
func TestEntries(t *testing.T) {
	m := newMachine(t, "claude", "codex", "gemini", "code", "windsurf")
	m.mkdir(".cursor")
	m.mkdir(".config/opencode")
	code, out := m.run("", map[string]string{"BOTCHAT_URL": "http://env:1"},
		"--agents", "all", "--url", "https://chat.example:8443/", "--token", "s3cret")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("token printed:\n%s", out)
	}
	u := "https://chat.example:8443/mcp"
	auth := map[string]any{"Authorization": "Bearer s3cret"}

	wantCalls := []string{
		"claude mcp add --scope user --transport http botchat " + u + " --header 'Authorization: Bearer s3cret'",
		"codex mcp add botchat --url " + u + " --bearer-token-env-var BOTCHAT_TOKEN",
	}
	for _, c := range wantCalls {
		if !slices.Contains(m.calls, c) {
			t.Errorf("missing call %q in %q", c, m.calls)
		}
	}
	if !strings.Contains(out, "BOTCHAT_TOKEN") {
		t.Errorf("expected a note about BOTCHAT_TOKEN for Codex:\n%s", out)
	}

	files := []struct {
		path, container string
		want            map[string]any
	}{
		{filepath.Join(m.home, ".cursor", "mcp.json"), "mcpServers", map[string]any{"url": u, "headers": auth}},
		{filepath.Join(m.home, ".gemini", "settings.json"), "mcpServers", map[string]any{"httpUrl": u, "headers": auth}},
		{filepath.Join(m.home, ".config", "opencode", "opencode.json"), "mcp", map[string]any{"type": "remote", "url": u, "enabled": true, "headers": auth}},
		{filepath.Join(m.cfg, "Code", "User", "mcp.json"), "servers", map[string]any{"type": "http", "url": u, "headers": auth}},
		{filepath.Join(m.home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", map[string]any{"serverUrl": u, "headers": auth}},
	}
	for _, f := range files {
		got := decode(t, m.read(f.path))[f.container].(map[string]any)[ServerName]
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(f.want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s: got %s, want %s", f.path, gotJSON, wantJSON)
		}
	}
}

// spec: INS-3
func TestURLAndTokenDefaults(t *testing.T) {
	m := newMachine(t, "claude")
	m.mkdir(".cursor")
	code, out := m.run("", map[string]string{"BOTCHAT_URL": "http://env:1/"}, "--agents", "all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !slices.Contains(m.calls, "claude mcp add --scope user --transport http botchat http://env:1/mcp") {
		t.Errorf("calls: %q", m.calls)
	}
	if got := m.read(filepath.Join(m.home, ".cursor", "mcp.json")); strings.Contains(got, "headers") {
		t.Errorf("no token, but headers written: %s", got)
	}

	m = newMachine(t, "claude")
	m.run("", nil, "--agents", "claude")
	if !slices.Contains(m.calls, "claude mcp add --scope user --transport http botchat "+DefaultURL+"/mcp") {
		t.Errorf("calls: %q", m.calls)
	}

	m = newMachine(t, "claude")
	m.run("", map[string]string{"BOTCHAT_TOKEN": "envtok"}, "--agents", "claude")
	if !strings.Contains(strings.Join(m.calls, "\n"), "Bearer envtok") {
		t.Errorf("env token not used: %q", m.calls)
	}

	if code, out := newMachine(t).run("", nil, "--url", "localhost:7878"); code != 2 {
		t.Errorf("bad URL: exit %d: %s", code, out)
	}
}

// spec: INS-4
func TestPreservesConfig(t *testing.T) {
	m := newMachine(t, "opencode")
	path := m.write(".config/opencode/opencode.json", `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {"z": 1, "a": 2},
  "mcp": {"other": {"type": "local", "command": ["x"]}},
  "theme": "dark"
}`)
	if code, out := m.run("", nil, "--agents", "opencode"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	got := m.read(path)
	order := []string{`"$schema"`, `"provider"`, `"z"`, `"a"`, `"mcp"`, `"other"`, `"botchat"`, `"theme"`}
	last := -1
	for _, k := range order {
		i := strings.Index(got, k)
		if i <= last {
			t.Fatalf("key %s out of order in:\n%s", k, got)
		}
		last = i
	}
	v := decode(t, got)
	if v["$schema"] != "https://opencode.ai/config.json" || v["theme"] != "dark" {
		t.Errorf("lost keys: %s", got)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

// spec: INS-4
func TestCreatesMissingFile(t *testing.T) {
	m := newMachine(t, "windsurf")
	if code, out := m.run("", nil, "--agents", "windsurf"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	path := filepath.Join(m.home, ".codeium", "windsurf", "mcp_config.json")
	decode(t, m.read(path))
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v, want 0700", fi.Mode().Perm())
		}
	}
}

// spec: INS-4
func TestUnparseableFileUntouched(t *testing.T) {
	m := newMachine(t, "claude", "gemini")
	orig := "{\n  // comment\n  \"theme\": \"x\"\n}\n"
	path := m.write(".gemini/settings.json", orig)
	code, out := m.run("", nil, "--agents", "all")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if m.read(path) != orig {
		t.Error("unparseable file was modified")
	}
	if !strings.Contains(out, "can't read config") || !strings.Contains(out, "Add this by hand") || !strings.Contains(out, `"httpUrl"`) {
		t.Errorf("expected a manual snippet:\n%s", out)
	}
	if !m.servers["claude"] {
		t.Error("other agents should still be installed")
	}
}

// spec: INS-4
func TestFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	m := newMachine(t, "cursor")
	target := m.write("dotfiles/cursor-mcp.json", `{"mcpServers":{}}`)
	m.mkdir(".cursor")
	link := filepath.Join(m.home, ".cursor", "mcp.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, out := m.run("", nil, "--agents", "cursor"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	if !strings.Contains(m.read(target), "botchat") {
		t.Error("target not updated")
	}
}

// spec: INS-5
func TestExistingEntry(t *testing.T) {
	m := newMachine(t, "claude", "cursor")
	m.servers["claude"] = true
	path := m.write(".cursor/mcp.json", `{"mcpServers":{"botchat":{"url":"old"},"x":{}}}`)

	code, out := m.run("", nil, "--agents", "all")
	if code != 0 || strings.Count(out, "already installed (use --force") != 2 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if slices.ContainsFunc(m.calls, func(c string) bool { return strings.Contains(c, " add ") }) {
		t.Errorf("add called without --force: %q", m.calls)
	}
	if !strings.Contains(m.read(path), `"old"`) {
		t.Error("entry changed without --force")
	}

	m.calls = nil
	code, out = m.run("", nil, "--agents", "all", "--force")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !slices.Contains(m.calls, "claude mcp remove --scope user botchat") {
		t.Errorf("no remove before add: %q", m.calls)
	}
	got := m.read(path)
	if strings.Contains(got, `"old"`) || !strings.Contains(got, DefaultURL+"/mcp") || !strings.Contains(got, `"x"`) {
		t.Errorf("not replaced correctly: %s", got)
	}
	if !strings.Contains(out, "replaced") {
		t.Errorf("output: %s", out)
	}
}

// spec: INS-5
func TestProjectScopedEntryIgnored(t *testing.T) {
	m := newMachine(t, "claude", "codex")
	m.projectOnly["claude"] = true
	m.projectOnly["codex"] = true
	code, out := m.run("", nil, "--agents", "all")
	if code != 0 || strings.Contains(out, "already installed") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !m.servers["claude"] || !m.servers["codex"] {
		t.Errorf("user-level entries not added: %v\n%s", m.servers, out)
	}
}

// spec: INS-6
func TestDryRun(t *testing.T) {
	m := newMachine(t, "claude", "codex")
	m.mkdir(".cursor")
	m.servers["codex"] = true
	code, out := m.run("", nil, "--agents", "all", "--dry-run", "--force", "--token", "tok")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, c := range m.calls {
		if !strings.Contains(c, " get ") {
			t.Errorf("dry run ran %q", c)
		}
	}
	if _, err := os.Stat(filepath.Join(m.home, ".cursor", "mcp.json")); err == nil {
		t.Error("dry run wrote a file")
	}
	for _, want := range []string{
		"claude mcp add --scope user --transport http botchat",
		"codex mcp remove botchat",
		filepath.Join(m.home, ".cursor", "mcp.json"),
		`"url": "http://127.0.0.1:7878/mcp"`,
		"Bearer <token>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bearer tok") {
		t.Errorf("token printed:\n%s", out)
	}
}

// spec: INS-6
func TestEscapedTokenNeverPrinted(t *testing.T) {
	for _, tok := range []string{`quote"secret`, `back\slash`, `quote'secret`, "plain-secret"} {
		leaked := func(out string) bool {
			j, _ := json.Marshal(tok)
			return strings.Contains(out, tok) ||
				strings.Contains(out, string(j[1:len(j)-1])) ||
				strings.Contains(out, strings.ReplaceAll(tok, "'", `'\''`))
		}
		// Dry run.
		m := newMachine(t, "claude", "codex")
		m.mkdir(".cursor")
		if _, out := m.run("", nil, "--agents", "all", "--dry-run", "--token", tok); leaked(out) || !strings.Contains(out, "<token>") {
			t.Errorf("%s: dry run:\n%s", tok, out)
		}
		// Errors: a failing CLI, and an unparseable file with a manual snippet.
		m = newMachine(t, "claude", "cursor")
		m.failAdd["claude"] = true
		m.write(".cursor/mcp.json", "null")
		code, out := m.run("", nil, "--agents", "all", "--token", tok)
		if code != 1 || leaked(out) || !strings.Contains(out, "Bearer <token>") {
			t.Errorf("%s: errors (exit %d):\n%s", tok, code, out)
		}
	}
}

// spec: INS-7
func TestFailuresAndUsage(t *testing.T) {
	m := newMachine(t, "claude", "codex")
	m.failAdd["codex"] = true
	code, out := m.run("all\n", nil)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	if !strings.Contains(out, "Claude Code: added") || !strings.Contains(out, "Codex: failed") || !strings.Contains(out, "boom") {
		t.Errorf("output: %s", out)
	}

	for _, args := range [][]string{{"--nope"}, {"extra"}} {
		if code, _ := newMachine(t).run("", nil, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _ := newMachine(t).run("", nil, "-h"); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
}

// spec: INS-4
func TestRejectedFilesUntouched(t *testing.T) {
	for _, orig := range []string{
		"null\n",
		`{"mcpServers":{"first":{}},"mcpServers":{"last":{}}}`,
		`{"mcpServers":{"botchat":{"url":"a"},"botchat":{"url":"b"}}}`,
	} {
		m := newMachine(t, "cursor")
		path := m.write(".cursor/mcp.json", orig)
		if code, out := m.run("", nil, "--agents", "cursor", "--force"); code != 1 || !strings.Contains(out, "Add this by hand") {
			t.Errorf("%s: exit %d: %s", orig, code, out)
		}
		if m.read(path) != orig {
			t.Errorf("%s: file modified", orig)
		}
	}
}

// spec: INS-4
func TestEmptyFileFilled(t *testing.T) {
	m := newMachine(t, "cursor")
	path := m.write(".cursor/mcp.json", " \n")
	if code, out := m.run("", nil, "--agents", "cursor"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	decode(t, m.read(path))
}

// spec: INS-4
func TestDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	m := newMachine(t, "cursor")
	m.mkdir(".cursor")
	link := filepath.Join(m.home, ".cursor", "mcp.json")
	if err := os.Symlink("../dotfiles/cursor/mcp.json", link); err != nil {
		t.Fatal(err)
	}
	if code, out := m.run("", nil, "--agents", "cursor"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	if !strings.Contains(m.read(filepath.Join(m.home, "dotfiles", "cursor", "mcp.json")), "botchat") {
		t.Error("target not written")
	}
}

func TestParseObject(t *testing.T) {
	for _, bad := range []string{`[]`, `{"a":1} x`, `{"a":}`, `"s"`, `null`, `{"a":1,"a":2}`} {
		if _, err := parseObject([]byte(bad)); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	for _, empty := range []string{``, ` `, `{}`} {
		if o, err := parseObject([]byte(empty)); err != nil || len(o) != 0 {
			t.Errorf("%q: %v %v", empty, o, err)
		}
	}
}
