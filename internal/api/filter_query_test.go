package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// filterQueryCase is one row of the filterFromQuery table: a raw query
// string, the principal to install on the request context (nil means the
// wildcard, untenanted principal the single-tenant deployment injects),
// and either the filter fields expected back or the error substring the
// parser must produce.
type filterQueryCase struct {
	name    string
	query   string
	want    func(t *testing.T, f store.EventFilter)
	wantErr string // empty means the parse must succeed
}

// runFilterQueryTable drives every case through filterFromQuery the same
// way a handler would: query string in, EventFilter (or typed error) out.
// wide selects the wildcard principal a single-tenant deployment injects;
// every table row uses it because the authorization-specific behaviour has
// its own test below.
func runFilterQueryTable(t *testing.T, tests []filterQueryCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/events?"+tt.query, nil)
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scope: store.WildcardScope(), Untenanted: true}))

			f, err := filterFromQuery(r)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.want != nil {
				tt.want(t, f)
			}
		})
	}
}

// TestFilterFromQuery_Table covers the shared event-filter query params
// with a table of inputs and the filters they must produce. The happy-path
// rows pin the exact mapping; the error rows pin the param name and
// message fragment the REST API contract promises.
func TestFilterFromQuery_Table(t *testing.T) {
	contract := validAddr('C')
	otherContract := validAddr('C')
	tests := []filterQueryCase{
		// --- happy paths -------------------------------------------------
		{
			name:  "empty query yields an unconstrained filter with the default limit",
			query: "",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Empty(t, f.ContractID)
				assert.Empty(t, f.Types)
				assert.Zero(t, f.FromLedger)
				assert.Zero(t, f.ToLedger)
				assert.True(t, f.FromTime.IsZero())
				assert.True(t, f.ToTime.IsZero())
				assert.Equal(t, store.DefaultQueryLimit, f.Limit, "absent limit defaults")
				assert.True(t, f.Scope.IsWildcard(), "scope comes from the request principal")
			},
		},
		{
			name:  "single contract_id sets ContractID",
			query: "contract_id=" + contract,
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, contract, f.ContractID)
				assert.Empty(t, f.ContractIDs)
			},
		},
		{
			name:  "comma-separated contract_id list sets ContractIDs and drops ContractID",
			query: "contract_id=" + contract + "," + otherContract,
			want: func(t *testing.T, f store.EventFilter) {
				assert.Empty(t, f.ContractID, "a multi-ID list must not set the single-ID field")
				assert.Equal(t, []string{contract, otherContract}, f.ContractIDs)
			},
		},
		{
			name:  "spaces around list elements are trimmed and empties skipped",
			query: "contract_id=" + contract + ",%20," + otherContract,
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, []string{contract, otherContract}, f.ContractIDs)
			},
		},
		{
			name:  "contract_id_prefix passes through",
			query: "contract_id_prefix=CAB",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, "CAB", f.ContractIDPrefix)
			},
		},
		{
			name:  "multiple types are kept in order",
			query: "type=contract,system",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, []string{"contract", "system"}, f.Types)
			},
		},
		{
			name:  "bare topic word is auto-quoted to a JSON string",
			query: "topic=transfer",
			want: func(t *testing.T, f store.EventFilter) {
				assert.JSONEq(t, `"transfer"`, string(f.Topic))
			},
		},
		{
			name:  "positional topic filters map to Topic0..Topic3",
			query: "topic0=transfer&topic2=mint",
			want: func(t *testing.T, f store.EventFilter) {
				assert.JSONEq(t, `"transfer"`, string(f.Topic0))
				assert.Empty(t, f.Topic1)
				assert.JSONEq(t, `"mint"`, string(f.Topic2))
				assert.Empty(t, f.Topic3)
			},
		},
		{
			name:  "topic_contains passes JSON through unwrapped",
			query: "topic_contains=%5B%7B%22symbol%22%3A%22transfer%22%7D%5D",
			want: func(t *testing.T, f store.EventFilter) {
				assert.JSONEq(t, `[{"symbol":"transfer"}]`, string(f.TopicContains))
			},
		},
		{
			name:  "ledger and time ranges map onto the filter",
			query: "from_ledger=100&to_ledger=200&from_time=2026-07-21T00%3A00%3A00Z&to_time=2026-07-22T00%3A00%3A00Z",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, int64(100), f.FromLedger)
				assert.Equal(t, int64(200), f.ToLedger)
				assert.Equal(t, "2026-07-21T00:00:00Z", f.FromTime.UTC().Format("2006-01-02T15:04:05Z"))
				assert.Equal(t, "2026-07-22T00:00:00Z", f.ToTime.UTC().Format("2006-01-02T15:04:05Z"))
			},
		},
		{
			name:  "order, order_by, tx_hash and cursor pass through",
			query: "order=desc&order_by=ledger&tx_hash=abc&cursor=cDcy",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, "desc", f.Order)
				assert.Equal(t, store.OrderByLedger, f.OrderBy)
				assert.Equal(t, "abc", f.TxHash)
				assert.Equal(t, "cDcy", f.Cursor)
			},
		},
		{
			name:  "explicit limit is honored",
			query: "limit=7",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Equal(t, 7, f.Limit)
			},
		},
		{
			name:  "tx_index and op_index become pointers",
			query: "tx_index=3&op_index=1",
			want: func(t *testing.T, f store.EventFilter) {
				require.NotNil(t, f.TxIndex)
				assert.Equal(t, int32(3), *f.TxIndex)
				require.NotNil(t, f.OpIndex)
				assert.Equal(t, int32(1), *f.OpIndex)
			},
		},
		{
			name:  "in_successful_call true and false are distinguished from absent",
			query: "in_successful_call=false",
			want: func(t *testing.T, f store.EventFilter) {
				require.NotNil(t, f.InSuccessfulCall)
				assert.False(t, *f.InSuccessfulCall)
			},
		},
		{
			name:  "in_successful_call absent stays nil",
			query: "",
			want: func(t *testing.T, f store.EventFilter) {
				assert.Nil(t, f.InSuccessfulCall)
			},
		},
		// --- error paths --------------------------------------------------
		{
			name:    "bad from_ledger names the parameter",
			query:   "from_ledger=abc",
			wantErr: "from_ledger must be a positive integer",
		},
		{
			name:    "zero to_ledger is not a positive integer",
			query:   "to_ledger=0",
			wantErr: "to_ledger must be a positive integer",
		},
		{
			name:    "inverted ledger range is rejected",
			query:   "from_ledger=200&to_ledger=100",
			wantErr: "after",
		},
		{
			name:    "bad from_time names the parameter",
			query:   "from_time=yesterday",
			wantErr: "from_time must be an RFC 3339 timestamp",
		},
		{
			name:    "sub-second precision is rejected",
			query:   "to_time=2026-07-21T00:00:00.123Z",
			wantErr: "to_time sub-second precision is not supported",
		},
		{
			name:    "inverted time range is rejected",
			query:   "from_time=2026-07-22T00%3A00%3A00Z&to_time=2026-07-21T00%3A00%3A00Z",
			wantErr: "is after",
		},
		{
			name:    "unknown event type is rejected",
			query:   "type=contract,bogus",
			wantErr: `invalid type "bogus"`,
		},
		{
			name:    "topic and positional topic cannot be combined",
			query:   "topic=transfer&topic0=transfer",
			wantErr: "cannot be combined",
		},
		{
			name:    "topic_contains must be valid JSON",
			query:   "topic_contains=transfer",
			wantErr: "topic_contains must be valid JSON",
		},
		{
			name:    "malformed contract_id names the bad value",
			query:   "contract_id=not-a-strkey",
			wantErr: `invalid contract_id "not-a-strkey"`,
		},
		{
			name:    "one bad element fails the whole contract_id list",
			query:   "contract_id=" + contract + ",oops",
			wantErr: `invalid contract_id "oops"`,
		},
		{
			name:    "contract_id and contract_id_prefix cannot be combined",
			query:   "contract_id=" + contract + "&contract_id_prefix=CAB",
			wantErr: "cannot be combined",
		},
		{
			name:    "invalid order is rejected",
			query:   "order=reverse",
			wantErr: `invalid order "reverse"`,
		},
		{
			name:    "invalid order_by is rejected",
			query:   "order_by=tx_hash",
			wantErr: "invalid order_by",
		},
		{
			name:    "cursor with disallowed characters is rejected",
			query:   "cursor=has%20space",
			wantErr: `invalid cursor "has space"`,
		},
		{
			name:    "limit zero is invalid when explicit",
			query:   "limit=0",
			wantErr: "limit must be an integer in [1,",
		},
		{
			name:    "limit above the cap is invalid",
			query:   "limit=501",
			wantErr: "limit must be an integer in [1,",
		},
		{
			name:    "non-numeric limit is invalid",
			query:   "limit=many",
			wantErr: "limit must be an integer in [1,",
		},
		{
			name:    "invalid in_successful_call is rejected",
			query:   "in_successful_call=maybe",
			wantErr: `invalid in_successful_call "maybe"`,
		},
		{
			name:    "negative tx_index is rejected",
			query:   "tx_index=-1",
			wantErr: "invalid tx_index",
		},
		{
			name:    "non-numeric op_index is rejected",
			query:   "op_index=first",
			wantErr: "invalid op_index",
		},
	}
	runFilterQueryTable(t, tests)
}

// TestFilterFromQuery_ScopeAuthorization pins the authorization behaviour
// that filterFromQuery owns for every list-shaped read: the scope comes
// from the request principal, and a contract outside the caller's grants
// is refused with the typed 403 error rather than silently filtered.
func TestFilterFromQuery_ScopeAuthorization(t *testing.T) {
	granted := validAddr('C')

	t.Run("scope is taken from the principal, not widened", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/events", nil)
		r = r.WithContext(WithPrincipal(r.Context(), Principal{
			Scope: store.NewScope([]string{granted}),
		}))
		f, err := filterFromQuery(r)
		require.NoError(t, err)
		assert.False(t, f.Scope.IsWildcard())
		assert.True(t, f.Scope.Allows(granted))
	})

	t.Run("missing principal denies everything", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/events", nil)
		f, err := filterFromQuery(r)
		require.NoError(t, err)
		assert.True(t, f.Scope.DeniesAll())
	})

	t.Run("ungranted contract is a typed forbidden error", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/events?contract_id="+granted, nil)
		r = r.WithContext(WithPrincipal(r.Context(), Principal{
			Scope: store.NewScope([]string{otherContractID(t)}),
		}))
		_, err := filterFromQuery(r)
		var forbidden errForbiddenContract
		require.ErrorAs(t, err, &forbidden)
		assert.Equal(t, granted, forbidden.contractID)
		assert.Contains(t, err.Error(), "is not granted to this tenant")
	})

	t.Run("ungranted contract with a missing principal is also forbidden", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/events?contract_id="+granted, nil)
		r = r.WithContext(context.Background())
		_, err := filterFromQuery(r)
		var forbidden errForbiddenContract
		require.ErrorAs(t, err, &forbidden)
	})
}

// otherContractID returns a second syntactically valid contract ID that is
// guaranteed to differ from the one validAddr('C') derives.
func otherContractID(t *testing.T) string {
	t.Helper()
	id := validAddr('C')
	// Flip the last base32 character (A -> B) so the two IDs never match.
	return id[:55] + "B"
}
