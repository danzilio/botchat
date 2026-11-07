package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

type env struct {
	t   *testing.T
	hub *hub.Hub
	h   *Handler
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	h := hub.New(hub.Options{})
	mh := &Handler{Hub: h, Version: "test"}
	srv := httptest.NewServer(mh)
	t.Cleanup(func() { h.Close(); srv.Close() })
	return &env{t: t, hub: h, h: mh, srv: srv}
}

type rpcResult struct {
	status int
	header http.Header
	body   []byte
	resp   struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
}

func (e *env) raw(method, query, session, body string, headers ...string) rpcResult {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+"/"+query, strings.NewReader(body))
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var r rpcResult
	r.status = res.StatusCode
	r.header = res.Header
	r.body, _ = io.ReadAll(res.Body)
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(r.body, &r.resp); err != nil {
			e.t.Fatalf("bad JSON %q: %v", r.body, err)
		}
	}
	return r
}

func (e *env) initialize(query string, headers ...string) string {
	e.t.Helper()
	r := e.raw("POST", query, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, headers...)
	if r.status != 200 || r.resp.Error != nil {
		e.t.Fatalf("initialize: %d %s", r.status, r.body)
	}
	sid := r.header.Get(sessionHeader)
	if sid == "" {
		e.t.Fatal("no session id")
	}
	return sid
}

func (e *env) rpc(sid, method string, params any) rpcResult {
	e.t.Helper()
	p, _ := json.Marshal(params)
	return e.raw("POST", "", sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":%s}`, method, p))
}

type callResult struct {
	Content           []map[string]string `json:"content"`
	StructuredContent map[string]any      `json:"structuredContent"`
	IsError           bool                `json:"isError"`
}

func (e *env) call(sid, tool string, args any) callResult {
	e.t.Helper()
	r := e.rpc(sid, "tools/call", map[string]any{"name": tool, "arguments": args})
	if r.status != 200 || r.resp.Error != nil {
		e.t.Fatalf("%s: %d %s", tool, r.status, r.body)
	}
	var c callResult
	if err := json.Unmarshal(r.resp.Result, &c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) ok(sid, tool string, args any) map[string]any {
	e.t.Helper()
	c := e.call(sid, tool, args)
	if c.IsError {
		e.t.Fatalf("%s(%v) failed: %v", tool, args, c.Content)
	}
	return c.StructuredContent
}

func messageTexts(v map[string]any) []string {
	var out []string
	for _, m := range v["messages"].([]any) {
		out = append(out, m.(map[string]any)["text"].(string))
	}
	return out
}

// spec: MCP-1
func TestTransport(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("")
	r := e.rpc(sid, "ping", nil)
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") || string(r.resp.ID) != "7" || r.resp.JSONRPC != "2.0" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	n := e.raw("POST", "", sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if n.status != 202 || len(n.body) != 0 {
		t.Fatalf("notification: %d %q", n.status, n.body)
	}
	if g := e.raw("GET", "", sid, ""); g.status != 405 {
		t.Fatalf("GET: %d", g.status)
	}
}

// spec: MCP-2
func TestInitializeAndSessions(t *testing.T) {
	e := newEnv(t)
	for req, want := range map[string]string{
		"2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26",
		"1999-01-01": SupportedVersions[0],
		"":           SupportedVersions[0],
	} {
		r := e.raw("POST", "", "", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`, req))
		var res struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      map[string]any `json:"serverInfo"`
			Instructions    string         `json:"instructions"`
		}
		json.Unmarshal(r.resp.Result, &res)
		if res.ProtocolVersion != want || res.Capabilities["tools"] == nil || res.ServerInfo["name"] != "botchat" || res.Instructions == "" {
			t.Errorf("requested %q: %s", req, r.body)
		}
		if r.header.Get(sessionHeader) == "" {
			t.Errorf("no session header")
		}
	}
	if r := e.rpc("", "tools/list", nil); r.status != 400 {
		t.Fatalf("missing session: %d", r.status)
	}
	if r := e.rpc("deadbeef", "tools/list", nil); r.status != 404 {
		t.Fatalf("unknown session: %d", r.status)
	}
	if r := e.raw("POST", "", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != 400 {
		t.Fatalf("notification without session: %d", r.status)
	}
	a, b := e.initialize(""), e.initialize("")
	if a == b {
		t.Fatal("session ids must be unique")
	}
}

// spec: MCP-3
func TestIdentity(t *testing.T) {
	e := newEnv(t)

	anon := e.initialize("")
	c := e.call(anon, "post", map[string]any{"channel": "general", "text": "hi"})
	if !c.IsError || !strings.Contains(c.Content[0]["text"], "join") {
		t.Fatalf("post without identity: %+v", c)
	}
	c = e.call(anon, "react", map[string]any{"message_id": 1, "emoji": "x"})
	if !c.IsError {
		t.Fatal("react without identity must fail")
	}
	c = e.call(anon, "wait", map[string]any{"mentions_only": true, "timeout_seconds": 0})
	if !c.IsError {
		t.Fatal("mentions_only without identity must fail")
	}
	j := e.ok(anon, "join", map[string]any{"name": "joiner", "meta": map[string]string{"harness": "test"}})
	if j["agent"].(map[string]any)["name"] != "joiner" {
		t.Fatalf("%v", j)
	}
	m := e.ok(anon, "post", map[string]any{"channel": "general", "text": "hi"})
	if m["from"] != "joiner" {
		t.Fatalf("%v", m)
	}
	if c := e.call(anon, "join", map[string]any{"name": "all"}); !c.IsError {
		t.Fatal("reserved name must fail")
	}

	byQuery := e.initialize("?agent=queried")
	if m := e.ok(byQuery, "post", map[string]any{"channel": "general", "text": "q"}); m["from"] != "queried" {
		t.Fatalf("%v", m)
	}
	// A preset identity can't be renamed, but meta can be updated.
	c = e.call(byQuery, "join", map[string]any{"name": "someone-else"})
	if !c.IsError || !strings.Contains(c.Content[0]["text"], `"queried"`) {
		t.Fatalf("rename must fail and name the current identity: %+v", c)
	}
	e.ok(byQuery, "join", map[string]any{"name": "queried", "meta": map[string]string{"harness": "x"}})
	if a, _ := e.hub.Agent("queried"); a.Meta["harness"] != "x" {
		t.Fatalf("meta update: %+v", a)
	}
	if _, err := e.hub.Agent("someone-else"); err == nil {
		t.Fatal("failed rename must not register the other name")
	}
	// The same applies after join.
	if c := e.call(anon, "join", map[string]any{"name": "joiner2"}); !c.IsError {
		t.Fatal("re-join under another name must fail")
	}
	byHeader := e.initialize("", "X-Botchat-Agent", "headed")
	if m := e.ok(byHeader, "post", map[string]any{"channel": "general", "text": "h"}); m["from"] != "headed" {
		t.Fatalf("%v", m)
	}
	if _, err := e.hub.Agent("queried"); err != nil {
		t.Fatal("initialize with identity must register the agent")
	}
	r := e.raw("POST", "?agent=bad%20name", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if r.resp.Error == nil || r.resp.Error.Code != codeInvalidParams {
		t.Fatalf("invalid identity: %s", r.body)
	}
}

// spec: MCP-4
func TestToolsList(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("")
	r := e.rpc(sid, "tools/list", nil)
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	json.Unmarshal(r.resp.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %s: bad definition", tl.Name)
		}
	}
	want := []string{"join", "list_channels", "create_channel", "list_agents", "post", "read", "read_thread", "wait", "react"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}
}

// spec: MCP-4
func TestToolBehaviour(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("?agent=alice")

	ch := e.ok(sid, "create_channel", map[string]any{"name": "dev", "topic": "dev talk"})
	if ch["name"] != "dev" || ch["topic"] != "dev talk" {
		t.Fatalf("%v", ch)
	}
	chs := e.ok(sid, "list_channels", nil)["channels"].([]any)
	if len(chs) != 2 {
		t.Fatalf("%v", chs)
	}
	agents := e.ok(sid, "list_agents", map[string]any{})["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["online"] != true {
		t.Fatalf("%v", agents)
	}

	root := e.ok(sid, "post", map[string]any{"channel": "dev", "text": "root"})
	rootID := root["id"].(float64)
	reply := e.ok(sid, "post", map[string]any{"thread_id": rootID, "text": "reply"})
	if reply["thread_id"].(float64) != rootID || reply["channel"] != "dev" {
		t.Fatalf("%v", reply)
	}
	if got := messageTexts(e.ok(sid, "read", map[string]any{"channel": "dev"})); !slices.Equal(got, []string{"root"}) {
		t.Fatalf("read: %v", got)
	}
	if got := messageTexts(e.ok(sid, "read", map[string]any{"channel": "dev", "include_replies": true})); !slices.Equal(got, []string{"root", "reply"}) {
		t.Fatalf("read replies: %v", got)
	}
	th := e.ok(sid, "read_thread", map[string]any{"message_id": reply["id"]})
	if th["root"].(map[string]any)["text"] != "root" || len(th["replies"].([]any)) != 1 {
		t.Fatalf("%v", th)
	}
	rx := e.ok(sid, "react", map[string]any{"message_id": rootID, "emoji": "👍"})
	if len(rx["reactions"].(map[string]any)["👍"].([]any)) != 1 {
		t.Fatalf("%v", rx)
	}
	rx = e.ok(sid, "react", map[string]any{"message_id": rootID, "emoji": "👍", "remove": true})
	if _, ok := rx["reactions"]; ok {
		t.Fatalf("%v", rx)
	}
	if c := e.call(sid, "read", map[string]any{"channel": "missing"}); !c.IsError {
		t.Fatal("read of missing channel must be a tool error")
	}
	if c := e.call(sid, "post", map[string]any{"text": "no channel"}); !c.IsError {
		t.Fatal("post without channel/thread must be a tool error")
	}
}

// spec: MCP-5
func TestErrors(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("?agent=a")

	if r := e.rpc(sid, "no/such/method", nil); r.resp.Error == nil || r.resp.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method: %s", r.body)
	}
	if r := e.rpc(sid, "tools/call", map[string]any{"name": "nope"}); r.resp.Error == nil || r.resp.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool: %s", r.body)
	}
	if r := e.rpc(sid, "tools/call", map[string]any{"name": "post", "arguments": map[string]any{"text": 5}}); r.resp.Error == nil || r.resp.Error.Code != codeInvalidParams {
		t.Fatalf("malformed args: %s", r.body)
	}
	if r := e.raw("POST", "", sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`); r.resp.Error == nil || r.resp.Error.Code != codeInvalidParams {
		t.Fatalf("malformed params: %s", r.body)
	}
	if r := e.raw("POST", "", sid, `{nope`); r.resp.Error == nil || r.resp.Error.Code != codeParse || string(r.resp.ID) != "null" {
		t.Fatalf("parse error: %s", r.body)
	}
	if r := e.raw("POST", "", sid, `{"jsonrpc":"1.0","id":1,"method":"ping"}`); r.resp.Error == nil || r.resp.Error.Code != codeInvalidRequest {
		t.Fatalf("invalid request: %s", r.body)
	}
	c := e.call(sid, "post", map[string]any{"channel": "general", "text": "   "})
	if !c.IsError || c.Content[0]["type"] != "text" || c.Content[0]["text"] == "" {
		t.Fatalf("tool error: %+v", c)
	}
	big := strings.Repeat("x", 1<<20+1)
	if r := e.raw("POST", "", sid, big); r.status != 413 {
		t.Fatalf("too large: %d", r.status)
	}
}

// spec: MCP-6
func TestResultShape(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("?agent=a")
	c := e.call(sid, "post", map[string]any{"channel": "general", "text": "hello"})
	if len(c.Content) != 1 || c.Content[0]["type"] != "text" {
		t.Fatalf("%+v", c)
	}
	var fromText map[string]any
	if err := json.Unmarshal([]byte(c.Content[0]["text"]), &fromText); err != nil {
		t.Fatal(err)
	}
	if fromText["text"] != "hello" || c.StructuredContent["text"] != "hello" {
		t.Fatalf("%+v", c)
	}
}

// spec: MCP-7
func TestDeleteSession(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("")
	if r := e.raw("DELETE", "", sid, ""); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	if r := e.rpc(sid, "ping", nil); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}
	if r := e.raw("DELETE", "", sid, ""); r.status != 404 {
		t.Fatalf("double delete: %d", r.status)
	}
	if r := e.raw("DELETE", "", "", ""); r.status != 400 {
		t.Fatalf("delete without header: %d", r.status)
	}
}

// spec: MCP-8
func TestWaitCursor(t *testing.T) {
	e := newEnv(t)
	other := e.initialize("?agent=bob")
	e.ok(other, "post", map[string]any{"channel": "general", "text": "before session"})

	sid := e.initialize("?agent=alice")
	// Cursor starts at the latest message: old messages are not returned.
	if got := e.ok(sid, "wait", map[string]any{"timeout_seconds": 0}); len(got["messages"].([]any)) != 0 || got["timed_out"] != true {
		t.Fatalf("initial wait: %v", got)
	}
	e.ok(sid, "post", map[string]any{"channel": "general", "text": "my own"})
	e.ok(other, "post", map[string]any{"channel": "general", "text": "one"})
	e.ok(other, "post", map[string]any{"channel": "general", "text": "two"})
	if got := messageTexts(e.ok(sid, "wait", map[string]any{"timeout_seconds": 1})); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("first wait: %v", got)
	}
	// Cursor advanced: nothing new now.
	if got := e.ok(sid, "wait", map[string]any{"timeout_seconds": 0.05}); len(got["messages"].([]any)) != 0 {
		t.Fatalf("second wait: %v", got)
	}
	// Explicit after overrides the cursor.
	if got := messageTexts(e.ok(sid, "wait", map[string]any{"after": 0, "timeout_seconds": 0})); len(got) != 3 {
		t.Fatalf("after=0: %v", got)
	}

	// Blocking wait wakes on a new message; mentions_only filters.
	done := make(chan map[string]any, 1)
	go func() { done <- e.ok(sid, "wait", map[string]any{"mentions_only": true, "timeout_seconds": 5}) }()
	time.Sleep(50 * time.Millisecond)
	e.ok(other, "post", map[string]any{"channel": "general", "text": "not for alice"})
	e.ok(other, "post", map[string]any{"channel": "general", "text": "@alice look"})
	select {
	case got := <-done:
		if texts := messageTexts(got); !slices.Equal(texts, []string{"@alice look"}) {
			t.Fatalf("mentions wait: %v", texts)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not wake")
	}

	// Channel / thread filters.
	root := e.ok(other, "post", map[string]any{"channel": "ops", "text": "ops root"})
	e.ok(other, "post", map[string]any{"thread_id": root["id"], "text": "ops reply"})
	if got := messageTexts(e.ok(sid, "wait", map[string]any{"thread_id": root["id"], "after": 0, "timeout_seconds": 0})); !slices.Equal(got, []string{"ops root", "ops reply"}) {
		t.Fatalf("thread wait: %v", got)
	}
	if got := messageTexts(e.ok(sid, "wait", map[string]any{"channel": "ops", "after": 0, "limit": 1, "timeout_seconds": 0})); !slices.Equal(got, []string{"ops root"}) {
		t.Fatalf("channel+limit wait: %v", got)
	}

	// Anonymous sessions see everything (nothing to exclude).
	anon := e.initialize("")
	e.ok(other, "post", map[string]any{"channel": "general", "text": "for anon"})
	if got := messageTexts(e.ok(anon, "wait", map[string]any{"timeout_seconds": 1})); !slices.Equal(got, []string{"for anon"}) {
		t.Fatalf("anon wait: %v", got)
	}
}

// spec: MCP-9
func TestSessionLimit(t *testing.T) {
	e := newEnv(t)
	e.h.MaxSessions = 3
	first := e.initialize("")
	second := e.initialize("")
	e.initialize("")
	e.rpc(first, "ping", nil) // first is now most recently used
	e.initialize("")          // evicts second
	if e.h.SessionCount() != 3 {
		t.Fatalf("sessions = %d", e.h.SessionCount())
	}
	if r := e.rpc(second, "ping", nil); r.status != 404 {
		t.Fatalf("LRU session should be evicted: %d", r.status)
	}
	if r := e.rpc(first, "ping", nil); r.status != 200 {
		t.Fatalf("recently used session should survive: %d", r.status)
	}
	if DefaultMaxSessions != 4096 {
		t.Fatal("default must be 4096")
	}
}

// spec: MCP-10
func TestSessionRecovery(t *testing.T) {
	e := newEnv(t)
	sid := e.initialize("?agent=alice")

	// Simulate a restart: the session table is empty.
	restarted := &Handler{Hub: e.hub, Version: "test"}
	e.srv.Config.Handler = restarted

	// A client that keeps its old session ID and ?agent= is still served.
	r := e.raw("POST", "?agent=alice", sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"post","arguments":{"channel":"general","text":"back"}}}`)
	if r.status != 200 || r.resp.Error != nil {
		t.Fatalf("recovered post: %d %s", r.status, r.body)
	}
	var c callResult
	json.Unmarshal(r.resp.Result, &c)
	if c.IsError || c.StructuredContent["from"] != "alice" {
		t.Fatalf("identity not restored: %s", r.body)
	}
	// Later requests reuse the recovered session rather than creating more.
	if r := e.raw("POST", "?agent=alice", sid, `{"jsonrpc":"2.0","id":8,"method":"ping"}`); r.status != 200 {
		t.Fatalf("ping: %d", r.status)
	}
	if n := restarted.SessionCount(); n != 1 {
		t.Fatalf("sessions: %d", n)
	}

	// The X-Botchat-Agent header works too.
	if r := e.raw("POST", "", "cafe", `{"jsonrpc":"2.0","id":9,"method":"ping"}`, "X-Botchat-Agent", "bob"); r.status != 200 {
		t.Fatalf("header recovery: %d %s", r.status, r.body)
	}
	// Without an agent name the client must re-initialize.
	if r := e.rpc("deadbeef", "tools/list", nil); r.status != 404 {
		t.Fatalf("anonymous unknown session: %d", r.status)
	}
	// Invalid agent names are not recovered.
	if r := e.raw("POST", "?agent=bad%20name", "f00d", `{"jsonrpc":"2.0","id":9,"method":"ping"}`); r.status != 404 {
		t.Fatalf("invalid agent: %d", r.status)
	}
}

// spec: DOC-3
func TestStayInLoopGuidance(t *testing.T) {
	if !strings.Contains(instructions, "Nobody can reach you once your turn ends") ||
		!strings.Contains(instructions, "post that you are leaving") {
		t.Error("initialize instructions must tell agents to keep waiting and announce departure")
	}
	for _, tl := range toolDefs {
		if tl.Name == "wait" && !strings.Contains(tl.Description, "nobody can reach you once you end your turn") {
			t.Error("wait tool description must tell agents to loop")
		}
	}
}

// spec: DOC-4
func TestAskOperatorGuidance(t *testing.T) {
	if !strings.Contains(instructions, "@mention your operator") || !strings.Contains(instructions, "kind=human") {
		t.Error("initialize instructions must tell agents how to ask their operator")
	}
}
