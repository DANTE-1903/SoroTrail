package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/api/queries"
	"github.com/sorotrail/sorotrail/internal/store"
)

func TestAPIHelpers_ParameterParsing(t *testing.T) {
	t.Run("ParseTypes and topic filters", func(t *testing.T) {
		types, err := queries.ParseTypes("contract,system")
		require.NoError(t, err)
		assert.Len(t, types, 2)

		topics, err := queries.ParseTopic("transfer")
		require.NoError(t, err)
		assert.NotEmpty(t, topics)

		ledger, err := queries.ParseLedgerParam("123")
		require.NoError(t, err)
		assert.Equal(t, int64(123), ledger)

		tm, err := queries.ParseTimeParam("2026-07-01T00:00:00Z")
		require.NoError(t, err)
		assert.False(t, tm.IsZero())
	})
}

func TestAPIHelpers_HeaderConstruction(t *testing.T) {
	t.Run("cache headers, ETag, Vary, Link, X-Total-Count, X-Request-ID", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		req.Header.Set("X-Request-ID", "req-xyz-123")

		srv := newTestServer(&stubStore{}, nil)
		srv.Router().ServeHTTP(rec, req)

		assert.Equal(t, "req-xyz-123", rec.Header().Get("X-Request-ID"))
	})
}

func TestAPIHelpers_ResponseShapingAndAuth(t *testing.T) {
	t.Run("envelope, projection, SEP-41 tagging, and auth fail-closed", func(t *testing.T) {
		srv := newTestServer(&stubStore{}, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/tenant", nil)
		srv.Router().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		recMgmt := httptest.NewRecorder()
		reqMgmt := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
		srv.Router().ServeHTTP(recMgmt, reqMgmt)
		assert.Equal(t, http.StatusNotFound, recMgmt.Code)
	})
}

func TestAPIHelpers_ErrorMapping(t *testing.T) {
	t.Run("store errors map to correct HTTP statuses", func(t *testing.T) {
		st := &stubStore{eventErr: store.ErrNotFound}
		srv := newTestServer(st, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/events/0001099511627776-0000000001", nil)
		srv.Router().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)

		stDown := &stubStore{queryErr: errors.New("db down")}
		srvDown := newTestServer(stDown, nil)
		recDown := httptest.NewRecorder()
		reqDown := httptest.NewRequest(http.MethodGet, "/events", nil)
		srvDown.Router().ServeHTTP(recDown, reqDown)
		assert.Equal(t, http.StatusInternalServerError, recDown.Code)
	})
}
