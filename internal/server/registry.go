package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"pricetag/internal/store"
)

// Registry opens each store's database lazily and closes idle ones, so one
// process can serve many stores without holding thousands of file handles.
type Registry struct {
	dir string
	log *slog.Logger

	mu   sync.Mutex
	open map[string]*entry
}

type entry struct {
	s           *store.Store
	refs        int
	last        time.Time
	lastCompact time.Time
}

func NewRegistry(dir string, log *slog.Logger) *Registry {
	return &Registry{dir: dir, log: log, open: map[string]*entry{}}
}

// Acquire returns the store and a release func that MUST be called when the
// caller is done (a store with outstanding references is never closed).
func (r *Registry) Acquire(slug string) (*store.Store, func(), error) {
	if !store.ValidSlug(slug) {
		return nil, nil, store.ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.open[slug]
	if e == nil {
		s, err := store.Open(r.dir, slug)
		if err != nil {
			return nil, nil, err
		}
		e = &entry{s: s, lastCompact: time.Now()}
		r.open[slug] = e
		go func() { // tidy tombstones/WAL once per process start, off the request path
			if err := s.Compact(context.Background()); err != nil {
				r.log.Warn("compact failed", "store", slug, "err", err)
			}
		}()
	}
	e.refs++
	e.last = time.Now()
	return e.s, func() { r.release(slug, e) }, nil
}

func (r *Registry) release(slug string, e *entry) {
	r.mu.Lock()
	e.refs--
	e.last = time.Now()
	r.mu.Unlock()
}

// Adopt registers a freshly created store (so the creator's handle is reused).
func (r *Registry) Adopt(s *store.Store) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.open[s.Slug]; ok {
		return errors.New("already open")
	}
	r.open[s.Slug] = &entry{s: s, last: time.Now(), lastCompact: time.Now()}
	return nil
}

// Exists is a cheap check that does not open the database.
func (r *Registry) Exists(slug string) bool { return store.Exists(r.dir, slug) }

func (r *Registry) List() ([]string, error) {
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".db"); ok && store.ValidSlug(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *Registry) OpenCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open)
}

// Janitor closes stores nobody has used for `idle` and compacts open ones
// daily, until ctx ends.
func (r *Registry) Janitor(ctx context.Context, every, idle time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(idle)
		}
	}
}

func (r *Registry) sweep(idle time.Duration) {
	now := time.Now()
	var toCompact []*store.Store
	r.mu.Lock()
	for slug, e := range r.open {
		if e.refs == 0 && e.s.Hub.Count() == 0 && now.Sub(e.last) > idle {
			e.s.Close()
			delete(r.open, slug)
			r.log.Debug("closed idle store", "store", slug)
			continue
		}
		if now.Sub(e.lastCompact) > 24*time.Hour {
			e.lastCompact = now
			toCompact = append(toCompact, e.s)
		}
	}
	r.mu.Unlock()
	for _, s := range toCompact {
		if err := s.Compact(context.Background()); err != nil {
			r.log.Warn("compact failed", "store", s.Slug, "err", err)
		}
	}
}

func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for slug, e := range r.open {
		e.s.Close()
		delete(r.open, slug)
	}
}
