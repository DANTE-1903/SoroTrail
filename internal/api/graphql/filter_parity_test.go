package graphql

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/api"
	"github.com/sorotrail/sorotrail/internal/store"
)

// otherKnownContractID is a second valid strkey used by the ContractIDs
// list-filter cases below.
const otherKnownContractID = "CAAV3AE3VBFTQPHSXDG6O2NM7DUYJTB2NDXVCBAKMAAV3AE3VBFTQPHS"

// TestFilterParity_RESTAndGraphQLProduceIdenticalFilters is the shared
// test table required by issue #577: every case sends an equivalent
// request through REST's query-parameter parsing (api.FilterFromQuery)
// and through GraphQL's argument parsing (buildEventFilter), then
// asserts the two produce the same store.EventFilter. This is the
// regression guard against the two surfaces drifting again — a filter
// that REST supports but GraphQL doesn't (or vice versa) fails a case
// here rather than shipping silently.
//
// Scope is deliberately excluded from the comparison: REST attaches it
// from the authenticated principal (see filterFromQuery), while the
// GraphQL resolvers attach it separately from context in the caller
// (resolveEvents / resolveTokenEvents), not inside buildEventFilter.
// Comparing it here would just be comparing two zero values by
// construction, not a semantic parity check.
func TestFilterParity_RESTAndGraphQLProduceIdenticalFilters(t *testing.T) {
	fromTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	toTime := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		query  string // REST query string, e.g. "contract_id=C...&type=contract"
		filter *FilterInput
		page   *PageInput
	}{
		{
			name:   "single contract_id",
			query:  "contract_id=" + knownContractID,
			filter: &FilterInput{ContractID: knownContractID},
		},
		{
			name:   "contract_id list",
			query:  "contract_id=" + knownContractID + "," + otherKnownContractID,
			filter: &FilterInput{ContractIDs: []string{knownContractID, otherKnownContractID}},
		},
		{
			name:   "contract_id_prefix",
			query:  "contract_id_prefix=CAB",
			filter: &FilterInput{ContractIDPrefix: "CAB"},
		},
		{
			name:   "types",
			query:  "type=contract,diagnostic",
			filter: &FilterInput{Types: []string{"contract", "diagnostic"}},
		},
		{
			name:   "topic",
			query:  "topic=" + `["transfer"]`,
			filter: &FilterInput{Topic: json.RawMessage(`["transfer"]`)},
		},
		{
			name:  "positional topics",
			query: "topic0=" + `"transfer"` + "&topic2=" + `"amount"`,
			filter: &FilterInput{Topics: &TopicPositionInput{
				T0: json.RawMessage(`"transfer"`),
				T2: json.RawMessage(`"amount"`),
			}},
		},
		{
			name:   "topic_contains",
			query:  "topic_contains=" + `[{"symbol":"transfer"}]`,
			filter: &FilterInput{TopicContains: json.RawMessage(`[{"symbol":"transfer"}]`)},
		},
		{
			name:   "tx_hash",
			query:  "tx_hash=abc123",
			filter: &FilterInput{TxHash: "abc123"},
		},
		{
			name:   "tx_index",
			query:  "tx_index=3",
			filter: &FilterInput{TxIndex: ptrInt32(3)},
		},
		{
			name:   "op_index",
			query:  "op_index=1",
			filter: &FilterInput{OpIndex: ptrInt32(1)},
		},
		{
			name:   "in_successful_call true",
			query:  "in_successful_call=true",
			filter: &FilterInput{InSuccessfulCall: ptrBool(true)},
		},
		{
			name:   "in_successful_call false",
			query:  "in_successful_call=false",
			filter: &FilterInput{InSuccessfulCall: ptrBool(false)},
		},
		{
			name:   "has_value true",
			query:  "has_value=true",
			filter: &FilterInput{HasValue: ptrBool(true)},
		},
		{
			name:   "from_ledger and to_ledger",
			query:  "from_ledger=10&to_ledger=20",
			filter: &FilterInput{FromLedger: ptrInt64(10), ToLedger: ptrInt64(20)},
		},
		{
			name:   "from_time and to_time",
			query:  "from_time=" + fromTime.Format(time.RFC3339) + "&to_time=" + toTime.Format(time.RFC3339),
			filter: &FilterInput{FromTime: &fromTime, ToTime: &toTime},
		},
		{
			name:   "order and order_by",
			query:  "order=desc&order_by=ledger",
			filter: &FilterInput{},
			page:   &PageInput{Order: "desc", OrderBy: "ledger"},
		},
		{
			name:   "limit",
			query:  "limit=50",
			filter: &FilterInput{},
			page:   &PageInput{First: ptrInt32(50)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/events?"+tc.query, nil)
			// filterFromQuery refuses an explicitly named contract the
			// caller's scope doesn't grant (see errForbiddenContract in
			// handlers.go); a wildcard principal here means this test is
			// only exercising filter-construction parity, not the grant
			// check itself.
			ctx := api.WithPrincipal(r.Context(), api.Principal{Scope: store.WildcardScope()})
			r = r.WithContext(ctx)
			restFilter, err := api.FilterFromQuery(r)
			require.NoError(t, err, "REST side")

			args := EventFilterArgs{Filter: tc.filter, Page: tc.page}
			gqlFilter, _, _, _, err := buildEventFilter(args)
			require.NoError(t, err, "GraphQL side")

			// Scope is attached by each transport's caller, not by the
			// shared filter builder — zero it on both sides so this test
			// only compares the filter semantics under test.
			restFilter.Scope = store.Scope{}
			gqlFilter.Scope = store.Scope{}

			assert.Equal(t, restFilter, gqlFilter, "REST and GraphQL must build identical store.EventFilter values for equivalent inputs")
		})
	}
}

func ptrBool(b bool) *bool    { return &b }
func ptrInt64(i int64) *int64 { return &i }
