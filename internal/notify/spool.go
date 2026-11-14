package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

// Spool persists outgoing push notifications on disk before trying to send
// them, and retries transient timeouts (such as HTTP 503 from Keelworks)
// until acknowledged. Permanent refusals are dropped after logging so they
// do not stall later notifications.
type Spool struct {
	dir    string
	inner  Sender
	log    *log.Logger
	seq    atomic.Uint64
	sendMu sync.Mutex
	wake   chan struct{}
}

// OpenSpool opens or creates the notification spool directory at dir, clears
// leftover temporary files, and verifies any queued notifications on disk.
func OpenSpool(dir string, inner Sender, logger *log.Logger) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, pferr.Parsef("the notification spool could not be opened")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, pferr.Parsef("the notification spool could not be read")
	}
	var maxSeq uint64
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(full)
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		prefix := strings.TrimSuffix(name, ".json")
		n, err := strconv.ParseUint(prefix, 16, 64)
		if err != nil {
			return nil, pferr.Parsef("the notification spool holds an unexpected file")
		}
		if _, err := readQueuedEvent(full); err != nil {
			return nil, err
		}
		if n > maxSeq {
			maxSeq = n
		}
	}
	sp := &Spool{
		dir:   dir,
		inner: inner,
		log:   logger,
		wake:  make(chan struct{}, 1),
	}
	sp.seq.Store(maxSeq)
	return sp, nil
}

func readQueuedEvent(path string) (Event, error) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 64<<10 {
		return Event{}, pferr.Parsef("a queued notification could not be read")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Event
	if err := dec.Decode(&e); err != nil {
		return Event{}, pferr.Parsef("a queued notification is malformed")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Event{}, pferr.Parsef("a queued notification is malformed")
	}
	if e.Repository == "" || e.Ref == "" {
		return Event{}, pferr.Parsef("a queued notification is malformed")
	}
	if _, err := store.ParseOID(e.Before); err != nil {
		return Event{}, err
	}
	if _, err := store.ParseOID(e.After); err != nil {
		return Event{}, err
	}
	return e, nil
}

// Send writes e to the spool on disk and immediately tries to deliver pending
// notifications in order. If delivery fails transiently (E_TIMEOUT), e stays
// on disk for background retry.
func (s *Spool) Send(ctx context.Context, e Event) error {
	id := s.seq.Add(1)
	final := filepath.Join(s.dir, fmt.Sprintf("%016x.json", id))
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return pferr.Parsef("the notification spool could not be written")
	}
	_, werr := f.Write(Body(e))
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return pferr.Parsef("the notification spool could not be written")
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return pferr.Parsef("the notification spool could not be written")
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	err = s.inner.Send(ctx, e)
	if err == nil {
		_ = os.Remove(final)
		return nil
	}
	pe := pferr.Public(err, "the push notification failed")
	if pe.Code == pferr.Timeout {
		s.signal()
		return pe
	}
	_ = os.Remove(final)
	return pe
}

func (s *Spool) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Pending returns how many notifications are currently waiting on disk.
func (s *Spool) Pending() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// Flush tries once to deliver every queued notification in FIFO order. It
// stops at the first E_TIMEOUT so order is preserved, and returns the count
// of notifications still waiting.
func (s *Spool) Flush(ctx context.Context) int {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for i, name := range names {
		path := filepath.Join(s.dir, name)
		ev, err := readQueuedEvent(path)
		if err != nil {
			_ = os.Remove(path)
			if s.log != nil {
				s.log.Printf("notification dropped: %s", pferr.Public(err, "a queued notification is malformed"))
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		err = s.inner.Send(cctx, ev)
		cancel()
		if err == nil {
			_ = os.Remove(path)
			if s.log != nil {
				s.log.Printf("queued notification sent repo=%s ref=%s", ev.Repository, ev.Ref)
			}
			continue
		}
		pe := pferr.Public(err, "the push notification failed")
		if pe.Code == pferr.Timeout {
			return len(names) - i
		}
		_ = os.Remove(path)
		if s.log != nil {
			s.log.Printf("queued notification refused repo=%s ref=%s: %s", ev.Repository, ev.Ref, pe)
		}
	}
	return 0
}

// Run retries queued notifications in the background until ctx is canceled.
func (s *Spool) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	s.Flush(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.Flush(ctx)
		case <-ticker.C:
			if s.Pending() > 0 {
				s.Flush(ctx)
			}
		}
	}
}
