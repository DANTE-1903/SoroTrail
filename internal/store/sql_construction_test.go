package store

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqlBoolPtr and sqlI32Ptr keep this file independent of the pointer helpers
// other test files define, so it stays compiling on its own.
func sqlBoolPtr(v bool) *bool  { return &v }
func sqlI32Ptr(v int32) *int32 { return &v }

// assertPlaceholders verifies the two properties every generated statement
// must hold: placeholders run contiguously from $1 to len(args) with no gaps
// or extras, and no caller-supplied value is ever concatenated into the SQL
// text. The second is the injection invariant — a value that appears verbatim
// in the SQL string is a value that could escape its quoting.
func assertPlaceholders(t *testing.T, fragments []string, args []any) {
	t.Helper()
	sql := strings.Join(fragments, " AND ")

	re := regexp.MustCompile(`\$(\d+)`)
	seen := map[int]bool{}
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		n, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		seen[n] = true
	}

	assert.Len(t, seen, len(args),
		"placeholder count must match the argument slice exactly, got SQL %q with %d args", sql, len(args))
	for i := 1; i <= len(args); i++ {
		assert.True(t, seen[i], "placeholder $%d is missing from %q", i, sql)
	}

	// Only textual arguments can be injection vectors, and only they are
	// meaningful here: a numeric value like 1 appears inside "$1" by
	// construction, so checking it would be a false positive.
	for _, a := range args {
		var s string
		switch v := a.(type) {
		case string:
			s = v
		case json.RawMessage:
			s = string(v)
		default:
			continue
		}
		if len(s) < 4 {
			continue
		}
		assert.NotContains(t, sql, s,
			"caller value %q must be bound as an argument, never concatenated into the query text", s)
	}
}

func TestBuildEventWhereClause(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		filter        EventFilter
		wantFragments []string
		wantArgs      []any
	}{
		{
			name:          "wildcard scope and no filters yields no predicate",
			filter:        EventFilter{Scope: WildcardScope()},
			wantFragments: nil,
			wantArgs:      nil,
		},
		{
			name:          "network filters on the resolved network",
			filter:        EventFilter{Network: "mainnet", Scope: WildcardScope()},
			wantFragments: []string{"network = $1"},
			wantArgs:      []any{"mainnet"},
		},
		{
			name:          "empty network is left unconstrained",
			filter:        EventFilter{Scope: WildcardScope()},
			wantFragments: nil,
		},
		{
			name:          "single contract id",
			filter:        EventFilter{ContractID: contractA, Scope: WildcardScope()},
			wantFragments: []string{"contract_id = $1"},
			wantArgs:      []any{contractA},
		},
		{
			name:          "contract id prefix uses a bound LIKE pattern",
			filter:        EventFilter{ContractIDPrefix: "CAAAA", Scope: WildcardScope()},
			wantFragments: []string{"contract_id LIKE $1"},
			wantArgs:      []any{"CAAAA%"},
		},
		{
			name:          "types become a bound ANY list",
			filter:        EventFilter{Types: []string{"contract", "diagnostic"}, Scope: WildcardScope()},
			wantFragments: []string{"type = ANY($1)"},
			wantArgs:      []any{[]string{"contract", "diagnostic"}},
		},
		{
			name:          "tx hash",
			filter:        EventFilter{TxHash: "deadbeef", Scope: WildcardScope()},
			wantFragments: []string{"tx_hash = $1"},
			wantArgs:      []any{"deadbeef"},
		},
		{
			name:          "in successful call true",
			filter:        EventFilter{InSuccessfulCall: sqlBoolPtr(true), Scope: WildcardScope()},
			wantFragments: []string{"in_successful_call = $1"},
			wantArgs:      []any{true},
		},
		{
			name:          "in successful call false",
			filter:        EventFilter{InSuccessfulCall: sqlBoolPtr(false), Scope: WildcardScope()},
			wantFragments: []string{"in_successful_call = $1"},
			wantArgs:      []any{false},
		},
		{
			name:          "topic is array-wrapped so containment matches any position",
			filter:        EventFilter{Topic: json.RawMessage(`{"u64":7}`), Scope: WildcardScope()},
			wantFragments: []string{"topics @> $1::jsonb"},
			wantArgs:      []any{`[{"u64":7}]`},
		},
		{
			name:          "topic_contains is passed through unwrapped",
			filter:        EventFilter{TopicContains: json.RawMessage(`[{"u64":7}]`), Scope: WildcardScope()},
			wantFragments: []string{"topics @> $1::jsonb"},
			wantArgs:      []any{`[{"u64":7}]`},
		},
		{
			name:          "positional topic keeps its index",
			filter:        EventFilter{Topic0: json.RawMessage(`{"a":1}`), Topic2: json.RawMessage(`{"b":2}`), Scope: WildcardScope()},
			wantFragments: []string{"topics->0 = $1::jsonb", "topics->2 = $2::jsonb"},
			wantArgs:      []any{json.RawMessage(`{"a":1}`), json.RawMessage(`{"b":2}`)},
		},
		{
			name:          "has value true",
			filter:        EventFilter{HasValue: sqlBoolPtr(true), Scope: WildcardScope()},
			wantFragments: []string{"value IS NOT NULL"},
			wantArgs:      nil,
		},
		{
			name:          "has value false",
			filter:        EventFilter{HasValue: sqlBoolPtr(false), Scope: WildcardScope()},
			wantFragments: []string{"value IS NULL"},
			wantArgs:      nil,
		},
		{
			name:          "tx and op index",
			filter:        EventFilter{TxIndex: sqlI32Ptr(3), OpIndex: sqlI32Ptr(1), Scope: WildcardScope()},
			wantFragments: []string{"tx_index = $1", "op_index = $2"},
			wantArgs:      []any{int32(3), int32(1)},
		},
		{
			name:          "ledger range is inclusive on both ends",
			filter:        EventFilter{FromLedger: 100, ToLedger: 200, Scope: WildcardScope()},
			wantFragments: []string{"ledger >= $1", "ledger <= $2"},
			wantArgs:      []any{int64(100), int64(200)},
		},
		{
			name:          "time range is inclusive on both ends",
			filter:        EventFilter{FromTime: now, ToTime: now.Add(time.Hour), Scope: WildcardScope()},
			wantFragments: []string{"created_at >= $1", "created_at <= $2"},
			wantArgs:      []any{now, now.Add(time.Hour)},
		},
		{
			name:          "leading contract id merges into the contract list",
			filter:        EventFilter{ContractID: contractB, ContractIDs: []string{contractA}, Scope: WildcardScope()},
			wantFragments: []string{"contract_id = ANY($1)"},
			wantArgs:      []any{[]string{contractA, contractB}},
		},
		{
			name:          "an explicit contract already in the list is not duplicated",
			filter:        EventFilter{ContractID: contractA, ContractIDs: []string{contractA}, Scope: WildcardScope()},
			wantFragments: []string{"contract_id = ANY($1)"},
			wantArgs:      []any{[]string{contractA}},
		},
		{
			name:          "granted scope restricts to the granted contracts",
			filter:        EventFilter{Scope: NewScope([]string{contractB, contractA})},
			wantFragments: []string{"contract_id = ANY($1)"},
			wantArgs:      []any{[]string{contractA, contractB}},
		},
		{
			name:          "zero scope still emits a predicate that matches nothing",
			filter:        EventFilter{},
			wantFragments: []string{"contract_id = ANY($1)"},
			wantArgs:      []any{[]string(nil)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fragments, args := buildEventWhereClause(tt.filter)

			if tt.wantFragments == nil {
				assert.Empty(t, fragments)
			} else {
				sql := strings.Join(fragments, " AND ")
				for _, want := range tt.wantFragments {
					assert.Contains(t, sql, want)
				}
			}
			if tt.wantArgs != nil {
				assert.Equal(t, tt.wantArgs, args)
			}
			assertPlaceholders(t, fragments, args)
		})
	}
}

// TestBuildEventWhereClause_CombinedFilters checks the ordering contract when
// every filter is set at once: scope is appended first (so the tenant boundary
// can never be dropped by a later early return), and the remaining fragments
// preserve their documented order with contiguous placeholders.
func TestBuildEventWhereClause_CombinedFilters(t *testing.T) {
	f := EventFilter{
		Network:          "mainnet",
		ContractID:       contractA,
		Types:            []string{"contract"},
		TxHash:           "deadbeef",
		InSuccessfulCall: sqlBoolPtr(true),
		TopicContains:    json.RawMessage(`[{"u64":7}]`),
		HasValue:         sqlBoolPtr(true),
		TxIndex:          sqlI32Ptr(1),
		OpIndex:          sqlI32Ptr(2),
		FromLedger:       100,
		ToLedger:         200,
		FromTime:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ToTime:           time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		Scope:            WildcardScope(),
	}

	fragments, args := buildEventWhereClause(f)
	require.NotEmpty(t, fragments)

	sql := strings.Join(fragments, " AND ")
	assert.True(t, strings.HasPrefix(sql, "network = $1"), "network is always the first fragment: %q", sql)
	assert.Contains(t, sql, "contract_id = $2")
	assertPlaceholders(t, fragments, args)
}

func TestContractsWhere(t *testing.T) {
	assert.Equal(t, "", contractsWhere(nil))
	assert.Equal(t, " WHERE a = 1 AND b = 2", contractsWhere([]string{"a = 1", "b = 2"}))
}

func TestTableSizeExpr(t *testing.T) {
	assert.Equal(t, "0::bigint", tableSizeExpr(false),
		"a scoped caller must never learn the instance-wide table size")
	assert.Contains(t, tableSizeExpr(true), "pg_total_relation_size")
}

func TestOwnerPredicate(t *testing.T) {
	t.Run("all owners contribute no predicate", func(t *testing.T) {
		pred, args := ownerPredicate(AllSubscriptions(), 2)
		assert.Empty(t, pred)
		assert.Nil(t, args)
	})

	t.Run("owned by tenant binds the tenant id", func(t *testing.T) {
		pred, args := ownerPredicate(OwnedBy(7), 2)
		assert.Equal(t, " AND tenant_id = $2", pred)
		assert.Equal(t, []any{int64(7)}, args)
	})

	t.Run("zero owner matches no row rather than everything", func(t *testing.T) {
		pred, args := ownerPredicate(SubscriptionOwner{}, 1)
		assert.Equal(t, " AND tenant_id = $1", pred)
		assert.Equal(t, []any{int64(0)}, args,
			"tenant_id is a bigserial that never takes 0, so the zero owner denies")
	})
}

func TestNullableHelpers(t *testing.T) {
	assert.Nil(t, nullableText(""))
	assert.Equal(t, "x", nullableText("x"))
	assert.Nil(t, nullableString(""))
	assert.Equal(t, "x", nullableString("x"))
	assert.Nil(t, nullableStringSlice(nil))
	assert.Equal(t, []string{"a"}, nullableStringSlice([]string{"a"}))
	assert.Nil(t, nullableXDRTopics(nil))
	got, ok := nullableXDRTopics([]string{"AAA="}).(string)
	require.True(t, ok, "raw XDR topics are stored as a JSON string")
	assert.JSONEq(t, `["AAA="]`, got)
}

func TestTopicContainsExprUsesPlaceholder(t *testing.T) {
	expr := topicContainsExpr("$1")
	assert.Contains(t, expr, "$1", "the match value must be bound, not concatenated")
	assert.Contains(t, expr, "json_each(topics)")
}

// TestClickHouseQuoteStringEscapesQuotes pins the escaping that stands in for
// parameter binding on the ClickHouse HTTP interface, where each statement is
// a single request body and there is no query/args split.
func TestClickHouseQuoteStringEscapesQuotes(t *testing.T) {
	got := clickHouseQuoteString(`O'Brien\`)
	assert.Equal(t, `'O\'Brien\\'`, got)
}
