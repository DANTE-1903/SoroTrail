package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// TestRequestID_PropagatesToStoreSlowQueryLog is the full-path acceptance
// test for correlation: the client-supplied X-Request-ID must reach the
// request-scoped logger, ride the request context through the tracing
// decorator into the guarded store, and show up on the slow-query log line.
// That is the chain an operator walks during an incident — from the
// X-Request-ID in a client's report to the database query that stalled.
func TestRequestID_PropagatesToStoreSlowQueryLog(t *testing.T) {
	var storeLogs strings.Builder
	guarded := store.NewGuardedStore(&slowQueryStore{stubStore: &stubStore{}}, store.GuardedStoreOptions{
		// The threshold is tiny and slowQueryStore spends a little over a
		// millisecond in the query, so every query here counts as slow.
		SlowQueryThreshold: time.Nanosecond,
		Logger:             slog.New(slog.NewTextHandler(&storeLogs, nil)),
	})

	s := New(guarded, nil, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), "test-key")
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	require.NoError(t, err)
	req.Header.Set("X-Request-ID", "prop-e2e-77")
	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "prop-e2e-77", resp.Header.Get("X-Request-ID"))

	out := storeLogs.String()
	assert.Contains(t, out, "slow store query",
		"the guarded store must log the slow query for this request")
	assert.Contains(t, out, "prop-e2e-77",
		"the slow-query log must carry the request's X-Request-ID")
}

// slowQueryStore wraps stubStore to make its queries measurably slow.
//
// The guarded store logs when time.Since(start) reaches the threshold. A stub
// that returns immediately can measure exactly zero: Windows advances the
// monotonic clock roughly every 500µs, so a trivial call between two ticks
// has no observable duration, and zero is below every positive threshold.
// Sleeping past a tick makes the test assert what it means to assert — that a
// slow query is logged with its correlation id — rather than depending on the
// host's clock resolution.
type slowQueryStore struct {
	*stubStore
}

func (s *slowQueryStore) QueryEvents(ctx context.Context, f store.EventFilter) ([]store.Event, string, error) {
	time.Sleep(time.Millisecond)
	return s.stubStore.QueryEvents(ctx, f)
}
