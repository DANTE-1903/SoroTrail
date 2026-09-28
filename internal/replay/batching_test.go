package replay

// Direct unit coverage for the two helpers at the heart of replay's batch
// loop. Run's orchestration (locking, resume, dry-run) has its own tests in
// replay_test.go; here the focus is decodeBatch and commit themselves, driven
// through the package's existing fakes so a regression in either fails
// loudly rather than hiding behind Run.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// currentEvent is an event whose stored decoding already matches what the
// improved decoder produces. The stored JSON may differ in key order and
// whitespace, the way Postgres-normalized jsonb does, without counting as a
// change — that difference is what keeps a replay from rewriting every row
// on every run.
func currentEvent(n int, ledger int64, storedTopics, storedValue string) store.DecodedEvent {
	e := seedEvent(n, ledger)
	e.Topics = json.RawMessage(storedTopics)
	e.Value = json.RawMessage(storedValue)
	return e
}

// seedEventWithValue is a seeded event whose raw value XDR is distinct, so a
// decoder can succeed on some rows and fail on others within the same batch.
func seedEventWithValue(n int, ledger int64, valueXDR string) store.DecodedEvent {
	e := seedEvent(n, ledger)
	e.RawValueXDR = valueXDR
	return e
}

// legacyDecoder decodes only the standard seed inputs; any other input
// fails, standing in for stored XDR the current decoder cannot handle.
func legacyDecoder() staticDecoder {
	return staticDecoder{out: map[string]string{
		"topic-xdr": `{"symbol":"transfer"}`,
		"value-xdr": `{"i128":"1000"}`,
	}}
}

// TestDecodeBatch walks the outcome matrix of a single batch: which rows end
// up as rewrites, and which counters the summary accumulates. The summary is
// what the operator sees, and the rewrites are what commit persists, so both
// must stay in agreement.
func TestDecodeBatch(t *testing.T) {
	tests := []struct {
		name    string
		events  []store.DecodedEvent
		decoder staticDecoder
		want    []store.EventDecoding
		wantSum Summary
	}{
		{
			name:    "a batch with changed decodings is rewritten",
			events:  []store.DecodedEvent{seedEvent(1, 100), seedEvent(2, 101)},
			decoder: improvedDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(1), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
				{ID: eventID(2), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 2, Changed: 2},
		},
		{
			name: "a batch with identical decodings is reported unchanged",
			events: []store.DecodedEvent{
				currentEvent(1, 100, `[{"symbol":"transfer"}]`, `{"i128":"1000"}`),
				currentEvent(2, 101, `[{"symbol":"transfer"}]`, `{"i128":"1000"}`),
			},
			decoder: improvedDecoder(),
			want:    []store.EventDecoding{},
			wantSum: Summary{Processed: 2},
		},
		{
			// Postgres normalizes jsonb, so the stored bytes rarely match a
			// freshly marshaled document even when the decoding is the same.
			// The comparison must be semantic, or every replay would rewrite
			// every row.
			name: "decodings equal up to jsonb normalization are unchanged",
			events: []store.DecodedEvent{
				currentEvent(1, 100, `[ { "symbol": "transfer" } ]`, `{ "i128": "1000" }`),
			},
			decoder: improvedDecoder(),
			want:    []store.EventDecoding{},
			wantSum: Summary{Processed: 1},
		},
		{
			// A batch normally contains both current and stale rows; only the
			// stale ones may be rewritten, so unchanged rows cost no writes
			// on a live database.
			name: "a mixed batch rewrites only the rows whose decoding differs",
			events: []store.DecodedEvent{
				seedEvent(1, 100),
				currentEvent(2, 101, `[{"symbol":"transfer"}]`, `{"i128":"1000"}`),
				seedEvent(3, 102),
			},
			decoder: improvedDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(1), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
				{ID: eventID(3), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 3, Changed: 2},
		},
		{
			// Only the topics improved for this row: the rewrite carries both
			// new columns even though just one of them changed.
			name: "a topics-only decoding change is still rewritten",
			events: []store.DecodedEvent{
				currentEvent(1, 100, `[{"unknown":{"type":"scvFoo"}}]`, `{"i128":"1000"}`),
			},
			decoder: improvedDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(1), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 1, Changed: 1},
		},
		{
			// Mirror case: only the value improved. Whichever half of the
			// comparison is dropped, one of these two rows sneaks through as
			// "unchanged" and keeps its stale column.
			name: "a value-only decoding change is still rewritten",
			events: []store.DecodedEvent{
				currentEvent(1, 100, `[{"symbol":"transfer"}]`, `{"unknown":{"type":"scvFoo"}}`),
			},
			decoder: improvedDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(1), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 1, Changed: 1},
		},
		{
			// One malformed row must not wedge a replay forever: it is
			// counted in Failed, its stored decoding is left alone, and the
			// rest of the batch still gets rewritten.
			name: "a decode failure within a batch is counted and skipped, not fatal",
			events: []store.DecodedEvent{
				seedEventWithValue(1, 100, "value-xdr"),
				seedEventWithValue(2, 101, "broken-xdr"),
				seedEventWithValue(3, 102, "value-xdr"),
			},
			decoder: legacyDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(1), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
				{ID: eventID(3), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 3, Changed: 2, Failed: 1},
		},
		{
			// Rows stored before raw XDR was retained have nothing to replay
			// from; they are expected, so they are counted as skipped rather
			// than failed and keep their stored decoding.
			name: "rows without raw XDR are skipped, not rewritten",
			events: func() []store.DecodedEvent {
				legacy := seedEvent(1, 100)
				legacy.RawTopicXDR, legacy.RawValueXDR = nil, ""
				return []store.DecodedEvent{legacy, seedEvent(2, 101)}
			}(),
			decoder: improvedDecoder(),
			want: []store.EventDecoding{
				{ID: eventID(2), Topics: json.RawMessage(`[{"symbol":"transfer"}]`), Value: json.RawMessage(`{"i128":"1000"}`)},
			},
			wantSum: Summary{Processed: 2, Changed: 1, Skipped: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore(tt.events...)
			r := newTestReplayer(st, tt.decoder, Options{FromLedger: 1, ToLedger: 1000})

			var sum Summary
			got := r.decodeBatch(tt.events, &sum)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSum, sum)
		})
	}
}

// decodeBatch is the read half of the pipeline: it decides what needs
// rewriting but must leave persistence to commit, so a crash between the two
// leaves the database consistent and the re-run deterministic.
func TestDecodeBatch_DoesNotTouchTheStore(t *testing.T) {
	events := []store.DecodedEvent{seedEvent(1, 100), seedEvent(2, 101)}
	st := newFakeStore(events...)
	before := st.snapshot()

	r := newTestReplayer(st, improvedDecoder(), Options{FromLedger: 1, ToLedger: 1000})

	var sum Summary
	rewrites := r.decodeBatch(events, &sum)

	require.NotEmpty(t, rewrites, "precondition: the batch does contain rewrites")
	assert.Equal(t, before, st.snapshot(), "decodeBatch must not rewrite any row")
	_, err := st.GetReplayState(context.Background())
	assert.ErrorIs(t, err, store.ErrNotFound, "decodeBatch must not persist progress either")
}

// driveReplayBatches feeds every batch in the replayer's range through the
// decodeBatch → commit cycle, the way Run's batch loop does, but without the
// orchestration (locking, resume, dry-run) that replay_test.go already
// covers. It returns the accumulated summary and how many rows were handed
// to commit as rewrites.
func driveReplayBatches(t *testing.T, r *Replayer, st *fakeStore) (Summary, int) {
	t.Helper()
	ctx := context.Background()

	var sum Summary
	rewrote := 0
	cursor := ""
	for {
		events, err := st.NextReplayBatch(ctx, r.opts.FromLedger, r.opts.ToLedger, cursor, r.opts.BatchSize)
		require.NoError(t, err)
		if len(events) == 0 {
			break
		}
		rewrites := r.decodeBatch(events, &sum)
		rewrote += len(rewrites)
		cursor = events[len(events)-1].ID
		require.NoError(t, r.commit(ctx, rewrites, cursor, sum, false))
	}
	require.NoError(t, r.commit(ctx, nil, cursor, sum, true))
	return sum, rewrote
}

// Idempotency, exercised directly on the helpers rather than through Run:
// replay is a pure function of the raw XDR, so a second pass over the same
// range with the same decoder must read every row again but rewrite none.
// Operators run replay more than once, so any drift here rewrites live rows
// for nothing.
func TestBatchingHelpers_SecondPassOverTheSameRangeChangesNothing(t *testing.T) {
	events := []store.DecodedEvent{seedEvent(1, 100), seedEvent(2, 101), seedEvent(3, 102)}
	st := newFakeStore(events...)
	r := newTestReplayer(st, improvedDecoder(), Options{FromLedger: 1, ToLedger: 1000, BatchSize: 2})

	_, firstRewrites := driveReplayBatches(t, r, st)
	afterFirst := st.snapshot()

	secondSum, secondRewrites := driveReplayBatches(t, r, st)

	assert.NotEmpty(t, firstRewrites, "precondition: the first pass had rows to rewrite")
	assert.Empty(t, secondRewrites, "the second pass must hand commit no rewrites")
	assert.Zero(t, secondSum.Changed)
	assert.EqualValues(t, len(events), secondSum.Processed, "the second pass still reads and counts every row")
	assert.Zero(t, secondSum.Failed)
	assert.Equal(t, afterFirst, st.snapshot(), "the second pass leaves every stored row untouched")
}

// Progress is persisted with each batch, in the batch's own transaction, so
// an interrupted run loses at most the batch it was inside — never the rows
// it already rewrote, and never its position. These assertions walk the
// exact state the store is left in after each commit.
func TestCommit_PersistsProgressPerBatch(t *testing.T) {
	ctx := context.Background()
	// The first row has no raw XDR (an expected legacy row) and the last is
	// left decodable, so the batch exercises the full counter set that commit
	// persists — a progress row that dropped a counter would resume an
	// interrupted run with silently wrong totals.
	legacy := seedEvent(1, 100)
	legacy.RawTopicXDR, legacy.RawValueXDR = nil, ""
	broken := seedEvent(2, 101)
	broken.RawValueXDR = "broken-xdr"
	st := newFakeStore(
		legacy, broken, seedEvent(3, 102), seedEvent(4, 103),
	)
	r := newTestReplayer(st, improvedDecoder(), Options{FromLedger: 7, ToLedger: 900, BatchSize: 2})

	// First batch commits: its cursor, counters, and ledger bounds must
	// already be durable, because a crash right now is exactly the case
	// progress persistence exists for.
	batch, err := st.NextReplayBatch(ctx, 7, 900, "", r.opts.BatchSize)
	require.NoError(t, err)
	require.Len(t, batch, 2)

	var sum Summary
	rewrites := r.decodeBatch(batch, &sum)
	cursor := batch[len(batch)-1].ID
	require.NoError(t, r.commit(ctx, rewrites, cursor, sum, false))

	state, err := st.GetReplayState(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 7, state.FromLedger, "the progress row records the range it belongs to")
	assert.EqualValues(t, 900, state.ToLedger)
	assert.Equal(t, eventID(2), state.LastEventID, "the progress row points at the last committed batch")
	assert.EqualValues(t, 2, state.Processed)
	assert.Zero(t, state.Changed, "a batch with nothing to rewrite still commits its progress")
	assert.EqualValues(t, 1, state.Skipped, "skipped rows are persisted too, so resumed totals stay truthful")
	assert.False(t, state.Done(), "an in-flight run must not be marked complete")
	assert.Equal(t, `[{"unknown":{"type":"scvFoo"}}]|{"unknown":{"type":"scvFoo"}}`, st.snapshot()[eventID(2)],
		"a failed row keeps its stored decoding after the commit")
	assert.Equal(t, `[{"unknown":{"type":"scvFoo"}}]|{"unknown":{"type":"scvFoo"}}`, st.snapshot()[eventID(4)],
		"rows in the uncommitted batch keep their old decoding")

	// The run resumes and commits the second batch: the cursor advances and
	// the counters accumulate rather than reset, or the persisted totals
	// would drift from reality.
	batch2, err := st.NextReplayBatch(ctx, 7, 900, cursor, r.opts.BatchSize)
	require.NoError(t, err)
	require.Len(t, batch2, 2)

	rewrites2 := r.decodeBatch(batch2, &sum)
	cursor2 := batch2[len(batch2)-1].ID
	require.NoError(t, r.commit(ctx, rewrites2, cursor2, sum, false))

	state2, err := st.GetReplayState(ctx)
	require.NoError(t, err)
	assert.Equal(t, eventID(4), state2.LastEventID)
	assert.EqualValues(t, 4, state2.Processed)
	assert.EqualValues(t, 2, state2.Changed, "counters accumulate across batches instead of resetting")
	assert.EqualValues(t, 1, state2.Skipped)
	assert.EqualValues(t, 1, sum.Failed, "the decode failure stayed in the summary without blocking later rows")
	assert.Equal(t, `[{"symbol":"transfer"}]|{"i128":"1000"}`, st.snapshot()[eventID(3)],
		"the second batch's rows carry the new decoding after its commit")

	// The final marker flags the range complete without moving the cursor,
	// so a re-run of the same range still resolves to the same end state.
	require.NoError(t, r.commit(ctx, nil, cursor2, sum, true))
	state3, err := st.GetReplayState(ctx)
	require.NoError(t, err)
	assert.True(t, state3.Done())
	assert.Equal(t, eventID(4), state3.LastEventID, "the completion marker must not move the cursor")
}
