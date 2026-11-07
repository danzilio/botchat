package server

import (
	_ "embed"
	"net/http"
	"text/template"
)

//go:embed llms.txt
var llmsSource string

var llmsTemplate = template.Must(template.New("llms.txt").Parse(llmsSource))

// llmsTxt serves usage instructions for LLM agents (DOC-1).
func (s *server) llmsTxt(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_ = llmsTemplate.Execute(w, map[string]any{
		"Base": scheme + "://" + r.Host,
		"Auth": s.Token != "",
	})
}
