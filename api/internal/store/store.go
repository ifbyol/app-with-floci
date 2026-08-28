// Package store is the PostgreSQL source of truth, reached through Floci's RDS
// auth proxy.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okteto/app-with-floci/api/internal/model"
)

var ErrNotFound = errors.New("movie not found")

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM movies`).Scan(&n)
	return n, err
}

func (s *Store) Get(ctx context.Context, id string) (*model.Movie, error) {
	const q = `SELECT id, title, year, genre, director, synopsis, rating, created_at
	           FROM movies WHERE id = $1`
	var m model.Movie
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&m.ID, &m.Title, &m.Year, &m.Genre, &m.Director, &m.Synopsis, &m.Rating, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) All(ctx context.Context) ([]model.Movie, error) {
	const q = `SELECT id, title, year, genre, director, synopsis, rating, created_at
	           FROM movies ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Movie
	for rows.Next() {
		var m model.Movie
		if err := rows.Scan(&m.ID, &m.Title, &m.Year, &m.Genre, &m.Director,
			&m.Synopsis, &m.Rating, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Insert writes one movie inside a transaction and returns the stored row, so
// server-side defaults such as created_at are reflected back to the caller.
func (s *Store) Insert(ctx context.Context, m *model.Movie) (*model.Movie, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	const q = `INSERT INTO movies (id, title, year, genre, director, synopsis, rating)
	           VALUES ($1,$2,$3,$4,$5,$6,$7)
	           RETURNING id, title, year, genre, director, synopsis, rating, created_at`
	var out model.Movie
	err = tx.QueryRow(ctx, q, m.ID, m.Title, m.Year, m.Genre, m.Director, m.Synopsis, m.Rating).
		Scan(&out.ID, &out.Title, &out.Year, &out.Genre, &out.Director,
			&out.Synopsis, &out.Rating, &out.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}
