package meta

// The RPC-simulation helpers this package once shipped were removed when the
// worker was simplified (see issue #808); what remains are the resolution
// entry points and their cache contract. The main table in worker_test.go
// covers the happy paths; the tests here pin the failure-side behaviour that
// keeps the cache honest: errors must surface instead of coming back as zero
// values, and a failed fetch must never seed the store — not even with
// IsToken=false, which would masquerade as a legitimate negative-cache entry.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errRPC is a mockRPCClient that always fails, standing in for an RPC
// outage; calls counts how often the contract was actually fetched.
type errRPC struct {
	calls int
}

func (e *errRPC) GetMetadata(context.Context, string) (Metadata, error) {
	e.calls++
	return Metadata{}, errors.New("rpc outage")
}

// A failing fetch must leave no trace in the store. Writing a zero-value
// entry here would poison the cache with an IsToken=false row — indistinguishable
// from a real negative-cache answer for a non-token contract — so the next
// resolution must reach the RPC again.
func TestResolveMetadata_FailedFetchWritesNoCacheEntry(t *testing.T) {
	store := newMockMetadataStore()
	rpc := &errRPC{}

	got, err := ResolveMetadata(context.Background(), rpc, store, "C_NEVER")
	require.Error(t, err, "the rpc failure must surface, not come back as zero values")
	assert.Equal(t, Metadata{}, got)

	_, ok := store.Get("C_NEVER")
	assert.False(t, ok, "a failed fetch must not seed the cache, not even as a negative entry")
}

// ResolveMetadataWithFallbacks serves the stored entry when the RPC fails;
// when the RPC succeeds it must still write the fresh result, or a stale
// entry would shadow a corrected answer forever.
func TestResolveMetadataWithFallbacks_RefreshesTheStoreOnSuccess(t *testing.T) {
	store := newMockMetadataStore()
	stale := Metadata{Name: "OldName", Symbol: "OLD", Decimals: 2, IsToken: true}
	store.Put("C_REFRESH", stale, time.Now())

	fresh := Metadata{Name: "NewName", Symbol: "NEW", Decimals: 6, IsToken: true}
	rpc := &mockRPCClient{fn: func(context.Context, string) (Metadata, error) {
		return fresh, nil
	}}

	got, err := ResolveMetadataWithFallbacks(context.Background(), rpc, store, "C_REFRESH")
	require.NoError(t, err)
	assert.Equal(t, fresh, got, "a successful fetch returns the fresh metadata, not the stale entry")

	stored, ok := store.Get("C_REFRESH")
	assert.True(t, ok)
	assert.Equal(t, fresh, stored, "the fresh result replaces the stale entry in the store")
}

// Single-flight must keep the store honest on failure: the error is surfaced
// to the caller and no entry is written, so the failed lookup is not
// mistaken for a resolved non-token contract.
//
// Note that the once-guarded item does memoize the error for callers arriving
// while the item is still resident; a failed resolution therefore is not
// retried until the item is evicted. That lifetime choice is flagged on the
// issue rather than asserted here, because pinning it as "correct" would
// bake in a policy the maintainers may want to change.
func TestResolveMetadataCachedSingleFlight_FailedFetchWritesNoCacheEntry(t *testing.T) {
	store := newMockMetadataStore()

	rpc := &errRPC{}
	got, err := ResolveMetadataCachedSingleFlight(context.Background(), rpc, store, "C_OUTAGE_SF")
	require.Error(t, err, "the rpc failure must surface to the caller")
	assert.Equal(t, Metadata{}, got)

	_, ok := store.Get("C_OUTAGE_SF")
	assert.False(t, ok, "a failed fetch must not seed the cache")
	assert.Equal(t, 1, rpc.calls, "the first resolution reaches the rpc exactly once")
}
