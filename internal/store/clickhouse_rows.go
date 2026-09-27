package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The ClickHouse HTTP interface can return rows in several formats. The
// migration path (clickhouse_migrate.go) only ever needs a scalar, so it
// uses TabSeparated. Everything the Store methods read is multi-column, and
// for that JSONEachRow is the only format that carries the column names
// with the data — which lets a caller select columns in whatever order it
// likes and scan them by name instead of by position, the way the pgx
// implementations do.

// clickHouseRow is one result row decoded from JSONEachRow. Numbers are
// kept as json.Number (not float64) so a 64-bit id survives the round trip
// without losing precision; see queryRows.
type clickHouseRow map[string]any

// queryRows runs a statement and decodes its JSONEachRow output. An empty
// result set is an empty slice, never nil, so callers can range over it
// without a nil check.
func (e *clickhouseExecutor) queryRows(ctx context.Context, database, statement string) ([]clickHouseRow, error) {
	out, err := e.query(ctx, database, statement+" FORMAT JSONEachRow")
	if err != nil {
		return nil, err
	}
	rows := []clickHouseRow{}
	if out == "" {
		return rows, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row clickHouseRow
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			return nil, fmt.Errorf("decoding clickhouse row %q: %w", truncateForError(line), err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// insertRows writes rows with an INSERT ... FORMAT JSONEachRow body. The
// statement and its data travel in one request body because ClickHouse's
// HTTP interface takes a single body per request, with no separate
// parameter channel the way database/sql gives the other backends. Values
// therefore go through json.Marshal (which escapes them) rather than
// string interpolation, so this is still injection-safe, unlike the
// inline-quoted filter builders in the query path.
func (e *clickhouseExecutor) insertRows(ctx context.Context, database, table string, columns []string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(columns, ", "))
	b.WriteString(") FORMAT JSONEachRow")
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encoding %s row: %w", table, err)
		}
		b.WriteByte('\n')
		b.Write(encoded)
	}
	return e.exec(ctx, database, b.String())
}

// nextClickHouseID mints an id for the tables whose Postgres counterpart
// uses a bigserial. ClickHouse's HTTP interface has no RETURNING and a
// MergeTree has no auto-increment, so the writer assigns the id itself
// from the wall clock at nanosecond resolution, then keeps it strictly
// increasing within the process so two calls in the same nanosecond (or a
// non-monotonic clock step) cannot collide.
func nextClickHouseID() int64 {
	for {
		last := lastClickHouseID.Load()
		now := time.Now().UnixNano()
		if now <= last {
			now = last + 1
		}
		if lastClickHouseID.CompareAndSwap(last, now) {
			return now
		}
	}
}

var lastClickHouseID atomic.Int64

// clickHouseTimeLayouts are the shapes ClickHouse emits for DateTime64 over
// JSONEachRow, plus RFC3339 for any value that made a round trip through a
// caller-supplied timestamp. Tried in order because DateTime64(3) yields
// milliseconds while a hand-supplied value may carry none.
var clickHouseTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
}

// formatClickHouseTime renders a time the way ClickHouse's JSONEachRow
// reader expects a DateTime64: UTC, space-separated, millisecond
// precision (matching the schema's DateTime64(3)).
func formatClickHouseTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

func parseClickHouseTime(s string) (time.Time, error) {
	for _, layout := range clickHouseTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized clickhouse timestamp %q", s)
}

// --- typed accessors over a decoded row ---

func (r clickHouseRow) str(key string) string {
	if v, ok := r[key].(string); ok {
		return v
	}
	return ""
}

func (r clickHouseRow) int64(key string) int64 {
	switch v := r[key].(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// bool reads a ClickHouse UInt8 (0/1) as a Go bool.
func (r clickHouseRow) bool(key string) bool { return r.int64(key) != 0 }

// strSlice reads an Array(String). Nullable/absent arrays decode as nil and
// yield an empty (non-nil) slice so callers get one representation of
// "empty" rather than two.
func (r clickHouseRow) strSlice(key string) []string {
	raw, ok := r[key].([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// time reads a non-nullable DateTime64. A malformed value is treated as the
// zero time rather than an error: every read path here already tolerates a
// zero timestamp (that is what a freshly-created row without one carries),
// and failing a whole list because one row's date is odd would be worse.
func (r clickHouseRow) time(key string) time.Time {
	t, err := parseClickHouseTime(r.str(key))
	if err != nil {
		return time.Time{}
	}
	return t
}

// nullableTime reads a Nullable(DateTime64), returning the zero time for
// SQL NULL.
func (r clickHouseRow) nullableTime(key string) time.Time {
	if r[key] == nil {
		return time.Time{}
	}
	return r.time(key)
}

// --- row -> domain converters ---

// eventFromRow maps an events row back to the Event struct. topics is
// stored as an opaque JSON string (ClickHouse has no jsonb), so it is
// returned as the RawMessage the API serializes unchanged; value is
// Nullable and becomes a nil RawMessage when absent.
func eventFromRow(r clickHouseRow) Event {
	e := Event{
		ID:               r.str("id"),
		Network:          r.str("network"),
		ContractID:       r.str("contract_id"),
		Ledger:           r.int64("ledger"),
		Type:             r.str("type"),
		TxHash:           r.str("tx_hash"),
		TxIndex:          int32(r.int64("tx_index")),
		OpIndex:          int32(r.int64("op_index")),
		InSuccessfulCall: r.bool("in_successful_call"),
		CreatedAt:        r.time("created_at"),
		RawTopicXDR:      r.strSlice("raw_topic_xdr"),
		RawValueXDR:      r.str("raw_value_xdr"),
	}
	if topics := r.str("topics"); topics != "" {
		e.Topics = json.RawMessage(topics)
	}
	if r["value"] != nil {
		if value := r.str("value"); value != "" {
			e.Value = json.RawMessage(value)
		}
	}
	return e
}

func subscriptionFromRow(r clickHouseRow) (Subscription, error) {
	s := Subscription{
		ID:           r.int64("id"),
		URL:          r.str("url"),
		Secret:       r.str("secret"),
		Enabled:      r.bool("enabled"),
		FailureCount: int(r.int64("failure_count")),
		CreatedAt:    r.time("created_at"),
		TenantID:     r.nullableInt64Ptr("tenant_id"),
	}
	var filter SubscriptionFilter
	if err := json.Unmarshal([]byte(r.str("filters")), &filter); err != nil {
		return Subscription{}, fmt.Errorf("unmarshaling subscription filters: %w", err)
	}
	s.Filters = filter
	return s, nil
}

func (r clickHouseRow) nullableInt64Ptr(key string) *int64 {
	if r[key] == nil {
		return nil
	}
	v := r.int64(key)
	return &v
}

func deadLetterFromRow(r clickHouseRow) DeadLetter {
	return DeadLetter{
		ID:          r.int64("id"),
		EventID:     r.str("event_id"),
		ContractID:  r.str("contract_id"),
		Ledger:      r.int64("ledger"),
		Type:        r.str("type"),
		TxHash:      r.str("tx_hash"),
		TopicXDR:    r.strSlice("topic_xdr"),
		ValueXDR:    r.str("value_xdr"),
		Error:       r.str("error"),
		Attempts:    int(r.int64("attempts")),
		LastAttempt: r.time("last_attempt"),
		CreatedAt:   r.time("created_at"),
	}
}

func deliveryAttemptFromRow(r clickHouseRow) DeliveryAttempt {
	return DeliveryAttempt{
		ID:             r.int64("id"),
		SubscriptionID: r.int64("subscription_id"),
		EventID:        r.str("event_id"),
		Status:         r.str("status"),
		ResponseCode:   int(r.int64("response_code")),
		DurationMs:     int(r.int64("duration_ms")),
		Error:          r.str("error"),
		CreatedAt:      r.time("created_at"),
	}
}

func auditFindingFromRow(r clickHouseRow) AuditFinding {
	return AuditFinding{
		ID:              r.int64("id"),
		Network:         r.str("network"),
		FromLedger:      r.int64("from_ledger"),
		ToLedger:        r.int64("to_ledger"),
		ExpectedCount:   int(r.int64("expected_count")),
		ActualCount:     int(r.int64("actual_count")),
		MissingIDs:      r.strSlice("missing_ids"),
		Status:          r.str("status"),
		Attempts:        int(r.int64("attempts")),
		LastAttemptedAt: r.nullableTime("last_attempted_at"),
		LastError:       r.str("last_error"),
		CreatedAt:       r.time("created_at"),
	}
}

func contractCursorFromRow(r clickHouseRow) ContractCursor {
	return ContractCursor{
		ContractID:         r.str("contract_id"),
		LastIngestedLedger: r.int64("last_ingested_ledger"),
		LastCursor:         r.str("last_cursor"),
		UpdatedAt:          r.time("updated_at"),
	}
}

func watchedContractFromRow(r clickHouseRow) WatchedContract {
	return WatchedContract{
		ContractID: r.str("contract_id"),
		AddedAt:    r.time("added_at"),
	}
}

func ingestionStateFromRow(r clickHouseRow) IngestionState {
	return IngestionState{
		Network:            r.str("network"),
		LastIngestedLedger: r.int64("last_ingested_ledger"),
		LastCursor:         r.str("last_cursor"),
		LastSuccessfulPoll: r.nullableTimePtr("last_successful_poll"),
		UpdatedAt:          r.time("updated_at"),
	}
}

func (r clickHouseRow) nullableTimePtr(key string) *time.Time {
	if r[key] == nil {
		return nil
	}
	t := r.time(key)
	return &t
}

func auditStateFromRow(r clickHouseRow) AuditState {
	return AuditState{
		Network:               r.str("network"),
		VerifiedThroughLedger: r.int64("verified_through_ledger"),
		UpdatedAt:             r.time("updated_at"),
	}
}
