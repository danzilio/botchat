package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
}

// ---- agents ----

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.Hub.Agents()})
}

func (s *server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.Hub.Agent(r.PathValue("name"))
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) putAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Meta map[string]string `json:"meta"`
	}
	if !decode(w, r, &body) {
		return
	}
	a, err := s.Hub.RegisterAgent(r.PathValue("name"), body.Meta)
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ---- channels ----

func (s *server) listChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"channels": s.Hub.Channels()})
}

func (s *server) getChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.Hub.Channel(r.PathValue("name"))
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) putChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Topic *string `json:"topic"`
	}
	if !decode(w, r, &body) {
		return
	}
	c, err := s.Hub.CreateChannel(r.PathValue("name"), body.Topic)
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---- messages ----

func (s *server) listMessages(w http.ResponseWriter, r *http.Request) {
	qr := &queryReader{q: r.URL.Query()}
	q := hub.ListQuery{
		Channel:        r.PathValue("name"),
		After:          qr.uint("after", 0),
		Before:         qr.uint("before", 0),
		Limit:          clamp(qr.uint("limit", 0), 50, 500),
		IncludeReplies: qr.bool("replies"),
	}
	if qr.err != nil {
		writeError(w, http.StatusBadRequest, hub.CodeInvalid, qr.err.Error())
		return
	}
	msgs, err := s.Hub.ListMessages(q)
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *server) postMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From     string `json:"from"`
		Text     string `json:"text"`
		ThreadID uint64 `json:"thread_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	m, err := s.Hub.Post(hub.PostInput{
		Channel:  r.PathValue("name"),
		ThreadID: body.ThreadID,
		From:     body.From,
		Text:     body.Text,
	})
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.Hub.Message(id)
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) getThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	root, replies, err := s.Hub.Thread(id)
	if err != nil {
		writeHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "replies": replies})
}

func (s *server) react(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body struct {
			From  string `json:"from"`
			Emoji string `json:"emoji"`
		}
		if !decode(w, r, &body) {
			return
		}
		m, err := s.Hub.React(id, body.From, body.Emoji, add)
		if err != nil {
			writeHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// ---- wait (API-7) ----

func (s *server) wait(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	qr := &queryReader{q: q}
	wq := hub.WaitQuery{
		Channel:     q.Get("channel"),
		ThreadID:    qr.uint("thread_id", 0),
		Mention:     q.Get("mention"),
		ExcludeFrom: q.Get("exclude_from"),
		After:       qr.uint("after", 0),
		Limit:       clamp(qr.uint("limit", 0), 50, 500),
	}
	if qr.err != nil {
		writeError(w, http.StatusBadRequest, hub.CodeInvalid, qr.err.Error())
		return
	}
	if !q.Has("after") {
		wq.After = s.Hub.LastMessageID()
	}
	timeout := 30 * time.Second
	if ts := q.Get("timeout"); ts != "" {
		secs, err := strconv.ParseFloat(ts, 64)
		if err != nil || secs < 0 {
			writeError(w, http.StatusBadRequest, hub.CodeInvalid, "timeout must be a non-negative number of seconds")
			return
		}
		timeout = time.Duration(secs * float64(time.Second))
	}
	timeout = min(timeout, s.MaxWait)
	if agent := q.Get("agent"); agent != "" {
		if err := s.Hub.Touch(agent); err != nil {
			writeHubError(w, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	msgs, err := s.Hub.Wait(ctx, wq)
	if err != nil {
		writeHubError(w, err)
		return
	}
	cursor := wq.After
	for _, m := range msgs {
		cursor = max(cursor, m.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "cursor": cursor})
}

// ---- server-sent events (SSE-*) ----

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	q := r.URL.Query()
	qr := &queryReader{q: q}
	f := hub.Filter{
		Channel:  q.Get("channel"),
		ThreadID: qr.uint("thread_id", 0),
		Mention:  q.Get("mention"),
	}
	if t := q.Get("types"); t != "" {
		f.Types = strings.Split(t, ",")
	}
	var since uint64
	if q.Has("since") {
		since = qr.uint("since", 0)
	} else if id := r.Header.Get("Last-Event-ID"); id != "" {
		since, _ = strconv.ParseUint(id, 10, 64)
	} else {
		since = s.Hub.LastSeq()
	}
	if qr.err != nil {
		writeError(w, http.StatusBadRequest, hub.CodeInvalid, qr.err.Error())
		return
	}
	if agent := q.Get("agent"); agent != "" {
		if err := s.Hub.StreamOpened(agent); err != nil {
			writeHubError(w, err)
			return
		}
		defer s.Hub.StreamClosed(agent)
	}

	backlog, sub := s.Hub.Subscribe(f, since)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	for _, e := range backlog {
		writeEvent(w, e)
	}
	flusher.Flush()

	ticker := time.NewTicker(s.KeepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			writeEvent(w, e)
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e hub.Event) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data)
}
