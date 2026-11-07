// Package store persists a hub to a data directory (SPEC.md "Persistence",
// PER-*). It keeps two kinds of files:
//
//   - snapshot.json: a hub.State, replaced atomically (write, fsync, rename).
//   - events.jsonl: every event emitted after the snapshot, one JSON object
//     per line. During compaction the log is first renamed to
//     events.jsonl.old, then a new snapshot is written, then the old log is
//     removed. Loading reads all three and skips events the snapshot already
//     covers, so a crash at any step loses nothing (PER-4).
//
// Events are written before the hub returns (PER-2), so a process crash loses
// nothing that was acknowledged; the log is fsynced on a timer.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/danzilio/botchat/internal/hub"
)

const (
	snapshotFile = "snapshot.json"
	logFile      = "events.jsonl"
	oldLogFile   = "events.jsonl.old"
)

// Options configures a Store. Zero values select the defaults.
type Options struct {
	// CompactEvery is the number of logged events after which the log is
	// folded into a new snapshot (PER-4). Default 10000.
	CompactEvery int
	// SyncInterval is how often the log is flushed to stable storage
	// (PER-2). Default 1s.
	SyncInterval time.Duration
	// Logf reports background errors. Default log.Printf.
	Logf func(format string, args ...any)
}

// Source is what a Store snapshots; *hub.Hub implements it.
type Source interface {
	Snapshot(locked func(hub.State)) hub.State
}

// Store is an open data directory.
type Store struct {
	dir  string
	opts Options

	mu      sync.Mutex
	f       *os.File
	off     int64 // end of the last complete record in f
	n       int   // events in f
	dirty   bool  // written since the last fsync
	closed  bool
	lastErr string // last reported write error, to avoid log spam

	src  Source
	kick chan struct{}
	stop chan struct{}
	done chan struct{}
	cmu  sync.Mutex // serializes compactions
}

// Open opens (creating if needed) the data directory dir and loads what it
// holds. It returns the snapshot (nil if there is none) and the events logged
// after it, for hub.Restore. A truncated final record is discarded; any other
// unreadable data is an error (PER-3).
func Open(dir string, opts Options) (*Store, *hub.State, []hub.Event, error) {
	if opts.CompactEvery <= 0 {
		opts.CompactEvery = 10000
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = time.Second
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("data directory: %w", err)
	}
	s := &Store{dir: dir, opts: opts, kick: make(chan struct{}, 1)}

	var st *hub.State
	if b, err := os.ReadFile(s.path(snapshotFile)); err == nil {
		st = new(hub.State)
		if err := json.Unmarshal(b, st); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.path(snapshotFile), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}

	var events []hub.Event
	if f, err := os.Open(s.path(oldLogFile)); err == nil {
		events, _, err = readLog(f, events)
		f.Close()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.path(oldLogFile), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}

	f, err := os.OpenFile(s.path(logFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, nil, err
	}
	before := len(events)
	events, off, err := readLog(f, events)
	if err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("%s: %w", s.path(logFile), err)
	}
	if err := f.Truncate(off); err != nil { // drop a torn final record
		f.Close()
		return nil, nil, nil, err
	}
	s.f, s.off, s.n = f, off, len(events)-before
	return s, st, events, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// readLog appends the events in r to events. It returns the offset just past
// the last complete line. A final line without a newline is a write cut short
// by a crash and is ignored; a bad line anywhere else is an error.
func readLog(r io.Reader, events []hub.Event) ([]hub.Event, int64, error) {
	br := bufio.NewReader(r)
	var off int64
	for line := 1; ; line++ {
		b, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return events, off, nil // b, if any, is a torn record
		}
		if err != nil {
			return nil, 0, err
		}
		if len(bytes.TrimSpace(b)) > 0 {
			var e hub.Event
			if err := json.Unmarshal(b, &e); err != nil {
				return nil, 0, fmt.Errorf("line %d: %w", line, err)
			}
			events = append(events, e)
		}
		off += int64(len(b))
	}
}

// Append writes e to the log. It is meant to be hub.Options.OnEvent and so is
// called with the hub's lock held, in event order.
func (s *Store) Append(e hub.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		s.opts.Logf("botchat: persist event %d: %v", e.Seq, err)
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, err := s.f.WriteAt(b, s.off); err != nil {
		// Cut off a partial record so later records stay readable.
		_ = s.f.Truncate(s.off)
		if msg := err.Error(); msg != s.lastErr {
			s.lastErr = msg
			s.opts.Logf("botchat: persist event %d: %v", e.Seq, err)
		}
		return
	}
	s.lastErr = ""
	s.off += int64(len(b))
	s.dirty = true
	s.n++
	if s.n >= s.opts.CompactEvery {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// Start writes a fresh snapshot of src, so the directory is known to be
// writable, then flushes and compacts in the background until Close.
func (s *Store) Start(src Source) error {
	s.src = src
	if err := s.Compact(); err != nil {
		return err
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go s.loop()
	return nil
}

func (s *Store) loop() {
	defer close(s.done)
	t := time.NewTicker(s.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.sync(); err != nil {
				s.opts.Logf("botchat: sync %s: %v", s.path(logFile), err)
			}
		case <-s.kick:
			if err := s.Compact(); err != nil {
				s.opts.Logf("botchat: compact %s: %v", s.dir, err)
			}
		}
	}
}

func (s *Store) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.dirty {
		return nil
	}
	s.dirty = false
	return s.f.Sync()
}

// Compact replaces the log with a snapshot of the source (PER-4).
func (s *Store) Compact() error {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	var rotErr error
	st := s.src.Snapshot(func(hub.State) {
		s.mu.Lock()
		defer s.mu.Unlock()
		rotErr = s.rotateLocked()
	})
	if rotErr != nil {
		return rotErr
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path(snapshotFile), b); err != nil {
		return err
	}
	syncDir(s.dir)
	// The snapshot now covers everything in the old log.
	if err := os.Remove(s.path(oldLogFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// rotateLocked moves the current log aside and starts an empty one. Every
// event in the moved log is covered by the snapshot being taken. If an old
// log is still there (a previous compaction failed after rotating), it keeps
// appending to the current log instead; loading skips what the snapshot
// covers, so that is only a little wasteful.
func (s *Store) rotateLocked() error {
	if s.closed {
		return errors.New("store is closed")
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.dirty = false
	s.n = 0
	if _, err := os.Stat(s.path(oldLogFile)); err == nil {
		return nil
	}
	f, err := os.OpenFile(s.path(logFile+".new"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Close before renaming: Windows cannot rename open files.
	if err := s.f.Close(); err != nil {
		f.Close()
		return err
	}
	if err := os.Rename(s.path(logFile), s.path(oldLogFile)); err != nil {
		f.Close()
		return s.reopenLocked(err)
	}
	if err := os.Rename(s.path(logFile+".new"), s.path(logFile)); err != nil {
		f.Close()
		// Put the old log back so appends continue where they were.
		if err2 := os.Rename(s.path(oldLogFile), s.path(logFile)); err2 != nil {
			return errors.Join(err, err2)
		}
		return s.reopenLocked(err)
	}
	syncDir(s.dir)
	s.f, s.off = f, 0
	return nil
}

// reopenLocked reopens the log after a failed rotation and returns cause.
func (s *Store) reopenLocked(cause error) error {
	f, err := os.OpenFile(s.path(logFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		s.closed = true // nothing to append to
		return errors.Join(cause, err)
	}
	s.f = f
	return cause
}

// Close stops background work, writes a final snapshot so the next start is
// fast, and closes the log. Events appended after Close are dropped.
func (s *Store) Close() error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil
	}
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop = nil
	}
	var err error
	if s.src != nil {
		err = s.Compact()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return err
	}
	s.closed = true
	return errors.Join(err, s.f.Sync(), s.f.Close())
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// syncDir makes renames in dir durable. It is best effort: some platforms
// (Windows) cannot sync directories.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
