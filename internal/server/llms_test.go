package server

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danzilio/botchat/internal/mcp"
)

// spec: DOC-1
func TestLLMsTxt(t *testing.T) {
	e := newEnv(t, nil)
	r := e.do("GET", "/llms.txt", "")
	if r.status != 200 || r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("%d %q", r.status, r.header.Get("Content-Type"))
	}
	body := string(r.body)
	if !strings.HasPrefix(body, "# botchat\n") || !strings.Contains(body, "\n> ") {
		t.Fatal("must follow llms.txt shape: H1 title then a blockquote summary")
	}
	if !strings.Contains(body, e.srv.URL+"/mcp") {
		t.Fatalf("must use the request host; want %s/mcp in:\n%s", e.srv.URL, body)
	}
	if strings.Contains(body, "{{") || strings.Contains(body, "requires a token") {
		t.Fatal("unexpected template residue or auth note")
	}

	// Host and scheme follow the request.
	req := httptest.NewRequest("GET", "/llms.txt", nil)
	req.Host = "chat.example:9000"
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	New(Config{Hub: e.hub}).ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "https://chat.example:9000/api/v1") {
		t.Fatal("must honor Host and X-Forwarded-Proto")
	}
	req = httptest.NewRequest("GET", "/llms.txt", nil)
	req.TLS = &tls.ConnectionState{}
	w = httptest.NewRecorder()
	New(Config{Hub: e.hub}).ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "https://") {
		t.Fatal("must use https over TLS")
	}

	// Public even with a token; mentions auth but never leaks the token.
	a := newEnv(t, func(c *Config) { c.Token = "hunter2" })
	r = a.do("GET", "/llms.txt", "")
	if r.status != 200 {
		t.Fatalf("llms.txt must be public, got %d", r.status)
	}
	if !strings.Contains(string(r.body), "requires a token") || !strings.Contains(string(r.body), "Authorization: Bearer <token>") {
		t.Fatal("must explain authentication when a token is configured")
	}
	if strings.Contains(string(r.body), "hunter2") {
		t.Fatal("must never contain the token")
	}
}

// spec: DOC-2
func TestLLMsTxtCoversEverything(t *testing.T) {
	e := newEnv(t, nil)
	body := string(e.do("GET", "/llms.txt", "").body)
	for _, name := range mcp.ToolNames() {
		if !strings.Contains(body, "`"+name+"(") {
			t.Errorf("llms.txt does not document MCP tool %q", name)
		}
	}
	for _, rt := range (&server{}).apiRoutes() {
		// "GET /api/v1/agents" must appear as "GET /api/v1/agents" followed by
		// a non-path character, so /agents doesn't count for /agents/{name}.
		found := false
		for i := 0; ; {
			j := strings.Index(body[i:], rt.pattern)
			if j < 0 {
				break
			}
			end := i + j + len(rt.pattern)
			if end == len(body) || !strings.ContainsRune("/{abcdefghijklmnopqrstuvwxyz", rune(body[end])) {
				found = true
				break
			}
			i = end
		}
		if !found {
			t.Errorf("llms.txt does not document route %q", rt.pattern)
		}
	}
}

// spec: DOC-3
func TestLLMsTxtStayInLoop(t *testing.T) {
	e := newEnv(t, nil)
	body := string(e.do("GET", "/llms.txt", "").body)
	for _, want := range []string{
		"botchat cannot wake you up",
		"botchat hook stop",
		"end your turn normally instead of looping",
		"It does not wake you for messages that arrive later",
		"Don't loop on `wait` while your operator is talking to you",
		"default 30",
		"call `wait` again",
		"Do not treat silence as a reason to stop",
		"post a short message saying you are leaving",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
}

// spec: DOC-4
func TestLLMsTxtAskOperator(t *testing.T) {
	e := newEnv(t, nil)
	body := string(e.do("GET", "/llms.txt", "").body)
	for _, want := range []string{
		"## Asking your operator",
		"@mention them in botchat",
		`"meta": {"kind": "human"}`,
		"Keep calling `wait` while you wait for the answer",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
}

// spec: DOC-5
func TestLLMsTxtThreadsAndChannels(t *testing.T) {
	e := newEnv(t, nil)
	body := string(e.do("GET", "/llms.txt", "").body)
	for _, want := range []string{
		"Stay in the channel your operator named",
		"Use a thread (`thread_id`) for anything about a specific message",
		"Create a channel only for a separate, long-running workstream",
		"announce it in the channel you came from",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
}
