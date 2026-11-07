package hub

import (
	"encoding/json"
	"errors"
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

// recorder is a Journal that keeps records in memory. Once fail is set, it
// returns that error for every record.
type recorder struct {
	t       *testing.T
	records []Record
	fail    error
}

func (r *recorder) Event(e Event) error {
	if r.fail != nil {
		return r.fail
	}
	e = roundTrip(r.t, e)
	r.records = append(r.records, Record{Event: &e})
	return nil
}

func (r *recorder) Touch(tc Touch) error {
	if r.fail != nil {
		return r.fail
	}
	tc = roundTrip(r.t, tc)
	r.records = append(r.records, Record{Touch: &tc})
	return nil
}

var busyStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// busyHub exercises every event type and every kind of activity that updates
// last_seen without an event. It returns the hub, its journal, and options for
// restoring it. Those options' clock is stopped at the hub's start, since the
// general channel's created_at is only in snapshots, not in the journal.
func busyHub(t *testing.T) (*Hub, *recorder, Options) {
	t.Helper()
	rec := &recorder{t: t}
	c := &clock{t: busyStart}
	opts := Options{Now: c.Now, MaxMessagesPerChannel: 3, Journal: rec}
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
	// Activity without events (AGT-4).
	h.RegisterAgent("alice", nil)
	step()
	h.Touch("bob")
	step()
	h.React(root.ID, "carol", "+1", false) // no-op
	step()
	h.StreamOpened("dave")
	step()
	h.StreamClosed("dave")
	step()

	opts.Journal = nil
	opts.Now = func() time.Time { return busyStart }
	return h, rec, opts
}

func eventsOnly(records []Record) []Record {
	var out []Record
	for _, r := range records {
		if r.Event != nil {
			out = append(out, r)
		}
	}
	return out
}

// spec: PER-1
func TestRestoreFromJournal(t *testing.T) {
	h, rec, opts := busyHub(t)
	want := stateJSON(t, h)

	r, err := Restore(opts, nil, rec.records)
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

// spec: PER-2
func TestActivityWithoutEventsIsJournaled(t *testing.T) {
	h, rec, opts := busyHub(t)
	// Events alone are not enough: last_seen from the activity at the end
	// of busyHub would be lost.
	r, err := Restore(opts, nil, eventsOnly(rec.records))
	if err != nil {
		t.Fatal(err)
	}
	if stateJSON(t, r) == stateJSON(t, h) {
		t.Fatal("busyHub should include activity that only touches record")
	}

	// The reviewer's case: a refresh with unchanged meta, a minute later.
	rec = &recorder{t: t}
	c := &clock{t: busyStart}
	h = New(Options{Now: c.Now, Journal: rec})
	h.RegisterAgent("alice", map[string]string{"role": "lead"})
	c.Advance(time.Minute)
	h.RegisterAgent("alice", map[string]string{"role": "lead"})
	r, err = Restore(Options{Now: c.Now}, nil, rec.records)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.Agent("alice")
	if !a.LastSeen.Equal(busyStart.Add(time.Minute)) {
		t.Fatalf("last_seen %v, want %v", a.LastSeen, busyStart.Add(time.Minute))
	}
}

// spec: PER-1
func TestRestoreFromSnapshotAndJournal(t *testing.T) {
	h, rec, opts := busyHub(t)
	want := stateJSON(t, h)

	for cut := 0; cut <= len(rec.records); cut++ {
		// A snapshot taken after `cut` records, followed by the whole
		// journal (including records the snapshot already covers).
		pre, err := Restore(opts, nil, rec.records[:cut])
		if err != nil {
			t.Fatal(err)
		}
		st := roundTrip(t, pre.Snapshot(nil))
		r, err := Restore(opts, &st, rec.records)
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
	h, rec, opts := busyHub(t)
	opts.MaxMessagesPerChannel = 1
	opts.EventHistory = 5
	st := roundTrip(t, h.Snapshot(nil))
	for _, r := range []func() (*Hub, error){
		func() (*Hub, error) { return Restore(opts, nil, rec.records) },
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
func TestRestoreRejectsBadJournals(t *testing.T) {
	_, rec, opts := busyHub(t)
	ev := eventsOnly(rec.records)

	gap := append(append([]Record{}, ev[:2]...), ev[3:]...)
	if _, err := Restore(opts, nil, gap); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("gap: %v", err)
	}
	bad := append([]Record{}, ev...)
	bad[0] = Record{Event: &Event{Seq: 1, Type: "bogus"}}
	if _, err := Restore(opts, nil, bad); err == nil {
		t.Fatal("unknown event type accepted")
	}
	bad[0] = Record{Event: &Event{Seq: 1, Type: EventMessageCreated}}
	if _, err := Restore(opts, nil, bad); err == nil {
		t.Fatal("event without payload accepted")
	}
	bad[0] = Record{}
	if _, err := Restore(opts, nil, bad); err == nil {
		t.Fatal("empty record accepted")
	}
}

func TestRestoreDoesNotEmit(t *testing.T) {
	_, rec, opts := busyHub(t)
	called := false
	opts.OnEvent = func(Event) { called = true }
	opts.Journal = &recorder{t: t, fail: errors.New("must not be called")}
	if _, err := Restore(opts, nil, rec.records); err != nil {
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

// spec: PER-5
func TestJournalFailureIsNotAcknowledged(t *testing.T) {
	rec := &recorder{t: t}
	logged := 0
	h, _ := newTestHub(t, Options{Journal: rec, OnEvent: func(Event) { logged++ }})
	root := mustPost(t, h, PostInput{Channel: "general", From: "alice", Text: "saved"})
	_, sub := h.Subscribe(Filter{}, h.LastSeq())
	defer sub.Close()
	before := logged

	rec.fail = errors.New("disk full")
	_, err := h.Post(PostInput{Channel: "general", From: "alice", Text: "lost"})
	if ErrorCode(err) != CodeUnavailable || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("post after failure: %v", err)
	}
	if h.Err() == nil {
		t.Fatal("Err should report the failure")
	}
	select {
	case e := <-sub.C:
		t.Fatalf("unsaved event delivered: %+v", e)
	default:
	}
	if logged != before {
		t.Fatal("unsaved event passed to OnEvent")
	}

	// Every later change fails, even once the journal works again.
	rec.fail = nil
	topic := "x"
	checks := map[string]error{
		"post":     func() error { _, err := h.Post(PostInput{Channel: "general", From: "alice", Text: "y"}); return err }(),
		"register": func() error { _, err := h.RegisterAgent("alice", nil); return err }(),
		"touch":    h.Touch("alice"),
		"stream":   h.StreamOpened("alice"),
		"channel":  func() error { _, err := h.CreateChannel("dev", &topic); return err }(),
		"react":    func() error { _, err := h.React(root.ID, "alice", "+1", true); return err }(),
	}
	for name, err := range checks {
		if ErrorCode(err) != CodeUnavailable {
			t.Errorf("%s after failure: %v", name, err)
		}
	}
	h.StreamClosed("alice")
	if n := len(rec.records); n != 2 { // agent.joined and message.created for "saved"
		t.Fatalf("journal has %d records; nothing after the failure should be written", n)
	}
}

// spec: PER-5
func TestTouchFailureIsNotAcknowledged(t *testing.T) {
	rec := &recorder{t: t}
	h, _ := newTestHub(t, Options{Journal: rec})
	if _, err := h.RegisterAgent("alice", nil); err != nil {
		t.Fatal(err)
	}
	rec.fail = errors.New("io error")
	if _, err := h.RegisterAgent("alice", nil); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("refresh after failure: %v", err)
	}
	if err := h.Touch("alice"); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("touch after failure: %v", err)
	}
}
