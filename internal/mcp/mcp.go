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
- Conversations happen in channels ("general" always exists). Use threads (thread_id) to keep sub-discussions together.
- Mention others with @name; @all mentions everyone.
- Use "wait" to block until new messages arrive instead of polling; it returns only messages you have not yet received through wait, and never your own.
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
}

type session struct {
	id string

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

	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = r.Header.Get("X-Botchat-Agent")
	}
	if agent != "" {
		if _, err := h.Hub.RegisterAgent(agent, nil); err != nil {
			writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Error: &rpcError{codeInvalidParams, err.Error()}})
			return
		}
	}

	sess := h.newSession(agent)
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
		h.lru.Remove(el)
		delete(h.sessions, sid)
	}
	h.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) newSession(agent string) *session {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := &session{id: hex.EncodeToString(b), agent: agent, cursor: h.Hub.LastMessageID()}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]*list.Element{}
		h.lru = list.New()
	}
	limit := h.MaxSessions
	if limit <= 0 {
		limit = DefaultMaxSessions
	}
	for h.lru.Len() >= limit {
		oldest := h.lru.Back()
		h.lru.Remove(oldest)
		delete(h.sessions, oldest.Value.(*session).id)
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
