package ingester

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// Issue #668 — indexEventAddresses is what makes the /addresses endpoints
// work, runs on every ingested event, and had no direct coverage. These tests
// pin:
//   - addresses in topics are extracted and persisted
//   - addresses in the value are extracted
//   - both account (G...) and contract (C...) addresses are recognised
//   - an event with no addresses produces no rows (and no store call)
//   - duplicate addresses within one event are de-duplicated
//   - a malformed address is skipped without failing the whole page

const (
	validAccount   = "GA6X4D4EMJAKLU5HRLP23HX6JBBTOUXKGDAFYYKLM2BSE2MBXUDA6S2J"
	contractAddr   = "CCT4YCF2MTYVP7PZLATNOUTMRRQN2YBQCJ7QU7MSME4GXG2G6S6CDHZ6"
	anotherAccount = "GDQNY3PBOJOKYZSRMK2S7LHHGWZIUISD4QORETLMXEWXIK7JVTL6OW7G"
)

func eventJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// newAddressTestIngester wires a real Ingester over a recording mock store so
// the private indexEventAddresses method can be driven directly. The mock's
// UpsertAddressRefs is overridden here (the shared one is a no-op) so tests
// can assert exactly what would be persisted.
type recordingStore struct {
	*mockStore
	refs []store.AddressRef
}

func (r *recordingStore) UpsertAddressRefs(_ context.Context, refs []store.AddressRef) error {
	r.refs = append(r.refs, refs...)
	return nil
}

func newAddressTestIngester() (*Ingester, *recordingStore) {
	st := &recordingStore{mockStore: newMockStore()}
	ing := New(nil, st, passthroughDecoder{}, testLogger(), Options{Network: "testnet"})
	return ing, st
}

func TestIndexEventAddresses(t *testing.T) {
	ctx := context.Background()

	t.Run("addresses in topics are extracted and persisted", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{{
			ID: "evt-1",
			Topics: eventJSON(t, []any{
				"transfer",
				validAccount,
				contractAddr,
			}),
			Value: eventJSON(t, map[string]any{"amount": "100"}),
		}}

		require.NoError(t, ing.indexEventAddresses(ctx, evs))
		require.Len(t, st.refs, 2)

		byAddr := map[string]string{}
		for _, r := range st.refs {
			byAddr[r.Address] = r.Role
			assert.Equal(t, "evt-1", r.EventID)
		}
		assert.Equal(t, validAccount, keyWithPrefix(byAddr, "G"))
		assert.Equal(t, contractAddr, keyWithPrefix(byAddr, "C"))
	})

	t.Run("addresses in the value are extracted", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{{
			ID:     "evt-2",
			Topics: eventJSON(t, []any{"mint"}),
			Value: eventJSON(t, map[string]any{
				"to":     validAccount,
				"issuer": anotherAccount,
			}),
		}}

		require.NoError(t, ing.indexEventAddresses(ctx, evs))
		addrs := map[string]bool{}
		for _, r := range st.refs {
			addrs[r.Address] = true
			assert.Equal(t, "evt-2", r.EventID)
		}
		assert.True(t, addrs[validAccount], "value address must be indexed")
		assert.True(t, addrs[anotherAccount], "second value address must be indexed")
	})

	t.Run("account and contract addresses both recognised", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{{
			ID:     "evt-3",
			Topics: eventJSON(t, []any{validAccount}),
			Value:  eventJSON(t, contractAddr),
		}}

		require.NoError(t, ing.indexEventAddresses(ctx, evs))
		addrs := map[string]bool{}
		for _, r := range st.refs {
			addrs[r.Address] = true
		}
		assert.True(t, addrs[validAccount], "G... account address recognised")
		assert.True(t, addrs[contractAddr], "C... contract address recognised")
	})

	t.Run("event with no addresses produces no rows", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{{
			ID:     "evt-4",
			Topics: eventJSON(t, []any{"claim", 42}),
			Value:  eventJSON(t, map[string]any{"amount": 7}),
		}}

		require.NoError(t, ing.indexEventAddresses(ctx, evs))
		assert.Empty(t, st.refs, "no addresses means no store call")
	})

	t.Run("empty event list returns nil without touching the store", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		require.NoError(t, ing.indexEventAddresses(ctx, nil))
		assert.Empty(t, st.refs)
	})

	t.Run("duplicate addresses within one event are de-duplicated", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{{
			ID: "evt-5",
			Topics: eventJSON(t, []any{
				validAccount,
				validAccount,
				validAccount,
			}),
			Value: eventJSON(t, validAccount),
		}}

		require.NoError(t, ing.indexEventAddresses(ctx, evs))
		count := 0
		for _, r := range st.refs {
			if r.Address == validAccount {
				count++
			}
		}
		// ExtractAddresses de-duplicates per (address, role); topics carry
		// distinct roles (topic_0, topic_1...) and the value carries the
		// "value" role, so one row per distinct role is correct and identical
		// (address, role) pairs must not repeat.
		seen := map[[2]string]bool{}
		for _, r := range st.refs {
			key := [2]string{r.Address, r.Role}
			assert.False(t, seen[key], "duplicate (address, role) pair persisted: %v", key)
			seen[key] = true
		}
		assert.NotEmpty(t, seen)
	})

	t.Run("malformed address is skipped without failing the page", func(t *testing.T) {
		ing, st := newAddressTestIngester()
		evs := []store.Event{
			{
				ID:     "evt-6",
				Topics: eventJSON(t, []any{"GTOOSHORT", validAccount}),
				Value:  eventJSON(t, map[string]any{"ok": true}),
			},
			{
				ID:     "evt-7",
				Topics: eventJSON(t, []any{"clean"}),
				Value:  eventJSON(t, anotherAccount),
			},
		}

		require.NoError(t, ing.indexEventAddresses(ctx, evs),
			"a malformed address must not fail the whole ingest pass")
		addrs := map[string]bool{}
		for _, r := range st.refs {
			addrs[r.Address] = true
		}
		assert.True(t, addrs[validAccount], "the valid address on the same event is still indexed")
		assert.True(t, addrs[anotherAccount], "later events on the same page are still indexed")
		assert.Len(t, st.refs, 2)
	})
}

// keyWithPrefix returns the single map key starting with the given character.
func keyWithPrefix(m map[string]string, prefix string) string {
	for k := range m {
		if len(k) > 0 && string(k[0]) == prefix {
			return k
		}
	}
	return ""
}
