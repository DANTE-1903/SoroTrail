package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRequestIDTestServer builds a Server whose logs land in the returned
// buffer so tests can assert that per-request log lines carry the request
// ID that was echoed in the response header.
func newRequestIDTestServer(st *stubStore) (*Server, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(st, nil, log, "test-key"), &buf
}

// TestRequestID_ClientSuppliedRespected walks a table of client-supplied
// X-Request-ID values (including ones a naive implementation might mangle)
// and requires the response to echo each one verbatim, with the value also
// present on the request summary log line.
func TestRequestID_ClientSuppliedRespected(t *testing.T) {
	tests := []struct {
		name       string
		incomingID string
		wantEcho   bool // response must carry the same ID back
	}{
		{name: "uuid style", incomingID: "f81d4fae-7dec-11d0-a765-00a0c91e6bf6", wantEcho: true},
		{name: "contains spaces", incomingID: "my custom id 42", wantEcho: true},
		{name: "contains unicode", incomingID: "richiesta-€-ünïcode", wantEcho: true},
		{name: "contains pipe", incomingID: "front|gateway|01", wantEcho: true},
		{name: "single char", incomingID: "x", wantEcho: true},
		{name: "absent means generated", incomingID: "", wantEcho: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, buf := newRequestIDTestServer(&stubStore{})
			srv := httptest.NewServer(s.Router())
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
			require.NoError(t, err)
			if tt.incomingID != "" {
				req.Header.Set("X-Request-ID", tt.incomingID)
			}
			resp, err := http.DefaultTransport.RoundTrip(req)
			require.NoError(t, err)
			resp.Body.Close()

			got := resp.Header.Get("X-Request-ID")
			require.NotEmpty(t, got, "every response must carry an X-Request-ID")
			if tt.wantEcho {
				assert.Equal(t, tt.incomingID, got, "client-supplied ID must be respected verbatim")
			} else {
				assert.NotEqual(t, tt.incomingID, got)
			}
			// The request summary log line correlates with the response.
			assert.Contains(t, buf.String(), got, "request summary log must carry the request ID")
		})
	}
}

// TestRequestID_GeneratedIDsAreUnique verifies two requests without the
// header get two different IDs, so log lines from different requests never
// alias each other.
func TestRequestID_GeneratedIDsAreUnique(t *testing.T) {
	s, _ := newRequestIDTestServer(&stubStore{})
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	seen := make(map[string]struct{})
	for range 5 {
		resp, err := http.Get(srv.URL + "/health")
		require.NoError(t, err)
		resp.Body.Close()
		id := resp.Header.Get("X-Request-ID")
		require.NotEmpty(t, id)
		_, dup := seen[id]
		assert.False(t, dup, "generated request IDs must be unique, got %q twice", id)
		seen[id] = struct{}{}
	}
}

// TestRequestID_HandlerLogsCarryRequestID is the heart of the
// request-correlation contract: a log line emitted by a handler (here the
// "querying events" error path) must carry the same request_id that was
// echoed to the client in the X-Request-ID response header.
func TestRequestID_HandlerLogsCarryRequestID(t *testing.T) {
	st := &stubStore{queryErr: assert.AnError}
	s, buf := newRequestIDTestServer(st)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	require.NoError(t, err)
	req.Header.Set("X-Request-ID", "corr-test-1")
	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	got := resp.Header.Get("X-Request-ID")
	assert.Equal(t, "corr-test-1", got)
	assert.Contains(t, buf.String(), `msg="querying events"`, "the handler error log line must be present")
	assert.Contains(t, buf.String(), got, "the handler log line must carry the response's request ID")
}

// TestRequestID_RecoveredPanicCarriesRequestID extends the correlation
// contract to the panic-recovery path: the recovered-panic log entry must
// carry the request ID echoed in the response header, so an operator can
// go from a client-visible 500 straight to the stack trace.
func TestRequestID_RecoveredPanicCarriesRequestID(t *testing.T) {
	s, buf := newRequestIDTestServer(&stubStore{})

	// Same chain order the router uses: RequestID → requestLogger →
	// recoverer → handler. A dedicated panicking route cannot be added to
	// the built router, so the chain is assembled from the same parts.
	h := middleware.RequestID(s.requestLogger(s.recoverer.Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }),
	)))

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("X-Request-ID", "panic-corr-7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "panic-corr-7", rec.Header().Get("X-Request-ID"))
	assert.Contains(t, buf.String(), `msg="http panic recovered"`)
	assert.Contains(t, buf.String(), "panic-corr-7", "recovered-panic log must carry the response's request ID")
}

// TestRequestID_ContextAccessor checks the context accessor: inside the
// RequestID middleware chain it reports the client-supplied ID, and outside
// any request it is empty rather than garbage.
func TestRequestID_ContextAccessor(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(RequestIDFrom(r.Context())))
	}))

	t.Run("returns the client-supplied ID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Request-Id", "ctx-prop-9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, "ctx-prop-9", rec.Body.String())
	})

	t.Run("returns a non-empty ID when generated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.NotEmpty(t, rec.Body.String())
	})

	t.Run("empty outside a request", func(t *testing.T) {
		assert.Empty(t, RequestIDFrom(context.Background()))
	})
}

// TestRequestID_GeneratedMatchesLogAndResponse pins the full loop for a
// generated (not client-supplied) ID: the response header value, the
// handler log line, and the request summary must all carry one identical ID.
func TestRequestID_GeneratedMatchesLogAndResponse(t *testing.T) {
	st := &stubStore{queryErr: assert.AnError}
	s, buf := newRequestIDTestServer(st)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events")
	require.NoError(t, err)
	resp.Body.Close()

	got := resp.Header.Get("X-Request-ID")
	require.NotEmpty(t, got)
	// Both the handler error line and the request summary carry it; the
	// two lines are the only ones in the buffer, so counting occurrences
	// (>= 2) plus the header check is a tight correlation assertion.
	assert.Equal(t, 2, strings.Count(buf.String(), got),
		"generated ID must appear on the handler line and the request summary")
}
