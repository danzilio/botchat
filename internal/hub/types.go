package hub

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Agent is a participant in the chat. See SPEC.md section 3 (AGT-*).
type Agent struct {
	Name     string            `json:"name"`
	Meta     map[string]string `json:"meta,omitempty"`
	JoinedAt time.Time         `json:"joined_at"`
	LastSeen time.Time         `json:"last_seen"`
	// LastMessageID is the ID of the agent's most recent message (AGT-6).
	LastMessageID uint64 `json:"last_message_id,omitempty"`
	Online        bool   `json:"online"`
}

// Channel is a named conversation. See SPEC.md section 3 (CHN-*).
type Channel struct {
	Name          string    `json:"name"`
	Topic         string    `json:"topic,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	MessageCount  int       `json:"message_count"`
	LastMessageID uint64    `json:"last_message_id,omitempty"`
}

// Message is a chat message, either a thread root or a reply.
// See SPEC.md section 3 (MSG-*).
type Message struct {
	ID          uint64              `json:"id"`
	Channel     string              `json:"channel"`
	ThreadID    uint64              `json:"thread_id,omitempty"`
	From        string              `json:"from"`
	Text        string              `json:"text"`
	Mentions    []string            `json:"mentions,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	Reactions   map[string][]string `json:"reactions,omitempty"`
	ReplyCount  int                 `json:"reply_count,omitempty"`
	LastReplyAt *time.Time          `json:"last_reply_at,omitempty"`
}

// Reaction identifies a single reaction change.
type Reaction struct {
	MessageID uint64 `json:"message_id"`
	Emoji     string `json:"emoji"`
	From      string `json:"from"`
}

// Event types. See SPEC.md EVT-2.
const (
	EventAgentJoined     = "agent.joined"
	EventAgentUpdated    = "agent.updated"
	EventChannelCreated  = "channel.created"
	EventChannelUpdated  = "channel.updated"
	EventMessageCreated  = "message.created"
	EventReactionAdded   = "reaction.added"
	EventReactionRemoved = "reaction.removed"
)

// EventTypes lists every event type the hub emits.
var EventTypes = []string{
	EventAgentJoined, EventAgentUpdated,
	EventChannelCreated, EventChannelUpdated,
	EventMessageCreated, EventReactionAdded, EventReactionRemoved,
}

// Event is a state change notification. Payload pointers are snapshots and
// MUST be treated as read-only by receivers.
type Event struct {
	Seq      uint64    `json:"seq"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Agent    *Agent    `json:"agent,omitempty"`
	Channel  *Channel  `json:"channel,omitempty"`
	Message  *Message  `json:"message,omitempty"`
	Reaction *Reaction `json:"reaction,omitempty"`
}

// Error codes returned by the hub.
const (
	CodeInvalid  = "invalid_argument"
	CodeNotFound = "not_found"
	// CodeUnavailable means a change could not be saved (PER-5).
	CodeUnavailable = "unavailable"
)

// Error is a typed hub error. Use errors.As to inspect it.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &Error{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode returns the hub error code of err, or "" if err is not a hub error.
func ErrorCode(err error) string {
	var he *Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

func (m *Message) clone() *Message {
	c := *m
	c.Mentions = slices.Clone(m.Mentions)
	if m.Reactions != nil {
		c.Reactions = make(map[string][]string, len(m.Reactions))
		for k, v := range m.Reactions {
			c.Reactions[k] = slices.Clone(v)
		}
	}
	if m.LastReplyAt != nil {
		t := *m.LastReplyAt
		c.LastReplyAt = &t
	}
	return &c
}

func (a *Agent) clone() *Agent {
	c := *a
	c.Meta = maps.Clone(a.Meta)
	return &c
}
