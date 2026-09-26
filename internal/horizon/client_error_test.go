package horizon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientErrors(t *testing.T) {
	t.Run("DoRequest error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"internal error"}`))
		}))
		defer server.Close()

		client := New(server.URL)
		ctx := context.Background()
		err := client.Health(ctx)
		assert.Error(t, err)
	})
}
