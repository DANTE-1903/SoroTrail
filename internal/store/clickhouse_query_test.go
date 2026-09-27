package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClickHouseEventWhereClause_NoFilters(t *testing.T) {
	where, err := clickHouseEventWhereClause(EventFilter{Scope: WildcardScope()})
	require.NoError(t, err)
	assert.Empty(t, where)
}

func TestClickHouseEventWhereClause_ScopeRestrictsToGrantedContracts(t *testing.T) {
	sc := NewScope([]string{"CA", "CB"})

	where, err := clickHouseEventWhereClause(EventFilter{Scope: sc})
	require.NoError(t, err)
	require.Len(t, where, 1)
	assert.Equal(t, "contract_id IN ('CA', 'CB')", where[0])
}

func TestClickHouseEventWhereClause_ContractIDMergesWithContractIDs(t *testing.T) {
	where, err := clickHouseEventWhereClause(EventFilter{
		Scope:       WildcardScope(),
		ContractID:  "CA",
		ContractIDs: []string{"CB"},
	})
	require.NoError(t, err)
	require.Len(t, where, 1)
	assert.Equal(t, "contract_id IN ('CB', 'CA')", where[0])
}

func TestClickHouseEventWhereClause_TopicFiltersAreExplicitlyUnsupported(t *testing.T) {
	cases := []EventFilter{
		{Scope: WildcardScope(), Topic: []byte(`{"a":1}`)},
		{Scope: WildcardScope(), TopicContains: []byte(`{"a":1}`)},
		{Scope: WildcardScope(), Topic0: []byte(`{"a":1}`)},
	}
	for _, f := range cases {
		_, err := clickHouseEventWhereClause(f)
		require.Error(t, err, "topic filters must error rather than silently ignoring the constraint")
	}
}

func TestClickHouseEventWhereClause_ScalarFilters(t *testing.T) {
	txIdx := int32(3)
	hasValue := true
	where, err := clickHouseEventWhereClause(EventFilter{
		Scope:            WildcardScope(),
		Network:          "testnet",
		TxHash:           "abc",
		TxIndex:          &txIdx,
		HasValue:         &hasValue,
		InSuccessfulCall: boolPtr(false),
		FromLedger:       10,
		ToLedger:         20,
	})
	require.NoError(t, err)
	assert.Contains(t, where, "network = 'testnet'")
	assert.Contains(t, where, "tx_hash = 'abc'")
	assert.Contains(t, where, "tx_index = 3")
	assert.Contains(t, where, "value IS NOT NULL")
	assert.Contains(t, where, "in_successful_call = 0")
	assert.Contains(t, where, "ledger >= 10")
	assert.Contains(t, where, "ledger <= 20")
}

func TestClickHouseQuoteString_EscapesQuotesAndBackslashes(t *testing.T) {
	assert.Equal(t, `'it\'s'`, clickHouseQuoteString(`it's`))
	assert.Equal(t, `'a\\b'`, clickHouseQuoteString(`a\b`))
}

func TestClickHouseRetentionWhere(t *testing.T) {
	where := clickHouseRetentionWhere(100, time.Time{})
	assert.Equal(t, "ledger < 100", where)

	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	where = clickHouseRetentionWhere(100, cutoff)
	assert.Equal(t, "ledger < 100 AND created_at < '2026-01-02 03:04:05.000'", where)
}

func TestLoadClickHouseMigrations_OrderedAndNonEmpty(t *testing.T) {
	migrations, err := loadClickHouseMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	for i := 1; i < len(migrations); i++ {
		assert.Less(t, migrations[i-1].version, migrations[i].version, "migrations must be sorted by version")
	}
}

func TestClickHouseMigrationVersion(t *testing.T) {
	v, err := clickHouseMigrationVersion("0001_init.up.sql")
	require.NoError(t, err)
	assert.Equal(t, uint(1), v)

	_, err = clickHouseMigrationVersion("bad-name.sql")
	assert.Error(t, err)
}

func TestSplitStatements(t *testing.T) {
	stmts := splitStatements("CREATE TABLE a (x Int32); \n\n CREATE TABLE b (y Int32);")
	require.Len(t, stmts, 2)
	assert.Equal(t, "CREATE TABLE a (x Int32)", stmts[0])
	assert.Equal(t, "CREATE TABLE b (y Int32)", stmts[1])
}

func boolPtr(b bool) *bool { return &b }
