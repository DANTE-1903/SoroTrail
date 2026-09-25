package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthExitCodesByStatus verifies the core contract: every 2xx
// exits 0, everything else exits 1.
func TestHealthExitCodesByStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		want   int
	}{
		{name: "200 ok", status: http.StatusOK, want: 0},
		{name: "204 no content", status: http.StatusNoContent, want: 0},
		{name: "301 moved", status: http.StatusMovedPermanently, want: 1},
		{name: "404 not found", status: http.StatusNotFound, want: 1},
		{name: "500 internal error", status: http.StatusInternalServerError, want: 1},
		{name: "503 unavailable", status: http.StatusServiceUnavailable, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/health", r.URL.Path)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")
			got := runHealth([]string{"--addr", addr})
			assert.Equal(t, tt.want, got, "exit code for status %d", tt.status)
		})
	}
}

// TestHealthURLMode verifies --url probes the exact URL given,
// including remote-style endpoints, without needing --addr/--endpoint.
func TestHealthURLMode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("full url probed verbatim", func(t *testing.T) {
		got := runHealth([]string{"--url", srv.URL + "/health"})
		assert.Equal(t, 0, got)
	})

	t.Run("missing path yields 404 -> exit 1", func(t *testing.T) {
		got := runHealth([]string{"--url", srv.URL + "/nope"})
		assert.Equal(t, 1, got)
	})
}

// TestHealthReturnsOneOnConnectRefused verifies "nothing listening"
// is unhealthy (exit 1), not a stuck probe or a usage error.
func TestHealthReturnsOneOnConnectRefused(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	got := runHealth([]string{"--addr", addr, "--timeout", "200ms"})
	assert.Equal(t, 1, got)
}

// TestHealthUsageErrors covers every flag combination that must exit
// 2 before any network traffic happens.
func TestHealthUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--nope"}},
		{name: "endpoint without slash", args: []string{"--endpoint", "health"}},
		{name: "empty endpoint", args: []string{"--endpoint", ""}},
		{name: "zero timeout", args: []string{"--timeout", "0s"}},
		{name: "negative timeout", args: []string{"--timeout", "-1s"}},
		{name: "url without scheme", args: []string{"--url", "example.com/health"}},
		{name: "url with ftp scheme", args: []string{"--url", "ftp://example.com/health"}},
		{name: "url without host", args: []string{"--url", "http:///health"}},
		{name: "url plus endpoint", args: []string{"--url", "http://h/health", "--endpoint", "/health"}},
		{name: "url plus addr", args: []string{"--url", "http://h/health", "--addr", "h:80"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runHealth(tt.args)
			assert.Equal(t, healthcodeExitUsage, got, "usage error must exit 2")
		})
	}
}

// TestHealthHelpExitsZero verifies asking for help is not a failure —
// probes and CI gates treat nonzero as unhealthy.
func TestHealthHelpExitsZero(t *testing.T) {
	t.Parallel()
	for _, flagName := range []string{"-h", "--help"} {
		got := runHealth([]string{flagName})
		require.Equal(t, 0, got, "%s must exit 0", flagName)
	}
}

// TestHealthHonoursHTTPAddr verifies the default target follows
// $HTTP_ADDR, matching healthcheck's address resolution.
func TestHealthHonoursHTTPAddr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("HTTP_ADDR", hostPort)

	got := runHealth(nil)
	assert.Equal(t, 0, got)
}

// TestHealthDoesNotFollowRedirects verifies a 3xx is misconfiguration,
// not health — same semantics as healthcheck.
func TestHealthDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	got := runHealth([]string{"--addr", addr})
	assert.Equal(t, 1, got)
}

// TestHealthCustomEndpoint verifies operators can probe /livez or any
// other path via --endpoint when not using --url.
func TestHealthCustomEndpoint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/livez" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	got := runHealth([]string{"--addr", addr, "--endpoint", "/livez"})
	assert.Equal(t, 0, got)
}

// TestValidateProbeURL exercises the URL validator directly across
// accepted and rejected shapes.
func TestValidateProbeURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "http with port", raw: "http://127.0.0.1:8080/health"},
		{name: "https remote", raw: "https://api.sorotrail.io/health"},
		{name: "scheme only missing host", raw: "http://", wantErr: true},
		{name: "no scheme", raw: "127.0.0.1:8080/health", wantErr: true},
		{name: "ws scheme rejected", raw: "ws://host/health", wantErr: true},
		{name: "garbage", raw: ":", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateProbeURL(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestHealthAndHealthcheckShareProbe verifies both subcommands decide
// health identically: same server, same statuses, same exit codes —
// only their stderr labels differ.
func TestHealthAndHealthcheckShareProbe(t *testing.T) {
	t.Parallel()
	statuses := []int{200, 503}
	for _, status := range statuses {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			addr := strings.TrimPrefix(srv.URL, "http://")
			args := []string{"--addr", addr, "--endpoint", "/health"}

			want := 0
			if status >= 300 {
				want = 1
			}
			assert.Equal(t, want, runHealth(args), "health exit code")
			assert.Equal(t, want, runHealthcheck(args), "healthcheck exit code")
		})
	}
}
