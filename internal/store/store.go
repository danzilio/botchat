// Package store persists a hub to a data directory (SPEC.md "Persistence",
// PER-*). It keeps two kinds of files:
//
//   - snapshot.json: a hub.State, replaced atomically (write, fsync, rename).
//   - events.jsonl: the journal since the snapshot, one JSON object per line:
//     either an event, or {"touch":{...}} for activity that updated an
//     agent's last_seen without an event. During compaction the log is first
//     renamed to events.jsonl.old, then a new snapshot is written, then the
//     old log is removed. Loading reads all three and skips what the snapshot
//     already covers, so a crash at any step loses nothing (PER-4).
//
// Records are written before the hub acknowledges the change (PER-2) and the
// log is fsynced on a timer. If a write or fsync fails, the store fails:
// every later write returns the error, Failed is closed, and no snapshot is
// written from then on, since the hub may hold a change that was never saved
// (PER-5).
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
	// CompactEvery is the number of journal records after which the log is
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

// Store is an open data directory. It implements hub.Journal.
type Store struct {
	dir  string
	opts Options

	mu     sync.Mutex
	f      *os.File
	off    int64 // end of the last complete record in f
	n      int   // records in f
	dirty  bool  // written since the last fsync
	closed bool
	err    error         // set once the store has failed (PER-5)
	failed chan struct{} // closed when err is set

	src  Source
	kick chan struct{}
	stop chan struct{}
	done chan struct{}
	cmu  sync.Mutex // serializes compactions
}

var errClosed = errors.New("data directory is closed")

// line is the on-disk form of a journal record. An event line has the
// event's own fields; a touch line has only "touch".
type line struct {
	hub.Event
	Touch *hub.Touch `json:"touch,omitempty"`
}

// Open opens (creating if needed) the data directory dir and loads what it
// holds. It returns the snapshot (nil if there is none) and the journal
// records written after it, for hub.Restore. A truncated final record is
// discarded; any other unreadable data is an error (PER-3).
func Open(dir string, opts Options) (*Store, *hub.State, []hub.Record, error) {
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
	s := &Store{dir: dir, opts: opts, kick: make(chan struct{}, 1), failed: make(chan struct{})}

	var st *hub.State
	if b, err := os.ReadFile(s.path(snapshotFile)); err == nil {
		st = new(hub.State)
		if err := json.Unmarshal(b, st); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.path(snapshotFile), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}

	var records []hub.Record
	if f, err := os.Open(s.path(oldLogFile)); err == nil {
		records, _, err = readLog(f, records)
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
	before := len(records)
	records, off, err := readLog(f, records)
	if err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("%s: %w", s.path(logFile), err)
	}
	if err := f.Truncate(off); err != nil { // drop a torn final record
		f.Close()
		return nil, nil, nil, err
	}
	s.f, s.off, s.n = f, off, len(records)-before
	return s, st, records, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// readLog appends the records in r to records. It returns the offset just
// past the last complete line. A final line without a newline is a write cut
// short by a crash and is ignored; a bad line anywhere else is an error.
func readLog(r io.Reader, records []hub.Record) ([]hub.Record, int64, error) {
	br := bufio.NewReader(r)
	var off int64
	for n := 1; ; n++ {
		b, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return records, off, nil // b, if any, is a torn record
		}
		if err != nil {
			return nil, 0, err
		}
		if len(bytes.TrimSpace(b)) > 0 {
			var l line
			if err := json.Unmarshal(b, &l); err != nil {
				return nil, 0, fmt.Errorf("line %d: %w", n, err)
			}
			switch {
			case l.Touch != nil:
				records = append(records, hub.Record{Touch: l.Touch})
			case l.Type != "":
				e := l.Event
				records = append(records, hub.Record{Event: &e})
			default:
				return nil, 0, fmt.Errorf("line %d: neither an event nor a touch", n)
			}
		}
		off += int64(len(b))
	}
}

// Event writes e to the log (hub.Journal).
func (s *Store) Event(e hub.Event) error { return s.append(e) }

// Touch writes t to the log (hub.Journal).
func (s *Store) Touch(t hub.Touch) error {
	return s.append(struct {
		Touch *hub.Touch `json:"touch"`
	}{&t})
}

func (s *Store) append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return errClosed
	}
	if _, err := s.f.WriteAt(b, s.off); err != nil {
		// Cut off a partial record in case the disk recovers before a
		// restart; the next start must still be able to read the log.
		_ = s.f.Truncate(s.off)
		return s.failLocked(fmt.Errorf("write %s: %w", s.path(logFile), err))
	}
	s.off += int64(len(b))
	s.dirty = true
	s.n++
	if s.n >= s.opts.CompactEvery {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *Store) failLocked(err error) error {
	if s.err == nil {
		s.err = err
		close(s.failed)
	}
	return s.err
}

// Failed is closed when the store can no longer save changes (PER-5).
func (s *Store) Failed() <-chan struct{} { return s.failed }

// Err returns why the store failed, or nil.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
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
			s.sync()
		case <-s.kick:
			if err := s.Compact(); err != nil {
				s.opts.Logf("botchat: compact %s: %v", s.dir, err)
			}
		}
	}
}

// sync flushes the log. A failed fsync may have lost acknowledged writes, so
// it fails the store (PER-5).
func (s *Store) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.err != nil || !s.dirty {
		return s.err
	}
	s.dirty = false
	if err := s.f.Sync(); err != nil {
		return s.failLocked(fmt.Errorf("sync %s: %w", s.path(logFile), err))
	}
	return nil
}

// Compact replaces the log with a snapshot of the source (PER-4). It does
// nothing useful once the store has failed: the source may then hold a change
// that was never saved, which must not reach the snapshot.
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
// record in the moved log is covered by the snapshot being taken. If an old
// log is still there (a previous compaction failed after rotating), it keeps
// appending to the current log instead; loading skips what the snapshot
// covers, so that is only a little wasteful.
func (s *Store) rotateLocked() error {
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return errClosed
	}
	if err := s.f.Sync(); err != nil {
		return s.failLocked(fmt.Errorf("sync %s: %w", s.path(logFile), err))
	}
	s.dirty = false
	s.n = 0
	if _, err := os.Stat(s.path(oldLogFile)); err == nil {
		return nil
	}
	// Close before renaming: Windows cannot rename open files. A crash
	// between the rename and the create leaves no log, which Open handles.
	if err := s.f.Close(); err != nil {
		return s.reopenLocked(err)
	}
	if err := os.Rename(s.path(logFile), s.path(oldLogFile)); err != nil {
		return s.reopenLocked(err)
	}
	f, err := os.OpenFile(s.path(logFile), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Put the old log back so appends continue where they were.
		if err2 := os.Rename(s.path(oldLogFile), s.path(logFile)); err2 != nil {
			return s.failLocked(errors.Join(err, err2))
		}
		return s.reopenLocked(err)
	}
	syncDir(s.dir)
	s.f, s.off = f, 0
	return nil
}

// reopenLocked reopens the log after a failed rotation and returns cause. If
// the log can't be reopened there is nowhere to write, so the store fails.
func (s *Store) reopenLocked(cause error) error {
	f, err := os.OpenFile(s.path(logFile), os.O_RDWR, 0o600)
	if err != nil {
		return s.failLocked(errors.Join(cause, err))
	}
	s.f = f
	return cause
}

// Close stops background work, writes a final snapshot so the next start is
// fast (unless the store has failed), and closes the log. Writes after Close
// return an error. Close returns the error the store failed with, if any.
func (s *Store) Close() error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return s.Err()
	}
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop = nil
	}
	var err error
	if s.src != nil && s.Err() == nil {
		err = s.Compact()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.err != nil {
		s.f.Close()
		return s.err
	}
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
