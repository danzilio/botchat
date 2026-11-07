// Package mcp implements a Model Context Protocol server (Streamable HTTP
// transport, JSON responses only) on top of the hub. See SPEC.md section 6.
package mcp

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/danzilio/botchat/internal/hub"
)

// SupportedVersions lists MCP protocol versions, newest first (MCP-2).
var SupportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// DefaultMaxSessions is the session limit (MCP-9).
const DefaultMaxSessions = 4096

const sessionHeader = "Mcp-Session-Id"

// JSON-RPC error codes (MCP-5).
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

const instructions = `botchat is a shared chat server where AI agents (and humans watching in a web UI) coordinate work.

- Call "join" first with a short, unique name (e.g. "claude-backend") unless your identity was preset.
- Stay in the channel your operator named ("general" always exists). Reply in a thread (thread_id) for anything about a specific message or task; keep top-level posts for starting, finishing, or being blocked. Create a channel only for a separate workstream or when asked, and announce it in the channel you came from.
- Mention others with @name; @all mentions everyone.
- Use "wait" to block until new messages arrive instead of polling; it returns only messages you have not yet received through wait, and never your own.
- Need a decision, approval, or info only a human can give? @mention your operator (the human who gave you the task; humans have meta kind=human in list_agents) with one clear question, then keep waiting.
- botchat cannot wake you once your turn ends: while collaborating, keep calling "wait" in a loop, even when it times out empty, unless something else will wake you (a Stop hook running "botchat hook stop", or your harness notifying you when a background task finishes). Don't loop while your operator is talking to you directly; answer them instead. Keep timeout_seconds at the default 30 unless you know your client allows longer tool calls. Before you stop, post that you are leaving and why.
- Keep messages concise and actionable. Say when you start, finish, or are blocked on a task.
- Full usage guide: GET /llms.txt on this server.`

// Handler serves the MCP endpoint.
type Handler struct {
	Hub     *hub.Hub
	Version string
	// MaxSessions bounds the session table (MCP-9). Default 4096.
	MaxSessions int
	// MaxWaitSeconds caps the wait tool's timeout. Default 120.
	MaxWaitSeconds float64

	mu       sync.Mutex
	sessions map[string]*list.Element // value: *session
	lru      *list.List               // front = most recently used
	run      string                   // prefix of session IDs this process issues (MCP-10)
	// retired holds IDs of recovered sessions since terminated or evicted;
	// recoveryOff is set once it would exceed the session limit (MCP-10).
	retired     map[string]struct{}
	recoveryOff bool
}

type session struct {
	id        string
	recovered bool // created by recoverSession (MCP-10)

	mu     sync.Mutex
	agent  string
	cursor uint64
}

func (s *session) identity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodDelete:
		h.delete(w, r)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Error: &rpcError{codeInvalidRequest, "invalid JSON-RPC 2.0 request"}})
		return
	}

	if req.Method == "initialize" {
		h.initialize(w, r, req)
		return
	}

	sid := r.Header.Get(sessionHeader)
	if sid == "" {
		http.Error(w, "missing "+sessionHeader+" header", http.StatusBadRequest)
		return
	}
	sess := h.lookup(sid)
	if sess == nil {
		sess = h.recoverSession(sid, requestAgent(r))
	}
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	if len(req.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "ping":
		writeRPC(w, rpcResponse{ID: req.ID, Result: struct{}{}})
	case "tools/list":
		writeRPC(w, rpcResponse{ID: req.ID, Result: map[string]any{"tools": toolDefs}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{codeInvalidParams, "invalid params: " + err.Error()}})
			return
		}
		result, rerr := h.callTool(r, sess, p.Name, p.Arguments)
		if rerr != nil {
			writeRPC(w, rpcResponse{ID: req.ID, Error: rerr})
			return
		}
		writeRPC(w, rpcResponse{ID: req.ID, Result: result})
	default:
		writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{codeMethodNotFound, "method not found: " + req.Method}})
	}
}

func (h *Handler) initialize(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)
	version := SupportedVersions[0]
	if slices.Contains(SupportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}

	agent := requestAgent(r)
	if agent != "" {
		if _, err := h.Hub.RegisterAgent(agent, nil); err != nil {
			writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Error: &rpcError{codeInvalidParams, err.Error()}})
			return
		}
	}

	sess := h.insertSession(&session{id: h.newSessionID(), agent: agent, cursor: h.Hub.LastMessageID()})
	w.Header().Set(sessionHeader, sess.id)
	writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Result: map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "botchat", "version": h.Version},
		"instructions":    instructions,
	}})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get(sessionHeader)
	if sid == "" {
		http.Error(w, "missing "+sessionHeader+" header", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	el, ok := h.sessions[sid]
	if ok {
		h.retireLocked(el)
	}
	h.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requestAgent is the agent named by the request's `agent` query parameter
// or X-Botchat-Agent header (MCP-3).
func requestAgent(r *http.Request) string {
	if agent := r.URL.Query().Get("agent"); agent != "" {
		return agent
	}
	return r.Header.Get("X-Botchat-Agent")
}

// recoverSession recreates an unknown session under the client's ID when the
// request names its agent, so clients that never re-initialize survive a
// server restart (MCP-10). Only IDs this process did not issue are recovered:
// one it issued was deleted or evicted and stays gone. The recovered session's
// cursor starts at 0 because every message in this process is newer than
// anything the client saw before the restart. It returns nil if the session
// can't be recovered.
func (h *Handler) recoverSession(id, agent string) *session {
	if agent == "" || len(id) > 128 || strings.HasPrefix(id, h.runPrefix()+"-") {
		return nil
	}
	h.mu.Lock()
	ok := h.recoverableLocked(id)
	h.mu.Unlock()
	if !ok {
		return nil
	}
	if _, err := h.Hub.RegisterAgent(agent, nil); err != nil {
		return nil
	}
	return h.insertSession(&session{id: id, agent: agent, recovered: true})
}

// recoverableLocked reports whether a recovered session may be created under
// id: it was not already recovered and then terminated or evicted in this
// process, and recovery has not been switched off (MCP-10).
func (h *Handler) recoverableLocked(id string) bool {
	if h.recoveryOff {
		return false
	}
	_, gone := h.retired[id]
	return !gone
}

// retireLocked removes a session (DELETE or eviction). A recovered session's
// ID is remembered so it cannot be recovered again. At most MaxSessions IDs
// are remembered; past that, recovery is switched off for the rest of the run
// rather than forgetting one, so a terminated session never comes back.
func (h *Handler) retireLocked(el *list.Element) {
	s := el.Value.(*session)
	h.lru.Remove(el)
	delete(h.sessions, s.id)
	if !s.recovered || h.recoveryOff {
		return
	}
	if len(h.retired) >= h.limit() {
		h.recoveryOff = true
		h.retired = nil
		return
	}
	if h.retired == nil {
		h.retired = map[string]struct{}{}
	}
	h.retired[s.id] = struct{}{}
}

func (h *Handler) limit() int {
	if h.MaxSessions <= 0 {
		return DefaultMaxSessions
	}
	return h.MaxSessions
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runPrefix identifies this process; every session ID it issues starts with
// it followed by "-".
func (h *Handler) runPrefix() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run == "" {
		h.run = randomHex(8)
	}
	return h.run
}

func (h *Handler) newSessionID() string {
	return h.runPrefix() + "-" + randomHex(16)
}

// insertSession adds s to the session table, evicting the least recently used
// session if full (MCP-9). For a recovered session it returns the existing
// session if a concurrent request recovered the same ID first, and nil if the
// ID may no longer be recovered; that check shares the lock with the insert so
// a session terminated meanwhile is never reinserted.
func (h *Handler) insertSession(s *session) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]*list.Element{}
		h.lru = list.New()
	}
	if el, ok := h.sessions[s.id]; ok { // lost a race with a concurrent recover
		h.lru.MoveToFront(el)
		return el.Value.(*session)
	}
	if s.recovered && !h.recoverableLocked(s.id) {
		return nil
	}
	for h.lru.Len() >= h.limit() {
		h.retireLocked(h.lru.Back())
	}
	h.sessions[s.id] = h.lru.PushFront(s)
	return s
}

func (h *Handler) lookup(id string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	el, ok := h.sessions[id]
	if !ok {
		return nil
	}
	h.lru.MoveToFront(el)
	return el.Value.(*session)
}

// SessionCount reports the number of live sessions.
func (h *Handler) SessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
