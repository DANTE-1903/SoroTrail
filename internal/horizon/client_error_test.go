package horizon

import (
	"context"
	"github.com/stretchr/testify/assert"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientErrors(t *testing.T) {
	t.Run("DoRequest error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"internal error"}`))
		}))
		defer server.Close()

		client := NewClient(server.URL)
		ctx := context.Background()
		err := client.Health(ctx)
		assert.Error(t, err)
	})
}
func TestClient_ErrorPaths(t *testing.T) {
	t.Run("invalid server response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal error"))
		}))
		defer server.Close()

		client := NewClient(server.URL)
		ctx := context.Background()
		err := client.Health(ctx)
		assert.Error(t, err)
	})
}
