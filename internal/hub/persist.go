package hub

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"
)

// State is a point-in-time copy of everything a Hub needs to resume after a
// restart (PER-1). It is plain data and round-trips through encoding/json.
type State struct {
	Seq           uint64    `json:"seq"`
	LastMessageID uint64    `json:"last_message_id"`
	Agents        []Agent   `json:"agents"`
	Channels      []Channel `json:"channels"`
	Messages      []Message `json:"messages"` // oldest first
	Events        []Event   `json:"events"`   // retained history, oldest first
}

// Snapshot returns the hub's current state. If locked is non-nil it is called
// while the hub's lock is still held, so nothing can happen between the
// snapshot and locked; like Options.OnEvent it MUST NOT call back into the
// hub. Store implementations use it to rotate their log atomically with the
// snapshot (PER-4).
func (h *Hub) Snapshot(locked func(State)) State {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := State{
		Seq:           h.seq,
		LastMessageID: h.lastMsgID,
		Agents:        make([]Agent, 0, len(h.agents)),
		Channels:      make([]Channel, 0, len(h.channels)),
		Messages:      make([]Message, 0, len(h.messages)),
		Events:        slices.Clone(h.history),
	}
	for _, as := range h.agents {
		a := as.agent.clone()
		a.Online = false
		st.Agents = append(st.Agents, *a)
	}
	slices.SortFunc(st.Agents, func(a, b Agent) int { return cmp.Compare(a.Name, b.Name) })
	for _, cs := range h.channels {
		st.Channels = append(st.Channels, cs.ch)
	}
	slices.SortFunc(st.Channels, func(a, b Channel) int { return cmp.Compare(a.Name, b.Name) })
	for _, m := range h.messages {
		st.Messages = append(st.Messages, *m.clone())
	}
	slices.SortFunc(st.Messages, func(a, b Message) int { return cmp.Compare(a.ID, b.ID) })
	if locked != nil {
		locked(st)
	}
	return st
}

// Record is one journal entry: exactly one of Event and Touch is set.
type Record struct {
	Event *Event
	Touch *Touch
}

// Restore creates a Hub from a snapshot (which may be nil) followed by the
// journal records written after it. Events with a seq at or below the
// snapshot's are skipped; the rest must follow on without gaps (EVT-1).
// Touches only ever move last_seen forward, so replaying one the snapshot
// already covers is harmless. Restoring calls neither opts.OnEvent nor
// opts.Journal. Retention limits from opts apply to the result (PER-1).
func Restore(opts Options, st *State, records []Record) (*Hub, error) {
	h := New(opts)
	h.mu.Lock()
	defer h.mu.Unlock()
	if st != nil {
		h.loadLocked(st)
	}
	for _, r := range records {
		if r.Touch != nil {
			if as, ok := h.agents[r.Touch.Agent]; ok && r.Touch.At.After(as.agent.LastSeen) {
				as.agent.LastSeen = r.Touch.At
			}
			continue
		}
		if r.Event == nil {
			return nil, fmt.Errorf("empty journal record")
		}
		e := *r.Event
		if e.Seq <= h.seq {
			continue
		}
		if e.Seq != h.seq+1 {
			return nil, fmt.Errorf("event log has a gap: expected seq %d, found %d", h.seq+1, e.Seq)
		}
		if err := h.applyLocked(e); err != nil {
			return nil, fmt.Errorf("event %d (%s): %w", e.Seq, e.Type, err)
		}
	}
	return h, nil
}

func (h *Hub) loadLocked(st *State) {
	h.seq = st.Seq
	h.lastMsgID = st.LastMessageID
	for _, a := range st.Agents {
		a.Online = false
		a.Meta = maps.Clone(a.Meta)
		h.agents[a.Name] = &agentState{agent: a}
	}
	for _, c := range st.Channels {
		c.MessageCount, c.LastMessageID = 0, 0
		h.channels[c.Name] = &channelState{ch: c}
	}
	for _, m := range st.Messages {
		h.insertLocked(m.clone())
	}
	h.history = slices.Clone(st.Events)
	h.trimHistoryLocked()
}

// applyLocked replays one logged event onto the state. Payloads are snapshots
// taken when the event was emitted, so most events simply overwrite state.
func (h *Hub) applyLocked(e Event) error {
	switch e.Type {
	case EventAgentJoined, EventAgentUpdated:
		if e.Agent == nil {
			return fmt.Errorf("missing agent")
		}
		a := e.Agent.clone()
		a.Online = false
		if as, ok := h.agents[a.Name]; ok {
			a.LastMessageID = max(a.LastMessageID, as.agent.LastMessageID)
			if as.agent.LastSeen.After(a.LastSeen) {
				a.LastSeen = as.agent.LastSeen
			}
		}
		h.agents[a.Name] = &agentState{agent: *a}
	case EventChannelCreated, EventChannelUpdated:
		if e.Channel == nil {
			return fmt.Errorf("missing channel")
		}
		c := *e.Channel
		c.MessageCount, c.LastMessageID = 0, 0
		if cs, ok := h.channels[c.Name]; ok {
			cs.ch = c
		} else {
			h.channels[c.Name] = &channelState{ch: c}
		}
	case EventMessageCreated:
		if e.Message == nil {
			return fmt.Errorf("missing message")
		}
		m := e.Message.clone()
		if m.ID <= h.lastMsgID {
			return fmt.Errorf("message %d is not newer than %d", m.ID, h.lastMsgID)
		}
		h.lastMsgID = m.ID
		if m.ThreadID != 0 {
			if root, ok := h.messages[m.ThreadID]; ok {
				root.ReplyCount++
				t := m.CreatedAt
				root.LastReplyAt = &t
			}
		}
		h.insertLocked(m)
		as := h.restoredAgentLocked(m.From, e.At)
		as.agent.LastMessageID = m.ID
	case EventReactionAdded, EventReactionRemoved:
		if e.Message == nil || e.Reaction == nil {
			return fmt.Errorf("missing message or reaction")
		}
		if m, ok := h.messages[e.Message.ID]; ok {
			m.Reactions = e.Message.clone().Reactions
		}
		h.restoredAgentLocked(e.Reaction.From, e.At)
	default:
		return fmt.Errorf("unknown event type")
	}
	h.seq = e.Seq
	h.history = append(h.history, e)
	h.trimHistoryLocked()
	return nil
}

// restoredAgentLocked returns the named agent, creating it if the log lacks
// its join event, and advances its last_seen to at.
func (h *Hub) restoredAgentLocked(name string, at time.Time) *agentState {
	as, ok := h.agents[name]
	if !ok {
		as = &agentState{agent: Agent{Name: name, JoinedAt: at}}
		h.agents[name] = as
	}
	if at.After(as.agent.LastSeen) {
		as.agent.LastSeen = at
	}
	return as
}

// insertLocked adds a message to its channel, creating the channel if needed,
// and applies retention (MSG-7). It emits nothing.
func (h *Hub) insertLocked(m *Message) {
	cs, ok := h.channels[m.Channel]
	if !ok {
		cs = &channelState{ch: Channel{Name: m.Channel, CreatedAt: m.CreatedAt}}
		h.channels[m.Channel] = cs
	}
	h.messages[m.ID] = m
	cs.msgs = append(cs.msgs, m.ID)
	h.evictLocked(cs)
}

// evictLocked drops a channel's oldest messages beyond the retention limit.
func (h *Hub) evictLocked(cs *channelState) {
	if over := len(cs.msgs) - h.opts.MaxMessagesPerChannel; over > 0 {
		for _, id := range cs.msgs[:over] {
			delete(h.messages, id)
		}
		cs.msgs = slices.Delete(cs.msgs, 0, over)
	}
}

func (h *Hub) trimHistoryLocked() {
	if over := len(h.history) - h.opts.EventHistory; over > 0 {
		h.history = slices.Delete(h.history, 0, over)
	}
}
