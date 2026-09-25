package api

import (
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
	guarded := store.NewGuardedStore(&stubStore{}, store.GuardedStoreOptions{
		// A one-nanosecond threshold makes every query "slow", so the test
		// does not have to sleep to trigger the log.
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
