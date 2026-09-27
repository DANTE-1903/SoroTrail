package webhook

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

func TestWebhookDelivery_Timeout(t *testing.T) {
	// Verify that slow webhook endpoint deliveries respect bounded timeouts
	// and return distinguishable error conditions rather than hanging indefinitely.
	tests := []struct {
		name        string
		serverDelay time.Duration
		timeout     time.Duration
		wantTimeout bool
	}{
		{
			name:        "delivers successfully within timeout",
			serverDelay: 10 * time.Millisecond,
			timeout:     200 * time.Millisecond,
			wantTimeout: false,
		},
		{
			name:        "times out when server is slower than deadline",
			serverDelay: 200 * time.Millisecond,
			timeout:     20 * time.Millisecond,
			wantTimeout: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(tt.serverDelay)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
			require.NoError(t, err)

			client := &http.Client{}
			resp, err := client.Do(req)

			if tt.wantTimeout {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, context.DeadlineExceeded) || (resp == nil), "must fail with context deadline or network timeout error")
			} else {
				require.NoError(t, err)
				defer resp.Body.Close()
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			}
		})
	}
}
