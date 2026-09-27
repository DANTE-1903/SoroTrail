//go:build integration

package ingester_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/api"
	"github.com/sorotrail/sorotrail/internal/decode"
	"github.com/sorotrail/sorotrail/internal/ingester"
	"github.com/sorotrail/sorotrail/internal/rpc"
	"github.com/sorotrail/sorotrail/internal/store"
	"github.com/sorotrail/sorotrail/internal/testdb"
)

// TestDeadLetterReplay_EndToEnd walks the operator workflow that follows a
// decode failure, end to end against a real Postgres store and the real API
// router:
//
//  1. the ingester dead-letters a poison event and keeps persisting the rest
//     of the page instead of aborting the cycle;
//  2. the quarantined event is retrievable through GET /dead-letters;
//  3. replaying the repaired event lands it in the events table exactly once;
//  4. replaying it again is a no-op;
//  5. deleting the row through DELETE /dead-letters/{id} empties the listing.
//
// The ingester — not the test — performs the dead-lettering: this test is
// about the seam between a failing decode and everything an operator does
// afterwards, so seeding the dead_letters table directly would skip the half
// most likely to regress.
func TestDeadLetterReplay_EndToEnd(t *testing.T) {
	pool := testdb.Setup(t, store.Migrate)
	st := store.NewPostgres(pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const (
		contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		txHash     = "deadbeef1234"
		poisonID   = "0000000100-0000000001"
		validID    = "0000000100-0000000002"
		apiKey     = "test-api-key"
	)

	// The poison event's topic payload is not valid JSON, so the ingester's
	// toStoreEvent fails on it. The valid event riding in the same page is
	// the control: if the cycle aborted on the poison event, it would be
	// missing from the events table.
	poison := rpc.Event{
		ID:         poisonID,
		Type:       "contract",
		Ledger:     100,
		ContractID: contractID,
		TxHash:     txHash,
		TopicJSON:  []json.RawMessage{json.RawMessage(`{`)},
	}
	valid := rpc.Event{
		ID:         validID,
		Type:       "contract",
		Ledger:     100,
		ContractID: contractID,
		TxHash:     txHash,
		TopicJSON:  []json.RawMessage{json.RawMessage(`{"symbol":"transfer"}`)},
		ValueJSON:  json.RawMessage(`{"u64":100}`),
	}

	client := &scriptedClient{
		health: rpc.Health{Status: "healthy", LatestLedger: 100, OldestLedger: 2},
		page:   rpc.GetEventsResponse{Events: []rpc.Event{poison, valid}, LatestLedger: 100},
	}
	ing := ingester.New(client, st, decode.XDRDecoder{}, logger, ingester.Options{
		StartLedger: 100,
		PageLimit:   1000,
	})
	// Reuse the production DeadLetterSink seam: the same *store.Postgres the
	// ingester dead-letters into is the one the API reads the row back from.
	ing.SetDeadLetterSink(st)

	_, err := ing.RunOnceForTest(ctx)
	require.NoError(t, err, "a poison event must not abort the ingestion cycle")

	// Wire the real API router so listing and deletion go through the same
	// handlers an operator would hit. The dead-letter routes sit behind
	// apiKeyAuth, which fails closed with 503 when no key is configured, so
	// the test must configure one and present it.
	handler := api.New(st, nil, logger, apiKey).Router()

	listDeadLetters := func(t *testing.T) []store.DeadLetter {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
		req.Header.Set("X-API-Key", apiKey)
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			DeadLetters []store.DeadLetter `json:"dead_letters"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.DeadLetters
	}

	t.Run("the poison event is dead-lettered and the rest of the page persists", func(t *testing.T) {
		_, err := st.GetEvent(ctx, validID, store.SystemScope())
		require.NoError(t, err, "the valid event on the poison page must still be stored")

		_, err = st.GetEvent(ctx, poisonID, store.SystemScope())
		assert.ErrorIs(t, err, store.ErrNotFound, "the poison event must not reach the events table")

		letters := listDeadLetters(t)
		require.Len(t, letters, 1)
		assert.Equal(t, poisonID, letters[0].EventID)
		assert.Equal(t, contractID, letters[0].ContractID)
		assert.Contains(t, letters[0].Error, "decoding event "+poisonID,
			"the row must carry the failure so the operator can triage it")
	})

	// The repaired event is what an operator upserts once the decoder (or the
	// payload) is fixed: replaying a dead letter is an idempotent re-ingest,
	// not a separate endpoint. The upsert's inserted count is therefore the
	// assertion that the replay landed exactly once.
	repaired := store.Event{
		ID:               poisonID,
		ContractID:       contractID,
		Ledger:           100,
		Type:             "contract",
		TxHash:           txHash,
		InSuccessfulCall: true,
		Topics:           json.RawMessage(`[{"symbol":"repaired"}]`),
		Value:            json.RawMessage(`{"u64":500}`),
		CreatedAt:        time.Now().UTC(),
		RawTopicXDR:      []string{"AAAADwAAAAh0cmFuc2Zlcg=="},
		RawValueXDR:      "AAAACgAAAAAAAAAE",
	}

	t.Run("replaying the repaired event is idempotent", func(t *testing.T) {
		cases := []struct {
			name       string
			wantInsert int64
		}{
			{name: "first replay lands the event", wantInsert: 1},
			{name: "second replay is a no-op", wantInsert: 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				inserted, err := st.UpsertEvents(ctx, []store.Event{repaired})
				require.NoError(t, err)
				assert.Equal(t, tc.wantInsert, inserted,
					"a replay must insert exactly once and then be a no-op")

				got, err := st.GetEvent(ctx, poisonID, store.SystemScope())
				require.NoError(t, err)
				assert.JSONEq(t, `[{"symbol":"repaired"}]`, string(got.Topics),
					"the stored row must be the repaired payload")
			})
		}
	})

	t.Run("deleting the dead letter is reflected in the list endpoint", func(t *testing.T) {
		letters := listDeadLetters(t)
		require.Len(t, letters, 1)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/dead-letters/%d", letters[0].ID), nil)
		req.Header.Set("X-API-Key", apiKey)
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code)

		assert.Empty(t, listDeadLetters(t), "a deleted dead letter must vanish from the listing")
	})
}

// scriptedClient is the minimal rpc.Client the dead-letter test needs: one
// scripted getEvents page plus a fixed health snapshot. The methods the
// ingest path never calls return zero values, mirroring the ingester's other
// test doubles.
type scriptedClient struct {
	health rpc.Health
	page   rpc.GetEventsResponse
}

func (c *scriptedClient) GetEvents(context.Context, rpc.GetEventsRequest) (rpc.GetEventsResponse, error) {
	return c.page, nil
}

func (c *scriptedClient) GetHealth(context.Context) (rpc.Health, error) {
	return c.health, nil
}

func (c *scriptedClient) GetLatestLedger(context.Context) (rpc.LatestLedger, error) {
	return rpc.LatestLedger{Sequence: c.health.LatestLedger}, nil
}

func (c *scriptedClient) GetLedgerEntries(context.Context, rpc.GetLedgerEntriesRequest) (rpc.GetLedgerEntriesResponse, error) {
	return rpc.GetLedgerEntriesResponse{}, nil
}

func (c *scriptedClient) SimulateTransaction(context.Context, rpc.SimulateTransactionRequest) (rpc.SimulateTransactionResponse, error) {
	return rpc.SimulateTransactionResponse{}, nil
}
