package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validBase() Config {
	return Config{
		DatabaseURL:         "postgres://user:pass@localhost:5432/db",
		RPCURL:              "https://rpc.example.com",
		PollInterval:        5 * time.Second,
		AuditPollInterval:   30 * time.Second,
		IngesterMinBackoff:  1 * time.Second,
		IngesterMaxBackoff:  5 * time.Second,
		RetentionLedgers:    17280,
		PartitionLedgerSpan: 120960,
		AuditBatchLedgers:   100,
		AuditLagThreshold:   200,
		AuditBudgetShare:    0.5,
		AuditMaxRPS:         10,
		AuditMaxRepair:      3,
		AuditFindingMaxLgrs: 100,
		LogLevel:            "info",
		LogFormat:           "json",
	}
}

func TestValidateAll_Coverage(t *testing.T) {
	tests := []struct {
		name        string
		modify      func(*Config)
		expectError string
	}{
		{"empty DATABASE_URL", func(c *Config) { c.DatabaseURL = "" }, "DATABASE_URL: required but empty"},
		{"empty RPC_URL and RPC_URLS", func(c *Config) { c.RPCURL = ""; c.RPCURLS = nil }, "RPC_URL: required but empty"},
		{"invalid RPC_URL", func(c *Config) { c.RPCURL = "invalid-url" }, "is not a valid absolute URL"},
		{"invalid RPC_URLS element", func(c *Config) { c.RPCURLS = []string{"http://ok.com", "not-a-url"} }, "is not a valid absolute URL"},
		{"negative PollInterval", func(c *Config) { c.PollInterval = -1 * time.Second }, "POLL_INTERVAL"},
		{"negative PollIntervalMin", func(c *Config) { c.PollIntervalMin = -1 * time.Second }, "POLL_INTERVAL_MIN"},
		{"negative PollIntervalMax", func(c *Config) { c.PollIntervalMax = -1 * time.Second }, "POLL_INTERVAL_MAX"},
		{"PollIntervalMin > Max", func(c *Config) { c.PollIntervalMin = 5 * time.Second; c.PollIntervalMax = 1 * time.Second }, "must be <= POLL_INTERVAL_MAX"},
		{"negative AuditPollInterval", func(c *Config) { c.AuditPollInterval = -1 * time.Second }, "AUDIT_POLL_INTERVAL"},
		{"negative IngesterMinBackoff", func(c *Config) { c.IngesterMinBackoff = -1 * time.Second }, "INGESTER_MIN_BACKOFF"},
		{"negative IngesterMaxBackoff", func(c *Config) { c.IngesterMaxBackoff = -1 * time.Second }, "INGESTER_MAX_BACKOFF"},
		{"IngesterMinBackoff > Max", func(c *Config) { c.IngesterMinBackoff = 5 * time.Second; c.IngesterMaxBackoff = 1 * time.Second }, "must not exceed INGESTER_MAX_BACKOFF"},
		{"negative IngesterJitterMin", func(c *Config) { c.IngesterJitterMin = -1 }, "jitter bounds must be non-negative"},
		{"IngesterJitterMin > Max", func(c *Config) { c.IngesterJitterMin = 5; c.IngesterJitterMax = 1 }, "must not exceed INGESTER_JITTER_MAX"},
		{"zero RetentionLedgers", func(c *Config) { c.RetentionLedgers = 0 }, "RETENTION_LEDGERS"},
		{"zero PartitionLedgerSpan", func(c *Config) { c.PartitionLedgerSpan = 0 }, "PARTITION_LEDGER_SPAN"},
		{"zero AuditBatchLedgers", func(c *Config) { c.AuditBatchLedgers = 0 }, "AUDIT_BATCH_LEDGERS"},
		{"zero AuditLagThreshold", func(c *Config) { c.AuditLagThreshold = 0 }, "AUDIT_LAG_THRESHOLD"},
		{"AuditBudgetShare > 1", func(c *Config) { c.AuditBudgetShare = 1.5 }, "AUDIT_BUDGET_SHARE"},
		{"AuditMaxRPS zero", func(c *Config) { c.AuditMaxRPS = 0 }, "AUDIT_MAX_RPS"},
		{"AuditMaxRepair zero", func(c *Config) { c.AuditMaxRepair = 0 }, "AUDIT_MAX_REPAIR_ATTEMPTS"},
		{"AuditFindingMaxLgrs zero", func(c *Config) { c.AuditFindingMaxLgrs = 0 }, "AUDIT_FINDING_MAX_LEDGERS"},
		{"RateLimitRPS negative", func(c *Config) { c.RateLimitRPS = -1 }, "RATE_LIMIT_RPS"},
		{"RateLimitBurst negative", func(c *Config) { c.RateLimitBurst = -1 }, "RATE_LIMIT_BURST"},
		{"HourlyQuota negative", func(c *Config) { c.HourlyQuota = -1 }, "HOURLY_QUOTA"},
		{"DailyQuota negative", func(c *Config) { c.DailyQuota = -1 }, "DAILY_QUOTA"},
		{"invalid LogLevel", func(c *Config) { c.LogLevel = "invalid" }, "LOG_LEVEL"},
		{"invalid LogFormat", func(c *Config) { c.LogFormat = "invalid" }, "LOG_FORMAT"},
		{"invalid WatchedContracts", func(c *Config) { c.WatchedContracts = []string{"invalid"} }, "WATCHED_CONTRACTS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseValidate()
			tt.modify(&cfg)
			err := cfg.ValidateAll()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}
