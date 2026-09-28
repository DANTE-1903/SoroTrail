package store

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// newSQLiteStore is the SQLite entry in the shared conformance suite's
// backend table (see conformance_test.go). It is deliberately not a test
// itself: TestStoreConformance runs the same assertions against every
// registered backend, so adding one does not add a near-duplicate test.
func newSQLiteStore(t *testing.T) Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Apply the full SQLite migration series in version order, mirroring
	// migrateSQLite. Applying only 0001_init (as this helper historically
	// did) would leave later schema additions out — in particular
	// 0003_last_successful_poll's ingestion_state column, which the
	// shared ingestion-state code now reads and writes.
	entries, err := sqliteMigrationsFS.ReadDir("migrations/sqlite")
	if err != nil {
		t.Fatalf("reading sqlite migrations: %v", err)
	}
	var upFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			upFiles = append(upFiles, e.Name())
		}
	}
	sort.Strings(upFiles)
	for _, name := range upFiles {
		schema, err := sqliteMigrationsFS.ReadFile("migrations/sqlite/" + name)
		if err != nil {
			t.Fatalf("reading sqlite migration %s: %v", name, err)
		}
		if _, err := db.ExecContext(context.Background(), string(schema)); err != nil {
			t.Fatalf("applying sqlite migration %s: %v", name, err)
		}
	}

	// Set up WAL mode for concurrent reads during writes
	if _, err := db.ExecContext(context.Background(), `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("setting WAL mode: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("setting busy timeout: %v", err)
	}

	return NewSQLite(db)
}

// TestSQLite_OpenExisting tests that a SQLite database can be opened from a
// file path. Uses a temporary file.
// TestSQLite_IngestionState_LastSuccessfulPoll verifies the SQLite backend
// persists and round-trips the ingester's last-successful-poll timestamp,
// mirroring the Postgres behavior exercised in
// TestIngestionState_LastSuccessfulPoll (postgres_test.go).
func TestSQLite_IngestionState_LastSuccessfulPoll(t *testing.T) {
	st := newSQLiteStore(t)
	ctx := context.Background()

	// Fresh state has no last-successful-poll yet.
	_, err := st.GetIngestionState(ctx)
	require.ErrorIs(t, err, ErrNotFound, "fresh database has no ingestion state")

	// Save without a poll timestamp -> nil on read.
	require.NoError(t, st.SaveIngestionState(ctx, IngestionState{LastIngestedLedger: 10, LastCursor: "c1"}))
	got, err := st.GetIngestionState(ctx)
	require.NoError(t, err)
	assert.Nil(t, got.LastSuccessfulPoll, "LastSuccessfulPoll is nil when not set")

	// Save with a poll timestamp -> round-trips exactly.
	truncated := time.Now().Truncate(time.Millisecond)
	poll := truncated.UTC()
	require.NoError(t, st.SaveIngestionState(ctx, IngestionState{
		LastIngestedLedger: 20,
		LastCursor:         "c2",
		LastSuccessfulPoll: &poll,
	}))
	got, err = st.GetIngestionState(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.LastSuccessfulPoll)
	assert.Equal(t, poll, *got.LastSuccessfulPoll)

	// The UPSERT sets last_successful_poll = excluded.last_successful_poll,
	// so a save without it clears the value (never leaves a stale one).
	require.NoError(t, st.SaveIngestionState(ctx, IngestionState{LastIngestedLedger: 30, LastCursor: "c3"}))
	got, err = st.GetIngestionState(ctx)
	require.NoError(t, err)
	assert.Nil(t, got.LastSuccessfulPoll, "LastSuccessfulPoll is nil when not provided in save")
}

func TestSQLite_OpenExisting(t *testing.T) {
	f, err := os.CreateTemp("", "sorotrail-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer db.Close()

	if err := Migrate("sqlite:" + path); err != nil {
		t.Fatalf("migrating sqlite: %v", err)
	}

	st := NewSQLite(db)
	ctx := context.Background()

	if err := st.Ping(ctx); err != nil {
		t.Fatalf("pinging sqlite: %v", err)
	}

	events, next, err := st.QueryEvents(ctx, EventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("querying events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
	if next != "" {
		t.Fatalf("expected empty next cursor, got %q", next)
	}
}

// TestSQLite_MigrationsCreateTxHashIndex verifies the sqlite migration
// series lands idx_events_tx_hash, mirroring the Postgres-side index from
// 0015_add_tx_hash_index so tx-hash lookups don't scan the whole events
// table on either backend.
func TestSQLite_MigrationsCreateTxHashIndex(t *testing.T) {
	f, err := os.CreateTemp("", "sorotrail-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer db.Close()

	if err := Migrate("sqlite:" + path); err != nil {
		t.Fatalf("migrating sqlite: %v", err)
	}

	var name string
	err = db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_tx_hash'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("idx_events_tx_hash should exist after migrations: %v", err)
	}
}

// TestFormatTime pins the string formatTime stores for the SQLite backend.
// SQLite has no timestamp type, so these strings are what ORDER BY and the
// created_at range filters compare; the exact shape is the contract.
func TestFormatTime(t *testing.T) {
	lagos := time.FixedZone("WAT", 1*60*60)
	newYork := time.FixedZone("EST", -5*60*60)

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "UTC whole second is padded to nine fractional digits",
			in:   time.Date(2024, 3, 15, 12, 30, 45, 0, time.UTC),
			want: "2024-03-15T12:30:45.000000000Z",
		},
		{
			name: "nanosecond precision is preserved",
			in:   time.Date(2024, 3, 15, 12, 30, 45, 123456789, time.UTC),
			want: "2024-03-15T12:30:45.123456789Z",
		},
		{
			name: "trailing zeros are kept, not trimmed",
			in:   time.Date(2024, 3, 15, 12, 30, 45, 500_000_000, time.UTC),
			want: "2024-03-15T12:30:45.500000000Z",
		},
		{
			name: "positive offset is normalised to UTC",
			in:   time.Date(2024, 3, 15, 13, 30, 45, 0, lagos),
			want: "2024-03-15T12:30:45.000000000Z",
		},
		{
			// The shift crosses midnight, so the date must change too.
			name: "negative offset is normalised to UTC across a day boundary",
			in:   time.Date(2024, 12, 31, 22, 0, 0, 0, newYork),
			want: "2025-01-01T03:00:00.000000000Z",
		},
		{
			// Unset optional timestamps must read as absent, not year 1.
			name: "zero time renders as empty string",
			in:   time.Time{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			require.NotPanics(t, func() { got = formatTime(tc.in) })
			assert.Equal(t, tc.want, got)
			if !tc.in.IsZero() {
				assert.True(t, tc.in.Equal(parseTime(got)),
					"parseTime must round-trip %q back to %v", got, tc.in)
			}
		})
	}
}

// TestFormatTime_SortsChronologically asserts that byte-wise string order
// matches time order, which is what SQLite relies on for ORDER BY created_at
// and for the >= / <= range filters. The pairs are the ones a trimmed
// RFC3339Nano layout gets wrong: a whole second against a fraction of the
// same second, and fractions of different lengths.
func TestFormatTime_SortsChronologically(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		earlier, later time.Time
	}{
		{name: "whole second before a fraction of it", earlier: base, later: base.Add(500 * time.Millisecond)},
		{name: "shorter fraction before longer one", earlier: base.Add(100 * time.Millisecond), later: base.Add(120 * time.Millisecond)},
		{name: "one nanosecond apart", earlier: base, later: base.Add(time.Nanosecond)},
		{name: "fraction before next whole second", earlier: base.Add(999_999_999), later: base.Add(time.Second)},
		{name: "across a year boundary", earlier: base.Add(-time.Nanosecond), later: base},
		{
			name:    "different input zones compare in UTC",
			earlier: time.Date(2024, 1, 1, 6, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
			later:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC-1", -1*60*60)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, tc.earlier.Before(tc.later), "fixture must be chronologically ordered")
			e, l := formatTime(tc.earlier), formatTime(tc.later)
			assert.Less(t, e, l, "%q must sort before %q", e, l)
		})
	}

	t.Run("a mixed batch sorts into chronological order", func(t *testing.T) {
		chrono := []time.Time{
			base.Add(-time.Second),
			base,
			base.Add(time.Nanosecond),
			base.Add(100 * time.Millisecond),
			base.Add(120 * time.Millisecond),
			base.Add(500 * time.Millisecond),
			base.Add(time.Second),
			base.Add(10 * time.Second),
		}
		want := make([]string, len(chrono))
		for i, ts := range chrono {
			want[i] = formatTime(ts)
		}
		got := append([]string(nil), want...)
		// Reverse first so sort.Strings has real work to do.
		sort.Sort(sort.Reverse(sort.StringSlice(got)))
		sort.Strings(got)
		assert.Equal(t, want, got)
	})
}
