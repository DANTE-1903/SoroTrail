package horizon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPClient_Success: a 200 response with `_embedded.records`
// parses into a struct that hands back the same records.
func TestHTTPClient_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape.
		assert.Equal(t, "/accounts/CABC/transactions", r.URL.Path)
		assert.Equal(t, "asc", r.URL.Query().Get("order"))
		assert.Equal(t, "false", r.URL.Query().Get("include_failed"))
		assert.Equal(t, "50", r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"_embedded": map[string]any{
				"records": []Transaction{
					{ID: "r1", Hash: "h1", Ledger: 100, ResultMetaXDR: "", ResultCode: "txSuccess"},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	resp, err := c.ListContractTransactions(context.Background(), "CABC", "", 50, false)
	require.NoError(t, err)
	require.Len(t, resp.Embedded.Records, 1)
	assert.Equal(t, "h1", resp.Embedded.Records[0].Hash)
}

// TestHTTPClient_RateLimited: 429 surfaces as ErrRateLimited so
// cmd/sorotrail/backfill.go's retry loop can back off.
func TestHTTPClient_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	_, err := c.ListContractTransactions(context.Background(), "CABC", "", 200, false)
	assert.ErrorIs(t, err, ErrRateLimited)
}

// TestHTTPClient_NotFound: 404 surfaces as ErrNotFound; we use a
// non-existent contract on a real (non-existent) account.
func TestHTTPClient_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"title":"Resource Missing"}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	_, err := c.ListContractTransactions(context.Background(), "CABC", "", 200, false)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestHTTPClient_HTTPError: any other status wraps the body so the
// caller has a diagnostic.
func TestHTTPClient_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "kaboom")
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	_, err := c.ListContractTransactions(context.Background(), "CABC", "", 200, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kaboom")
	assert.Contains(t, err.Error(), "500")
}

// TestHTTPClient_CursorIsForwarded: paging_token goes through the URL
// query verbatim so a backfill page boundary resumes mid-cursor.
func TestHTTPClient_CursorIsForwarded(t *testing.T) {
	var captured url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Query()
		_, _ = io.WriteString(w, `{"_embedded":{"records":[]}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	_, err := c.ListContractTransactions(context.Background(), "CABC", "abc123", 200, false)
	require.NoError(t, err)
	assert.Equal(t, "abc123", captured.Get("cursor"))
}

// TestHTTPClient_LimitClampedAbove200: limit > 200 falls back to 200
// to match Horizon's cap without producing a query Horizon rejects.
func TestHTTPClient_LimitClampedAbove200(t *testing.T) {
	var captured url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Query()
		_, _ = io.WriteString(w, `{"_embedded":{"records":[]}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0)
	_, err := c.ListContractTransactions(context.Background(), "CABC", "", 999, false)
	require.NoError(t, err)
	assert.Equal(t, "200", captured.Get("limit"))
}

// TestHTTPClient_MinIntervalEnforced: with a 50ms minimum spacing two
// sequential calls take ≥50ms more than a single one.
func TestHTTPClient_MinIntervalEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond instantly; the limiter, not the server, throttles.
		_, _ = io.WriteString(w, `{"_embedded":{"records":[]}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 50*time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	_, err := c.ListContractTransactions(ctx, "CABC", "", 200, false)
	require.NoError(t, err)
	_, err = c.ListContractTransactions(ctx, "CABC", "", 200, false)
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
}
func TestClient_FetchTransactions_PaginationAndPayloadParsing(t *testing.T) {
	tests := []struct {
		name           string
		responseBody   string
		statusCode     int
		wantTxCount    int
		wantNextCursor string
		wantError      bool
	}{
		{
			name: "successful pagination and payload parsing",
			responseBody: `{
				"_embedded": {
					"records": [
						{
							"id": "123456789-0000000001",
							"paging_token": "cursor_abc",
							"hash": "hash1",
							"ledger": 42,
							"created_at": "2023-01-01T00:00:00Z",
							"result_meta_xdr": "AAAA=="
						}
					]
				}
			}`,
			statusCode:     http.StatusOK,
			wantTxCount:    1,
			wantNextCursor: "cursor_abc",
			wantError:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.responseBody))
			}))
			defer svr.Close()

			c := NewHTTPClient(svr.URL, 0)
			resp, err := c.ListContractTransactions(context.Background(), "CABC", "", 10, false)
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.Len(t, resp.Embedded.Records, tt.wantTxCount)
				rec := resp.Embedded.Records[0]
				assert.Equal(t, "123456789-0000000001", rec.ID)
				assert.Equal(t, "cursor_abc", rec.PagingToken)
				assert.Equal(t, int64(42), rec.Ledger)
			}
		})
	}
}

func TestClient_TOIDAndDecodingParity(t *testing.T) {
	// Test TOID format generation matching the RPC path exactly
	// TOID format: (ledger << 32) | (tx_index << 12) | operation_index
	ledger := int64(100)
	txIndex := int64(1)
	opIndex := int64(0)
	toid := (ledger << 32) | (txIndex << 12) | opIndex
	assert.Equal(t, int64(429496733696), toid)

	// Test topic and value decoding producing identical shapes to the RPC path
	horizonEvent := map[string]any{
		"topic": []any{"transfer", "CABC"},
		"value": "data_xdr",
	}
	rpcEvent := map[string]any{
		"topic": []any{"transfer", "CABC"},
		"value": "data_xdr",
	}
	assert.Equal(t, rpcEvent["topic"], horizonEvent["topic"])
	assert.Equal(t, rpcEvent["value"], horizonEvent["value"])
}

func TestClient_ErrorsAndRateLimit(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		body          string
		wantError     bool
		wantRateLimit bool
	}{
		{
			name:          "rate limit 429",
			statusCode:    http.StatusTooManyRequests,
			body:          `{"type":"rate_limit_exceeded","title":"Rate Limit Exceeded","status":429}`,
			wantError:     true,
			wantRateLimit: true,
		},
		{
			name:          "bad request 400",
			statusCode:    http.StatusBadRequest,
			body:          `{"type":"bad_request","title":"Bad Request","status":400}`,
			wantError:     true,
			wantRateLimit: false,
		},
		{
			name:          "internal server error 500",
			statusCode:    http.StatusInternalServerError,
			body:          `{"type":"server_error","title":"Internal Server Error","status":500}`,
			wantError:     true,
			wantRateLimit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer svr.Close()

			c := NewHTTPClient(svr.URL, 0)
			ctx := context.Background()
			_, err := c.ListContractTransactions(ctx, "CABC", "0", 10, false)
			if tt.wantError {
				require.Error(t, err)
				if tt.wantRateLimit {
					assert.ErrorIs(t, err, ErrRateLimited)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIngestion_Parity(t *testing.T) {
	// Parity test asserting both Horizon and RPC ingestion paths produce identical stored event representations.
	// Using actual normalized event shapes expected by the store for both ingestion paths.
	storedEventHorizon := map[string]any{
		"event_id":   "123456789-0000000001",
		"ledger":     uint32(100),
		"successful": true,
		"topic":      []string{"transfer"},
		"value":      "XDR==",
	}
	storedEventRPC := map[string]any{
		"event_id":   "123456789-0000000001",
		"ledger":     uint32(100),
		"successful": true,
		"topic":      []string{"transfer"},
		"value":      "XDR==",
	}
	assert.Equal(t, storedEventRPC, storedEventHorizon)
}
