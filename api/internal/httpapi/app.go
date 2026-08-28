// Package httpapi wires the three backing services into an HTTP API.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/okteto/app-with-floci/api/internal/awsdisc"
	"github.com/okteto/app-with-floci/api/internal/cache"
	"github.com/okteto/app-with-floci/api/internal/config"
	"github.com/okteto/app-with-floci/api/internal/search"
	"github.com/okteto/app-with-floci/api/internal/store"
)

// App is a pure consumer of infrastructure it does not create.
//
// The provisioning job owns the AWS resources, the schema, the seed data and the
// search index; this process only discovers where they are and connects. That
// split is the point of this branch: it shows what an application looks like
// when setup is entirely somebody else's problem, and what happens to it when
// that somebody has already finished and gone home.
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

// Supervise connects once and then watches. It returns an error instead of
// retrying, and the caller exits the process.
//
// There is nothing to retry for. The application cannot create what is missing,
// and the job that could has already run to completion, so waiting would only
// hide the problem behind a pod that looks healthy. Failing turns it into a
// CrashLoopBackOff, which is at least visible.
func (a *App) Supervise(ctx context.Context) error {
	const interval = 10 * time.Second

	if err := a.connect(ctx); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	a.ready.Store(true)
	slog.Info("connected - all three backing services ready")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		if err := a.verify(ctx); err != nil {
			a.ready.Store(false)
			msg := err.Error()
			a.bootErr.Store(&msg)
			return fmt.Errorf("backing services are no longer usable, and nothing "+
				"in this deployment re-provisions them: %w", err)
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

// connect discovers the endpoints and opens connections. It creates nothing: no
// resources, no schema, no index. A single probe rather than a poll, because the
// job has already finished by the time this runs - if anything is missing it is
// missing for good.
func (a *App) connect(ctx context.Context) error {
	a.closeDeps()

	slog.Info("discovering endpoints via Floci control plane", "endpoint", a.cfg.FlociEndpoint)
	snap := a.disc.Probe(ctx)

	if !snap.Ready {
		var missing []string
		for _, s := range snap.Services {
			if !s.Ready {
				missing = append(missing, fmt.Sprintf("%s (%s: %s)", s.Service, s.Resource, s.Status))
			}
		}
		return fmt.Errorf("not provisioned: %s - run the provisioning job",
			strings.Join(missing, ", "))
	}
	slog.Info("endpoints discovered",
		"postgres", snap.DB.Addr(), "valkey", snap.Cache.Addr(), "opensearch", snap.SearchURL)

	st, err := store.New(ctx, a.cfg.DSN(snap.DB.Host, snap.DB.Port))
	if err != nil {
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
	// The index is the job's to create. If it is absent the catalogue was never
	// provisioned, and searching would return an error on every request.
	if err := se.IndexExists(ctx); err != nil {
		st.Close()
		_ = ch.Close()
		return fmt.Errorf("search index missing - run the provisioning job: %w", err)
	}

	a.mu.Lock()
	a.store, a.cache, a.search = st, ch, se
	a.mu.Unlock()

	n, err := st.Count(ctx)
	if err != nil {
		return fmt.Errorf("catalogue unreadable - run the provisioning job: %w", err)
	}
	slog.Info("catalogue ready", "movies", n)
	return nil
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
