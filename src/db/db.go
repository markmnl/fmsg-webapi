// Package db provides PostgreSQL database connectivity and helper methods.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgxpool.Pool providing helper methods for the application.
type DB struct {
	Pool *pgxpool.Pool
}

// New opens a connection pool to PostgreSQL using the standard PG environment
// variables (PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE, etc.).
// An empty dsn string causes pgxpool to read from the environment.
func New(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: failed to create pool: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("db: failed to ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close releases all connections held by the pool.
func (d *DB) Close() {
	d.Pool.Close()
}
