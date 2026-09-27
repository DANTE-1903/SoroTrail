//go:build integration

package store

// Database-backed tests for the store's connection recovery: the periodic
// health check pings the pool and, on a transient failure, builds a replacement
// pool from the stored URL and swaps it in. A healthy pool must be left alone.
// They need a real Postgres and skip without TEST_DATABASE_URL, like the rest of
// this package's integration suite (see postgres_test.go). Run with:
//
//	go test -tags=integration -run 'TestPerformHealthCheck' ./internal/store/

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerformHealthCheck_TransientFailureReconnects covers the happy recovery:
// a ping of the broken pool fails, the health check builds a replacement from
// the stored URL, and the instance becomes usable again with the attempt counter
// reset.
func TestPerformHealthCheck_TransientFailureReconnects(t *testing.T) {
	url := dbURL(t)
	ctx := context.Background()
	pg := healthTestPostgres(t, newBrokenPool(t), url)
	broken := pg.pool

	attempt := 0
	pg.performHealthCheck(ctx, healthTestLogger(), &attempt)

	assert.Equal(t, 0, attempt, "a successful reconnect resets the attempt counter")
	assert.True(t, pg.healthyAtomic.Load(), "a working pool must be marked healthy")
	assert.NotSame(t, broken, pg.pool, "the broken pool must be replaced")
	require.NoError(t, pg.Ping(ctx), "Ping must succeed through the replacement pool")
}

// TestPerformHealthCheck_HealthyPoolNotReconnected covers the no-op case: a
// healthy pool must not be replaced and must not consume a reconnect attempt.
func TestPerformHealthCheck_HealthyPoolNotReconnected(t *testing.T) {
	url := dbURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx), "precondition: the pool must be reachable")

	pg := healthTestPostgres(t, pool, url)
	before := pg.pool

	attempt := 5
	pg.performHealthCheck(ctx, healthTestLogger(), &attempt)

	assert.Equal(t, 5, attempt, "a healthy pool must not trigger a reconnect attempt")
	assert.Same(t, before, pg.pool, "a healthy pool must not be replaced")
	assert.True(t, pg.healthyAtomic.Load())
}
