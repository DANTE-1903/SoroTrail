package store

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/requestid"
)

// TestGuardedStore_SlowQueryLogCarriesCorrelationID pins the contract that
// the guarded store reads the correlation id off the context rather than
// taking it as a parameter: a slow query is tagged with the HTTP request id
// or the background job id that drove it, and a context with neither leaves
// the field out entirely.
func TestGuardedStore_SlowQueryLogCarriesCorrelationID(t *testing.T) {
	tests := []struct {
		name      string
		ctx       func() context.Context
		wantInLog string
		wantField bool
	}{
		{
			name:      "HTTP request id",
			ctx:       func() context.Context { return requestid.WithRequestID(context.Background(), "req-abc-123") },
			wantInLog: "req-abc-123",
			wantField: true,
		},
		{
			name:      "background job id",
			ctx:       func() context.Context { return requestid.WithJob(context.Background(), requestid.JobPruner) },
			wantInLog: requestid.JobPruner,
			wantField: true,
		},
		{
			name:      "no correlation id",
			ctx:       context.Background,
			wantField: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			guarded := NewGuardedStore(&testGuardedStore{}, GuardedStoreOptions{
				Timeout: 50 * time.Millisecond,
				// A one-nanosecond threshold makes every query "slow", so the
				// test does not have to sleep to trigger the log.
				SlowQueryThreshold: time.Nanosecond,
				Logger:             slog.New(slog.NewTextHandler(&buf, nil)),
			})

			_, _, err := guarded.QueryEvents(tt.ctx(), EventFilter{})
			require.Error(t, err)

			out := buf.String()
			require.Contains(t, out, "slow store query")
			if tt.wantField {
				assert.Contains(t, out, tt.wantInLog)
				assert.Contains(t, out, requestid.Field)
			} else {
				assert.NotContains(t, out, requestid.Field,
					"a context without a correlation id must not invent one")
			}
		})
	}
}
