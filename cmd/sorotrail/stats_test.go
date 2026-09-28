package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

func int64p(v int64) *int64 { return &v }

// TestStatsRows verifies the metric rows rendered for each shape of
// store.Stats: a fully populated report, one where the backend has no
// chain head yet, and a fresh store that has never polled.
func TestStatsRows(t *testing.T) {
	t.Parallel()
	fixedTime := time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		stats store.Stats
		want  map[string]string
	}{
		{
			name: "fully populated",
			stats: store.Stats{
				TotalEvents:           123456,
				LastIngestedLedger:    987654,
				VerifiedThroughLedger: 987000,
				OldestStoredLedger:    100000,
				ChainHeadLedger:       int64p(987700),
				IngestLagLedgers:      int64p(46),
				LastSuccessfulPoll:    &fixedTime,
				ContractCount:         12,
				WatchedContracts:      5,
				TableSizeBytes:        134744064, // 128.5 MiB
			},
			want: map[string]string{
				"total events":            "123456",
				"last ingested ledger":    "987654",
				"verified through ledger": "987000",
				"oldest stored ledger":    "100000",
				"chain head ledger":       "987700",
				"ingest lag":              "46 ledgers",
				"last successful poll":    "2026-09-24 12:30:00",
				"contracts":               "12",
				"watched contracts":       "5",
				"events table size":       "128.5 MiB",
			},
		},
		{
			name: "no chain head yet",
			stats: store.Stats{
				TotalEvents:        1,
				LastIngestedLedger: 42,
				OldestStoredLedger: 42,
				TableSizeBytes:     512,
			},
			want: map[string]string{
				"chain head ledger":    "-",
				"ingest lag":           "-",
				"last successful poll": "never",
				"events table size":    "512 B",
			},
		},
		{
			name:  "fresh empty store",
			stats: store.Stats{},
			want: map[string]string{
				"total events":         "0",
				"last ingested ledger": "0",
				"chain head ledger":    "-",
				"ingest lag":           "-",
				"last successful poll": "never",
				"events table size":    "0 B",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rows := statsRows(tt.stats)
			got := make(map[string]string, len(rows))
			for _, r := range rows {
				got[r.metric] = r.value
			}
			for metric, want := range tt.want {
				assert.Equal(t, want, got[metric], "metric %q", metric)
			}
		})
	}
}

// TestStatsRowsHasExpectedMetrics guards the row set: every metric an
// operator expects from `sorotrail stats` must be present exactly once.
func TestStatsRowsHasExpectedMetrics(t *testing.T) {
	t.Parallel()
	want := []string{
		"total events",
		"last ingested ledger",
		"verified through ledger",
		"oldest stored ledger",
		"chain head ledger",
		"ingest lag",
		"last successful poll",
		"contracts",
		"watched contracts",
		"events table size",
	}
	rows := statsRows(store.Stats{})
	require.Equal(t, len(want), len(rows), "statsRows must emit one row per expected metric")
	for i, r := range rows {
		assert.Equal(t, want[i], r.metric, "row %d", i)
	}
}

// TestRenderStatsTable verifies the table header and that values land
// in the output stream.
func TestRenderStatsTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := renderStatsTable(&buf, store.Stats{
		TotalEvents:        7,
		LastIngestedLedger: 1000,
		TableSizeBytes:     2048,
	})
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "METRIC")
	assert.Contains(t, out, "VALUE")
	assert.Contains(t, out, "total events")
	assert.Contains(t, out, "7")
	assert.Contains(t, out, "2.0 KiB")
}

// TestRunStatsArgs covers the subcommand's flag handling: --help and
// unexpected arguments must be resolved before any database connection
// is attempted.
func TestRunStatsArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "help short", args: []string{"-h"}},
		{name: "help long", args: []string{"--help"}},
		{name: "unexpected arg", args: []string{"extra"}, wantErr: true},
		{name: "unknown flag", args: []string{"--bogus"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			err := runStatsTo(&buf, tt.args)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestStatsUsageMentionsReadOnly checks the help text documents the
// read-only contract and the process-local-counter exclusion.
func TestStatsUsageMentionsReadOnly(t *testing.T) {
	t.Parallel()
	assert.Contains(t, statsUsage, "usage: sorotrail stats")
	assert.Contains(t, statsUsage, "Read-only")
	assert.Contains(t, statsUsage, "/stats endpoint")
}
