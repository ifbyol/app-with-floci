// Package cache is the Valkey read cache, reached through Floci's ElastiCache
// proxy. It uses two data structures: string keys for cache-aside movie reads,
// and a sorted set plus hash for trending counts.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/okteto/app-with-floci/api/internal/model"
)

const (
	movieKeyPrefix = "movie:"
	trendingZSet   = "trending"
	trendingTitles = "trending:titles"
)

type Cache struct{ rdb *redis.Client }

func New(addr string) *Cache {
	return &Cache{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func (c *Cache) Close() error { return c.rdb.Close() }

func (c *Cache) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// GetMovie returns the cached movie and whether it was a hit. A cache miss is
// not an error.
func (c *Cache) GetMovie(ctx context.Context, id string) (*model.Movie, bool, error) {
	raw, err := c.rdb.Get(ctx, movieKeyPrefix+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m model.Movie
	if err := json.Unmarshal(raw, &m); err != nil {
		// A corrupt entry should behave as a miss, not break the request.
		return nil, false, nil
	}
	return &m, true, nil
}

func (c *Cache) SetMovie(ctx context.Context, m *model.Movie, ttl time.Duration) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, movieKeyPrefix+m.ID, raw, ttl).Err()
}

func (c *Cache) Invalidate(ctx context.Context, id string) error {
	return c.rdb.Del(ctx, movieKeyPrefix+id).Err()
}

// BumpView increments the trending counter and records the title alongside it
// so the leaderboard can be rendered without touching PostgreSQL.
func (c *Cache) BumpView(ctx context.Context, id, title string) error {
	pipe := c.rdb.Pipeline()
	pipe.ZIncrBy(ctx, trendingZSet, 1, id)
	pipe.HSet(ctx, trendingTitles, id, title)
	_, err := pipe.Exec(ctx)
	return err
}

type Trend struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Views int64  `json:"views"`
}

func (c *Cache) Trending(ctx context.Context, n int) ([]Trend, error) {
	entries, err := c.rdb.ZRevRangeWithScores(ctx, trendingZSet, 0, int64(n-1)).Result()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return []Trend{}, nil
	}

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, fmt.Sprint(e.Member))
	}
	titles, err := c.rdb.HMGet(ctx, trendingTitles, ids...).Result()
	if err != nil {
		return nil, err
	}

	out := make([]Trend, 0, len(entries))
	for i, e := range entries {
		t := Trend{ID: ids[i], Views: int64(e.Score)}
		if i < len(titles) {
			if s, ok := titles[i].(string); ok {
				t.Title = s
			}
		}
		out = append(out, t)
	}
	return out, nil
}
