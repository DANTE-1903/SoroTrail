package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func mkReq(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func startLimiter(t *testing.T, lim *RateLimiter) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	lim.Start(ctx)
	return func() {
		cancel()
		lim.Stop()
	}
}

// TestClientIP pins the extraction rules the rate limiter keys on.
// They matter for security: trusting XFF blindly lets a caller mint
// unlimited identities, and skipping a valid hop silently regroups
// traffic under the wrong bucket.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		trustXFF   bool
		want       string
	}{
		{
			name:       "direct connection uses the remote address",
			remoteAddr: "203.0.113.9:5432",
			want:       "203.0.113.9",
		},
		{
			name:       "port is stripped from the remote address",
			remoteAddr: "198.51.100.1:65535",
			want:       "198.51.100.1",
		},
		{
			name:       "trusted proxy honors the leftmost XFF entry",
			remoteAddr: "10.0.0.1:443",
			xff:        "198.51.100.7, 10.0.0.1",
			trustXFF:   true,
			want:       "198.51.100.7",
		},
		{
			name:       "untrusted proxy ignores XFF entirely",
			remoteAddr: "203.0.113.9:5432",
			xff:        "198.51.100.7, 10.0.0.1",
			want:       "203.0.113.9",
		},
		{
			name:       "trusted with no XFF falls back to remote",
			remoteAddr: "203.0.113.9:5432",
			trustXFF:   true,
			want:       "203.0.113.9",
		},
		{
			name:       "multi-hop chain picks the first valid entry",
			remoteAddr: "10.0.0.1:443",
			xff:        "not-an-ip, 198.51.100.7, 172.16.0.9",
			trustXFF:   true,
			want:       "198.51.100.7",
		},
		{
			name:       "trusted with all-invalid XFF falls back to remote",
			remoteAddr: "203.0.113.9:5432",
			xff:        "not-an-ip, , also-bad",
			trustXFF:   true,
			want:       "203.0.113.9",
		},
		{
			name:       "IPv6 remote keeps brackets off the key",
			remoteAddr: "[2001:db8::1]:443",
			want:       "2001:db8::1",
		},
		{
			name:       "bare IPv6 remote without port",
			remoteAddr: "2001:db8::1",
			want:       "2001:db8::1",
		},
		{
			name:       "bracketed IPv6 remote without port",
			remoteAddr: "[2001:db8::1]",
			want:       "2001:db8::1",
		},
		{
			name:       "unbracketed IPv6 in XFF",
			remoteAddr: "10.0.0.1:443",
			xff:        "2001:db8::1, 10.0.0.1",
			trustXFF:   true,
			want:       "2001:db8::1",
		},
		{
			name:       "bracketed IPv6 with port in XFF",
			remoteAddr: "10.0.0.1:443",
			xff:        "[2001:db8::1]:4711, 10.0.0.1",
			trustXFF:   true,
			want:       "2001:db8::1",
		},
		{
			name:       "bracketed IPv6 without port in XFF",
			remoteAddr: "10.0.0.1:443",
			xff:        "[2001:db8::1], 10.0.0.1",
			trustXFF:   true,
			want:       "2001:db8::1",
		},
		{
			// A malformed address must produce the documented empty
			// result so clientKey can substitute its stable
			// "unknown" key instead of bucketing on junk.
			name:       "malformed remote yields empty result",
			remoteAddr: "garbage-without-a-port",
			want:       "",
		},
		{
			// host:port syntax alone is not enough: "foo:bar"
			// splits cleanly but names no IP, and returning it raw
			// would give every typo its own bucket.
			name:       "non-IP host in remote yields empty result",
			remoteAddr: "foo:bar",
			want:       "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			got := clientIP(r, c.trustXFF)
			assert.Equal(t, c.want, got,
				"clientIP(trustXFF=%v, RemoteAddr=%q, XFF=%q)",
				c.trustXFF, c.remoteAddr, c.xff)
		})
	}
}

// TestClientKeyStableFallbackForMalformedAddress covers the hand-off
// the issue is about: a malformed address must not become an empty
// rate-limit key, because an empty key would merge every broken client
// into one bucket (or, worse, if treated as "no key", let them through
// unthrottled).
func TestClientKeyStableFallbackForMalformedAddress(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{name: "malformed remote maps to unknown", remoteAddr: "garbage-without-a-port", want: "unknown"},
		{name: "empty remote maps to unknown", remoteAddr: "", want: "unknown"},
		{name: "valid remote keeps its IP key", remoteAddr: "203.0.113.9:1234", want: "203.0.113.9"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := NewRateLimiter(1, 1, false)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remoteAddr
			assert.Equal(t, c.want, l.clientKey(r))
		})
	}
}

func TestClientKeyUsesRemoteAddrWhenNoCredential(t *testing.T) {
	l := NewRateLimiter(1, 1, false)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	if got := l.clientKey(r); got != "203.0.113.9" {
		t.Fatalf("clientKey() = %q, want 203.0.113.9", got)
	}
}

// TestCeilSeconds pins the Retry-After rounding. Rounding down would
// tell a throttled client to retry before its budget refills, which
// produces a second 429 and looks like the limiter is broken; a
// negative value would violate RFC 7231's delta-seconds contract
// outright.
func TestCeilSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero returns the documented minimum", in: 0, want: time.Second},
		{name: "sub-second never rounds down to zero", in: time.Millisecond, want: time.Second},
		{name: "one nanosecond rounds up to a full second", in: 1, want: time.Second},
		{name: "sub-second rounds up to one", in: 500 * time.Millisecond, want: time.Second},
		{name: "exact whole second stays as is", in: time.Second, want: time.Second},
		{name: "larger exact multiple stays as is", in: 3 * time.Second, want: 3 * time.Second},
		{name: "fractional duration rounds up", in: 1500 * time.Millisecond, want: 2 * time.Second},
		{name: "just over a second rounds up to two", in: time.Second + time.Nanosecond, want: 2 * time.Second},
		{name: "999 milliseconds round up to one", in: 999 * time.Millisecond, want: time.Second},
		{name: "negative duration clamps to the minimum", in: -500 * time.Millisecond, want: time.Second},
		{name: "large negative never yields a negative header", in: -time.Hour, want: time.Second},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ceilSeconds(c.in)
			assert.Equal(t, c.want, got, "ceilSeconds(%s)", c.in)
			assert.Greater(t, got, time.Duration(0),
				"Retry-After must never be zero or negative (input %s)", c.in)
		})
	}
}

func TestBucketEntryForReturnsSameInstance(t *testing.T) {
	l := NewRateLimiter(1, 1, false)
	e1 := l.bucketEntryFor("k1", 1, 1)
	if e1 == nil {
		t.Fatal("bucketEntryFor() returned nil")
	}
	if e2 := l.bucketEntryFor("k1", 1, 1); e2 != e1 {
		t.Fatal("bucketEntryFor() did not return the same bucket instance")
	}
}
