package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

type env struct {
	t   *testing.T
	hub *hub.Hub
	srv *httptest.Server
}

func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	h := hub.New(hub.Options{})
	cfg := Config{Hub: h, Version: "test", KeepAlive: 50 * time.Millisecond}
	cfg.MCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	cfg.Web = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "web") })
	if mod != nil {
		mod(&cfg)
	}
	srv := httptest.NewServer(New(cfg))
	t.Cleanup(func() { h.Close(); srv.Close() })
	return &env{t: t, hub: h, srv: srv}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", r.body, err)
	}
	return v
}

func (e *env) do(method, path, body string, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (e *env) expect(method, path, body string, status int) map[string]any {
	e.t.Helper()
	r := e.do(method, path, body)
	if r.status != status {
		e.t.Fatalf("%s %s = %d %s, want %d", method, path, r.status, r.body, status)
	}
	if !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		e.t.Fatalf("%s %s content-type = %q", method, path, r.header.Get("Content-Type"))
	}
	return r.json(e.t)
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestHealthz(t *testing.T) {
	e := newEnv(t, nil)
	v := e.expect("GET", "/healthz", "", 200)
	if v["ok"] != true || v["version"] != "test" {
		t.Fatalf("%v", v)
	}
}

// spec: API-1
func TestErrorFormat(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/api/v1/agents/nobody", "", 404, "not_found"},
		{"PUT", "/api/v1/agents/bad%20name", "", 400, "invalid_argument"},
		{"POST", "/api/v1/channels/general/messages", `{"from":"a","text":""}`, 400, "invalid_argument"},
		{"POST", "/api/v1/channels/general/messages", `not json`, 400, "invalid_argument"},
		{"GET", "/api/v1/messages/abc", "", 400, "invalid_argument"},
		{"GET", "/api/v1/messages/0", "", 400, "invalid_argument"},
		{"GET", "/api/v1/messages/42", "", 404, "not_found"},
		{"GET", "/api/v1/channels/general/messages?limit=x", "", 400, "invalid_argument"},
		{"GET", "/api/v1/channels/general/messages?replies=maybe", "", 400, "invalid_argument"},
	}
	for _, c := range cases {
		v := e.expect(c.method, c.path, c.body, c.status)
		if errCode(v) != c.code {
			t.Errorf("%s %s: code %q, want %q", c.method, c.path, errCode(v), c.code)
		}
		if msg := v["error"].(map[string]any)["message"]; msg == "" {
			t.Errorf("%s %s: empty message", c.method, c.path)
		}
	}
	// 201 on POST creates, 200 elsewhere.
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"x"}`, 201)
	e.expect("PUT", "/api/v1/channels/dev", `{}`, 200)
	e.expect("PUT", "/api/v1/agents/a", ``, 200)
}

// spec: API-2
func TestListEndpointsWrap(t *testing.T) {
	e := newEnv(t, nil)
	e.expect("PUT", "/api/v1/agents/bob", `{"meta":{"role":"tester"}}`, 200)
	if v := e.expect("GET", "/api/v1/agents", "", 200); len(v["agents"].([]any)) != 1 {
		t.Fatalf("%v", v)
	}
	if v := e.expect("GET", "/api/v1/channels", "", 200); len(v["channels"].([]any)) != 1 {
		t.Fatalf("%v", v)
	}
	v := e.expect("GET", "/api/v1/channels/general/messages", "", 200)
	if msgs, ok := v["messages"].([]any); !ok || len(msgs) != 0 {
		t.Fatalf("empty list must be [] not null: %s", e.do("GET", "/api/v1/channels/general/messages", "").body)
	}
	a := e.expect("GET", "/api/v1/agents/bob", "", 200)
	if a["meta"].(map[string]any)["role"] != "tester" {
		t.Fatalf("%v", a)
	}
	topic := e.expect("PUT", "/api/v1/channels/dev", `{"topic":"the dev channel"}`, 200)
	if topic["topic"] != "the dev channel" {
		t.Fatalf("%v", topic)
	}
	if c := e.expect("GET", "/api/v1/channels/dev", "", 200); c["topic"] != "the dev channel" {
		t.Fatalf("%v", c)
	}
	e.expect("GET", "/api/v1/channels/nope", "", 404)
}

// spec: API-3
func TestBodyLimit(t *testing.T) {
	e := newEnv(t, nil)
	big := `{"from":"a","text":"` + strings.Repeat("x", MaxBodyBytes) + `"}`
	v := e.expect("POST", "/api/v1/channels/general/messages", big, 413)
	if errCode(v) != "too_large" {
		t.Fatalf("%v", v)
	}
}

// spec: API-4
// spec: CLI-3
func TestAuth(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Token = "s3cret" })
	for _, p := range []string{"/api/v1/agents", "/api/v1/events", "/mcp"} {
		r := e.do("GET", p, "")
		if r.status != 401 || errCode(r.json(t)) != "unauthorized" || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s without token = %d %s", p, r.status, r.body)
		}
		if r := e.do("GET", p, "", "Authorization", "Bearer wrong"); r.status != 401 {
			t.Errorf("GET %s with wrong token = %d", p, r.status)
		}
	}
	if r := e.do("GET", "/api/v1/agents", "", "Authorization", "Bearer s3cret"); r.status != 200 {
		t.Errorf("bearer token rejected: %d", r.status)
	}
	if r := e.do("GET", "/api/v1/agents?token=s3cret", ""); r.status != 200 {
		t.Errorf("query token rejected: %d", r.status)
	}
	if r := e.do("POST", "/mcp?token=s3cret", ""); r.status != 299 {
		t.Errorf("mcp with token = %d", r.status)
	}
	// Public paths.
	if r := e.do("GET", "/healthz", ""); r.status != 200 {
		t.Errorf("healthz = %d", r.status)
	}
	if r := e.do("GET", "/", ""); r.status != 200 || string(r.body) != "web" {
		t.Errorf("web = %d", r.status)
	}
}

// spec: API-5
func TestListMessagesQuery(t *testing.T) {
	e := newEnv(t, nil)
	for i := range 5 {
		e.expect("POST", "/api/v1/channels/general/messages", fmt.Sprintf(`{"from":"a","text":"m%d"}`, i), 201)
	}
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"r","thread_id":1}`, 201)
	ids := func(v map[string]any) (out []int) {
		for _, m := range v["messages"].([]any) {
			out = append(out, int(m.(map[string]any)["id"].(float64)))
		}
		return
	}
	cases := map[string]string{
		"":                      "[1 2 3 4 5]",
		"?replies=true":         "[1 2 3 4 5 6]",
		"?after=3":              "[4 5]",
		"?before=5&limit=2":     "[3 4]",
		"?limit=2":              "[1 2]",
		"?after=1&before=4":     "[2 3]",
		"?limit=9999&replies=1": "[1 2 3 4 5 6]",
	}
	for q, want := range cases {
		got := fmt.Sprint(ids(e.expect("GET", "/api/v1/channels/general/messages"+q, "", 200)))
		if got != want {
			t.Errorf("%q: got %s, want %s", q, got, want)
		}
	}
	if clamp(0, 50, 500) != 50 || clamp(9999, 50, 500) != 500 {
		t.Fatal("limit defaults to 50, max 500")
	}
}

// spec: API-6
func TestThreadEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"root"}`, 201)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"b","text":"reply","thread_id":1}`, 201)
	for _, id := range []string{"1", "2"} {
		v := e.expect("GET", "/api/v1/messages/"+id+"/thread", "", 200)
		root := v["root"].(map[string]any)
		if root["id"].(float64) != 1 || root["reply_count"].(float64) != 1 || len(v["replies"].([]any)) != 1 {
			t.Fatalf("thread of %s: %v", id, v)
		}
	}
	m := e.expect("GET", "/api/v1/messages/2", "", 200)
	if m["thread_id"].(float64) != 1 {
		t.Fatalf("%v", m)
	}
}

// spec: API-7
// spec: AGT-4
func TestWaitEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"old"}`, 201)

	// after=0 returns retained messages immediately.
	v := e.expect("GET", "/api/v1/wait?after=0", "", 200)
	if len(v["messages"].([]any)) != 1 || v["cursor"].(float64) != 1 {
		t.Fatalf("%v", v)
	}

	// Without after, waits for something new; times out with an empty list.
	start := time.Now()
	v = e.expect("GET", "/api/v1/wait?timeout=0.1&agent=waiter", "", 200)
	if len(v["messages"].([]any)) != 0 || v["cursor"].(float64) != 1 || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("timeout: %v after %v", v, time.Since(start))
	}
	if a, err := e.hub.Agent("waiter"); err != nil || !a.Online {
		t.Fatalf("wait with agent= must register and mark online: %+v %v", a, err)
	}

	// Blocks until a matching message is posted.
	got := make(chan map[string]any, 1)
	go func() {
		got <- e.expect("GET", "/api/v1/wait?mention=bob&exclude_from=bob&timeout=5", "", 200)
	}()
	time.Sleep(50 * time.Millisecond)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"bob","text":"@bob note to self"}`, 201)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"no mention"}`, 201)
	e.expect("POST", "/api/v1/channels/other/messages", `{"from":"a","text":"hey @bob"}`, 201)
	select {
	case v := <-got:
		msgs := v["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "hey @bob" || v["cursor"].(float64) != 4 {
			t.Fatalf("%v", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return")
	}

	for _, q := range []string{"timeout=-1", "timeout=abc", "after=x", "thread_id=-2"} {
		e.expect("GET", "/api/v1/wait?"+q, "", 400)
	}
	e.expect("GET", "/api/v1/wait?agent=bad%20name&after=0", "", 400)
}

func TestWaitTimeoutCapped(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxWait = 50 * time.Millisecond })
	start := time.Now()
	e.expect("GET", "/api/v1/wait?timeout=100", "", 200)
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout must be capped")
	}
}

// spec: API-8
func TestUnknownRoutes(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Web = nil })
	if r := e.do("GET", "/api/v1/nope", ""); r.status != 404 {
		t.Errorf("unknown path = %d", r.status)
	}
	if r := e.do("POST", "/api/v1/agents", "{}"); r.status != 405 {
		t.Errorf("wrong method = %d", r.status)
	}
	if r := e.do("DELETE", "/api/v1/channels/general", ""); r.status != 405 {
		t.Errorf("wrong method = %d", r.status)
	}
}

// spec: API-9
func TestOriginCheck(t *testing.T) {
	e := newEnv(t, nil)
	host := strings.TrimPrefix(e.srv.URL, "http://")
	body := `{"from":"a","text":"x"}`
	if r := e.do("POST", "/api/v1/channels/general/messages", body, "Origin", "http://evil.example"); r.status != 403 || errCode(r.json(t)) != "forbidden" {
		t.Errorf("cross-origin POST = %d", r.status)
	}
	if r := e.do("POST", "/mcp", "{}", "Origin", "http://evil.example"); r.status != 403 {
		t.Errorf("cross-origin MCP = %d", r.status)
	}
	if r := e.do("POST", "/api/v1/channels/general/messages", body, "Origin", "http://"+host); r.status != 201 {
		t.Errorf("same-origin POST = %d", r.status)
	}
	if r := e.do("POST", "/api/v1/channels/general/messages", body); r.status != 201 {
		t.Errorf("no-origin POST = %d", r.status)
	}
	if r := e.do("GET", "/api/v1/agents", "", "Origin", "http://evil.example"); r.status != 200 {
		t.Errorf("cross-origin GET = %d", r.status)
	}
	if r := e.do("POST", "/api/v1/channels/general/messages", body, "Origin", "null"); r.status != 403 {
		t.Errorf("opaque origin = %d", r.status)
	}
}

func TestReactionsEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"x"}`, 201)
	m := e.expect("PUT", "/api/v1/messages/1/reactions", `{"from":"b","emoji":"👍"}`, 200)
	if len(m["reactions"].(map[string]any)["👍"].([]any)) != 1 {
		t.Fatalf("%v", m)
	}
	m = e.expect("DELETE", "/api/v1/messages/1/reactions", `{"from":"b","emoji":"👍"}`, 200)
	if _, ok := m["reactions"]; ok {
		t.Fatalf("%v", m)
	}
	e.expect("PUT", "/api/v1/messages/9/reactions", `{"from":"b","emoji":"x"}`, 404)
	e.expect("PUT", "/api/v1/messages/1/reactions", `{"from":"b","emoji":"a b"}`, 400)
}

// ---- SSE ----

type sseEvent struct {
	id, typ, data string
}

type sseStream struct {
	t      *testing.T
	res    *http.Response
	events chan sseEvent
	lines  chan string // comment lines
}

func (e *env) stream(path string, headers ...string) *sseStream {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	s := &sseStream{t: e.t, res: res, events: make(chan sseEvent, 100), lines: make(chan string, 100)}
	e.t.Cleanup(func() { res.Body.Close() })
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(res.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.typ != "" {
					s.events <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, ":"):
				select {
				case s.lines <- line:
				default:
				}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.typ = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data = line[6:]
			}
		}
	}()
	// Wait for the initial ": connected" comment so subscriptions are live.
	select {
	case <-s.lines:
	case <-time.After(2 * time.Second):
		e.t.Fatal("stream did not connect")
	}
	return s
}

func (s *sseStream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatal("stream closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		s.t.Fatal("timed out waiting for event")
	}
	return sseEvent{}
}

func (s *sseStream) none() {
	s.t.Helper()
	select {
	case ev := <-s.events:
		s.t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// spec: SSE-1
func TestSSEFormat(t *testing.T) {
	e := newEnv(t, nil)
	s := e.stream("/api/v1/events")
	if ct := s.res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"hi"}`, 201)
	ev := s.next()
	if ev.typ != "agent.joined" || ev.id != "1" {
		t.Fatalf("%+v", ev)
	}
	ev = s.next()
	var payload hub.Event
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		t.Fatal(err)
	}
	if ev.typ != "message.created" || ev.id != "2" || payload.Seq != 2 || payload.Message.Text != "hi" {
		t.Fatalf("%+v", ev)
	}

	// Filters as query parameters; no resume point means live only.
	f := e.stream("/api/v1/events?channel=dev&types=message.created")
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"wrong channel"}`, 201)
	e.expect("POST", "/api/v1/channels/dev/messages", `{"from":"a","text":"right"}`, 201)
	if ev := f.next(); ev.typ != "message.created" || !strings.Contains(ev.data, `"right"`) {
		t.Fatalf("%+v", ev)
	}
	f.none()

	m := e.stream("/api/v1/events?mention=bob")
	e.expect("POST", "/api/v1/channels/dev/messages", `{"from":"a","text":"hi @bob"}`, 201)
	if ev := m.next(); !strings.Contains(ev.data, "hi @bob") {
		t.Fatalf("%+v", ev)
	}
	th := e.stream("/api/v1/events?thread_id=1")
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"in thread","thread_id":1}`, 201)
	if ev := th.next(); !strings.Contains(ev.data, "in thread") {
		t.Fatalf("%+v", ev)
	}
	e.expect("GET", "/api/v1/events?since=abc", "", 400)
}

// spec: SSE-2
func TestSSEResume(t *testing.T) {
	e := newEnv(t, nil)
	for i := range 3 {
		e.expect("POST", "/api/v1/channels/general/messages", fmt.Sprintf(`{"from":"a","text":"m%d"}`, i), 201)
	}
	// seq: 1 agent.joined, 2..4 messages
	s := e.stream("/api/v1/events?since=2")
	if a, b := s.next(), s.next(); a.id != "3" || b.id != "4" {
		t.Fatalf("replay: %v %v", a, b)
	}
	e.expect("POST", "/api/v1/channels/general/messages", `{"from":"a","text":"live"}`, 201)
	if ev := s.next(); ev.id != "5" {
		t.Fatalf("live: %+v", ev)
	}

	h := e.stream("/api/v1/events", "Last-Event-ID", "4")
	if ev := h.next(); ev.id != "5" {
		t.Fatalf("Last-Event-ID: %+v", ev)
	}
	q := e.stream("/api/v1/events?since=3", "Last-Event-ID", "4")
	if ev := q.next(); ev.id != "4" {
		t.Fatalf("query wins over header: %+v", ev)
	}
}

// spec: SSE-3
func TestSSEKeepAlive(t *testing.T) {
	e := newEnv(t, nil)
	s := e.stream("/api/v1/events")
	select {
	case line := <-s.lines:
		if line != ": keepalive" {
			t.Fatalf("%q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no keepalive")
	}
}

// spec: SSE-4
func TestSSEAgentPresence(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	h := hub.New(hub.Options{Now: clock})
	e := newEnv(t, func(c *Config) { c.Hub = h })
	t.Cleanup(h.Close)

	s := e.stream("/api/v1/events?agent=watcher")
	advance(10 * time.Minute)
	if a, err := h.Agent("watcher"); err != nil || !a.Online {
		t.Fatalf("agent with open stream must be online: %+v %v", a, err)
	}
	s.res.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		advance(2 * time.Minute)
		if a, _ := h.Agent("watcher"); !a.Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent still online after its stream closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.expect("GET", "/api/v1/events?agent=bad%20name", "", 400)
}

func TestSSEEndsOnHubClose(t *testing.T) {
	e := newEnv(t, nil)
	s := e.stream("/api/v1/events")
	e.hub.Close()
	select {
	case _, ok := <-s.events:
		if ok {
			t.Fatal("expected stream to end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end")
	}
}
