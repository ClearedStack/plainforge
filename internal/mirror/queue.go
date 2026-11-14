package mirror

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

const pendingMarker = "mirror.pending"

// Enqueuer schedules a repository for outbound mirroring without blocking
// internal Git operations.
type Enqueuer interface {
	Enqueue(repo *store.Repo)
}

// Queue persists per-repository mirror markers and pushes updates in the
// background, retrying transient failures (such as GitHub timeouts or HTTP
// 429 rate limits) while internal commits continue unimpeded.
type Queue struct {
	pusher Pusher
	log    *log.Logger

	mu      sync.Mutex
	pending map[string]*store.Repo
	gen     map[string]uint64
	syncMu  sync.Mutex
	wake    chan struct{}
}

// NewQueue builds a mirror queue backed by pusher.
func NewQueue(pusher Pusher, logger *log.Logger) *Queue {
	return &Queue{
		pusher:  pusher,
		log:     logger,
		pending: make(map[string]*store.Repo),
		gen:     make(map[string]uint64),
		wake:    make(chan struct{}, 1),
	}
}

func markerPath(repo *store.Repo) string {
	return filepath.Join(repo.SpoolDir(), pendingMarker)
}

// Recover scans repos at startup and re-queues any repository that still has
// an unfinished mirror marker on disk.
func (q *Queue) Recover(repos []*store.Repo) {
	q.mu.Lock()
	for _, r := range repos {
		if _, err := os.Stat(markerPath(r)); err == nil {
			q.gen[r.Name()]++
			q.pending[r.Name()] = r
		}
	}
	has := len(q.pending) > 0
	q.mu.Unlock()
	if has {
		q.signal()
	}
}

// Enqueue marks repo as needing a mirror push and wakes the background loop.
func (q *Queue) Enqueue(repo *store.Repo) {
	if err := os.MkdirAll(repo.SpoolDir(), 0o755); err == nil {
		tmp := markerPath(repo) + ".tmp"
		if err := os.WriteFile(tmp, []byte("1\n"), 0o600); err == nil {
			_ = os.Rename(tmp, markerPath(repo))
		}
	}
	q.mu.Lock()
	q.gen[repo.Name()]++
	q.pending[repo.Name()] = repo
	q.mu.Unlock()
	q.signal()
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// PendingCount returns how many repositories are waiting to be mirrored.
func (q *Queue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// SyncOnce tries once to mirror every currently queued repository and returns
// how many repositories remain queued afterwards.
func (q *Queue) SyncOnce(ctx context.Context) int {
	q.syncMu.Lock()
	defer q.syncMu.Unlock()

	type item struct {
		name string
		repo *store.Repo
		gen  uint64
	}
	q.mu.Lock()
	items := make([]item, 0, len(q.pending))
	for name, repo := range q.pending {
		items = append(items, item{name: name, repo: repo, gen: q.gen[name]})
	}
	q.mu.Unlock()

	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	for _, it := range items {
		err := q.pusher.Sync(ctx, it.repo)
		if err == nil {
			q.finish(it.repo, it.gen)
			if q.log != nil {
				q.log.Printf("mirror synced repo=%s", it.name)
			}
			continue
		}
		pe := pferr.Public(err, "the mirror push failed")
		if pe.Code == pferr.Timeout {
			if q.log != nil {
				q.log.Printf("mirror deferred repo=%s: %s", it.name, pe)
			}
			continue
		}
		// Permanent refusal: log and clear this generation so we do not loop
		// forever on a configuration mismatch.
		q.finish(it.repo, it.gen)
		if q.log != nil {
			q.log.Printf("mirror refused repo=%s: %s", it.name, pe)
		}
	}
	return q.PendingCount()
}

func (q *Queue) finish(repo *store.Repo, syncedGen uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.gen[repo.Name()] != syncedGen {
		return
	}
	delete(q.pending, repo.Name())
	_ = os.Remove(markerPath(repo))
}

// Run processes queued mirror pushes until ctx is canceled.
func (q *Queue) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if q.PendingCount() > 0 {
		q.SyncOnce(ctx)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
			for q.PendingCount() > 0 {
				before := q.PendingCount()
				after := q.SyncOnce(ctx)
				if after >= before || ctx.Err() != nil {
					break
				}
			}
		case <-ticker.C:
			if q.PendingCount() > 0 {
				q.SyncOnce(ctx)
			}
		}
	}
}
