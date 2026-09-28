package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/config"
)

func TestRedactDatabaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "empty url",
			raw:  "",
			want: "",
		},
		{
			name: "sqlite memory",
			raw:  "sqlite::memory:",
			want: "sqlite::memory:",
		},
		{
			name: "sqlite file",
			raw:  "sqlite:/tmp/sorotrail.db",
			want: "sqlite:/tmp/sorotrail.db",
		},
		{
			name: "postgres with password",
			raw:  "postgres://alice:secret123@localhost:5432/sorotrail?sslmode=disable",
			want: "postgres://alice:%2A%2A%2A@localhost:5432/sorotrail?sslmode=disable",
		},
		{
			name: "postgres without password",
			raw:  "postgres://alice@localhost:5432/sorotrail?sslmode=disable",
			want: "postgres://alice@localhost:5432/sorotrail?sslmode=disable",
		},
		{
			name: "postgres host only",
			raw:  "postgres://localhost:5432/sorotrail",
			want: "postgres://localhost:5432/sorotrail",
		},
		{
			name: "invalid url with no scheme or host",
			raw:  "://not a url",
			want: "<redacted>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := redactDatabaseURL(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRedactedConfigItems_Redaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		cfg           config.Config
		checkVar      string
		wantValue     string
		forbidContent string
	}{
		{
			name: "database url password redacted",
			cfg: config.Config{
				DatabaseURL: "postgres://appuser:supersecretpass@db.example.com:5432/sorotrail",
			},
			checkVar:      "DATABASE_URL",
			forbidContent: "supersecretpass",
		},
		{
			name: "rpc url password redacted",
			cfg: config.Config{
				RPCURL: "https://nodeuser:rpcsecret@stellar-rpc.example.com",
			},
			checkVar:      "RPC_URL",
			forbidContent: "rpcsecret",
		},
		{
			name: "rpc urls list passwords redacted",
			cfg: config.Config{
				RPCURLS: []string{
					"https://rpc1.example.com",
					"https://user:failoverpass@rpc2.example.com",
				},
			},
			checkVar:      "RPC_URLS",
			forbidContent: "failoverpass",
		},
		{
			name: "api key redacted when set",
			cfg: config.Config{
				APIKey: "st_live_abcdef1234567890",
			},
			checkVar:      "API_KEY",
			wantValue:     "***",
			forbidContent: "abcdef1234567890",
		},
		{
			name: "api key empty when unset",
			cfg: config.Config{
				APIKey: "",
			},
			checkVar:  "API_KEY",
			wantValue: "",
		},
		{
			name: "archive secret access key redacted when set",
			cfg: config.Config{
				ArchiveSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			checkVar:      "ARCHIVE_SECRET_ACCESS_KEY",
			wantValue:     "***",
			forbidContent: "EXAMPLEKEY",
		},
		{
			name: "multi tenant bootstrap key redacted when set",
			cfg: config.Config{
				MultiTenantBootstrapKey: "bootstrap-super-secret-key",
			},
			checkVar:      "MULTI_TENANT_BOOTSTRAP_KEY",
			wantValue:     "***",
			forbidContent: "super-secret-key",
		},
		{
			name: "non-sensitive string preserved",
			cfg: config.Config{
				Network: "futurenet",
			},
			checkVar:  "NETWORK",
			wantValue: "futurenet",
		},
		{
			name: "duration formatted",
			cfg: config.Config{
				PollInterval: 10 * time.Second,
			},
			checkVar:  "POLL_INTERVAL",
			wantValue: "10s",
		},
		{
			name: "boolean formatted",
			cfg: config.Config{
				MetricsEnabled: true,
			},
			checkVar:  "METRICS_ENABLED",
			wantValue: "true",
		},
		{
			name: "integer formatted",
			cfg: config.Config{
				APIMaxLimit: 1000,
			},
			checkVar:  "API_MAX_LIMIT",
			wantValue: "1000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items := redactedConfigItems(tt.cfg)

			var found *configItem
			for i := range items {
				if items[i].Name == tt.checkVar {
					found = &items[i]
					break
				}
			}

			require.NotNil(t, found, "variable %s should be in redacted config items", tt.checkVar)
			if tt.wantValue != "" {
				assert.Equal(t, tt.wantValue, found.Value)
			}
			if tt.forbidContent != "" {
				assert.NotContains(t, found.Value, tt.forbidContent)
			}
		})
	}
}

func TestRenderRedactedConfig_Formats(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Network:      "testnet",
		RPCURL:       "https://user:mypassword@soroban-testnet.stellar.org",
		DatabaseURL:  "postgres://dbuser:dbpass@localhost:5432/sorotrail",
		APIKey:       "secret-api-key",
		PollInterval: 5 * time.Second,
		APIMaxLimit:  500,
	}

	t.Run("text format", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := renderRedactedConfig(&buf, cfg, "text")
		require.NoError(t, err)

		out := buf.String()
		assert.Contains(t, out, "NETWORK=testnet\n")
		assert.Contains(t, out, "API_KEY=***\n")
		assert.NotContains(t, out, "secret-api-key")
		assert.NotContains(t, out, "dbpass")
		assert.NotContains(t, out, "mypassword")
		assert.Contains(t, out, "DATABASE_URL=postgres://dbuser:%2A%2A%2A@localhost:5432/sorotrail\n")

		// Verify output is sorted alphabetically
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		for i := 1; i < len(lines); i++ {
			prevKey := strings.SplitN(lines[i-1], "=", 2)[0]
			currKey := strings.SplitN(lines[i], "=", 2)[0]
			assert.True(t, prevKey <= currKey, "lines should be sorted: %s <= %s", prevKey, currKey)
		}
	})

	t.Run("json format", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := renderRedactedConfig(&buf, cfg, "json")
		require.NoError(t, err)

		var parsed map[string]any
		err = json.Unmarshal(buf.Bytes(), &parsed)
		require.NoError(t, err)

		assert.Equal(t, "testnet", parsed["NETWORK"])
		assert.Equal(t, "***", parsed["API_KEY"])
		assert.Equal(t, "5s", parsed["POLL_INTERVAL"])
		assert.Equal(t, float64(500), parsed["API_MAX_LIMIT"])

		rawJSON := buf.String()
		assert.NotContains(t, rawJSON, "secret-api-key")
		assert.NotContains(t, rawJSON, "dbpass")
		assert.NotContains(t, rawJSON, "mypassword")
	})

	t.Run("unsupported format", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		err := renderRedactedConfig(&buf, cfg, "yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported format")
	})
}

func TestRunConfigTo_ArgumentHandling(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantErr     bool
		errContains string
		wantOut     string
	}{
		{
			name:    "help short",
			args:    []string{"-h"},
			wantErr: false,
			wantOut: "",
		},
		{
			name:    "help long",
			args:    []string{"--help"},
			wantErr: false,
			wantOut: "",
		},
		{
			name:        "unexpected argument",
			args:        []string{"extra"},
			wantErr:     true,
			errContains: "takes no positional arguments",
		},
		{
			name:        "unsupported format",
			args:        []string{"-format", "xml"},
			wantErr:     true,
			errContains: "unsupported format",
		},
		{
			name: "text format explicit",
			args: []string{"-format", "text"},
			env: map[string]string{
				"DATABASE_URL": "sqlite::memory:",
				"NETWORK":      "testnet",
			},
			wantErr: false,
			wantOut: "NETWORK=testnet\n",
		},
		{
			name: "json format explicit",
			args: []string{"-format", "json"},
			env: map[string]string{
				"DATABASE_URL": "sqlite::memory:",
				"NETWORK":      "testnet",
			},
			wantErr: false,
			wantOut: `"NETWORK": "testnet"`,
		},
		{
			name: "json flag shorthand",
			args: []string{"-json"},
			env: map[string]string{
				"DATABASE_URL": "sqlite::memory:",
				"NETWORK":      "testnet",
			},
			wantErr: false,
			wantOut: `"NETWORK": "testnet"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var buf bytes.Buffer
			err := runConfigTo(&buf, tt.args)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			if tt.wantOut != "" {
				assert.Contains(t, buf.String(), tt.wantOut)
			}
		})
	}
}

func TestLoadEffectiveConfig_DefaultsWhenDatabaseURLUnset(t *testing.T) {
	// Ensure DATABASE_URL is unset
	t.Setenv("DATABASE_URL", "")
	t.Setenv("NETWORK", "testnet")

	cfg, err := loadEffectiveConfig()
	require.NoError(t, err)
	assert.Equal(t, "testnet", cfg.Network)
	assert.Equal(t, "https://soroban-testnet.stellar.org", cfg.RPCURL)
	assert.Equal(t, "", cfg.DatabaseURL)
}

func TestDispatchConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "sqlite::memory:")

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	derr := dispatch([]string{"config"})

	closeErr := w.Close()
	os.Stdout = old
	require.NoError(t, closeErr)

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	assert.NoError(t, derr)
	assert.Contains(t, buf.String(), "NETWORK=")
	assert.Contains(t, buf.String(), "DATABASE_URL=")
}
