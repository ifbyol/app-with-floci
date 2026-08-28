package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/okteto/app-with-floci/api/internal/model"
	"github.com/okteto/app-with-floci/api/internal/store"
)

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/ready", a.handleReady)
	mux.HandleFunc("GET /api/aws/status", a.handleAWSStatus)

	mux.HandleFunc("GET /api/movies", a.requireReady(a.handleSearch))
	mux.HandleFunc("POST /api/movies", a.requireReady(a.handleCreate))
	mux.HandleFunc("GET /api/movies/{id}", a.requireReady(a.handleDetail))
	mux.HandleFunc("GET /api/trending", a.requireReady(a.handleTrending))

	return withLogging(mux)
}

// requireReady rejects data-plane requests until bootstrap has finished, so a
// half-initialised app returns a clear 503 rather than a nil-pointer panic.
func (a *App) requireReady(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() {
			detail := "backing services are still being provisioned"
			if msg := a.bootErr.Load(); msg != nil {
				detail = *msg
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":  "not ready",
				"detail": detail,
			})
			return
		}
		next(w, r)
	}
}

func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false})
		return
	}
	st, ch, se := a.deps()
	checks := map[string]string{"postgres": "ok", "valkey": "ok", "opensearch": "ok"}
	code := http.StatusOK

	if err := st.Ping(r.Context()); err != nil {
		checks["postgres"], code = err.Error(), http.StatusServiceUnavailable
	}
	if err := ch.Ping(r.Context()); err != nil {
		checks["valkey"], code = err.Error(), http.StatusServiceUnavailable
	}
	if err := se.Ping(r.Context()); err != nil {
		checks["opensearch"], code = err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": code == http.StatusOK, "checks": checks})
}

// handleAWSStatus is the emulator inspector. It re-runs the control-plane
// discovery on every call so the page reflects live state, including the
// advertised-versus-effective endpoint gap for OpenSearch.
func (a *App) handleAWSStatus(w http.ResponseWriter, r *http.Request) {
	snap := a.disc.Probe(r.Context())
	resp := map[string]any{
		"flociEndpoint": a.cfg.FlociEndpoint,
		"appReady":      a.ready.Load(),
		"ready":         snap.Ready,
		"services":      snap.Services,
	}
	if msg := a.bootErr.Load(); msg != nil {
		resp["bootstrapError"] = *msg
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	_, _, se := a.deps()
	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("size"))
	from, _ := strconv.Atoi(q.Get("from"))

	res, err := se.Query(r.Context(), q.Get("q"), q.Get("genre"), from, size)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search failed", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type cacheInfo struct {
	Hit          bool   `json:"hit"`
	Source       string `json:"source"`
	LookupMicros int64  `json:"lookupMicros"`
	TTLSeconds   int    `json:"ttlSeconds"`
}

type movieDetail struct {
	Movie model.Movie `json:"movie"`
	Cache cacheInfo   `json:"cache"`
}

// handleDetail is the cache-aside read path: Valkey first, PostgreSQL on a
// miss, then populate the cache. The timing and outcome are returned to the
// client so the cache is visible in the UI rather than merely implied.
func (a *App) handleDetail(w http.ResponseWriter, r *http.Request) {
	st, ch, _ := a.deps()
	id := r.PathValue("id")
	ttl := time.Duration(a.cfg.CacheTTLSeconds) * time.Second

	start := time.Now()
	if m, hit, err := ch.GetMovie(r.Context(), id); err == nil && hit {
		info := cacheInfo{Hit: true, Source: "valkey",
			LookupMicros: time.Since(start).Microseconds(), TTLSeconds: a.cfg.CacheTTLSeconds}
		_ = ch.BumpView(r.Context(), m.ID, m.Title)
		w.Header().Set("X-Cache", "HIT")
		writeJSON(w, http.StatusOK, movieDetail{Movie: *m, Cache: info})
		return
	}

	m, err := st.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "movie not found", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "database read failed", err)
		return
	}
	info := cacheInfo{Hit: false, Source: "postgres",
		LookupMicros: time.Since(start).Microseconds(), TTLSeconds: a.cfg.CacheTTLSeconds}

	if err := ch.SetMovie(r.Context(), m, ttl); err != nil {
		slog.Warn("cache populate failed", "id", id, "error", err)
	}
	_ = ch.BumpView(r.Context(), m.ID, m.Title)

	w.Header().Set("X-Cache", "MISS")
	writeJSON(w, http.StatusOK, movieDetail{Movie: *m, Cache: info})
}

type createRequest struct {
	Title    string  `json:"title"`
	Year     int     `json:"year"`
	Genre    string  `json:"genre"`
	Director string  `json:"director"`
	Synopsis string  `json:"synopsis"`
	Rating   float64 `json:"rating"`
}

// handleCreate is the write path that has to keep all three services
// consistent: commit to PostgreSQL, index into OpenSearch, drop the cache key.
func (a *App) handleCreate(w http.ResponseWriter, r *http.Request) {
	st, ch, se := a.deps()

	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", err)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Genre = strings.TrimSpace(req.Genre)
	req.Director = strings.TrimSpace(req.Director)

	if req.Title == "" || req.Genre == "" {
		writeErr(w, http.StatusBadRequest, "title and genre are required", nil)
		return
	}
	if req.Year < 1880 || req.Year > 2100 {
		writeErr(w, http.StatusBadRequest, "year must be between 1880 and 2100", nil)
		return
	}
	if req.Rating < 0 || req.Rating > 10 {
		writeErr(w, http.StatusBadRequest, "rating must be between 0 and 10", nil)
		return
	}

	m := &model.Movie{
		ID:       fmt.Sprintf("%s-%d", slugify(req.Title), req.Year),
		Title:    req.Title,
		Year:     req.Year,
		Genre:    req.Genre,
		Director: req.Director,
		Synopsis: req.Synopsis,
		Rating:   req.Rating,
	}

	saved, err := st.Insert(r.Context(), m)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeErr(w, http.StatusConflict, "a movie with that title and year already exists", nil)
			return
		}
		writeErr(w, http.StatusBadGateway, "database write failed", err)
		return
	}

	// PostgreSQL has committed. If indexing fails the row still exists, so the
	// error is reported rather than swallowed - the caller needs to know the
	// movie will not appear in search until a restart reindexes it.
	if err := se.Index(r.Context(), saved); err != nil {
		writeErr(w, http.StatusBadGateway, "saved to database but indexing failed", err)
		return
	}
	if err := se.Refresh(r.Context()); err != nil {
		slog.Warn("index refresh failed", "error", err)
	}
	if err := ch.Invalidate(r.Context(), saved.ID); err != nil {
		slog.Warn("cache invalidation failed", "id", saved.ID, "error", err)
	}

	writeJSON(w, http.StatusCreated, saved)
}

func (a *App) handleTrending(w http.ResponseWriter, r *http.Request) {
	_, ch, _ := a.deps()
	trend, err := ch.Trending(r.Context(), 10)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "trending read failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trending": trend})
}

// ---------- helpers ----------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "untitled"
	}
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string, err error) {
	body := map[string]any{"error": msg}
	if err != nil {
		body["detail"] = err.Error()
		slog.Error(msg, "error", err)
	}
	writeJSON(w, code, body)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start))
	})
}
