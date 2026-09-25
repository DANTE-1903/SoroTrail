package graphql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/api"
	"github.com/sorotrail/sorotrail/internal/api/queries"
	"github.com/sorotrail/sorotrail/internal/store"
)

// restEventFilter reproduces the REST /events filter construction for the
// subset of parameters the GraphQL EventFilterInput also exposes. It parses
// through the same shared queries package internal/api.filterFromQuery uses,
// then calls queries.BuildEventFilter exactly as the REST handler does, so a
// resolver that builds a different store.EventFilter for the same logical
// filter shows up as a mismatch.
//
// Fields the GraphQL surface does not carry yet (multi-value contractIDs,
// tx_index/op_index/in_successful_call, has_value, recent) and Scope are
// deliberately out of this helper's scope: scope parity is tracked
// separately in issue #577.
func restEventFilter(rawQuery string) (store.EventFilter, error) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return store.EventFilter{}, err
	}

	types, err := queries.ParseTypes(q.Get("type"))
	if err != nil {
		return store.EventFilter{}, err
	}
	topic, err := queries.ParseTopic(q.Get("topic"))
	if err != nil {
		return store.EventFilter{}, err
	}
	t0, err := queries.ParseTopic(q.Get("topic0"))
	if err != nil {
		return store.EventFilter{}, err
	}
	t1, err := queries.ParseTopic(q.Get("topic1"))
	if err != nil {
		return store.EventFilter{}, err
	}
	t2, err := queries.ParseTopic(q.Get("topic2"))
	if err != nil {
		return store.EventFilter{}, err
	}
	t3, err := queries.ParseTopic(q.Get("topic3"))
	if err != nil {
		return store.EventFilter{}, err
	}
	tc, err := queries.ParseTopicContains(q.Get("topic_contains"))
	if err != nil {
		return store.EventFilter{}, err
	}
	fromLedger, err := queries.ParseLedgerParam(q.Get("from_ledger"))
	if err != nil {
		return store.EventFilter{}, err
	}
	toLedger, err := queries.ParseLedgerParam(q.Get("to_ledger"))
	if err != nil {
		return store.EventFilter{}, err
	}
	fromTime, err := queries.ParseTimeParam(q.Get("from_time"))
	if err != nil {
		return store.EventFilter{}, err
	}
	toTime, err := queries.ParseTimeParam(q.Get("to_time"))
	if err != nil {
		return store.EventFilter{}, err
	}

	args := queries.EventFilterArgs{
		ContractID:    q.Get("contract_id"),
		Types:         types,
		Topic:         topic,
		T0:            t0,
		T1:            t1,
		T2:            t2,
		T3:            t3,
		TopicContains: tc,
		TxHash:        q.Get("tx_hash"),
		FromLedger:    fromLedger,
		ToLedger:      toLedger,
		FromTime:      fromTime,
		ToTime:        toTime,
		Order:         q.Get("order"),
		OrderBy:       q.Get("order_by"),
		Cursor:        q.Get("cursor"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return store.EventFilter{}, fmt.Errorf("invalid limit %q", raw)
		}
		args.Limit = n
	}
	return queries.BuildEventFilter(args)
}

// TestResolvers_FilterParityWithREST is the guard for the divergence issue
// #566 calls out: for the same logical filter the GraphQL resolver must hand
// the store exactly the store.EventFilter the REST /events path builds. Each
// case is built twice — once the way REST parses it, once the way GraphQL
// does — and compared both at the builder boundary and at the store call the
// resolver actually makes.
func TestResolvers_FilterParityWithREST(t *testing.T) {
	fromLedger := int64(10)
	toLedger := int64(20)
	fromTime := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	toTime := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		restQuery string
		gqlArgs   EventFilterArgs
	}{
		{
			name:      "contract id",
			restQuery: "contract_id=" + knownContractID,
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{ContractID: knownContractID}},
		},
		{
			name:      "event types",
			restQuery: "type=contract,system",
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{Types: []string{"contract", "system"}}},
		},
		{
			name:      "any-position topic",
			restQuery: "topic=transfer",
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{Topic: json.RawMessage(`"transfer"`)}},
		},
		{
			name:      "positional topic",
			restQuery: url.Values{"topic1": {`"abc"`}}.Encode(),
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{Topics: &TopicPositionInput{T1: json.RawMessage(`"abc"`)}}},
		},
		{
			name:      "topic contains",
			restQuery: url.Values{"topic_contains": {`["x"]`}}.Encode(),
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{TopicContains: json.RawMessage(`["x"]`)}},
		},
		{
			name:      "transaction hash",
			restQuery: "tx_hash=deadbeef",
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{TxHash: "deadbeef"}},
		},
		{
			name:      "ledger range",
			restQuery: "from_ledger=10&to_ledger=20",
			gqlArgs:   EventFilterArgs{Filter: &FilterInput{FromLedger: &fromLedger, ToLedger: &toLedger}},
		},
		{
			name: "time range",
			restQuery: url.Values{
				"from_time": {"2026-07-01T00:00:00Z"},
				"to_time":   {"2026-07-02T00:00:00Z"},
			}.Encode(),
			gqlArgs: EventFilterArgs{Filter: &FilterInput{FromTime: &fromTime, ToTime: &toTime}},
		},
		{
			name:      "order, order_by and page size",
			restQuery: "order=desc&order_by=ledger&limit=25",
			gqlArgs:   EventFilterArgs{Page: &PageInput{Order: "desc", OrderBy: "ledger", First: ptrInt32(25)}},
		},
		{
			name:      "pagination omitted falls back to the shared default",
			restQuery: "",
			gqlArgs:   EventFilterArgs{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := restEventFilter(tt.restQuery)
			require.NoError(t, err)

			got, _, _, _, err := buildEventFilter(tt.gqlArgs)
			require.NoError(t, err)
			assert.Equal(t, want, got, "buildEventFilter must produce the REST-equivalent filter")

			// The resolver must send that same filter to the store: the
			// assertion above proves the translation, this one proves the
			// translation is what actually reaches storage.
			st := &stubStore{}
			_, err = (&Resolver{store: st}).resolveEvents(context.Background(), tt.gqlArgs)
			require.NoError(t, err)
			assert.Equal(t, want, st.lastFilter, "the resolver must query the store with the REST-equivalent filter")
		})
	}
}

// TestDecodeCursor_TableRejections pins the cursor contract Relay clients
// depend on: a valid cursor round-trips, and each malformed shape is rejected
// with a message that names what was wrong rather than panicking.
func TestDecodeCursor_TableRejections(t *testing.T) {
	valid := EncodeCursor("0000000001-0000000001", "id", "desc")
	missingID := base64.StdEncoding.EncodeToString([]byte(`{"order_by":"id","order":"asc"}`))
	notJSON := base64.StdEncoding.EncodeToString([]byte("hello world"))

	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "empty cursor is a no-op", in: "", wantErr: ""},
		{name: "valid cursor round-trips", in: valid, wantErr: ""},
		{name: "not base64 is rejected", in: "%%%not-base64%%%", wantErr: "not base64"},
		{name: "base64 that is not JSON is rejected", in: notJSON, wantErr: "not JSON"},
		{name: "JSON without an id is rejected", in: missingID, wantErr: "missing id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := DecodeCursor(tt.in)
			if tt.wantErr == "" {
				require.NoError(t, err)
				if tt.in == valid {
					assert.Equal(t, "0000000001-0000000001", p.LastID)
					assert.Equal(t, "id", p.OrderBy)
					assert.Equal(t, "desc", p.Order)
				}
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestResolvers_RejectMalformedCursor checks a bad `after:` is surfaced as a
// GraphQL error on both connection resolvers and that the store is never
// queried with an undecodable cursor.
func TestResolvers_RejectMalformedCursor(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "events", query: `{ events(page: {after: "not-base64!!"}) { totalCount } }`},
		{name: "contracts", query: `{ contracts(after: "not-base64!!") { totalCount } }`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &stubStore{}
			h := newGraphQLTestServer(t, st)

			resp := postQuery(t, h, tt.query)

			errs := errorsOf(t, resp)
			require.NotEmpty(t, errs, "a malformed cursor must surface as a GraphQL error")
			assert.Contains(t, errs[0].(map[string]any)["message"], "cursor")
			assert.Zero(t, st.lastFilter, "the store must not be queried with an undecodable cursor")
		})
	}
}

// nestedFieldQuery builds a query whose nesting is `levels` deep plus a leaf
// selection, i.e. an AST depth of levels+2 (the root selection set counts as
// one, the leaf as another).
func nestedFieldQuery(levels int) string {
	// Deliberately parameterised by `levels` rather than by the depth cap so
	// the depth cases below can name absolute depths; a case written as
	// `DepthLimit - 2` would silently follow the cap if it were raised.

	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&b, " f%d {", i)
	}
	b.WriteString(" id ")
	for i := 0; i < levels; i++ {
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

// TestCheckDepth_TableBoundaries exercises the depth guard at and past the cap
// through the real parser. The cases are absolute (depth 4, 10, 11) rather
// than expressed relative to DepthLimit, so raising the cap is itself caught
// as a regression instead of moving the goalposts with the test.
func TestCheckDepth_TableBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		levels    int
		wantDepth int
		wantErr   bool
	}{
		{name: "well under the cap passes", levels: 2, wantDepth: 4, wantErr: false},
		{name: "exactly at the cap passes", levels: 8, wantDepth: 10, wantErr: false},
		{name: "one past the cap is rejected", levels: 9, wantDepth: 11, wantErr: true},
		{name: "far past the cap is rejected", levels: 20, wantDepth: 22, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := parseOp(t, nestedFieldQuery(tt.levels))

			// Pin the query's actual depth so the case names stay honest: if
			// nestedFieldQuery or the depth walker changes, the boundary cases
			// are re-checked rather than silently testing something else.
			require.Equal(t, tt.wantDepth, depth(op.SelectionSet, 1))

			err := CheckDepth(op)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "depth")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestExecutor_RejectsOverLargeQueries is the end-to-end companion: an
// over-deep or over-wide operation must be refused through the GraphQL error
// envelope before any resolver runs, not executed and not panicked.
func TestExecutor_RejectsOverLargeQueries(t *testing.T) {
	var wide strings.Builder
	wide.WriteString("{")
	for i := 0; i < 51; i++ {
		wide.WriteString(" events { totalCount }")
	}
	wide.WriteString(" }")

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "over-deep query is rejected", query: nestedFieldQuery(9), want: "depth"},
		{name: "over-wide query is rejected", query: wide.String(), want: "complexity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &stubStore{}
			h := newGraphQLTestServer(t, st)

			resp := postQuery(t, h, tt.query)

			errs := errorsOf(t, resp)
			require.NotEmpty(t, errs)
			assert.Contains(t, errs[0].(map[string]any)["message"], tt.want)
			assert.Zero(t, st.lastFilter, "an over-large query must be rejected before hitting the store")
		})
	}
}

// TestResolvers_OmittedNullableArguments drives the wire path with the
// nullable filter/page arguments omitted, which is how most real clients call
// these fields. Omitted optionals must fall back to the shared defaults;
// omitted *required* arguments must still come back as GraphQL errors.
func TestResolvers_OmittedNullableArguments(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
		wantMsg string
	}{
		{name: "events with no arguments", query: `{ events { totalCount } }`},
		{name: "events with an empty filter object", query: `{ events(filter: {}) { totalCount } }`},
		{name: "events with a filter of nulls", query: `{ events(filter: {contractId: null, types: null}) { totalCount } }`},
		{name: "events with an empty page object", query: `{ events(page: {}) { totalCount } }`},
		{name: "tokenEvents with no arguments", query: `{ tokenEvents { totalCount } }`},
		{name: "contracts with no arguments", query: `{ contracts { totalCount } }`},
		{name: "event missing its required id", query: `{ event { id } }`, wantErr: true, wantMsg: "event id is required"},
		{name: "contract missing its required id", query: `{ contract { contractId } }`, wantErr: true, wantMsg: "contract id is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &stubStore{}
			h := newGraphQLTestServer(t, st)

			resp := postQuery(t, h, tt.query)

			errs := errorsOf(t, resp)
			if tt.wantErr {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs[0].(map[string]any)["message"], tt.wantMsg)
				assert.Zero(t, st.lastFilter, "a rejected request must not reach the store")
				return
			}
			require.Empty(t, errs, "unexpected errors: %v", errs)
			require.NotNil(t, resp["data"])
			if strings.Contains(tt.query, "events") || strings.Contains(tt.query, "tokenEvents") {
				assert.Equal(t, store.DefaultQueryLimit, st.lastFilter.Limit,
					"an omitted page must use the shared default page size")
			}
		})
	}
}

// TestResolvers_ErrorsAreReturnedNotPanicked covers the failure modes a
// misconfigured handler or bad arguments can reach. Each returns an error the
// executor can wrap in the GraphQL envelope; none of them panics.
func TestResolvers_ErrorsAreReturnedNotPanicked(t *testing.T) {
	ctx := context.Background()
	tooBig := ptrInt32(int32(store.MaxQueryLimit + 1))

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "events with an unwired store",
			call: func() error { _, err := (&Resolver{}).resolveEvents(ctx, EventFilterArgs{}); return err },
		},
		{
			name: "tokenEvents with an unwired store",
			call: func() error { _, err := (&Resolver{}).resolveTokenEvents(ctx, EventFilterArgs{}); return err },
		},
		{
			name: "contracts with an unwired store",
			call: func() error { _, err := (&Resolver{}).resolveContracts(ctx, PageInput{}); return err },
		},
		{
			name: "event with an unwired store",
			call: func() error { _, err := (&Resolver{}).resolveEvent(ctx, "id"); return err },
		},
		{
			name: "contract with an unwired store",
			call: func() error { _, err := (&Resolver{}).resolveContract(ctx, "CAAA"); return err },
		},
		{
			name: "event with an empty id",
			call: func() error { _, err := (&Resolver{store: &stubStore{}}).resolveEvent(ctx, ""); return err },
		},
		{
			name: "contract with an empty id",
			call: func() error { _, err := (&Resolver{store: &stubStore{}}).resolveContract(ctx, ""); return err },
		},
		{
			name: "events with backward pagination",
			call: func() error {
				_, err := (&Resolver{store: &stubStore{}}).resolveEvents(ctx, EventFilterArgs{Page: &PageInput{Last: ptrInt32(5)}})
				return err
			},
		},
		{
			name: "events with first above the page cap",
			call: func() error {
				_, err := (&Resolver{store: &stubStore{}}).resolveEvents(ctx, EventFilterArgs{Page: &PageInput{First: tooBig}})
				return err
			},
		},
		{
			name: "contracts with first above the page cap",
			call: func() error {
				_, err := (&Resolver{store: &stubStore{}}).resolveContracts(ctx, PageInput{First: tooBig})
				return err
			},
		},
		{
			name: "events with a malformed cursor",
			call: func() error {
				_, err := (&Resolver{store: &stubStore{}}).resolveEvents(ctx, EventFilterArgs{Page: &PageInput{After: "!!!"}})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() { err = tt.call() })
			require.Error(t, err)
			assert.NotEmpty(t, err.Error())
		})
	}
}

// TestPlayground_ServesAndPointsAtGraphQL pins the two things the playground
// has to get right: it serves HTML when enabled, and the page targets this
// transport's /graphql endpoint rather than the upstream default.
func TestPlayground_ServesAndPointsAtGraphQL(t *testing.T) {
	h, err := New(api.ServerDeps{Store: &stubStore{}}, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.PlaygroundHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/graphiql", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "SoroTrail GraphQL Playground")
	assert.Contains(t, body, "endpoint: '/graphql'", "the playground must target this server's endpoint")
	assert.Contains(t, body, "graphql-playground-react")
}

// TestContractsResolver_CursorPagination covers the contracts connection's
// own pagination, which shares the cursor envelope with events but has its
// own resume-by-contract-id logic.
func TestContractsResolver_CursorPagination(t *testing.T) {
	rows := []store.WatchedContract{
		{ContractID: "CAAAA"},
		{ContractID: "CBBBB"},
		{ContractID: "CCCCC"},
	}
	afterSecond := EncodeCursor("CBBBB", "", "")
	unknown := EncodeCursor("CZZZZ", "", "")

	tests := []struct {
		name        string
		page        string
		wantIDs     []string
		wantHasNext bool
	}{
		{name: "no page returns every watched contract", page: "", wantIDs: []string{"CAAAA", "CBBBB", "CCCCC"}},
		{name: "first truncates and advertises the next page", page: "(first: 2)", wantIDs: []string{"CAAAA", "CBBBB"}, wantHasNext: true},
		{name: "after resumes past the matching row", page: fmt.Sprintf("(after: %q)", afterSecond), wantIDs: []string{"CCCCC"}},
		{name: "an unknown cursor yields an empty page", page: fmt.Sprintf("(after: %q)", unknown), wantIDs: []string{}},
		{name: "first below one falls back to the default page size", page: "(first: 0)", wantIDs: []string{"CAAAA", "CBBBB", "CCCCC"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGraphQLTestServer(t, &stubStore{watchedList: rows})

			resp := postQuery(t, h, fmt.Sprintf(
				`{ contracts%s { nodes { contractId } pageInfo { hasNextPage endCursor } totalCount } }`, tt.page))

			require.Empty(t, errorsOf(t, resp))
			conn := resp["data"].(map[string]any)["contracts"].(map[string]any)
			nodes := conn["nodes"].([]any)
			got := make([]string, 0, len(nodes))
			for _, n := range nodes {
				got = append(got, n.(map[string]any)["contractId"].(string))
			}
			assert.Equal(t, tt.wantIDs, got)
			assert.Equal(t, float64(len(rows)), conn["totalCount"], "totalCount counts the whole watch list, not the page")
			assert.Equal(t, tt.wantHasNext, conn["pageInfo"].(map[string]any)["hasNextPage"])
		})
	}
}

// TestContractResolver_Lookup covers both halves of the single-contract
// resolver: a watched contract comes back as an object, an unwatched one as
// null rather than an error.
func TestContractResolver_Lookup(t *testing.T) {
	h := newGraphQLTestServer(t, &stubStore{watchedList: []store.WatchedContract{
		{ContractID: "CAAAA"},
		{ContractID: "CBBBB"},
	}})

	found := postQuery(t, h, `{ contract(id: "CAAAA") { contractId } }`)
	require.Empty(t, errorsOf(t, found))
	require.NotNil(t, found["data"].(map[string]any)["contract"])
	assert.Equal(t, "CAAAA", found["data"].(map[string]any)["contract"].(map[string]any)["contractId"])

	missing := postQuery(t, h, `{ contract(id: "CZZZZ") { contractId } }`)
	require.Empty(t, errorsOf(t, missing))
	assert.Nil(t, missing["data"].(map[string]any)["contract"], "an unwatched contract serializes as null")
}

// stubEnricher satisfies api.Enricher so tokenEvents can be driven with a
// controlled decode result, including the short-result case the resolver has
// to degrade gracefully.
type stubEnricher struct {
	out []store.EnrichedEvent
}

func (s stubEnricher) EnrichEvents(context.Context, []store.Event) []store.EnrichedEvent {
	return s.out
}

func (s stubEnricher) DecodeStats() store.DecodeStats { return store.DecodeStats{} }

// TestTokenEventsResolver_DecodedShape verifies the enriched connection
// mirrors the REST `?decoded=true` shape for all three enricher outcomes: a
// decoded page, a nil enricher, and an enricher that returns fewer rows than
// it was given.
func TestTokenEventsResolver_DecodedShape(t *testing.T) {
	ev := store.Event{ID: "e1", ContractID: "CAAAA", Ledger: 1, Type: "contract"}
	decoded := store.DecodedEventResponse{Event: "transfer", Fields: map[string]any{"amount": "100"}}

	tests := []struct {
		name        string
		enricher    api.Enricher
		wantDecoded bool
		wantName    string
	}{
		{name: "a nil enricher leaves every row undecoded", enricher: nil, wantDecoded: false},
		{
			name:        "a decoded row is surfaced on the node",
			enricher:    stubEnricher{out: []store.EnrichedEvent{{Event: ev, Decoded: true, DecodedEvent: &decoded}}},
			wantDecoded: true,
			wantName:    "transfer",
		},
		{name: "a short enricher result degrades to undecoded rows", enricher: stubEnricher{out: nil}, wantDecoded: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &stubStore{events: []store.Event{ev}}
			h, err := New(api.ServerDeps{Store: st, Enricher: tt.enricher},
				slog.New(slog.NewTextHandler(io.Discard, nil)), false)
			require.NoError(t, err)

			resp := postQuery(t, h, `{ tokenEvents { nodes { id decoded decodedEvent { name } } } }`)

			require.Empty(t, errorsOf(t, resp))
			conn := resp["data"].(map[string]any)["tokenEvents"].(map[string]any)
			nodes := conn["nodes"].([]any)
			require.Len(t, nodes, 1)
			node := nodes[0].(map[string]any)
			assert.Equal(t, "e1", node["id"])
			assert.Equal(t, tt.wantDecoded, node["decoded"])
			if tt.wantName != "" {
				require.NotNil(t, node["decodedEvent"])
				assert.Equal(t, tt.wantName, node["decodedEvent"].(map[string]any)["name"])
			}
		})
	}
}
