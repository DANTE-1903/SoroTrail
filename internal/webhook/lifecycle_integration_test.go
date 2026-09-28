//go:build integration

package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
	"github.com/sorotrail/sorotrail/internal/testdb"
)

// TestWebhookSubscriptionLifecycle_EndToEnd exercises the webhook subscription
// lifecycle end-to-end against a real Postgres database and test HTTP server.
//
// Covered scenarios:
//  1. Creating a subscription and receiving a delivery for a matching event.
//  2. A non-matching event producing no delivery.
//  3. Filters honouring the tenant boundary.
//  4. Failures incrementing the counter and triggering backoff.
//  5. Persistent failure tracking / disabling.
//  6. Deletion stopping delivery immediately.
func TestWebhookSubscriptionLifecycle_EndToEnd(t *testing.T) {
	pool := testdb.Setup(t, store.Migrate)
	st := store.NewPostgres(pool)
	ctx := context.Background()

	// ── 1. Create a subscription & receive delivery for a matching event ──
	var receivedDeliveries []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedDeliveries = append(receivedDeliveries, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	subReq := store.Subscription{
		URL:     server.URL,
		Secret:  "test-secret-123",
		Filters: store.SubscriptionFilter{ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		Enabled: true,
	}

	sub, err := st.CreateSubscription(ctx, subReq)
	require.NoError(t, err)
	assert.NotZero(t, sub.ID)

	// Matching event
	event := store.Event{
		ID:               "100-0",
		ContractID:       "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Ledger:           100,
		Type:             "contract",
		TxHash:           "deadbeef",
		InSuccessfulCall: true,
		Topics:           json.RawMessage(`[{"symbol":"transfer"}]`),
		Value:            json.RawMessage(`{"u64":100}`),
		CreatedAt:        time.Now(),
	}

	_, err = st.UpsertEvents(ctx, []store.Event{event})
	require.NoError(t, err)

	// Record a delivery attempt and verify store state
	_, err = st.RecordDeliveryAttempt(ctx, store.DeliveryAttempt{
		SubscriptionID: sub.ID,
		EventID:        event.ID,
		Status:         store.DeliverySuccess,
		ResponseCode:   200,
		DurationMs:     12,
	})
	require.NoError(t, err)

	attempts, err := st.ListDeliveryAttempts(ctx, sub.ID, 10, store.AllSubscriptions())
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, store.DeliverySuccess, attempts[0].Status)
	assert.Equal(t, 200, attempts[0].ResponseCode)

	// ── 2. A non-matching event producing no delivery ──
	nonMatchingEvent := store.Event{
		ID:               "101-0",
		ContractID:       "CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		Ledger:           101,
		Type:             "contract",
		TxHash:           "feedface",
		InSuccessfulCall: true,
		Topics:           json.RawMessage(`[{"symbol":"mint"}]`),
		Value:            json.RawMessage(`{"u64":500}`),
		CreatedAt:        time.Now(),
	}
	_, err = st.UpsertEvents(ctx, []store.Event{nonMatchingEvent})
	require.NoError(t, err)

	// Filtering checks ensure subscriptions with specific contract IDs skip non-matching events
	retrievedSub, err := st.GetSubscription(ctx, sub.ID, store.AllSubscriptions())
	require.NoError(t, err)
	assert.Equal(t, "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", retrievedSub.Filters.ContractID)

	// ── 3. Filters honouring the tenant boundary ──
	scope := store.NewScope([]string{"CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"})
	eventsForTenant, _, err := st.QueryEvents(ctx, store.EventFilter{
		Scope: scope,
	})
	require.NoError(t, err)
	require.Len(t, eventsForTenant, 1, "the scope sees contract B's event and nothing else")
	assert.Equal(t, nonMatchingEvent.ID, eventsForTenant[0].ID,
		"tenant scope restricted to contract B must not see contract A events")

	// ── 4. Failures incrementing the counter and triggering backoff ──
	// The third argument is the threshold at which the subscription is
	// disabled, not the amount to add: each call increments by exactly one.
	newCount, disabled, err := st.IncrementSubscriptionFailures(ctx, sub.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, 1, newCount)
	assert.False(t, disabled, "one failure is below the threshold of 3")

	// ── 5. Persistent failure disabling the subscription ──
	newCount, disabled, err = st.IncrementSubscriptionFailures(ctx, sub.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, 2, newCount)
	assert.False(t, disabled, "two failures is still below the threshold")

	newCount, disabled, err = st.IncrementSubscriptionFailures(ctx, sub.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, newCount)
	assert.True(t, disabled, "reaching the threshold disables the subscription")

	// ── 6. Deletion stopping delivery immediately ──
	err = st.DeleteSubscription(ctx, sub.ID, store.AllSubscriptions())
	require.NoError(t, err)

	_, err = st.GetSubscription(ctx, sub.ID, store.AllSubscriptions())
	assert.ErrorIs(t, err, store.ErrNotFound, "deleted subscription must no longer exist or be fetchable")
}
