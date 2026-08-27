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
	"github.com/okteto/app-with-floci/api/internal/model"
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

// seed populates an empty catalogue so the demo always has something to search,
// and indexes it so PostgreSQL and OpenSearch start out consistent.
func (a *App) seed(ctx context.Context, st *store.Store, se *search.Search) error {
	n, err := st.Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("catalogue already populated", "movies", n)
		return a.reindexAll(ctx, st, se)
	}

	slog.Info("seeding catalogue")
	for _, m := range seedMovies {
		saved, err := st.Insert(ctx, &m)
		if err != nil {
			return err
		}
		if err := se.Index(ctx, saved); err != nil {
			return err
		}
	}
	return se.Refresh(ctx)
}

// reindexAll keeps OpenSearch consistent with PostgreSQL. The search index lives
// in a container whose lifecycle is independent of the database's, so the two
// can drift - most obviously when only one of them is replaced.
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

var seedMovies = []model.Movie{
	{ID: "the-shape-of-containers-2019", Title: "The Shape of Containers", Year: 2019, Genre: "Documentary",
		Director: "Ana Ruiz", Rating: 7.8, Synopsis: "A crew of engineers packages an entire datacentre into a single laptop and learns what leaks out."},
	{ID: "namespace-1998", Title: "Namespace", Year: 1998, Genre: "Thriller",
		Director: "Piotr Nowak", Rating: 8.1, Synopsis: "Two processes share a machine but cannot see each other. One of them starts leaving messages."},
	{ID: "the-privileged-2024", Title: "The Privileged", Year: 2024, Genre: "Drama",
		Director: "Marta Oliveira", Rating: 7.2, Synopsis: "A container escapes its sandbox and discovers it was root on the node all along."},
	{ID: "cold-start-2021", Title: "Cold Start", Year: 2021, Genre: "Comedy",
		Director: "Dan Whitfield", Rating: 6.9, Synopsis: "A function takes eleven seconds to wake up and the whole company waits."},
	{ID: "eventual-consistency-2016", Title: "Eventual Consistency", Year: 2016, Genre: "Romance",
		Director: "Yuki Tanaka", Rating: 7.5, Synopsis: "They agreed on everything, just never at the same time."},
	{ID: "the-cache-invalidation-2012", Title: "The Cache Invalidation", Year: 2012, Genre: "Horror",
		Director: "Greta Lindqvist", Rating: 8.4, Synopsis: "The data was correct. It was simply four hours old, and nobody noticed until the audit."},
	{ID: "fuzzy-match-2020", Title: "Fuzzy Match", Year: 2020, Genre: "Mystery",
		Director: "Samuel Adeyemi", Rating: 7.0, Synopsis: "A search engine returns the right answer to a question nobody asked."},
	{ID: "port-forward-2023", Title: "Port Forward", Year: 2023, Genre: "Thriller",
		Director: "Claire Beaumont", Rating: 6.6, Synopsis: "The only way into the cluster is a tunnel that closes every thirty seconds."},
}
