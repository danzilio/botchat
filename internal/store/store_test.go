package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func fixedNow() time.Time { return t0 }

// open opens dir and restores a hub wired to it, as the app does.
func open(t *testing.T, dir string, opts Options) (*Store, *hub.Hub) {
	t.Helper()
	return openAt(t, dir, opts, fixedNow)
}

func openAt(t *testing.T, dir string, opts Options, now func() time.Time) (*Store, *hub.Hub) {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	s, st, records, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Restore(hub.Options{Now: now, Journal: s}, st, records)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(h); err != nil {
		t.Fatal(err)
	}
	return s, h
}

// crash abandons s without the final snapshot Close would write.
func (s *Store) crash() {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.f.Close()
}

func post(t *testing.T, h *hub.Hub, n int) {
	t.Helper()
	for i := range n {
		if _, err := h.Post(hub.PostInput{Channel: "general", From: "bob", Text: fmt.Sprint("m", i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func stateJSON(t *testing.T, h *hub.Hub) string {
	t.Helper()
	b, err := json.Marshal(h.Snapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// spec: PER-1
func TestCloseAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	s, h := open(t, dir, Options{})
	post(t, h, 3)
	want := stateJSON(t, h)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := lines(t, filepath.Join(dir, logFile)); n != 0 {
		t.Fatalf("log has %d lines after a clean close", n)
	}
	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// spec: PER-2
func TestEventsSurviveProcessCrash(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	post(t, h, 3)
	want := stateJSON(t, h)
	s.crash()

	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// spec: PER-2
func TestLogIsSyncedPeriodically(t *testing.T) {
	s, h := open(t, t.TempDir(), Options{SyncInterval: 10 * time.Millisecond})
	defer s.Close()
	post(t, h, 1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		dirty := s.dirty
		s.mu.Unlock()
		if !dirty {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("log was not synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spec: PER-3
func TestTornRecordIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	post(t, h, 2)
	want := stateJSON(t, h)
	s.crash()

	f, err := os.OpenFile(filepath.Join(dir, logFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":99,"type":"message.cre`)
	f.Close()

	s, h = open(t, dir, Options{})
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	// Later records are readable: the torn bytes were cut off.
	post(t, h, 1)
	want = stateJSON(t, h)
	s.crash()
	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("after append: got %s\nwant %s", got, want)
	}
}

// spec: PER-3
func TestCorruptDataIsAnError(t *testing.T) {
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		s, h := open(t, dir, Options{})
		post(t, h, 2)
		s.crash()
		return dir
	}
	cases := map[string]func(dir string){
		"log line": func(dir string) {
			p := filepath.Join(dir, logFile)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, append([]byte("garbage\n"), b...), 0o600)
		},
		"snapshot": func(dir string) {
			os.WriteFile(filepath.Join(dir, snapshotFile), []byte("{"), 0o600)
		},
		"old log": func(dir string) {
			os.WriteFile(filepath.Join(dir, oldLogFile), []byte("nope\n"), 0o600)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setup(t)
			corrupt(dir)
			if s, _, _, err := Open(dir, Options{}); err == nil {
				s.Close()
				t.Fatal("corrupt data accepted")
			}
		})
	}
	if _, _, _, err := Open(filepath.Join(setup(t), snapshotFile, "x"), Options{}); err == nil {
		t.Fatal("unusable directory accepted")
	}
}

// spec: PER-4
func TestCompactionBoundsTheLog(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{CompactEvery: 10})
	post(t, h, 95)
	deadline := time.Now().Add(2 * time.Second)
	for lines(t, filepath.Join(dir, logFile)) >= 10 {
		if time.Now().After(deadline) {
			t.Fatal("log was not compacted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := stateJSON(t, h)
	s.crash()

	s, h = open(t, dir, Options{CompactEvery: 10})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, oldLogFile)); err == nil {
		t.Fatal("old log left behind")
	}
}

// spec: PER-4
func TestCrashDuringCompaction(t *testing.T) {
	steps := map[string]func(s *Store, h *hub.Hub){
		"after rotating the log": func(s *Store, h *hub.Hub) {
			h.Snapshot(func(hub.State) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if err := s.rotateLocked(); err != nil {
					t.Fatal(err)
				}
			})
		},
		"after writing the snapshot": func(s *Store, h *hub.Hub) {
			st := h.Snapshot(func(hub.State) {
				s.mu.Lock()
				defer s.mu.Unlock()
				if err := s.rotateLocked(); err != nil {
					t.Fatal(err)
				}
			})
			b, _ := json.Marshal(st)
			if err := writeFileAtomic(filepath.Join(s.dir, snapshotFile), b); err != nil {
				t.Fatal(err)
			}
		},
		"while writing the snapshot": func(s *Store, h *hub.Hub) {
			os.WriteFile(filepath.Join(s.dir, snapshotFile+".tmp"), []byte(`{"seq":`), 0o600)
		},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, h := open(t, dir, Options{})
			post(t, h, 3)
			step(s, h)
			post(t, h, 2) // events after the snapshot point
			want := stateJSON(t, h)
			s.crash()

			s, h = open(t, dir, Options{})
			if got := stateJSON(t, h); got != want {
				t.Fatalf("got %s\nwant %s", got, want)
			}
			if _, err := os.Stat(filepath.Join(dir, oldLogFile)); err == nil {
				t.Fatal("startup compaction should remove the old log")
			}
			post(t, h, 1)
			want = stateJSON(t, h)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, h = open(t, dir, Options{})
			defer s.Close()
			if got := stateJSON(t, h); got != want {
				t.Fatalf("after another restart: got %s\nwant %s", got, want)
			}
		})
	}
}

// A compaction that finds an old log from a failed one keeps appending to the
// current log, and still loses nothing.
func TestCompactionWithLeftoverOldLog(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	post(t, h, 2)
	h.Snapshot(func(hub.State) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rotateLocked()
	})
	post(t, h, 2)
	// Simulate a failed snapshot write: the old log is still there.
	if _, err := os.Stat(filepath.Join(dir, oldLogFile)); err != nil {
		t.Fatal(err)
	}
	h.Snapshot(func(hub.State) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rotateLocked() // must not overwrite the old log
	})
	post(t, h, 2)
	want := stateJSON(t, h)
	s.crash()
	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestWritesAfterCloseFail(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Post(hub.PostInput{Channel: "general", From: "bob", Text: "late"}); hub.ErrorCode(err) != hub.CodeUnavailable {
		t.Fatalf("post after close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// spec: PER-2
func TestRefreshSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	now := t0
	clock := func() time.Time { return now }
	s, h := openAt(t, dir, Options{}, clock)
	if _, err := h.RegisterAgent("alice", map[string]string{"role": "lead"}); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Minute)
	if _, err := h.RegisterAgent("alice", map[string]string{"role": "lead"}); err != nil {
		t.Fatal(err)
	}
	want := stateJSON(t, h)
	s.crash()

	s, h = openAt(t, dir, Options{}, clock)
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if a, _ := h.Agent("alice"); !a.LastSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last_seen %v", a.LastSeen)
	}
}

// spec: PER-5
func TestWriteFailureIsNotAcknowledged(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	post(t, h, 2)
	want := stateJSON(t, h)

	s.mu.Lock()
	s.f.Close() // every write now fails
	s.mu.Unlock()
	_, err := h.Post(hub.PostInput{Channel: "general", From: "bob", Text: "lost"})
	if hub.ErrorCode(err) != hub.CodeUnavailable {
		t.Fatalf("post with a failing log: %v", err)
	}
	select {
	case <-s.Failed():
	default:
		t.Fatal("Failed should be closed")
	}
	if err := s.Compact(); err == nil {
		t.Fatal("compaction after a failure must not snapshot unsaved changes")
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close should report the failure")
	}

	// After a restart, the state is that of the last saved change, and
	// the journal has no gap.
	s, h = open(t, dir, Options{})
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	post(t, h, 1)
	want = stateJSON(t, h)
	s.crash()
	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("after recovery: got %s\nwant %s", got, want)
	}
}

// spec: PER-5
func TestSyncFailureFailsTheStore(t *testing.T) {
	s, h := open(t, t.TempDir(), Options{})
	post(t, h, 1)
	s.mu.Lock()
	s.f.Close()
	s.mu.Unlock()
	if err := s.sync(); err == nil {
		t.Fatal("sync of a closed file should fail")
	}
	select {
	case <-s.Failed():
	default:
		t.Fatal("Failed should be closed")
	}
	if _, err := h.Post(hub.PostInput{Channel: "general", From: "bob", Text: "x"}); hub.ErrorCode(err) != hub.CodeUnavailable {
		t.Fatalf("post after sync failure: %v", err)
	}
	s.Close()
}

// spec: PER-4
func TestCrashBetweenRotationSteps(t *testing.T) {
	dir := t.TempDir()
	s, h := open(t, dir, Options{})
	post(t, h, 3)
	want := stateJSON(t, h)
	s.crash()
	// The log was moved aside but no new one was created yet.
	if err := os.Rename(filepath.Join(dir, logFile), filepath.Join(dir, oldLogFile)); err != nil {
		t.Fatal(err)
	}
	s, h = open(t, dir, Options{})
	defer s.Close()
	if got := stateJSON(t, h); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
