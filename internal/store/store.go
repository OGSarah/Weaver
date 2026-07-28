package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by lookups when the requested row does not exist, so
// the API can map it to a 404 rather than leaking a pgx-specific error.
var ErrNotFound = errors.New("not found")

// isMalformedID reports whether err is Postgres refusing to parse a value as a
// UUID: SQLSTATE 22P02, invalid_text_representation.
//
// Every id in this package arrives from a URL, so a caller can always send one
// that is not a UUID at all. Left alone that comes back as a database error and
// the API answers 500, which says "the server is broken" about a client typo, and
// logs it as though an operator should look. An id no row can hold is a row that
// does not exist, so lookups treat it exactly like one.
//
// The queries this guards interpolate the id and nothing else, so 22P02 cannot
// mean anything but the id here.
func isMalformedID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

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
