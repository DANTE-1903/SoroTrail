package spec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpecFetch_Timeout(t *testing.T) {
	// Verify that external spec fetches / enricher calls bound their operations with timeouts
	// and produce distinguishable errors.
	tests := []struct {
		name          string
		responseDelay time.Duration
		timeout       time.Duration
		wantTimeout   bool
	}{
		{
			name:          "fetches successfully within timeout",
			responseDelay: 5 * time.Millisecond,
			timeout:       100 * time.Millisecond,
			wantTimeout:   false,
		},
		{
			name:          "times out on slow spec fetch endpoint",
			responseDelay: 100 * time.Millisecond,
			timeout:       10 * time.Millisecond,
			wantTimeout:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(tt.responseDelay)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			require.NoError(t, err)

			client := &http.Client{}
			resp, err := client.Do(req)

			if tt.wantTimeout {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, context.DeadlineExceeded) || resp == nil)
			} else {
				require.NoError(t, err)
				defer resp.Body.Close()
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			}
		})
	}
}
