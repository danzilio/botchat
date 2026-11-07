// Package server exposes the hub over HTTP: the JSON API, server-sent events,
// and mounting points for the MCP endpoint and web UI. See SPEC.md section 5.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

// MaxBodyBytes is the request body limit (API-3).
const MaxBodyBytes = 1 << 20

// Config configures the HTTP handler.
type Config struct {
	Hub     *hub.Hub
	Version string
	// Token, if non-empty, is required on /api/ and /mcp (API-4).
	Token string
	// MCP is mounted at /mcp if non-nil.
	MCP http.Handler
	// Web is mounted at / if non-nil.
	Web http.Handler
	// KeepAlive is the SSE comment interval (SSE-3). Default 15s.
	KeepAlive time.Duration
	// MaxWait caps /api/v1/wait timeouts (API-7). Default 120s.
	MaxWait time.Duration
}

type server struct {
	Config
}

// New returns the root HTTP handler.
func New(cfg Config) http.Handler {
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 15 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 120 * time.Second
	}
	s := &server{cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /llms.txt", s.llmsTxt)
	for _, rt := range s.apiRoutes() {
		mux.HandleFunc(rt.pattern, rt.handler)
	}

	if cfg.MCP != nil {
		for _, m := range []string{"GET", "POST", "DELETE"} {
			mux.Handle(m+" /mcp", cfg.MCP)
		}
	}
	if cfg.Web != nil {
		mux.Handle("GET /", cfg.Web)
	}
	return s.guard(mux)
}

type route struct {
	pattern string
	handler http.HandlerFunc
}

// apiRoutes is the HTTP API (SPEC.md section 5). llms.txt is tested against
// this list, so new routes must be documented there too (DOC-2).
func (s *server) apiRoutes() []route {
	return []route{
		{"GET /api/v1/agents", s.listAgents},
		{"GET /api/v1/agents/{name}", s.getAgent},
		{"PUT /api/v1/agents/{name}", s.putAgent},

		{"GET /api/v1/channels", s.listChannels},
		{"GET /api/v1/channels/{name}", s.getChannel},
		{"PUT /api/v1/channels/{name}", s.putChannel},
		{"GET /api/v1/channels/{name}/messages", s.listMessages},
		{"POST /api/v1/channels/{name}/messages", s.postMessage},

		{"GET /api/v1/messages/{id}", s.getMessage},
		{"GET /api/v1/messages/{id}/thread", s.getThread},
		{"PUT /api/v1/messages/{id}/reactions", s.react(true)},
		{"DELETE /api/v1/messages/{id}/reactions", s.react(false)},

		{"GET /api/v1/wait", s.wait},
		{"GET /api/v1/events", s.events},
	}
}

// guard applies authentication (API-4) and origin checks (API-9).
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp"
		if !protected {
			next.ServeHTTP(w, r)
			return
		}
		if s.Token != "" && !tokenOK(r, s.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="botchat"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin := r.Header.Get("Origin"); origin != "" && !sameHost(origin, r.Host) {
				writeError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func tokenOK(r *http.Request, token string) bool {
	got := r.URL.Query().Get("token")
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			got = after
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeHubError(w http.ResponseWriter, err error) {
	switch hub.ErrorCode(err) {
	case hub.CodeNotFound:
		writeError(w, http.StatusNotFound, hub.CodeNotFound, err.Error())
	case hub.CodeInvalid:
		writeError(w, http.StatusBadRequest, hub.CodeInvalid, err.Error())
	case hub.CodeUnavailable:
		writeError(w, http.StatusServiceUnavailable, hub.CodeUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// decode reads a JSON body into v. An empty body leaves v untouched.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds 1 MiB")
		return false
	}
	writeError(w, http.StatusBadRequest, hub.CodeInvalid, "invalid JSON body: "+err.Error())
	return false
}

type queryReader struct {
	q   url.Values
	err error
}

func (qr *queryReader) uint(name string, def uint64) uint64 {
	s := qr.q.Get(name)
	if s == "" || qr.err != nil {
		return def
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		qr.err = errors.New("query parameter " + name + " must be a non-negative integer")
	}
	return v
}

func (qr *queryReader) bool(name string) bool {
	s := qr.q.Get(name)
	if s == "" || qr.err != nil {
		return false
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		qr.err = errors.New("query parameter " + name + " must be a boolean")
	}
	return v
}

func pathID(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(w, http.StatusBadRequest, hub.CodeInvalid, "message id must be a positive integer")
		return 0, false
	}
	return id, true
}

func clamp(v, def, max uint64) int {
	if v == 0 {
		v = def
	}
	if v > max {
		v = max
	}
	return int(v)
}
