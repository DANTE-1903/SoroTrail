package horizon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClient_ErrorPaths(t *testing.T) {
	// TestClient_ErrorPaths verifies error handling in client requests.
}

func TestClientErrors(t *testing.T) {
	t.Run("DoRequest error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"internal error"}`))
		}))
		defer server.Close()

		client := NewClient(server.URL)
		ctx := context.Background()
		err := client.CheckHealth(ctx)
		assert.Error(t, err)
	})
}
func TestHorizonClientErrors(t *testing.T) {
	t.Run("context cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		client := NewClient(srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := client.CheckHealth(ctx)
		assert.Error(t, err)
	})

	t.Run("bad server response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewClient(srv.URL)
		err := client.CheckHealth(context.Background())
		assert.Error(t, err)
	})
}
