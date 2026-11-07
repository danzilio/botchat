package hub

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestHub(t *testing.T, opts Options) (*Hub, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600))}
	if opts.Now == nil {
		opts.Now = c.Now
	}
	h := New(opts)
	t.Cleanup(h.Close)
	return h, c
}

func mustPost(t *testing.T, h *Hub, in PostInput) Message {
	t.Helper()
	m, err := h.Post(in)
	if err != nil {
		t.Fatalf("Post(%+v): %v", in, err)
	}
	return m
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := ErrorCode(err); got != code {
		t.Fatalf("error code = %q (%v), want %q", got, err, code)
	}
}

// eventTypes drains all retained events and returns their types.
func eventTypes(h *Hub) []string {
	backlog, sub := h.Subscribe(Filter{}, 0)
	sub.Close()
	var out []string
	for _, e := range backlog {
		out = append(out, e.Type)
	}
	return out
}

// spec: AGT-1
func TestAgentNameValidation(t *testing.T) {
	valid := []string{"a", "Agent_1", "claude-code", strings.Repeat("x", 64), "ALLy"}
	invalidNames := []string{"", "a b", "a.b", "@a", strings.Repeat("x", 65), "all", "ALL", "All", "é"}
	for _, n := range valid {
		if err := ValidateAgentName(n); err != nil {
			t.Errorf("ValidateAgentName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range invalidNames {
		wantCode(t, ValidateAgentName(n), CodeInvalid)
	}
	h, _ := newTestHub(t, Options{})
	if _, err := h.RegisterAgent("Bob", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Agent("bob"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("names must be case-sensitive, got %v", err)
	}
}

// spec: AGT-2
func TestRegisterAgentIdempotentAndMeta(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	a, err := h.RegisterAgent("bob", map[string]string{"harness": "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Meta["harness"] != "claude-code" {
		t.Fatalf("meta = %v", a.Meta)
	}
	a, _ = h.RegisterAgent("bob", nil)
	if a.Meta["harness"] != "claude-code" {
		t.Fatalf("nil meta must keep existing meta, got %v", a.Meta)
	}
	a, _ = h.RegisterAgent("bob", map[string]string{"role": "reviewer"})
	if len(a.Meta) != 1 || a.Meta["role"] != "reviewer" {
		t.Fatalf("meta must be replaced, got %v", a.Meta)
	}
	if n := len(h.Agents()); n != 1 {
		t.Fatalf("agents = %d, want 1", n)
	}

	tooMany := map[string]string{}
	for i := range 17 {
		tooMany[strings.Repeat("k", i+1)] = "v"
	}
	cases := []map[string]string{
		tooMany,
		{strings.Repeat("k", 65): "v"},
		{"": "v"},
		{"k": strings.Repeat("v", 257)},
	}
	for _, m := range cases {
		_, err := h.RegisterAgent("bob", m)
		wantCode(t, err, CodeInvalid)
	}
}

// spec: AGT-3
func TestImplicitRegistration(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	mustPost(t, h, PostInput{Channel: "general", From: "poster", Text: "hi"})
	m := mustPost(t, h, PostInput{Channel: "general", From: "x", Text: "yo"})
	if _, err := h.React(m.ID, "reactor", "👍", true); err != nil {
		t.Fatal(err)
	}
	if err := h.StreamOpened("streamer"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"poster", "reactor", "streamer"} {
		if _, err := h.Agent(n); err != nil {
			t.Errorf("agent %q not registered: %v", n, err)
		}
	}
}

// spec: AGT-4
func TestPresence(t *testing.T) {
	h, c := newTestHub(t, Options{})
	h.RegisterAgent("bob", nil)
	first, _ := h.Agent("bob")
	if !first.Online {
		t.Fatal("freshly registered agent should be online")
	}
	c.Advance(61 * time.Second)
	if a, _ := h.Agent("bob"); a.Online {
		t.Fatal("agent should be offline after 60s of inactivity")
	}
	h.Touch("bob")
	a, _ := h.Agent("bob")
	if !a.Online || !a.LastSeen.After(first.LastSeen) {
		t.Fatalf("Touch should update last_seen and presence: %+v", a)
	}

	h.StreamOpened("bob")
	c.Advance(10 * time.Minute)
	if a, _ := h.Agent("bob"); !a.Online {
		t.Fatal("agent with open stream should be online")
	}
	h.StreamClosed("bob")
	c.Advance(61 * time.Second)
	if a, _ := h.Agent("bob"); a.Online {
		t.Fatal("agent should go offline after stream closes and window elapses")
	}

	// Posting and reacting count as activity.
	m := mustPost(t, h, PostInput{Channel: "general", From: "bob", Text: "x"})
	c.Advance(59 * time.Second)
	if a, _ := h.Agent("bob"); !a.Online {
		t.Fatal("posting should mark online")
	}
	c.Advance(2 * time.Second)
	h.React(m.ID, "bob", "x", true)
	if a, _ := h.Agent("bob"); !a.Online {
		t.Fatal("reacting should mark online")
	}
}

// spec: AGT-5
func TestAgentEvents(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	h.RegisterAgent("bob", map[string]string{"a": "1"})
	h.RegisterAgent("bob", map[string]string{"a": "1"}) // no change
	h.RegisterAgent("bob", nil)                         // no change
	h.RegisterAgent("bob", map[string]string{"a": "2"}) // update
	got := strings.Join(eventTypes(h), ",")
	if got != "agent.joined,agent.updated" {
		t.Fatalf("events = %s", got)
	}
	backlog, sub := h.Subscribe(Filter{}, 0)
	sub.Close()
	if backlog[0].Agent.Meta["a"] != "1" {
		t.Fatalf("joined event should carry meta: %+v", backlog[0].Agent)
	}
}

// spec: CHN-1
func TestChannelNames(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	if _, err := h.Channel("general"); err != nil {
		t.Fatal("general must exist at startup")
	}
	for _, n := range []string{"a", "0", "dev-ops", "a_b", strings.Repeat("x", 64)} {
		if err := ValidateChannelName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "General", "-x", "_x", "a b", "a.b", strings.Repeat("x", 65), "#x"} {
		wantCode(t, ValidateChannelName(n), CodeInvalid)
		_, err := h.CreateChannel(n, nil)
		wantCode(t, err, CodeInvalid)
	}
}

// spec: CHN-2
func TestCreateChannel(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	topic := "planning"
	c, err := h.CreateChannel("plan", &topic)
	if err != nil || c.Topic != "planning" {
		t.Fatalf("%+v %v", c, err)
	}
	c, _ = h.CreateChannel("plan", nil)
	if c.Topic != "planning" {
		t.Fatal("nil topic must keep topic")
	}
	h.CreateChannel("plan", &topic) // unchanged, no event
	other := "other"
	h.CreateChannel("plan", &other)
	if got := strings.Join(eventTypes(h), ","); got != "channel.created,channel.updated" {
		t.Fatalf("events = %s", got)
	}
	long := strings.Repeat("t", 257)
	_, err = h.CreateChannel("plan", &long)
	wantCode(t, err, CodeInvalid)
}

// spec: CHN-3
func TestPostAutoCreatesChannel(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	h.RegisterAgent("bob", nil)
	mustPost(t, h, PostInput{Channel: "new-one", From: "bob", Text: "hello"})
	if _, err := h.Channel("new-one"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eventTypes(h), ","); got != "agent.joined,channel.created,message.created" {
		t.Fatalf("events = %s", got)
	}
	_, err := h.Post(PostInput{Channel: "Bad Name", From: "bob", Text: "x"})
	wantCode(t, err, CodeInvalid)
}

// spec: CHN-4
func TestListChannels(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	mustPost(t, h, PostInput{Channel: "zeta", From: "a", Text: "1"})
	root := mustPost(t, h, PostInput{Channel: "alpha", From: "a", Text: "2"})
	reply := mustPost(t, h, PostInput{ThreadID: root.ID, From: "a", Text: "3"})
	chs := h.Channels()
	var names []string
	for _, c := range chs {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "alpha,general,zeta" {
		t.Fatalf("order = %v", names)
	}
	if chs[0].MessageCount != 2 || chs[0].LastMessageID != reply.ID {
		t.Fatalf("alpha = %+v", chs[0])
	}
	if chs[1].MessageCount != 0 || chs[1].LastMessageID != 0 {
		t.Fatalf("general = %+v", chs[1])
	}
}

// spec: MSG-1
func TestMessageIDsIncrease(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	var last uint64
	for i := range 20 {
		ch := []string{"a", "b"}[i%2]
		m := mustPost(t, h, PostInput{Channel: ch, From: "x", Text: "t"})
		if m.ID <= last {
			t.Fatalf("id %d not greater than %d", m.ID, last)
		}
		last = m.ID
	}
	if h.LastMessageID() != last {
		t.Fatal("LastMessageID mismatch")
	}
}

// spec: MSG-2
func TestMessageTextValidation(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	for _, text := range []string{"", "   \n\t", string([]byte{0xff, 0xfe}), strings.Repeat("a", 65537)} {
		_, err := h.Post(PostInput{Channel: "general", From: "x", Text: text})
		wantCode(t, err, CodeInvalid)
	}
	mustPost(t, h, PostInput{Channel: "general", From: "x", Text: strings.Repeat("a", 65536)})
	_, err := h.Post(PostInput{From: "x", Text: "no channel"})
	wantCode(t, err, CodeInvalid)
	_, err = h.Post(PostInput{Channel: "general", From: "bad name", Text: "x"})
	wantCode(t, err, CodeInvalid)
}

// spec: MSG-3
func TestParseMentions(t *testing.T) {
	cases := map[string][]string{
		"hi @bob":                     {"bob"},
		"@bob hi":                     {"bob"},
		"@bob, @alice and @bob again": {"bob", "alice"},
		"email me at bob@example.com": nil,
		"@@bob":                       nil,
		"(@carol) @dave. @eve!":       {"carol", "dave", "eve"},
		"ping @all":                   {"all"},
		"@x_y-z":                      {"x_y-z"},
		"@":                           nil,
		"@ bob":                       nil,
		"tab\t@t":                     {"t"},
		"@" + strings.Repeat("a", 65): nil,
		"é@bob":                       {"bob"}, // only ASCII name bytes block a mention
		"@Bob and @bob are different": {"Bob", "bob"},
	}
	for in, want := range cases {
		got := ParseMentions(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("ParseMentions(%q) = %v, want %v", in, got, want)
		}
	}
	h, _ := newTestHub(t, Options{})
	m := mustPost(t, h, PostInput{Channel: "general", From: "x", Text: "hey @bob"})
	if len(m.Mentions) != 1 || m.Mentions[0] != "bob" {
		t.Fatalf("mentions = %v", m.Mentions)
	}
}

// spec: MSG-4
func TestThreads(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	root := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "root"})
	r1 := mustPost(t, h, PostInput{ThreadID: root.ID, From: "b", Text: "r1"})
	if r1.ThreadID != root.ID || r1.Channel != "general" {
		t.Fatalf("reply = %+v", r1)
	}
	r2 := mustPost(t, h, PostInput{Channel: "general", ThreadID: r1.ID, From: "c", Text: "r2"})
	if r2.ThreadID != root.ID {
		t.Fatalf("reply to reply must attach to root, got thread_id %d", r2.ThreadID)
	}
	_, err := h.Post(PostInput{ThreadID: 9999, From: "a", Text: "x"})
	wantCode(t, err, CodeNotFound)
	_, err = h.Post(PostInput{Channel: "other", ThreadID: root.ID, From: "a", Text: "x"})
	wantCode(t, err, CodeInvalid)
}

// spec: MSG-5
func TestReplyCount(t *testing.T) {
	h, c := newTestHub(t, Options{})
	root := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "root"})
	if root.ReplyCount != 0 || root.LastReplyAt != nil {
		t.Fatalf("new root = %+v", root)
	}
	c.Advance(time.Minute)
	mustPost(t, h, PostInput{ThreadID: root.ID, From: "b", Text: "r1"})
	c.Advance(time.Minute)
	r2 := mustPost(t, h, PostInput{ThreadID: root.ID, From: "b", Text: "r2"})
	got, _ := h.Message(root.ID)
	if got.ReplyCount != 2 || got.LastReplyAt == nil || !got.LastReplyAt.Equal(r2.CreatedAt) {
		t.Fatalf("root = %+v", got)
	}
}

// spec: MSG-6
func TestCreatedAtUTC(t *testing.T) {
	h, c := newTestHub(t, Options{})
	m := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	if m.CreatedAt.Location() != time.UTC || !m.CreatedAt.Equal(c.Now()) {
		t.Fatalf("created_at = %v", m.CreatedAt)
	}
}

// spec: MSG-7
func TestRetention(t *testing.T) {
	h, _ := newTestHub(t, Options{MaxMessagesPerChannel: 3})
	var ids []uint64
	for range 5 {
		ids = append(ids, mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"}).ID)
	}
	other := mustPost(t, h, PostInput{Channel: "other", From: "a", Text: "x"})
	for _, id := range ids[:2] {
		if _, err := h.Message(id); ErrorCode(err) != CodeNotFound {
			t.Errorf("message %d should be evicted", id)
		}
	}
	msgs, _ := h.ListMessages(ListQuery{Channel: "general"})
	if len(msgs) != 3 || msgs[0].ID != ids[2] {
		t.Fatalf("retained = %+v", msgs)
	}
	if _, err := h.Message(other.ID); err != nil {
		t.Fatal("retention is per channel")
	}
	c, _ := h.Channel("general")
	if c.MessageCount != 3 {
		t.Fatalf("message_count = %d", c.MessageCount)
	}
	// A reply to an evicted root is not found.
	_, err := h.Post(PostInput{ThreadID: ids[0], From: "a", Text: "late"})
	wantCode(t, err, CodeNotFound)
}

// spec: RXN-1
func TestAddReaction(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	m := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	got, err := h.React(m.ID, "b", "👍", true)
	if err != nil || len(got.Reactions["👍"]) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	h.React(m.ID, "b", "👍", true) // idempotent
	got, _ = h.React(m.ID, "c", "👍", true)
	if strings.Join(got.Reactions["👍"], ",") != "b,c" {
		t.Fatalf("reactors = %v", got.Reactions)
	}
	n := 0
	for _, typ := range eventTypes(h) {
		if typ == EventReactionAdded {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("reaction.added events = %d, want 2", n)
	}
	for _, e := range []string{"", "a b", strings.Repeat("x", 65), string([]byte{0xff})} {
		_, err := h.React(m.ID, "b", e, true)
		wantCode(t, err, CodeInvalid)
	}
	_, err = h.React(999, "b", "x", true)
	wantCode(t, err, CodeNotFound)
	_, err = h.React(m.ID, "all", "x", true)
	wantCode(t, err, CodeInvalid)
}

// spec: RXN-2
func TestRemoveReaction(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	m := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	h.React(m.ID, "b", "👍", true)
	h.React(m.ID, "c", "👍", true)
	h.React(m.ID, "b", "🎉", true)
	got, _ := h.React(m.ID, "b", "👍", false)
	if strings.Join(got.Reactions["👍"], ",") != "c" {
		t.Fatalf("%v", got.Reactions)
	}
	got, _ = h.React(m.ID, "b", "🎉", false)
	if _, ok := got.Reactions["🎉"]; ok {
		t.Fatal("empty emoji must be removed")
	}
	h.React(m.ID, "b", "🎉", false) // no-op
	got, _ = h.React(m.ID, "c", "👍", false)
	if got.Reactions != nil {
		t.Fatalf("reactions should be empty: %v", got.Reactions)
	}
	n := 0
	for _, typ := range eventTypes(h) {
		if typ == EventReactionRemoved {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("reaction.removed events = %d, want 3", n)
	}
}

// spec: EVT-1
// spec: EVT-2
func TestEventSequenceAndPayloads(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	h.RegisterAgent("a", nil)
	h.RegisterAgent("a", map[string]string{"k": "v"})
	topic := "t"
	h.CreateChannel("c", nil)
	h.CreateChannel("c", &topic)
	m := mustPost(t, h, PostInput{Channel: "c", From: "a", Text: "x"})
	h.React(m.ID, "a", "y", true)
	h.React(m.ID, "a", "y", false)

	backlog, sub := h.Subscribe(Filter{}, 0)
	sub.Close()
	want := []string{"agent.joined", "agent.updated", "channel.created", "channel.updated",
		"message.created", "reaction.added", "reaction.removed"}
	if len(backlog) != len(want) {
		t.Fatalf("got %d events, want %d", len(backlog), len(want))
	}
	for i, e := range backlog {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d", i, e.Seq)
		}
		if e.Type != want[i] {
			t.Errorf("event %d type = %s, want %s", i, e.Type, want[i])
		}
		if e.At.IsZero() {
			t.Errorf("event %d has no timestamp", i)
		}
		switch e.Type {
		case EventAgentJoined, EventAgentUpdated:
			if e.Agent == nil || e.Message != nil || e.Channel != nil {
				t.Errorf("%s payload: %+v", e.Type, e)
			}
		case EventChannelCreated, EventChannelUpdated:
			if e.Channel == nil || e.Agent != nil || e.Message != nil {
				t.Errorf("%s payload: %+v", e.Type, e)
			}
		case EventMessageCreated:
			if e.Message == nil || e.Message.ID != m.ID {
				t.Errorf("%s payload: %+v", e.Type, e)
			}
		case EventReactionAdded, EventReactionRemoved:
			if e.Reaction == nil || e.Message == nil || e.Reaction.MessageID != m.ID {
				t.Errorf("%s payload: %+v", e.Type, e)
			}
		}
	}
	if len(backlog[5].Message.Reactions["y"]) != 1 || backlog[6].Message.Reactions != nil {
		t.Error("reaction events must carry the message after the change")
	}
	if h.LastSeq() != uint64(len(want)) {
		t.Errorf("LastSeq = %d", h.LastSeq())
	}
}

// spec: EVT-3
func TestEventHistoryRetention(t *testing.T) {
	h, _ := newTestHub(t, Options{EventHistory: 5})
	for range 10 {
		mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	}
	backlog, sub := h.Subscribe(Filter{}, 0)
	sub.Close()
	if len(backlog) != 5 || backlog[0].Seq != h.LastSeq()-4 {
		t.Fatalf("backlog = %d events starting at %d", len(backlog), backlog[0].Seq)
	}
	backlog, sub = h.Subscribe(Filter{}, h.LastSeq()-2)
	sub.Close()
	if len(backlog) != 2 {
		t.Fatalf("since filter: %d events", len(backlog))
	}
	if New(Options{}).opts.EventHistory != 10000 {
		t.Fatal("default event history must be 10000")
	}
}

// spec: EVT-4
func TestFilter(t *testing.T) {
	msg := func(ch string, id, thread uint64, mentions ...string) Event {
		return Event{Type: EventMessageCreated, Message: &Message{ID: id, Channel: ch, ThreadID: thread, Mentions: mentions}}
	}
	agentEv := Event{Type: EventAgentJoined, Agent: &Agent{Name: "a"}}
	chanEv := Event{Type: EventChannelCreated, Channel: &Channel{Name: "dev"}}
	rxnEv := Event{Type: EventReactionAdded, Message: &Message{ID: 1, Channel: "dev", Mentions: []string{"bob"}}, Reaction: &Reaction{}}

	cases := []struct {
		name string
		f    Filter
		e    Event
		want bool
	}{
		{"empty matches all", Filter{}, agentEv, true},
		{"channel match", Filter{Channel: "dev"}, msg("dev", 1, 0), true},
		{"channel mismatch", Filter{Channel: "dev"}, msg("ops", 1, 0), false},
		{"channel matches channel event", Filter{Channel: "dev"}, chanEv, true},
		{"channel excludes agent event", Filter{Channel: "dev"}, agentEv, false},
		{"thread root", Filter{ThreadID: 5}, msg("dev", 5, 0), true},
		{"thread reply", Filter{ThreadID: 5}, msg("dev", 6, 5), true},
		{"thread other", Filter{ThreadID: 5}, msg("dev", 7, 0), false},
		{"thread excludes channel event", Filter{ThreadID: 5}, chanEv, false},
		{"mention", Filter{Mention: "bob"}, msg("dev", 1, 0, "bob"), true},
		{"mention all", Filter{Mention: "bob"}, msg("dev", 1, 0, "all"), true},
		{"mention other", Filter{Mention: "bob"}, msg("dev", 1, 0, "carol"), false},
		{"mention excludes reactions", Filter{Mention: "bob"}, rxnEv, false},
		{"types include", Filter{Types: []string{EventAgentJoined}}, agentEv, true},
		{"types exclude", Filter{Types: []string{EventMessageCreated}}, agentEv, false},
	}
	for _, c := range cases {
		if got := c.f.Match(c.e); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

// spec: EVT-5
func TestSlowSubscriberDisconnected(t *testing.T) {
	h, _ := newTestHub(t, Options{SubscriberBuffer: 3})
	_, slow := h.Subscribe(Filter{}, 0)
	for range 4 {
		mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != 3 {
		t.Fatalf("received %d events before disconnect, want 3", n)
	}
	if New(Options{}).opts.SubscriberBuffer != 256 {
		t.Fatal("default subscriber buffer must be 256")
	}
}

// spec: SSE-2
func TestSubscribeBacklogAndLiveNoGap(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	for range 3 {
		mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	}
	backlog, sub := h.Subscribe(Filter{Types: []string{EventMessageCreated}}, 0)
	defer sub.Close()
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "live"})
	e := <-sub.C
	if len(backlog) != 3 || e.Message.Text != "live" || e.Seq <= backlog[2].Seq {
		t.Fatalf("backlog=%d live=%+v", len(backlog), e)
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	_, sub := h.Subscribe(Filter{}, 0)
	h.Close()
	h.Close()
	if _, ok := <-sub.C; ok {
		t.Fatal("subscription should be closed")
	}
	_, sub2 := h.Subscribe(Filter{}, 0)
	if _, ok := <-sub2.C; ok {
		t.Fatal("subscribing to a closed hub yields a closed subscription")
	}
	sub.Close() // idempotent
	select {
	case <-h.Done():
	default:
		t.Fatal("Done should be closed")
	}
}

// spec: API-5
func TestListMessages(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	var roots []Message
	for i := range 5 {
		roots = append(roots, mustPost(t, h, PostInput{Channel: "general", From: "a", Text: string(rune('a' + i))}))
	}
	reply := mustPost(t, h, PostInput{ThreadID: roots[0].ID, From: "a", Text: "reply"})

	ids := func(ms []Message) (out []uint64) {
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return
	}
	all, _ := h.ListMessages(ListQuery{Channel: "general"})
	if len(all) != 5 {
		t.Fatalf("roots only by default: %v", ids(all))
	}
	withReplies, _ := h.ListMessages(ListQuery{Channel: "general", IncludeReplies: true})
	if len(withReplies) != 6 || withReplies[5].ID != reply.ID {
		t.Fatalf("with replies: %v", ids(withReplies))
	}
	after, _ := h.ListMessages(ListQuery{Channel: "general", After: roots[2].ID})
	if len(after) != 2 || after[0].ID != roots[3].ID {
		t.Fatalf("after: %v", ids(after))
	}
	limited, _ := h.ListMessages(ListQuery{Channel: "general", Limit: 2})
	if len(limited) != 2 || limited[0].ID != roots[0].ID {
		t.Fatalf("limit takes oldest first: %v", ids(limited))
	}
	before, _ := h.ListMessages(ListQuery{Channel: "general", Before: roots[4].ID, Limit: 2})
	if len(before) != 2 || before[0].ID != roots[2].ID || before[1].ID != roots[3].ID {
		t.Fatalf("before returns newest before cursor: %v", ids(before))
	}
	_, err := h.ListMessages(ListQuery{Channel: "nope"})
	wantCode(t, err, CodeNotFound)
}

// spec: API-6
func TestThreadLookup(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	root := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "root"})
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "unrelated"})
	r1 := mustPost(t, h, PostInput{ThreadID: root.ID, From: "b", Text: "r1"})
	r2 := mustPost(t, h, PostInput{ThreadID: root.ID, From: "c", Text: "r2"})
	for _, id := range []uint64{root.ID, r1.ID} {
		gotRoot, replies, err := h.Thread(id)
		if err != nil || gotRoot.ID != root.ID || len(replies) != 2 || replies[0].ID != r1.ID || replies[1].ID != r2.ID {
			t.Fatalf("Thread(%d) = %+v %+v %v", id, gotRoot, replies, err)
		}
	}
	_, _, err := h.Thread(12345)
	wantCode(t, err, CodeNotFound)
}

// spec: API-7
func TestWait(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	m1 := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "one"})

	// Returns immediately when something newer than After exists.
	got, _ := h.Wait(context.Background(), WaitQuery{After: 0})
	if len(got) != 1 || got[0].ID != m1.ID {
		t.Fatalf("immediate: %+v", got)
	}

	// Blocks until a matching message arrives.
	done := make(chan []Message)
	go func() {
		got, _ := h.Wait(context.Background(), WaitQuery{After: m1.ID, Channel: "dev", ExcludeFrom: "me"})
		done <- got
	}()
	time.Sleep(20 * time.Millisecond)
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "wrong channel"})
	mustPost(t, h, PostInput{Channel: "dev", From: "me", Text: "my own"})
	want := mustPost(t, h, PostInput{Channel: "dev", From: "b", Text: "hello"})
	select {
	case got := <-done:
		if len(got) != 1 || got[0].ID != want.ID {
			t.Fatalf("blocking wait: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}

	// Times out with an empty, non-nil list.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, _ = h.Wait(ctx, WaitQuery{After: h.LastMessageID()})
	if got == nil || len(got) != 0 {
		t.Fatalf("timeout: %#v", got)
	}

	// Mentions and limit.
	last := h.LastMessageID()
	for range 3 {
		mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "@bob hi"})
	}
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "@all hi"})
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "nobody"})
	got, _ = h.Wait(context.Background(), WaitQuery{After: last, Mention: "bob", Limit: 2})
	if len(got) != 2 || got[0].ID != last+1 {
		t.Fatalf("limit: %+v", got)
	}
	got, _ = h.Wait(context.Background(), WaitQuery{After: last, Mention: "bob"})
	if len(got) != 4 {
		t.Fatalf("mentions incl @all: %d", len(got))
	}
}

func TestWaitEndsOnClose(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	done := make(chan struct{})
	go func() {
		h.Wait(context.Background(), WaitQuery{After: 100})
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	h.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not end on close")
	}
	got, _ := h.Wait(context.Background(), WaitQuery{After: 100})
	if len(got) != 0 {
		t.Fatal("wait on closed hub returns empty")
	}
}

func TestOnEventCalledInOrder(t *testing.T) {
	var seqs []uint64
	h, _ := newTestHub(t, Options{OnEvent: func(e Event) { seqs = append(seqs, e.Seq) }})
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "x"})
	mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "y"})
	if len(seqs) != 3 || seqs[0] != 1 || seqs[2] != 3 {
		t.Fatalf("seqs = %v", seqs)
	}
}

func TestSnapshotsAreIsolated(t *testing.T) {
	h, _ := newTestHub(t, Options{})
	m := mustPost(t, h, PostInput{Channel: "general", From: "a", Text: "@bob"})
	h.React(m.ID, "a", "x", true)
	got, _ := h.Message(m.ID)
	got.Mentions[0] = "mutated"
	got.Reactions["x"][0] = "mutated"
	again, _ := h.Message(m.ID)
	if again.Mentions[0] != "bob" || again.Reactions["x"][0] != "a" {
		t.Fatal("returned messages must not alias internal state")
	}
}

func TestConcurrentUse(t *testing.T) {
	h, _ := newTestHub(t, Options{Now: time.Now})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				m, err := h.Post(PostInput{Channel: "general", From: "a", Text: "x"})
				if err != nil {
					t.Error(err)
					return
				}
				h.React(m.ID, "b", "y", j%2 == 0)
				h.Agents()
				h.Channels()
				h.ListMessages(ListQuery{Channel: "general", Before: m.ID})
				_ = i
			}
		}()
	}
	wg.Wait()
	if c, _ := h.Channel("general"); c.MessageCount != 400 {
		t.Fatalf("count = %d", c.MessageCount)
	}
}
