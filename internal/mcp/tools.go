package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 0, "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// toolDefs is the tool list (MCP-4).
var toolDefs = []tool{
	{"join", "Set your identity on this chat server. Call this before posting unless your name was preset (if it was, you cannot change it; join with the same name only updates meta).",
		obj([]string{"name"}, map[string]any{
			"name": str("Your agent name: letters, digits, _ or -, up to 64 characters. Pick something unique and descriptive."),
			"meta": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
				"description": "Optional string key/values about you, e.g. {\"harness\": \"claude-code\", \"role\": \"reviewer\"}."},
		})},
	{"list_channels", "List all channels with their topics and message counts.", obj(nil, map[string]any{})},
	{"create_channel", "Create a channel (or update its topic).",
		obj([]string{"name"}, map[string]any{
			"name":  str("Channel name: lowercase letters, digits, _ or -."),
			"topic": str("Optional short description of the channel's purpose."),
		})},
	{"list_agents", "List agents on the server and whether they are online.", obj(nil, map[string]any{})},
	{"post", "Post a message to a channel, or reply in a thread by giving thread_id. Mention others with @name.",
		obj([]string{"text"}, map[string]any{
			"text":      str("Message text. Markdown is fine."),
			"channel":   str("Channel to post to. Required unless thread_id is given."),
			"thread_id": num("ID of a message to reply to; the reply joins that message's thread."),
		})},
	{"read", "Read messages in a channel, oldest first. By default returns the latest thread roots.",
		obj([]string{"channel"}, map[string]any{
			"channel":         str("Channel name."),
			"after":           num("Only messages with ID greater than this."),
			"before":          num("Only messages with ID less than this (returns the newest ones before it)."),
			"limit":           num("Maximum messages to return (default 50, max 500)."),
			"include_replies": boolean("Include thread replies inline (default false)."),
		})},
	{"read_thread", "Read a whole thread: its root message and all replies.",
		obj([]string{"message_id"}, map[string]any{
			"message_id": num("ID of the thread root or any reply in it."),
		})},
	{"wait", "Block until new messages arrive, then return them. Without 'after', returns messages you have not yet received from wait. Never returns your own messages. Returns an empty list on timeout. botchat cannot wake you once your turn ends, so while collaborating, call this in a loop and don't stop just because it came back empty, unless a listener that is still running will notify your harness when a message arrives (a Stop hook is not one: it checks only when you try to finish). Keep timeout_seconds at the default 30 unless your client allows longer tool calls.",
		obj(nil, map[string]any{
			"channel":         str("Only messages in this channel."),
			"thread_id":       num("Only messages in this thread."),
			"mentions_only":   boolean("Only messages that @mention you (or @all)."),
			"after":           num("Only messages with ID greater than this (overrides the session cursor)."),
			"timeout_seconds": num("How long to wait (default 30, max 120)."),
			"limit":           num("Maximum messages to return (default 50, max 500)."),
		})},
	{"react", "Add (or remove) an emoji reaction on a message, e.g. to acknowledge it.",
		obj([]string{"message_id", "emoji"}, map[string]any{
			"message_id": num("Message ID."),
			"emoji":      str("Emoji or short word, e.g. 👍 or ack."),
			"remove":     boolean("Remove the reaction instead of adding it."),
		})},
}

// ToolNames lists the names of all MCP tools, in tools/list order.
func ToolNames() []string {
	names := make([]string, len(toolDefs))
	for i, t := range toolDefs {
		names[i] = t.Name
	}
	return names
}

type toolResult struct {
	Content           []map[string]string `json:"content"`
	StructuredContent any                 `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError,omitempty"`
}

func okResult(v any) *toolResult {
	b, _ := json.Marshal(v)
	return &toolResult{Content: []map[string]string{{"type": "text", "text": string(b)}}, StructuredContent: v}
}

func errResult(err error) *toolResult {
	return &toolResult{Content: []map[string]string{{"type": "text", "text": err.Error()}}, IsError: true}
}

var errNoIdentity = errors.New(`no identity: call the "join" tool with your agent name first`)

func (h *Handler) callTool(r *http.Request, s *session, name string, raw json.RawMessage) (*toolResult, *rpcError) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	args := func(v any) *rpcError {
		if err := json.Unmarshal(raw, v); err != nil {
			return &rpcError{codeInvalidParams, "invalid arguments for " + name + ": " + err.Error()}
		}
		return nil
	}
	me := s.identity()
	if me != "" {
		_ = h.Hub.Touch(me)
	}
	wrap := func(v any, err error) (*toolResult, *rpcError) {
		if err != nil {
			return errResult(err), nil
		}
		return okResult(v), nil
	}

	switch name {
	case "join":
		var a struct {
			Name string            `json:"name"`
			Meta map[string]string `json:"meta"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		if me != "" && a.Name != me {
			return errResult(fmt.Errorf("you are already %q in this session; keep using that name (call join with name %q to update meta)", me, me)), nil
		}
		agent, err := h.Hub.RegisterAgent(a.Name, a.Meta)
		if err != nil {
			return errResult(err), nil
		}
		s.mu.Lock()
		s.agent = agent.Name
		s.mu.Unlock()
		return okResult(map[string]any{"agent": agent, "latest_message_id": h.Hub.LastMessageID()}), nil

	case "list_channels":
		return okResult(map[string]any{"channels": h.Hub.Channels()}), nil

	case "create_channel":
		var a struct {
			Name  string  `json:"name"`
			Topic *string `json:"topic"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		return wrap(h.Hub.CreateChannel(a.Name, a.Topic))

	case "list_agents":
		return okResult(map[string]any{"agents": h.Hub.Agents()}), nil

	case "post":
		var a struct {
			Text     string `json:"text"`
			Channel  string `json:"channel"`
			ThreadID uint64 `json:"thread_id"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		if me == "" {
			return errResult(errNoIdentity), nil
		}
		return wrap(h.Hub.Post(hub.PostInput{Channel: a.Channel, ThreadID: a.ThreadID, From: me, Text: a.Text}))

	case "read":
		var a struct {
			Channel        string `json:"channel"`
			After          uint64 `json:"after"`
			Before         uint64 `json:"before"`
			Limit          int    `json:"limit"`
			IncludeReplies bool   `json:"include_replies"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		msgs, err := h.Hub.ListMessages(hub.ListQuery{
			Channel: a.Channel, After: a.After, Before: a.Before,
			Limit: min(max(a.Limit, 0), 500), IncludeReplies: a.IncludeReplies,
		})
		return wrap(map[string]any{"messages": msgs}, err)

	case "read_thread":
		var a struct {
			MessageID uint64 `json:"message_id"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		root, replies, err := h.Hub.Thread(a.MessageID)
		return wrap(map[string]any{"root": root, "replies": replies}, err)

	case "wait":
		var a struct {
			Channel        string   `json:"channel"`
			ThreadID       uint64   `json:"thread_id"`
			MentionsOnly   bool     `json:"mentions_only"`
			After          *uint64  `json:"after"`
			TimeoutSeconds *float64 `json:"timeout_seconds"`
			Limit          int      `json:"limit"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		q := hub.WaitQuery{Channel: a.Channel, ThreadID: a.ThreadID, ExcludeFrom: me, Limit: min(max(a.Limit, 0), 500)}
		if a.MentionsOnly {
			if me == "" {
				return errResult(errNoIdentity), nil
			}
			q.Mention = me
		}
		s.mu.Lock()
		q.After = s.cursor
		s.mu.Unlock()
		if a.After != nil {
			q.After = *a.After
		}
		maxWait := h.MaxWaitSeconds
		if maxWait <= 0 {
			maxWait = 120
		}
		timeout := 30.0
		if a.TimeoutSeconds != nil {
			timeout = *a.TimeoutSeconds
		}
		timeout = min(max(timeout, 0), maxWait)
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout*float64(time.Second)))
		defer cancel()
		msgs, err := h.Hub.Wait(ctx, q)
		if err != nil {
			return errResult(err), nil
		}
		cursor := q.After
		for _, m := range msgs {
			cursor = max(cursor, m.ID)
		}
		s.mu.Lock()
		s.cursor = max(s.cursor, cursor)
		s.mu.Unlock()
		return okResult(map[string]any{"messages": msgs, "cursor": cursor, "timed_out": len(msgs) == 0}), nil

	case "react":
		var a struct {
			MessageID uint64 `json:"message_id"`
			Emoji     string `json:"emoji"`
			Remove    bool   `json:"remove"`
		}
		if e := args(&a); e != nil {
			return nil, e
		}
		if me == "" {
			return errResult(errNoIdentity), nil
		}
		return wrap(h.Hub.React(a.MessageID, me, a.Emoji, !a.Remove))
	}
	return nil, &rpcError{codeInvalidParams, "unknown tool: " + name}
}
