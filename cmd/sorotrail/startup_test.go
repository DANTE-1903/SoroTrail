package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/config"
)

// scrubEnv empties the process environment for the duration of one
// subtest so run() sees exactly the configuration the case sets and
// nothing a developer or CI runner happens to have exported. The full
// scrub (rather than unsetting a fixed list of variables) keeps the
// table honest: a stray POLL_INTERVAL in the caller's shell cannot
// turn a "fails fast" assertion into a false pass or fail.
func scrubEnv(t *testing.T) {
	t.Helper()
	saved := os.Environ()
	os.Clearenv()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(k, v)
			}
		}
	})
}

// TestRunFailsFastOnBadConfiguration drives run() with broken
// configuration and asserts the two properties an operator depends on:
// the failure is fast (validation runs before any connection is
// attempted, so nobody waits on a database or RPC timeout to learn the
// config is wrong) and the failure is clear (the error names the
// offending variable).
func TestRunFailsFastOnBadConfiguration(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "missing DATABASE_URL",
			env:  map[string]string{"DATABASE_URL": ""},
			want: "DATABASE_URL: required but empty",
		},
		{
			name: "unknown NETWORK",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "NETWORK": "solana"},
			want: "NETWORK must be one of testnet, mainnet, futurenet",
		},
		{
			name: "rate limit configured only halfway",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "RATE_LIMIT_RPS": "5"},
			want: "RATE_LIMIT_RPS and RATE_LIMIT_BURST must both be set or both unset",
		},
		{
			name: "non-positive POLL_INTERVAL",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "POLL_INTERVAL": "0s"},
			want: "POLL_INTERVAL: 0s must be a positive duration",
		},
		{
			name: "unknown LOG_LEVEL",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "LOG_LEVEL": "verbose"},
			want: `LOG_LEVEL: "verbose" must be one of debug|info|warn|error`,
		},
		{
			name: "malformed watched contract",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "WATCHED_CONTRACTS": "not-a-contract"},
			want: "WATCHED_CONTRACTS entry",
		},
		{
			name: "failover RPC URL without a scheme",
			env:  map[string]string{"DATABASE_URL": "sqlite::memory:", "RPC_URLS": "localhost:9999"},
			want: "RPC_URLS[0]",
		},
		{
			// The second validation stage must be just as decisive:
			// a bootstrap credential for a feature that is switched
			// off protects nothing, and the operator has to hear
			// about it at boot rather than at first login attempt.
			name: "bootstrap key without multi-tenancy",
			env: map[string]string{
				"DATABASE_URL":               "sqlite::memory:",
				"MULTI_TENANT_BOOTSTRAP_KEY": "st_ABCDEFGHIJKLMNOP_secret",
			},
			want: "MULTI_TENANT_BOOTSTRAP_KEY is set but MULTI_TENANT is false",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scrubEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			start := time.Now()
			err := run()
			elapsed := time.Since(start)

			require.Error(t, err, "startup must refuse a broken configuration")
			assert.Contains(t, err.Error(), c.want,
				"the error must name the offending configuration")
			assert.Less(t, elapsed, 5*time.Second,
				"startup must fail before attempting any network I/O")
		})
	}
}

// TestLoggerConstructionAndRedaction asserts that every documented log format
// (json, text) and log level (debug, info, warn, error) initializes successfully
// and that sensitive URLs or credentials in log output are properly redacted.
func TestLoggerConstructionAndRedaction(t *testing.T) {
	formats := []string{"json", "text"}
	levels := []string{"debug", "info", "warn", "error"}

	for _, format := range formats {
		for _, level := range levels {
			t.Run(format+"_+"+level, func(t *testing.T) {
				logger := newLogger(format, level)
				require.NotNil(t, logger)

				// Verify credential redaction helper directly used in logger setup/startup
				redacted := rpcURLsForLog(config.Config{
					RPCURL: "https://admin:super-secret-password@rpc.stellar.org",
				})
				require.Len(t, redacted, 1)
				assert.NotContains(t, redacted[0], "super-secret-password")
				assert.Contains(t, redacted[0], "%2A%2A%2A")
			})
		}
	}
}
