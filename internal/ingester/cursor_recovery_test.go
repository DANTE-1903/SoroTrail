package ingester

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
	"github.com/sorotrail/sorotrail/internal/store"
)

// A resume position can go bad in two ways: the RPC no longer accepts the
// cursor it was set from, or the ledger it points at has fallen below the
// RPC's retention window. reclampToOldest handles the second by advancing the
// frontier to just before the oldest retained ledger; discardCursor handles
// the first by dropping the cursor so the next cycle falls back to the
// ledger position. Both are data-loss adjacent and had no direct coverage.
// The tests below pin the frontier math, the gap that reclamping admits to in
// the log, and the guarantee that a failed RPC or store read leaves whatever
// was already persisted untouched.

func TestReclampToOldest(t *testing.T) {
	tests := []struct {
		name         string
		health       rpc.Health
		healthErr    error
		seed         *store.IngestionState
		wantErr      bool
		wantLedger   int64
		wantCursor   string
		wantLogParts []string
	}{
		{
			name:       "moves the frontier to the oldest retained ledger minus one",
			health:     rpc.Health{LatestLedger: 100_000, OldestLedger: 40_000},
			wantLedger: 39_999,
		},
		{
			name:       "logs the gap rather than passing over it silently",
			health:     rpc.Health{LatestLedger: 100_000, OldestLedger: 40_000},
			wantLedger: 39_999,
			wantLogParts: []string{
				"resume ledger fell outside RPC retention window",
				"oldest_retained",
				"events in the gap are lost",
			},
		},
		{
			name:       "an RPC failure leaves the stored state unchanged",
			healthErr:  errors.New("rpc unavailable"),
			seed:       &store.IngestionState{LastIngestedLedger: 777, LastCursor: "keep-me"},
			wantErr:    true,
			wantLedger: 777,
			wantCursor: "keep-me",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockRPC{health: tt.health, healthErr: tt.healthErr}
			st := newMockStore()
			st.state = tt.seed
			logger, buf := recordingLogger()
			ing := New(client, st, passthroughDecoder{}, logger, Options{Network: "testnet"})

			err := ing.reclampToOldest(context.Background(), 3_000)
			if tt.wantErr {
				require.Error(t, err, "a failed health lookup must surface, not silently reclamp")
			} else {
				require.NoError(t, err)
			}

			for _, part := range tt.wantLogParts {
				assert.Contains(t, buf.String(), part,
					"the operator must be able to see which events reclamping skipped")
			}

			state, gerr := st.GetIngestionState(context.Background())
			require.NoError(t, gerr)
			assert.Equal(t, tt.wantLedger, state.LastIngestedLedger,
				"the frontier must land on oldest_retained-1")
			assert.Equal(t, tt.wantCursor, state.LastCursor)
			if !tt.wantErr {
				assert.Equal(t, "testnet", state.Network,
					"the reclamped state belongs to this ingester's network")
			}
		})
	}
}

func TestDiscardCursor(t *testing.T) {
	tests := []struct {
		name       string
		seed       *store.IngestionState
		ingestErr  error
		wantLedger int64
		wantCursor string
	}{
		{
			name:       "clearing the cursor leaves the ledger position intact",
			seed:       &store.IngestionState{LastIngestedLedger: 500, LastCursor: "cursor-abc"},
			wantLedger: 500,
			wantCursor: "",
		},
		{
			name:       "an already-cursorless state is left untouched",
			seed:       &store.IngestionState{LastIngestedLedger: 500},
			wantLedger: 500,
			wantCursor: "",
		},
		{
			name:       "a state read failure leaves the stored state unchanged",
			seed:       &store.IngestionState{LastIngestedLedger: 500, LastCursor: "cursor-abc"},
			ingestErr:  errors.New("store unavailable"),
			wantLedger: 500,
			wantCursor: "cursor-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newMockStore()
			st.state = tt.seed
			st.ingestErr = tt.ingestErr
			ing := newTestIngester(&mockRPC{}, st, Options{})

			ing.discardCursor(context.Background())

			// Clear the injected read error so the persisted state can be
			// inspected through the same getter the helper uses.
			st.ingestErr = nil
			state, err := st.GetIngestionState(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantLedger, state.LastIngestedLedger,
				"discarding a cursor must never move the ledger frontier")
			assert.Equal(t, tt.wantCursor, state.LastCursor)
		})
	}
}

// TestDiscardCursor_NextCycleResumesFromLedgerPath proves the point of
// discarding: resolvePosition prefers a cursor when one is present, so the
// stored cursor is what decides between the cursor path and the ledger path.
// Once discarded, the next cycle must resume from last_ingested_ledger+1.
func TestDiscardCursor_NextCycleResumesFromLedgerPath(t *testing.T) {
	st := newMockStore()
	st.state = &store.IngestionState{LastIngestedLedger: 100, LastCursor: "cursor-abc"}
	ing := newTestIngester(&mockRPC{health: rpc.Health{LatestLedger: 500, OldestLedger: 10}}, st, Options{})

	// Before discarding, the cursor is the resume position.
	_, cursor, err := ing.resolvePosition(context.Background())
	require.NoError(t, err)
	require.Equal(t, "cursor-abc", cursor, "precondition: a stored cursor is preferred")

	ing.discardCursor(context.Background())

	startLedger, cursor, err := ing.resolvePosition(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint32(101), startLedger, "the next cycle resumes from last_ingested_ledger+1")
	assert.Empty(t, cursor, "the discarded cursor must not come back as the resume position")
}
