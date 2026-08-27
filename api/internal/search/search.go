// Package search is the OpenSearch query layer.
//
// Unlike PostgreSQL and Valkey, traffic here does not pass through Floci: the
// OpenSearch container publishes its own port, so this talks to the engine
// directly. Floci's only role is the control-plane call that told us where it is.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/okteto/app-with-floci/api/internal/model"
)

const IndexName = "movies"

type Search struct {
	c     *opensearchapi.Client
	index string
}

func New(addr string) (*Search, error) {
	c, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{Addresses: []string{addr}},
	})
	if err != nil {
		return nil, fmt.Errorf("opensearch client: %w", err)
	}
	return &Search{c: c, index: IndexName}, nil
}

func (s *Search) Ping(ctx context.Context) error {
	_, err := s.c.Cluster.Health(ctx, nil)
	return err
}

// IndexExists reports an error when the index is gone. A restarted Floci hands
// out a brand new, empty OpenSearch node on the same address, so reachability
// alone is not evidence that our data is still there.
func (s *Search) IndexExists(ctx context.Context) error {
	resp, err := s.c.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{s.index},
	})
	if err != nil {
		return fmt.Errorf("index %q check failed: %w", s.index, err)
	}
	if resp != nil && resp.StatusCode == 404 {
		return fmt.Errorf("index %q no longer exists", s.index)
	}
	return nil
}

// mapping keeps genre a keyword so it can be aggregated and filtered exactly,
// while title, director and synopsis stay analysed for full-text matching.
// Replicas are zero because Floci runs a single node - otherwise the cluster
// would sit permanently yellow.
const mapping = `{
  "settings": { "number_of_shards": 1, "number_of_replicas": 0 },
  "mappings": {
    "properties": {
      "id":        { "type": "keyword" },
      "title":     { "type": "text", "fields": { "raw": { "type": "keyword" } } },
      "year":      { "type": "integer" },
      "genre":     { "type": "keyword" },
      "director":  { "type": "text", "fields": { "raw": { "type": "keyword" } } },
      "synopsis":  { "type": "text" },
      "rating":    { "type": "float" },
      "createdAt": { "type": "date" }
    }
  }
}`

func (s *Search) EnsureIndex(ctx context.Context) error {
	_, err := s.c.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: s.index,
		Body:  strings.NewReader(mapping),
	})
	if err != nil && !strings.Contains(err.Error(), "resource_already_exists_exception") {
		return fmt.Errorf("create index: %w", err)
	}
	return nil
}

func (s *Search) Index(ctx context.Context, m *model.Movie) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.c.Index(ctx, opensearchapi.IndexReq{
		Index:      s.index,
		DocumentID: m.ID,
		Body:       bytes.NewReader(raw),
	})
	if err != nil {
		return fmt.Errorf("index document: %w", err)
	}
	return nil
}

// Refresh makes just-indexed documents visible to search. Production code would
// rely on the refresh interval; a demo wants the write to show up immediately.
func (s *Search) Refresh(ctx context.Context) error {
	_, err := s.c.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{
		Index: []string{s.index},
	})
	return err
}

type Hit struct {
	model.Movie
	Score float32 `json:"score"`
}

type Facet struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type Results struct {
	Total      int     `json:"total"`
	TookMillis int     `json:"tookMillis"`
	Hits       []Hit   `json:"hits"`
	Genres     []Facet `json:"genres"`
}

// Query runs a fuzzy multi-field search with an optional exact genre filter and
// returns genre facets alongside the hits.
func (s *Search) Query(ctx context.Context, q, genre string, size int) (*Results, error) {
	if size <= 0 || size > 100 {
		size = 20
	}

	var must any
	if strings.TrimSpace(q) == "" {
		must = map[string]any{"match_all": map[string]any{}}
	} else {
		must = map[string]any{
			"multi_match": map[string]any{
				"query":     q,
				"fields":    []string{"title^3", "director^2", "synopsis"},
				"fuzziness": "AUTO",
			},
		}
	}

	filters := []any{}
	if genre != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"genre": genre}})
	}

	body := map[string]any{
		"size": size,
		"query": map[string]any{
			"bool": map[string]any{"must": must, "filter": filters},
		},
		"aggs": map[string]any{
			"genres": map[string]any{
				"terms": map[string]any{"field": "genre", "size": 20},
			},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	resp, err := s.c.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{s.index},
		Body:    bytes.NewReader(raw),
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	out := &Results{
		Total:      resp.Hits.Total.Value,
		TookMillis: resp.Took,
		Hits:       make([]Hit, 0, len(resp.Hits.Hits)),
		Genres:     []Facet{},
	}
	for _, h := range resp.Hits.Hits {
		var m model.Movie
		if err := json.Unmarshal(h.Source, &m); err != nil {
			continue
		}
		out.Hits = append(out.Hits, Hit{Movie: m, Score: h.Score})
	}

	if len(resp.Aggregations) > 0 {
		var aggs struct {
			Genres struct {
				Buckets []struct {
					Key      string `json:"key"`
					DocCount int    `json:"doc_count"`
				} `json:"buckets"`
			} `json:"genres"`
		}
		if err := json.Unmarshal(resp.Aggregations, &aggs); err == nil {
			for _, b := range aggs.Genres.Buckets {
				out.Genres = append(out.Genres, Facet{Key: b.Key, Count: b.DocCount})
			}
		}
	}
	return out, nil
}
