package listen

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danzilio/botchat/internal/client"
	"github.com/danzilio/botchat/internal/hub"
	"github.com/danzilio/botchat/internal/server"
)

func init() {
	minBackoff = 20 * time.Millisecond
	maxBackoff = 50 * time.Millisecond
	client.PollTimeout = 200 * time.Millisecond
}

// lines is a concurrency-safe stdout that tests can wait on.
type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lines) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(l.String(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in:\n%s", sub, l.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newServer(t *testing.T, token string) (*hub.Hub, string) {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := httptest.NewServer(server.New(server.Config{Hub: h, Token: token}))
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv.URL
}

func post(t *testing.T, h *hub.Hub, from, text string) hub.Message {
	t.Helper()
	m, err := h.Post(hub.PostInput{Channel: "general", From: from, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// start runs Main in the background and returns its stdout and a function
// that cancels it and returns the exit code.
func start(t *testing.T, args ...string) (*lines, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &lines{}
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, args, func(string) string { return "" }, out, &bytes.Buffer{})
	}()
	stop := func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(5 * time.Second):
			t.Fatal("listen did not stop")
			return -1
		}
	}
	t.Cleanup(func() { cancel() })
	return out, stop
}

// spec: LSN-1
func TestFlags(t *testing.T) {
	env := map[string]string{"BOTCHAT_URL": "http://env:1/", "BOTCHAT_AGENT": "envbot", "BOTCHAT_TOKEN": "secret"}
	getenv := func(k string) string { return env[k] }
	var stderr bytes.Buffer
	c, err := parse(nil, getenv, &stderr)
	if err != nil || c.URL != "http://env:1" || c.Agent != "envbot" || c.Token != "secret" || c.All || c.Once {
		t.Fatalf("env: %+v %v", c, err)
	}
	c, err = parse([]string{"--url", "http://flag:2", "--agent", "flagbot", "--token", "t2", "--all", "--once"}, getenv, &stderr)
	if err != nil || c.URL != "http://flag:2" || c.Agent != "flagbot" || c.Token != "t2" || !c.All || !c.Once {
		t.Fatalf("flags win: %+v %v", c, err)
	}
	c, _ = parse(nil, func(string) string { return "" }, &stderr)
	if c.URL != client.DefaultURL {
		t.Fatalf("default url: %q", c.URL)
	}

	none := func(string) string { return "" }
	for _, args := range [][]string{{}, {"--agent", "a", "--bogus"}, {"--agent", "a", "extra"}} {
		var errb bytes.Buffer
		if code := Main(context.Background(), args, none, &bytes.Buffer{}, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	var errb bytes.Buffer
	if code := Main(context.Background(), []string{"-h"}, getenv, &bytes.Buffer{}, &errb); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
	if strings.Contains(errb.String(), "secret") {
		t.Error("usage must not show a token from the environment")
	}
}

// spec: LSN-2
// spec: LSN-3
func TestPrintsMentionsSinceLastPost(t *testing.T) {
	h, url := newServer(t, "")
	post(t, h, "alice", "@bot old, before bot spoke")
	post(t, h, "bot", "hello")
	post(t, h, "alice", "@bot pending\nsecond line")

	out, stop := start(t, "--url", url, "--agent", "bot")
	out.waitFor(t, "pending second line")
	if strings.Contains(out.String(), "old, before") {
		t.Fatalf("delivered a message from before the agent's last post:\n%s", out)
	}
	if a, err := h.Agent("bot"); err != nil || !a.Online {
		t.Errorf("listening agent should be online: %+v %v", a, err)
	}

	post(t, h, "alice", "no mention")
	post(t, h, "bot", "@bot talking to myself")
	post(t, h, "carol", "@all heads up")
	root := post(t, h, "dave", "@bot thread root")
	if _, err := h.Post(hub.PostInput{ThreadID: root.ID, From: "dave", Text: "@bot " + strings.Repeat("x", 600)}); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, " in thread ")
	if code := stop(); code != 0 {
		t.Fatalf("exit %d after interrupt", code)
	}

	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != 4 {
		t.Fatalf("want 4 lines, got %d:\n%s", len(got), out)
	}
	if !strings.HasPrefix(got[0], "#general · message 3 · from alice: @bot pending second line") {
		t.Errorf("format: %q", got[0])
	}
	if !strings.Contains(got[1], "from carol: @all heads up") {
		t.Errorf("@all: %q", got[1])
	}
	want := "#general · message 8 in thread 7 · from dave: @bot " + strings.Repeat("x", 495) + " …"
	if got[3] != want {
		t.Errorf("thread/truncation:\n got %q\nwant %q", got[3], want)
	}

	// --all prints every message from other agents.
	out, stop = start(t, "--url", url, "--agent", "bot", "--all")
	post(t, h, "bot", "mine")
	post(t, h, "alice", "plain chatter")
	out.waitFor(t, "plain chatter")
	stop()
	if strings.Contains(out.String(), "mine") {
		t.Errorf("own messages must be excluded:\n%s", out)
	}
}

// spec: LSN-4
func TestOnce(t *testing.T) {
	h, url := newServer(t, "")
	go func() {
		time.Sleep(100 * time.Millisecond)
		post(t, h, "alice", "@bot wake up")
	}()
	var out bytes.Buffer
	code := Main(context.Background(), []string{"--url", url, "--agent", "bot", "--once"}, func(string) string { return "" }, &out, &bytes.Buffer{})
	if code != 0 || !strings.Contains(out.String(), "wake up") {
		t.Fatalf("exit %d, out %q", code, out.String())
	}
}

// spec: LSN-5
func TestReconnects(t *testing.T) {
	// Reserve an address, then start with nothing listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	out, stop := start(t, "--url", "http://"+addr, "--agent", "bot")
	out.waitFor(t, "botchat: lost connection to http://"+addr)

	serve := func(h *hub.Hub) *httptest.Server {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(server.New(server.Config{Hub: h}))
		srv.Listener = ln
		srv.Start()
		return srv
	}
	h1 := hub.New(hub.Options{})
	for range 5 {
		post(t, h1, "alice", "filler")
	}
	srv := serve(h1)
	out.waitFor(t, "botchat: reconnected to http://"+addr)
	post(t, h1, "alice", "@bot first server")
	out.waitFor(t, "message 6 · from alice: @bot first server")
	if n := strings.Count(out.String(), "lost connection"); n != 1 {
		t.Errorf("want one outage line, got %d:\n%s", n, out)
	}

	// Restart without persistence: IDs start over and must still arrive.
	// The listener may notice the outage or, if the new server is up first,
	// only that the latest message ID went backwards.
	h1.Close()
	srv.Close()
	h2 := hub.New(hub.Options{})
	srv2 := serve(h2)
	defer func() { h2.Close(); srv2.Close() }()
	time.Sleep(200 * time.Millisecond)
	post(t, h2, "alice", "@bot second server")
	out.waitFor(t, "message 1 · from alice: @bot second server")
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// spec: LSN-5
func TestRejectedToken(t *testing.T) {
	_, url := newServer(t, "right")
	var stderr bytes.Buffer
	code := Main(context.Background(), []string{"--url", url, "--agent", "bot", "--token", "wrong"}, func(string) string { return "" }, &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "401") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	_, url = newServer(t, "right")
	out, stop := start(t, "--url", url, "--agent", "bot", "--token", "right")
	time.Sleep(100 * time.Millisecond)
	if code := stop(); code != 0 || strings.Contains(out.String(), "lost") {
		t.Fatalf("correct token: exit %d, out %q", code, out)
	}
}

// spec: LSN-3
func TestFormat(t *testing.T) {
	got := Format(client.Message{ID: 9, Channel: "dev", From: "a", Text: "x\r\ny\rz"})
	if got != "#dev · message 9 · from a: x y z" {
		t.Fatal(got)
	}
}

// spec: LSN-2
func TestStaysOnlineWhileIdle(t *testing.T) {
	old := client.PollTimeout
	client.PollTimeout = 100 * time.Millisecond
	defer func() { client.PollTimeout = old }()

	h := hub.New(hub.Options{PresenceWindow: 300 * time.Millisecond})
	srv := httptest.NewServer(server.New(server.Config{Hub: h}))
	defer func() { h.Close(); srv.Close() }()

	_, stop := start(t, "--url", srv.URL, "--agent", "bot")
	defer stop()
	time.Sleep(150 * time.Millisecond)
	for range 10 { // well past the presence window, with nothing posted
		if a, err := h.Agent("bot"); err != nil || !a.Online {
			t.Fatalf("idle listener went offline: %+v %v", a, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// spec: LSN-5
func TestBacksOffWhenLongPollsFail(t *testing.T) {
	h := hub.New(hub.Options{})
	api := server.New(server.Config{Hub: h})
	var mu sync.Mutex
	failed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probes (no agent) succeed; the agent's long polls fail.
		if r.URL.Path == "/api/v1/wait" && r.URL.Query().Get("agent") != "" {
			mu.Lock()
			failed++
			mu.Unlock()
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer func() { h.Close(); srv.Close() }()

	out, stop := start(t, "--url", srv.URL, "--agent", "bot")
	time.Sleep(300 * time.Millisecond)
	stop()
	mu.Lock()
	n := failed
	mu.Unlock()
	// Backoff of 20, 40, 50, 50… ms allows about 7 attempts in 300 ms.
	if n < 2 || n > 10 {
		t.Errorf("%d failed long polls in 300ms; want backoff between them", n)
	}
	if strings.Contains(out.String(), "reconnected") || strings.Count(out.String(), "lost connection") != 1 {
		t.Errorf("must report one outage and no recovery while long polls fail:\n%s", out)
	}
}
