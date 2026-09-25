package rpc

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/requestid"
)

// TestRetryClient_RetryLogCarriesRequestID pins the RPC-side half of the
// correlation contract: when a call is retried, the retry log carries the
// request id (or job id) from the context, so a failed upstream hop can be
// traced back to the client request or background job that caused it.
func TestRetryClient_RetryLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var calls int
	inner := &retryMockClient{
		getHealth: func(context.Context) (Health, error) {
			calls++
			if calls == 1 {
				return Health{}, &RateLimitedError{StatusCode: 429, RetryAfter: time.Millisecond}
			}
			return Health{Status: "healthy"}, nil
		},
	}
	rc := NewRetryClient(inner, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
		Jitter:      false,
		Logger:      log,
	})

	ctx := requestid.WithRequestID(context.Background(), "rpc-corr-9")
	_, err := rc.GetHealth(ctx)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "rpc retry scheduled")
	assert.Contains(t, out, "rpc-corr-9",
		"the retry log must carry the request id from the context")
}
