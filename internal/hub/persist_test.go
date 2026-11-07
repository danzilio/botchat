package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// roundTrip passes v through JSON, as persistence does.
func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func stateJSON(t *testing.T, h *Hub) string {
	t.Helper()
	b, err := json.Marshal(h.Snapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var busyStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// busyHub exercises every event type and returns the hub, the events it
// emitted, and options for restoring it. Those options' clock is stopped at
// the hub's start, since the general channel's created_at is only in
// snapshots, not in events.
func busyHub(t *testing.T) (*Hub, *[]Event, Options) {
	t.Helper()
	var events []Event
	c := &clock{t: busyStart}
	opts := Options{Now: c.Now, MaxMessagesPerChannel: 3}
	opts.OnEvent = func(e Event) { events = append(events, *roundTrip(t, &e)) }
	h := New(opts)
	t.Cleanup(h.Close)
	step := func() { c.Advance(time.Second) }

	h.RegisterAgent("alice", map[string]string{"role": "lead"})
	step()
	topic := "build stuff"
	h.CreateChannel("dev", &topic)
	step()
	root := mustPost(t, h, PostInput{Channel: "dev", From: "bob", Text: "hi @alice"})
	step()
	mustPost(t, h, PostInput{ThreadID: root.ID, From: "alice", Text: "reply"})
	step()
	h.React(root.ID, "carol", "+1", true)
	step()
	h.React(root.ID, "alice", "eyes", true)
	step()
	h.React(root.ID, "carol", "+1", false)
	step()
	topic = "ship it"
	h.CreateChannel("dev", &topic)
	step()
	h.RegisterAgent("alice", map[string]string{"role": "reviewer"})
	step()
	for i := range 4 { // evicts the oldest general messages (MSG-7)
		mustPost(t, h, PostInput{Channel: "general", From: "bob", Text: strings.Repeat("x", i+1)})
		step()
	}
	opts.OnEvent = nil
	opts.Now = func() time.Time { return busyStart }
	return h, &events, opts
}

// spec: PER-1
func TestRestoreFromEvents(t *testing.T) {
	h, events, opts := busyHub(t)
	want := stateJSON(t, h)

	r, err := Restore(opts, nil, *events)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateJSON(t, r); got != want {
		t.Fatalf("restored state differs\n got %s\nwant %s", got, want)
	}

	// IDs and seqs continue (MSG-1, EVT-1).
	m := mustPost(t, r, PostInput{Channel: "dev", From: "bob", Text: "after"})
	if m.ID != h.LastMessageID()+1 || r.LastSeq() <= h.LastSeq() {
		t.Fatalf("id %d seq %d after restore of id %d seq %d", m.ID, r.LastSeq(), h.LastMessageID(), h.LastSeq())
	}
	// Resuming a stream across the restart sees retained events (SSE-2).
	backlog, sub := r.Subscribe(Filter{}, 2)
	sub.Close()
	if len(backlog) == 0 || backlog[0].Seq != 3 || backlog[len(backlog)-1].Seq != r.LastSeq() {
		t.Fatalf("backlog after restore: %d events starting %d", len(backlog), backlog[0].Seq)
	}
}

// spec: PER-1
func TestRestoreFromSnapshotAndLog(t *testing.T) {
	h, events, opts := busyHub(t)
	want := stateJSON(t, h)

	for cut := 0; cut <= len(*events); cut++ {
		// A snapshot taken after `cut` events, followed by the whole log
		// (including events the snapshot already covers).
		pre, err := Restore(opts, nil, (*events)[:cut])
		if err != nil {
			t.Fatal(err)
		}
		st := roundTrip(t, pre.Snapshot(nil))
		r, err := Restore(opts, &st, *events)
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if got := stateJSON(t, r); got != want {
			t.Fatalf("cut %d: restored state differs\n got %s\nwant %s", cut, got, want)
		}
	}
}

// spec: PER-1
func TestRestoreAppliesRetention(t *testing.T) {
	h, events, opts := busyHub(t)
	opts.MaxMessagesPerChannel = 1
	opts.EventHistory = 5
	st := roundTrip(t, h.Snapshot(nil))
	for _, r := range []func() (*Hub, error){
		func() (*Hub, error) { return Restore(opts, nil, *events) },
		func() (*Hub, error) { return Restore(opts, &st, nil) },
	} {
		r, err := r()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range r.Channels() {
			if ch.MessageCount > 1 {
				t.Fatalf("channel %s retains %d", ch.Name, ch.MessageCount)
			}
		}
		if backlog, sub := r.Subscribe(Filter{}, 0); len(backlog) != 5 {
			t.Fatalf("history %d", len(backlog))
		} else {
			sub.Close()
		}
	}
}

// spec: PER-3
func TestRestoreRejectsBadLogs(t *testing.T) {
	_, events, opts := busyHub(t)
	ev := *events

	gap := append(append([]Event{}, ev[:2]...), ev[3:]...)
	if _, err := Restore(opts, nil, gap); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("gap: %v", err)
	}
	bad := append([]Event{}, ev...)
	bad[0] = Event{Seq: 1, Type: "bogus"}
	if _, err := Restore(opts, nil, bad); err == nil {
		t.Fatal("unknown event type accepted")
	}
	bad[0] = Event{Seq: 1, Type: EventMessageCreated}
	if _, err := Restore(opts, nil, bad); err == nil {
		t.Fatal("event without payload accepted")
	}
}

func TestRestoreDoesNotEmit(t *testing.T) {
	_, events, opts := busyHub(t)
	called := false
	opts.OnEvent = func(Event) { called = true }
	if _, err := Restore(opts, nil, *events); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("Restore must not call OnEvent")
	}
}

func TestSnapshotLockedCallback(t *testing.T) {
	h, _, _ := busyHub(t)
	var seen uint64
	st := h.Snapshot(func(s State) { seen = s.Seq })
	if seen != st.Seq || seen != h.LastSeq() {
		t.Fatalf("callback saw seq %d, snapshot %d", seen, st.Seq)
	}
}
