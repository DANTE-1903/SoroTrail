package store

// Tests for the connection recovery helpers that run without a database:
// attemptReconnect's refusal and failure paths, Ping's reporting of a pool
// already known to be unhealthy, and performHealthCheck's behaviour when the
// replacement pool cannot be built either. The cases that need a real database
// — a successful reconnect and a healthy pool left alone — live in
// postgres_health_integration_test.go.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreachablePoolURL parses as a valid pool URL but refuses every connection,
// standing in for a lost database without needing to tear one down.
const unreachablePoolURL = "postgres://sorotrail:sorotrail@127.0.0.1:1/sorotrail?sslmode=disable&connect_timeout=1"

func healthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBrokenPool returns a pool pointing at a refused port. The pool is created
// lazily: only a ping fails, which is exactly the transient failure the health
// check reacts to.
func newBrokenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), unreachablePoolURL)
	require.NoError(t, err, "pool creation is lazy and must not dial")
	return pool
}

// healthTestPostgres wires a Postgres with health monitoring and arranges for
// the ticker goroutine and whichever pool ends up installed to be torn down.
func healthTestPostgres(t *testing.T, pool *pgxpool.Pool, databaseURL string) *Postgres {
	t.Helper()
	pg := NewPostgresWithHealthCheck(context.Background(), pool, databaseURL)
	t.Cleanup(func() {
		pg.StopHealthCheck()
		if pg.pool != nil {
			pg.pool.Close()
		}
	})
	return pg
}

// TestAttemptReconnect_MissingDatabaseURL covers the permanent failure the
// health checker cannot recover from: without a stored URL there is nothing to
// reconnect to, so the attempt must report an error rather than pretend a new
// pool exists.
func TestAttemptReconnect_MissingDatabaseURL(t *testing.T) {
	pg := &Postgres{} // no databaseURL was ever recorded

	err := pg.attemptReconnect(context.Background(), healthTestLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "database URL not available",
		"the error must name the missing URL so an operator can act on it")
}

// TestAttemptReconnect_InvalidURL covers a configured-but-unusable URL: creating
// the replacement pool fails and the error is surfaced rather than swallowed, so
// the health check can keep retrying without ever believing it reconnected.
func TestAttemptReconnect_InvalidURL(t *testing.T) {
	pg := &Postgres{databaseURL: "postgres://user:pass@127.0.0.1:not-a-port/db"}

	err := pg.attemptReconnect(context.Background(), healthTestLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating new pool",
		"a URL that cannot even be parsed must fail at pool creation")
}

// TestAttemptReconnect_CancelledContext covers shutdown: a cancelled context
// must abort the attempt instead of blocking a graceful stop on a dial.
func TestAttemptReconnect_CancelledContext(t *testing.T) {
	pg := &Postgres{databaseURL: unreachablePoolURL}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := pg.attemptReconnect(ctx, healthTestLogger())
	elapsed := time.Since(start)

	require.Error(t, err, "a cancelled attempt must not report success")
	assert.Less(t, elapsed, poolHealthCheckTimeout,
		"the attempt must abort on cancellation rather than wait out a dial")
}

// TestPing_UnhealthyPoolSurfacesLastError covers the health-gated Ping: once the
// pool is marked unhealthy, Ping returns the recorded cause without touching the
// pool, so callers get the real reason rather than a generic failure.
func TestPing_UnhealthyPoolSurfacesLastError(t *testing.T) {
	t.Run("the recorded health error is wrapped", func(t *testing.T) {
		cause := errors.New("connection reset")
		pg := &Postgres{lastHealthErr: cause} // healthyAtomic defaults to false

		err := pg.Ping(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, cause, "callers must be able to inspect the underlying cause")
	})

	t.Run("no recorded error still reports unhealthy", func(t *testing.T) {
		pg := &Postgres{}

		err := pg.Ping(context.Background())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "pool unhealthy")
	})
}

// TestPerformHealthCheck_PermanentFailureGivesUpAndBacksOff covers a reconnect
// that cannot succeed: the attempt is made, fails, and is surfaced to callers
// without marking the pool healthy. The call must also wait out its backoff
// rather than spinning on the broken endpoint.
func TestPerformHealthCheck_PermanentFailureGivesUpAndBacksOff(t *testing.T) {
	ctx := context.Background()
	pg := healthTestPostgres(t, newBrokenPool(t), unreachablePoolURL)

	attempt := 0
	start := time.Now()
	pg.performHealthCheck(ctx, healthTestLogger(), &attempt)
	elapsed := time.Since(start)

	assert.Equal(t, 1, attempt, "a failed reconnect leaves the attempt counter advanced for the next backoff")
	assert.False(t, pg.healthyAtomic.Load(), "a failed reconnect must not mark the pool healthy")
	assert.Error(t, pg.Ping(ctx), "the failure must be surfaced to callers, not hidden")
	assert.GreaterOrEqual(t, elapsed, poolReconnectBaseBackoff,
		"the attempt must wait out its backoff rather than immediately retrying")
}

// TestPerformHealthCheck_BackoffGrowsBetweenAttempts pins the linear growth of
// the wait, which is what stops a failing database from being hammered once per
// check interval.
func TestPerformHealthCheck_BackoffGrowsBetweenAttempts(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{name: "first attempt waits one base backoff", attempt: 0, want: poolReconnectBaseBackoff},
		{name: "second attempt waits twice the base backoff", attempt: 1, want: 2 * poolReconnectBaseBackoff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pg := healthTestPostgres(t, newBrokenPool(t), unreachablePoolURL)

			attempt := tt.attempt
			start := time.Now()
			pg.performHealthCheck(ctx, healthTestLogger(), &attempt)
			elapsed := time.Since(start)

			assert.Equal(t, tt.attempt+1, attempt, "the attempt counter advances by one")
			assert.GreaterOrEqual(t, elapsed, tt.want,
				"a later attempt must wait longer than an earlier one")
		})
	}
}
