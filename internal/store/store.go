package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the database connection pool. The pool is safe for concurrent
// use, so every worker shares one.
type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	// New is lazy, so ping to fail fast on a bad URL or unreachable database.
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}