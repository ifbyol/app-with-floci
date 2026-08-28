// Package httpapi wires the three backing services into an HTTP API.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/okteto/app-with-floci/api/internal/awsdisc"
	"github.com/okteto/app-with-floci/api/internal/cache"
	"github.com/okteto/app-with-floci/api/internal/config"
	"github.com/okteto/app-with-floci/api/internal/search"
	"github.com/okteto/app-with-floci/api/internal/store"
)

// App serves HTTP straight away, connects to its backing services in the
// background, and then keeps verifying that the services it connected to are
// still the ones holding its data.
//
// Serving before connecting is deliberate: provisioning OpenSearch can take
// minutes on a cold image cache, and a process that refused connections until
// then would fail its Kubernetes probes and be restarted in a loop. Instead
// /api/health and /api/aws/status answer immediately, so provisioning is
// observable while it happens.
type App struct {
	cfg  *config.Config
	disc *awsdisc.Discoverer

	ready   atomic.Bool
	bootErr atomic.Pointer[string]

	mu     sync.RWMutex
	store  *store.Store
	cache  *cache.Cache
	search *search.Search
}

func NewApp(cfg *config.Config) *App {
	return &App{cfg: cfg, disc: awsdisc.New(cfg)}
}

// Supervise owns the connection lifecycle for the whole process lifetime.
//
// Connecting once is not sufficient here. When Floci restarts it provisions
// brand new engine containers behind the same proxy addresses, so this process
// stays "connected" to a PostgreSQL that no longer has its schema and an
// OpenSearch node with no index - and nothing about the TCP connection looks
// wrong. Okteto namespaces scale to zero, which makes that routine rather than
// exceptional, so the check has to be semantic: is our data still there?
func (a *App) Supervise(ctx context.Context) {
	const interval = 10 * time.Second

	for {
		if !a.ready.Load() {
			if err := a.connect(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				msg := err.Error()
				a.bootErr.Store(&msg)
				slog.Error("connect failed, will retry", "error", err)
			} else {
				a.bootErr.Store(nil)
				a.ready.Store(true)
				slog.Info("connected - all three backing services ready")
			}
		} else if err := a.verify(ctx); err != nil {
			// Never spin: dropping ready here means the next tick reconnects.
			slog.Warn("backing services were replaced underneath us; reconnecting",
				"error", err)
			msg := err.Error()
			a.bootErr.Store(&msg)
			a.ready.Store(false)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// verify is a liveness check with teeth. Count touches the movies table and
// IndexExists touches the index, so a restored-but-empty engine fails here
// instead of quietly serving nothing.
func (a *App) verify(ctx context.Context) error {
	st, ch, se := a.deps()
	if st == nil || ch == nil || se == nil {
		return errors.New("not connected")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := st.Count(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := ch.Ping(ctx); err != nil {
		return fmt.Errorf("valkey: %w", err)
	}
	if err := se.IndexExists(ctx); err != nil {
		return fmt.Errorf("opensearch: %w", err)
	}
	return nil
}

// connect discovers endpoints, connects, migrates and seeds. Any previous
// generation of connections is closed first.
func (a *App) connect(ctx context.Context) error {
	a.closeDeps()

	timeout := time.Duration(a.cfg.ReadyTimeoutSeconds) * time.Second
	slog.Info("provisioning and discovering via Floci control plane",
		"endpoint", a.cfg.FlociEndpoint, "timeout", timeout)

	// Create anything missing before waiting for it. Idempotent, so this is also
	// what re-provisions after a Floci restart wiped its in-memory state.
	if err := a.disc.Ensure(ctx); err != nil {
		return err
	}

	snap, err := a.disc.WaitReady(ctx, timeout)
	if err != nil {
		return err
	}
	slog.Info("endpoints discovered",
		"postgres", snap.DB.Addr(), "valkey", snap.Cache.Addr(), "opensearch", snap.SearchURL)

	st, err := store.New(ctx, a.cfg.DSN(snap.DB.Host, snap.DB.Port))
	if err != nil {
		return err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return err
	}

	ch := cache.New(snap.Cache.Addr())
	if err := ch.Ping(ctx); err != nil {
		st.Close()
		return err
	}

	se, err := search.New(snap.SearchURL)
	if err != nil {
		st.Close()
		_ = ch.Close()
		return err
	}
	if err := se.EnsureIndex(ctx); err != nil {
		st.Close()
		_ = ch.Close()
		return err
	}

	a.mu.Lock()
	a.store, a.cache, a.search = st, ch, se
	a.mu.Unlock()

	return a.seed(ctx, st, se)
}

func (a *App) deps() (*store.Store, *cache.Cache, *search.Search) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store, a.cache, a.search
}

func (a *App) closeDeps() {
	a.mu.Lock()
	st, ch := a.store, a.cache
	a.store, a.cache, a.search = nil, nil, nil
	a.mu.Unlock()

	if st != nil {
		st.Close()
	}
	if ch != nil {
		_ = ch.Close()
	}
}

func (a *App) Close() { a.closeDeps() }

// seed loads the starter catalogue into PostgreSQL when it is empty, then
// rebuilds the search index from whatever PostgreSQL holds.
//
// The rows live in store/seed.sql rather than in Go: it is data, it is easier to
// extend, and it keeps PostgreSQL unambiguously the source of truth. OpenSearch
// is never seeded directly - reindexAll derives it - which means one code path
// covers both a first run and a Floci restart that replaced the search node
// while leaving the database intact.
// reindexAll keeps OpenSearch consistent with PostgreSQL. The search index lives
// in a container whose lifecycle is independent of the database's, so the two
// can drift - most obviously when only one of them is replaced.
func (a *App) seed(ctx context.Context, st *store.Store, se *search.Search) error {
	n, err := st.Count(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		inserted, err := st.Seed(ctx)
		if err != nil {
			return err
		}
		slog.Info("seeded catalogue from seed.sql", "movies", inserted)
	} else {
		slog.Info("catalogue already populated", "movies", n)
	}
	return a.reindexAll(ctx, st, se)
}

func (a *App) reindexAll(ctx context.Context, st *store.Store, se *search.Search) error {
	movies, err := st.All(ctx)
	if err != nil {
		return err
	}
	for i := range movies {
		if err := se.Index(ctx, &movies[i]); err != nil {
			return err
		}
	}
	slog.Info("reindexed catalogue into OpenSearch", "movies", len(movies))
	return se.Refresh(ctx)
}
