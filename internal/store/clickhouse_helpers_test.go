package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClickHouseEventOrderColumns(t *testing.T) {
	assert.Equal(t, "id ASC", clickHouseEventOrderColumns(EventFilter{}))
	assert.Equal(t, "id DESC", clickHouseEventOrderColumns(EventFilter{Order: "desc"}))
	assert.Equal(t, "ledger ASC, id ASC", clickHouseEventOrderColumns(EventFilter{OrderBy: OrderByLedger}))
	assert.Equal(t, "ledger DESC, id DESC",
		clickHouseEventOrderColumns(EventFilter{OrderBy: OrderByLedger, Order: "desc"}))
	assert.Equal(t, "created_at ASC, id ASC", clickHouseEventOrderColumns(EventFilter{OrderBy: OrderByCreatedAt}))
}

func TestClickHouseEventCursorClause(t *testing.T) {
	t.Run("no cursor contributes nothing", func(t *testing.T) {
		pred, err := clickHouseEventCursorClause(EventFilter{})
		require.NoError(t, err)
		assert.Empty(t, pred)
	})

	t.Run("id ordering compares ids directly", func(t *testing.T) {
		pred, err := clickHouseEventCursorClause(EventFilter{Cursor: "id-9"})
		require.NoError(t, err)
		assert.Equal(t, "id > 'id-9'", pred)

		pred, err = clickHouseEventCursorClause(EventFilter{Cursor: "id-9", Order: "desc"})
		require.NoError(t, err)
		assert.Equal(t, "id < 'id-9'", pred)
	})

	t.Run("ledger ordering uses the composite cursor as a row value", func(t *testing.T) {
		cursor := encodeCompositeCursor("42", "id-9")
		pred, err := clickHouseEventCursorClause(EventFilter{Cursor: cursor, OrderBy: OrderByLedger})
		require.NoError(t, err)
		assert.Equal(t, "(ledger, id) > (42, 'id-9')", pred)

		pred, err = clickHouseEventCursorClause(EventFilter{Cursor: cursor, Order: "desc", OrderBy: OrderByLedger})
		require.NoError(t, err)
		assert.Equal(t, "(ledger, id) < (42, 'id-9')", pred)
	})

	t.Run("created_at ordering normalizes the timestamp literal", func(t *testing.T) {
		ts := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		cursor := EncodeCursor(OrderByCreatedAt, Event{ID: "id-9", CreatedAt: ts})
		pred, err := clickHouseEventCursorClause(EventFilter{Cursor: cursor, OrderBy: OrderByCreatedAt})
		require.NoError(t, err)
		assert.Equal(t, "(created_at, id) > ('2026-07-20 12:00:00.000', 'id-9')", pred)
	})

	t.Run("malformed cursors are caller errors", func(t *testing.T) {
		bad := encodeCompositeCursor("not-a-number", "id-9")
		_, err := clickHouseEventCursorClause(EventFilter{Cursor: bad, OrderBy: OrderByLedger})
		assert.ErrorIs(t, err, ErrInvalidCursor)

		badTime := encodeCompositeCursor("not-a-time", "id-9")
		_, err = clickHouseEventCursorClause(EventFilter{Cursor: badTime, OrderBy: OrderByCreatedAt})
		assert.ErrorIs(t, err, ErrInvalidCursor)
	})

	t.Run("unsupported ordering errors", func(t *testing.T) {
		_, err := clickHouseEventCursorClause(EventFilter{Cursor: "x", OrderBy: "bogus"})
		assert.Error(t, err)
	})
}

func TestNextClickHouseIDIsMonotonic(t *testing.T) {
	first := nextClickHouseID()
	second := nextClickHouseID()
	assert.Greater(t, second, first, "ids must be strictly increasing so keyset order is stable")
	assert.Positive(t, first)
}

func TestClickHouseEventRowZeroCreatedAtIsRepresentable(t *testing.T) {
	// ClickHouse DateTime64 cannot store Go's zero year, so an event without
	// an explicit timestamp must be coerced to something in range.
	row := clickHouseEventRow(Event{ID: "e1"})
	created, ok := row["created_at"].(string)
	require.True(t, ok)
	parsed, err := parseClickHouseTime(created)
	require.NoError(t, err)
	assert.True(t, parsed.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)),
		"a zero CreatedAt must not reach ClickHouse as year 1")
}

func TestClickHouseEventRowCarriesRawXDR(t *testing.T) {
	row := clickHouseEventRow(Event{
		ID:          "e1",
		ContractID:  contractA,
		Ledger:      5,
		Topics:      json.RawMessage(`[{"sym":"x"}]`),
		RawTopicXDR: []string{"AAA="},
		RawValueXDR: "BBB",
	})
	assert.Equal(t, []string{"AAA="}, row["raw_topic_xdr"])
	assert.Equal(t, "BBB", row["raw_value_xdr"])
	assert.Equal(t, `[{"sym":"x"}]`, row["topics"])
}

func TestClickHouseTopicsJSONDefaultsToArray(t *testing.T) {
	assert.Equal(t, "[]", clickHouseTopicsJSON(nil))
	assert.Equal(t, `[{"a":1}]`, clickHouseTopicsJSON(json.RawMessage(`[{"a":1}]`)))
}

func TestClickHouseOwnerPredicate(t *testing.T) {
	assert.Empty(t, clickHouseOwnerPredicate(AllSubscriptions()))
	assert.Equal(t, " AND tenant_id = 7", clickHouseOwnerPredicate(OwnedBy(7)))
	assert.Equal(t, " AND tenant_id = 0", clickHouseOwnerPredicate(OwnedBy(0)))
}

func TestClickHouseTimeRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 20, 30, 123000000, time.UTC)
	got, err := parseClickHouseTime(formatClickHouseTime(now))
	require.NoError(t, err)
	assert.True(t, got.Equal(now))
}

func TestClickHouseMigrationsLoadInOrder(t *testing.T) {
	migrations, err := loadClickHouseMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(migrations), 2)
	for i := 1; i < len(migrations); i++ {
		assert.Less(t, migrations[i-1].version, migrations[i].version,
			"versions must load in ascending order")
	}

	var all strings.Builder
	for _, m := range migrations {
		all.WriteString(m.sql)
		assert.NotEmpty(t, splitStatements(m.sql), "migration %s produced no statements", m.name)
	}

	for _, table := range []string{
		"events", "ingestion_state", "audit_state", "watched_contracts",
		"subscriptions", "delivery_attempts", "dead_letters",
		"contract_cursors", "audit_findings",
	} {
		assert.Contains(t, all.String(), "CREATE TABLE IF NOT EXISTS "+table,
			"schema must define the %s table", table)
	}
}

func TestDedupeEventsByID(t *testing.T) {
	events := []Event{{ID: "a"}, {ID: "b"}, {ID: "a"}}
	got := dedupeEventsByID(events)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "b", got[1].ID)
}
