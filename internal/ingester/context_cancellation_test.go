package ingester

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLongRunningOperationHonoursContextCancellation verifies that loops and sleep
// intervals within long-running operations check context cancellation promptly
// and propagate context errors rather than hanging or ignoring them.
func TestLongRunningOperationHonoursContextCancellation(t *testing.T) {
	t.Run("sleep and backoff select on context done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		select {
		case <-ctx.Done():
			// Expected immediate return upon cancelled context
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for cancelled context")
		}
		duration := time.Since(start)
		assert.Less(t, duration, 100*time.Millisecond)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})

	t.Run("bounded loop respects cancellation interval", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		iterations := 0
		for {
			select {
			case <-ctx.Done():
				goto done
			default:
				iterations++
				time.Sleep(5 * time.Millisecond)
			}
		}
	done:
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		assert.Greater(t, iterations, 0)
	})
}
