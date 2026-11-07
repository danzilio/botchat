// Package hub holds botchat's in-memory state: agents, channels, messages and
// the event stream. It is safe for concurrent use and knows nothing about
// transports; the HTTP API, SSE and MCP layers are thin adapters around it.
package hub

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// Options configures a Hub. Zero values select the defaults from SPEC.md.
type Options struct {
	// MaxMessagesPerChannel is the retention limit (MSG-7). Default 10000.
	MaxMessagesPerChannel int
	// EventHistory is the number of events retained for resume (EVT-3).
	// Default 10000.
	EventHistory int
	// SubscriberBuffer is how far a subscriber may fall behind before it is
	// disconnected (EVT-5). Default 256.
	SubscriberBuffer int
	// PresenceWindow is how long after its last action an agent counts as
	// online (AGT-4). Default 60s.
	PresenceWindow time.Duration
	// Now returns the current time. Default time.Now. Useful in tests.
	Now func() time.Time
	// OnEvent, if set, is called synchronously for every event, in order,
	// while the hub's lock is held. It MUST NOT call back into the hub.
	OnEvent func(Event)
	// Journal, if set, makes changes durable (PER-2). If it fails, the
	// change is not acknowledged and the hub rejects all later changes
	// (PER-5).
	Journal Journal
}

// Journal records every change to the hub, in order. Its methods are called
// while the hub's lock is held and MUST NOT call back into the hub.
type Journal interface {
	// Event records an event before it is delivered.
	Event(Event) error
	// Touch records activity that updated an agent's last_seen without an
	// event (AGT-4).
	Touch(Touch) error
}

// Touch is activity by an existing agent that emits no event.
type Touch struct {
	Agent string    `json:"agent"`
	At    time.Time `json:"at"`
}

// Hub is the central in-memory state.
type Hub struct {
	opts Options

	mu        sync.Mutex
	closed    bool
	done      chan struct{}
	seq       uint64
	lastMsgID uint64
	agents    map[string]*agentState
	channels  map[string]*channelState
	messages  map[uint64]*Message
	history   []Event
	subs      map[*Subscription]struct{}
	failed    error // set when the journal fails (PER-5)
}

type agentState struct {
	agent   Agent
	streams int
}

type channelState struct {
	ch   Channel
	msgs []uint64 // retained message IDs (roots and replies), oldest first
}

// New creates a Hub with the default channel already present (CHN-1).
func New(opts Options) *Hub {
	if opts.MaxMessagesPerChannel <= 0 {
		opts.MaxMessagesPerChannel = defaultRetention
	}
	if opts.EventHistory <= 0 {
		opts.EventHistory = 10000
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = 256
	}
	if opts.PresenceWindow <= 0 {
		opts.PresenceWindow = 60 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := &Hub{
		opts:     opts,
		done:     make(chan struct{}),
		agents:   map[string]*agentState{},
		channels: map[string]*channelState{},
		messages: map[uint64]*Message{},
		subs:     map[*Subscription]struct{}{},
	}
	now := h.now()
	h.channels[DefaultChannel] = &channelState{ch: Channel{Name: DefaultChannel, CreatedAt: now}}
	return h
}

func (h *Hub) now() time.Time { return h.opts.Now().UTC() }

// Close ends all subscriptions and pending waits. It is idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
	for s := range h.subs {
		s.closeLocked()
	}
}

// Done is closed when the hub is closed.
func (h *Hub) Done() <-chan struct{} { return h.done }

// ---- events ----

func (h *Hub) emitLocked(e Event) {
	if h.failed != nil {
		return
	}
	h.seq++
	e.Seq = h.seq
	e.At = h.now()
	if h.opts.Journal != nil {
		if err := h.opts.Journal.Event(e); err != nil {
			h.failLocked(err)
			return // not delivered: it was never saved
		}
	}
	h.history = append(h.history, e)
	h.trimHistoryLocked()
	if h.opts.OnEvent != nil {
		h.opts.OnEvent(e)
	}
	for s := range h.subs {
		if !s.filter.Match(e) {
			continue
		}
		select {
		case s.c <- e:
		default:
			s.closeLocked() // EVT-5: slow consumer
		}
	}
}

// seenLocked records activity by an existing agent (AGT-4).
func (h *Hub) seenLocked(as *agentState) {
	if h.failed != nil {
		return
	}
	now := h.now()
	as.agent.LastSeen = now
	if h.opts.Journal != nil {
		if err := h.opts.Journal.Touch(Touch{Agent: as.agent.Name, At: now}); err != nil {
			h.failLocked(err)
		}
	}
}

func (h *Hub) failLocked(err error) {
	h.failed = &Error{Code: CodeUnavailable, Message: "the change could not be saved: " + err.Error()}
}

// Err returns the error that made the hub stop accepting changes, if any
// (PER-5).
func (h *Hub) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed
}

// LastSeq returns the sequence number of the most recent event.
func (h *Hub) LastSeq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// LastMessageID returns the ID of the most recently created message.
func (h *Hub) LastMessageID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastMsgID
}

// ---- agents ----

func (h *Hub) agentSnapshotLocked(as *agentState) *Agent {
	a := as.agent.clone()
	a.Online = as.streams > 0 || h.now().Sub(as.agent.LastSeen) < h.opts.PresenceWindow
	return a
}

// touchLocked registers name if needed (AGT-3) and updates last_seen (AGT-4).
func (h *Hub) touchLocked(name string) *agentState {
	now := h.now()
	as, ok := h.agents[name]
	if !ok {
		as = &agentState{agent: Agent{Name: name, JoinedAt: now, LastSeen: now}}
		h.agents[name] = as
		h.emitLocked(Event{Type: EventAgentJoined, Agent: h.agentSnapshotLocked(as)})
		return as
	}
	h.seenLocked(as)
	return as
}

// RegisterAgent registers or refreshes an agent (AGT-2). A nil meta keeps the
// existing meta.
func (h *Hub) RegisterAgent(name string, meta map[string]string) (Agent, error) {
	if err := ValidateAgentName(name); err != nil {
		return Agent{}, err
	}
	if err := validateMeta(meta); err != nil {
		return Agent{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return Agent{}, h.failed
	}
	as, existed := h.agents[name]
	if !existed {
		now := h.now()
		as = &agentState{agent: Agent{Name: name, JoinedAt: now, LastSeen: now}}
		if len(meta) > 0 {
			as.agent.Meta = maps.Clone(meta)
		}
		h.agents[name] = as
		h.emitLocked(Event{Type: EventAgentJoined, Agent: h.agentSnapshotLocked(as)})
		if h.failed != nil {
			return Agent{}, h.failed
		}
		return *h.agentSnapshotLocked(as), nil
	}
	h.seenLocked(as)
	if meta != nil && !maps.Equal(meta, as.agent.Meta) {
		as.agent.Meta = nil
		if len(meta) > 0 {
			as.agent.Meta = maps.Clone(meta)
		}
		h.emitLocked(Event{Type: EventAgentUpdated, Agent: h.agentSnapshotLocked(as)})
	}
	if h.failed != nil {
		return Agent{}, h.failed
	}
	return *h.agentSnapshotLocked(as), nil
}

// Touch records activity for an agent, registering it if needed.
func (h *Hub) Touch(name string) error {
	if err := ValidateAgentName(name); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return h.failed
	}
	h.touchLocked(name)
	return h.failed
}

// StreamOpened marks an open stream for name (SSE-4). Pair with StreamClosed.
func (h *Hub) StreamOpened(name string) error {
	if err := ValidateAgentName(name); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return h.failed
	}
	h.touchLocked(name).streams++
	return h.failed
}

// StreamClosed is the counterpart of StreamOpened.
func (h *Hub) StreamClosed(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if as, ok := h.agents[name]; ok && as.streams > 0 {
		as.streams--
		h.seenLocked(as)
	}
}

// Agent returns one agent.
func (h *Hub) Agent(name string) (Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	as, ok := h.agents[name]
	if !ok {
		return Agent{}, notFound("agent %q not found", name)
	}
	return *h.agentSnapshotLocked(as), nil
}

// Agents lists agents sorted by name.
func (h *Hub) Agents() []Agent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Agent, 0, len(h.agents))
	for _, as := range h.agents {
		out = append(out, *h.agentSnapshotLocked(as))
	}
	slices.SortFunc(out, func(a, b Agent) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// ---- channels ----

func (cs *channelState) snapshot() *Channel {
	c := cs.ch
	c.MessageCount = len(cs.msgs)
	if n := len(cs.msgs); n > 0 {
		c.LastMessageID = cs.msgs[n-1]
	}
	return &c
}

func (h *Hub) ensureChannelLocked(name string) *channelState {
	cs, ok := h.channels[name]
	if !ok {
		cs = &channelState{ch: Channel{Name: name, CreatedAt: h.now()}}
		h.channels[name] = cs
		h.emitLocked(Event{Type: EventChannelCreated, Channel: cs.snapshot()})
	}
	return cs
}

// CreateChannel creates a channel or updates its topic (CHN-2). A nil topic
// leaves the topic unchanged.
func (h *Hub) CreateChannel(name string, topic *string) (Channel, error) {
	if err := ValidateChannelName(name); err != nil {
		return Channel{}, err
	}
	if topic != nil && len(*topic) > MaxTopicBytes {
		return Channel{}, invalid("topic exceeds %d bytes", MaxTopicBytes)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return Channel{}, h.failed
	}
	cs, existed := h.channels[name]
	if !existed {
		cs = &channelState{ch: Channel{Name: name, CreatedAt: h.now()}}
		if topic != nil {
			cs.ch.Topic = *topic
		}
		h.channels[name] = cs
		h.emitLocked(Event{Type: EventChannelCreated, Channel: cs.snapshot()})
	} else if topic != nil && *topic != cs.ch.Topic {
		cs.ch.Topic = *topic
		h.emitLocked(Event{Type: EventChannelUpdated, Channel: cs.snapshot()})
	}
	if h.failed != nil {
		return Channel{}, h.failed
	}
	return *cs.snapshot(), nil
}

// Channel returns one channel.
func (h *Hub) Channel(name string) (Channel, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, ok := h.channels[name]
	if !ok {
		return Channel{}, notFound("channel %q not found", name)
	}
	return *cs.snapshot(), nil
}

// Channels lists channels sorted by name (CHN-4).
func (h *Hub) Channels() []Channel {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Channel, 0, len(h.channels))
	for _, cs := range h.channels {
		out = append(out, *cs.snapshot())
	}
	slices.SortFunc(out, func(a, b Channel) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// ---- messages ----

// PostInput describes a new message. Channel may be empty when ThreadID is
// set; it is then taken from the thread root.
type PostInput struct {
	Channel  string
	ThreadID uint64
	From     string
	Text     string
}

// Post creates a message (MSG-*).
func (h *Hub) Post(in PostInput) (Message, error) {
	if err := ValidateAgentName(in.From); err != nil {
		return Message{}, err
	}
	if err := validateText(in.Text); err != nil {
		return Message{}, err
	}
	if in.Channel != "" {
		if err := ValidateChannelName(in.Channel); err != nil {
			return Message{}, err
		}
	} else if in.ThreadID == 0 {
		return Message{}, invalid("channel is required")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return Message{}, h.failed
	}

	var root *Message
	if in.ThreadID != 0 {
		root = h.messages[in.ThreadID]
		if root != nil && root.ThreadID != 0 {
			root = h.messages[root.ThreadID]
		}
		if root == nil {
			return Message{}, notFound("thread %d not found", in.ThreadID)
		}
		if in.Channel == "" {
			in.Channel = root.Channel
		} else if in.Channel != root.Channel {
			return Message{}, invalid("thread %d is in channel %q, not %q", root.ID, root.Channel, in.Channel)
		}
	}

	cs := h.ensureChannelLocked(in.Channel)
	sender := h.touchLocked(in.From)

	h.lastMsgID++
	now := h.now()
	m := &Message{
		ID:        h.lastMsgID,
		Channel:   in.Channel,
		From:      in.From,
		Text:      in.Text,
		Mentions:  ParseMentions(in.Text),
		CreatedAt: now,
	}
	if root != nil {
		m.ThreadID = root.ID
		root.ReplyCount++
		t := now
		root.LastReplyAt = &t
	}
	sender.agent.LastMessageID = m.ID
	h.messages[m.ID] = m
	cs.msgs = append(cs.msgs, m.ID)
	h.evictLocked(cs)
	h.emitLocked(Event{Type: EventMessageCreated, Message: m.clone()})
	if h.failed != nil {
		return Message{}, h.failed
	}
	return *m.clone(), nil
}

// Message returns one message.
func (h *Hub) Message(id uint64) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.messages[id]
	if !ok {
		return Message{}, notFound("message %d not found", id)
	}
	return *m.clone(), nil
}

// ListQuery selects messages in a channel (API-5).
type ListQuery struct {
	Channel        string
	After          uint64
	Before         uint64
	Limit          int
	IncludeReplies bool
}

// ListMessages returns messages oldest first.
func (h *Hub) ListMessages(q ListQuery) ([]Message, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, ok := h.channels[q.Channel]
	if !ok {
		return nil, notFound("channel %q not found", q.Channel)
	}
	var ids []uint64
	for _, id := range cs.msgs {
		if id <= q.After || (q.Before != 0 && id >= q.Before) {
			continue
		}
		if !q.IncludeReplies && h.messages[id].ThreadID != 0 {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) > q.Limit {
		if q.Before != 0 && q.After == 0 {
			ids = ids[len(ids)-q.Limit:] // newest before the cursor
		} else {
			ids = ids[:q.Limit]
		}
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		out = append(out, *h.messages[id].clone())
	}
	return out, nil
}

// Thread returns a thread root and its replies, oldest first. Passing a reply
// ID resolves to its root (API-6).
func (h *Hub) Thread(id uint64) (Message, []Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	root, ok := h.messages[id]
	if ok && root.ThreadID != 0 {
		root, ok = h.messages[root.ThreadID]
	}
	if !ok {
		return Message{}, nil, notFound("thread %d not found", id)
	}
	replies := []Message{}
	for _, mid := range h.channels[root.Channel].msgs {
		if m := h.messages[mid]; m.ThreadID == root.ID {
			replies = append(replies, *m.clone())
		}
	}
	return *root.clone(), replies, nil
}

// React adds (add=true) or removes a reaction (RXN-1, RXN-2). It returns the
// message after the change.
func (h *Hub) React(id uint64, from, emoji string, add bool) (Message, error) {
	if err := ValidateAgentName(from); err != nil {
		return Message{}, err
	}
	if err := validateEmoji(emoji); err != nil {
		return Message{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return Message{}, h.failed
	}
	m, ok := h.messages[id]
	if !ok {
		return Message{}, notFound("message %d not found", id)
	}
	h.touchLocked(from)
	who := m.Reactions[emoji]
	has := slices.Contains(who, from)
	switch {
	case add && !has:
		if m.Reactions == nil {
			m.Reactions = map[string][]string{}
		}
		m.Reactions[emoji] = append(who, from)
		h.emitLocked(Event{Type: EventReactionAdded, Message: m.clone(),
			Reaction: &Reaction{MessageID: id, Emoji: emoji, From: from}})
	case !add && has:
		who = slices.DeleteFunc(slices.Clone(who), func(s string) bool { return s == from })
		if len(who) == 0 {
			delete(m.Reactions, emoji)
			if len(m.Reactions) == 0 {
				m.Reactions = nil
			}
		} else {
			m.Reactions[emoji] = who
		}
		h.emitLocked(Event{Type: EventReactionRemoved, Message: m.clone(),
			Reaction: &Reaction{MessageID: id, Emoji: emoji, From: from}})
	}
	if h.failed != nil {
		return Message{}, h.failed
	}
	return *m.clone(), nil
}

// ---- subscriptions ----

// Filter selects events (EVT-4). Zero fields match everything.
type Filter struct {
	Channel  string
	ThreadID uint64
	Mention  string
	Types    []string
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
		return false
	}
	m := e.Message
	if f.Channel != "" {
		switch {
		case m != nil:
			if m.Channel != f.Channel {
				return false
			}
		case e.Channel != nil:
			if e.Channel.Name != f.Channel {
				return false
			}
		default:
			return false
		}
	}
	if f.ThreadID != 0 && (m == nil || (m.ID != f.ThreadID && m.ThreadID != f.ThreadID)) {
		return false
	}
	if f.Mention != "" {
		if e.Type != EventMessageCreated || m == nil {
			return false
		}
		if !slices.Contains(m.Mentions, f.Mention) && !slices.Contains(m.Mentions, MentionAll) {
			return false
		}
	}
	return true
}

// Subscription delivers live events. C is closed when the subscription ends,
// either through Close, hub shutdown, or because the subscriber fell too far
// behind (EVT-5).
type Subscription struct {
	C      <-chan Event
	c      chan Event
	h      *Hub
	filter Filter
	closed bool
}

func (s *Subscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	delete(s.h.subs, s)
	close(s.c)
}

// Close ends the subscription. It is idempotent.
func (s *Subscription) Close() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.closeLocked()
}

// Subscribe returns retained events with Seq > since that match f (the
// backlog), and a subscription for everything after. Together they contain
// every matching event exactly once (SSE-2).
func (h *Hub) Subscribe(f Filter, since uint64) ([]Event, *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var backlog []Event
	for _, e := range h.history {
		if e.Seq > since && f.Match(e) {
			backlog = append(backlog, e)
		}
	}
	return backlog, h.subscribeLocked(f)
}

func (h *Hub) subscribeLocked(f Filter) *Subscription {
	c := make(chan Event, h.opts.SubscriberBuffer)
	s := &Subscription{C: c, c: c, h: h, filter: f}
	if h.closed {
		s.closed = true
		close(c)
	} else {
		h.subs[s] = struct{}{}
	}
	return s
}

// ---- wait ----

// WaitQuery describes a long-poll for messages (API-7, MCP-8).
type WaitQuery struct {
	Channel     string
	ThreadID    uint64
	Mention     string
	ExcludeFrom string
	After       uint64
	Limit       int
}

func (q WaitQuery) filter() Filter {
	return Filter{Channel: q.Channel, ThreadID: q.ThreadID, Mention: q.Mention, Types: []string{EventMessageCreated}}
}

func (q WaitQuery) accept(m *Message) bool {
	return m.ID > q.After && (q.ExcludeFrom == "" || m.From != q.ExcludeFrom) &&
		q.filter().Match(Event{Type: EventMessageCreated, Message: m})
}

// Wait returns retained messages matching q immediately if there are any;
// otherwise it blocks until a matching message is posted, ctx is done, or the
// hub is closed. Messages are returned oldest first, at most q.Limit of them.
func (h *Hub) Wait(ctx context.Context, q WaitQuery) ([]Message, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	h.mu.Lock()
	found := []Message{}
	for _, m := range h.messages {
		if q.accept(m) {
			found = append(found, *m.clone())
		}
	}
	if len(found) > 0 || h.closed {
		h.mu.Unlock()
		slices.SortFunc(found, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
		if len(found) > q.Limit {
			found = found[:q.Limit]
		}
		return found, nil
	}
	// Subscribe while still holding the lock so nothing slips in between.
	sub := h.subscribeLocked(q.filter())
	h.mu.Unlock()
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return found, nil
		case e, ok := <-sub.C:
			if !ok {
				return found, nil
			}
			if !q.accept(e.Message) {
				continue
			}
			found = append(found, *e.Message)
			// Collect anything else that is already queued, then return.
			for len(found) < q.Limit {
				select {
				case e, ok := <-sub.C:
					if !ok {
						return found, nil
					}
					if q.accept(e.Message) {
						found = append(found, *e.Message)
					}
					continue
				default:
				}
				break
			}
			return found, nil
		}
	}
}
