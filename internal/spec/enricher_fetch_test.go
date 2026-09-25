package spec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
	"github.com/sorotrail/sorotrail/internal/store"
)

// buildMinimalWasmWithEmptySpecSection builds the smallest Wasm module that
// parseSpecFromWasm can walk: the magic header, version, and a single custom
// section named "contractspecv0" with no payload. An empty section is a
// legitimate encoding (a contract that publishes the section but declares no
// events), and it is deliberately minimal here because the fetch-dedup test
// below only needs fetchAndParseWasmSpec to succeed, not to parse anything
// non-trivial out of it.
func buildMinimalWasmWithEmptySpecSection(t *testing.T) []byte {
	t.Helper()
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d) // magic "\0asm"
	buf = append(buf, 0x01, 0x00, 0x00, 0x00) // version 1

	name := []byte(WasmCustomSectionName)
	var section []byte
	section = append(section, byte(len(name))) // LEB128 name length (fits in one byte)
	section = append(section, name...)
	// No content after the name: an empty spec section.

	buf = append(buf, 0x00)               // section ID 0 (custom)
	buf = append(buf, byte(len(section))) // LEB128 section size (fits in one byte)
	buf = append(buf, section...)
	return buf
}

// fetchCountingRPCClient answers GetLedgerEntries for both the contract
// instance (wasm_hash) lookup and the contract code lookup that
// Fetcher.FetchSpec makes, discriminating by the LedgerKey's type so it
// works regardless of call order. Every call is counted and, when delay is
// set, sleeps first so concurrent callers have time to pile up behind
// Enricher's singleflight group before the leader call returns.
type fetchCountingRPCClient struct {
	rpc.Client
	wasmHash  string
	wasmBytes []byte
	delay     time.Duration
	calls     atomic.Int64
}

func (c *fetchCountingRPCClient) GetLedgerEntries(ctx context.Context, req rpc.GetLedgerEntriesRequest) (rpc.GetLedgerEntriesResponse, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}

	var key xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(req.Keys[0], &key); err != nil {
		return rpc.GetLedgerEntriesResponse{}, err
	}

	switch key.Type {
	case xdr.LedgerEntryTypeContractCode:
		entry := xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeContractCode,
				ContractCode: &xdr.ContractCodeEntry{
					Hash: key.ContractCode.Hash,
					Code: c.wasmBytes,
				},
			},
		}
		xdrStr, err := xdr.MarshalBase64(entry)
		if err != nil {
			return rpc.GetLedgerEntriesResponse{}, err
		}
		return rpc.GetLedgerEntriesResponse{Entries: []rpc.LedgerEntryResult{{XDR: xdrStr}}}, nil

	default: // contract instance lookup
		wasmHashBytes, err := decodeWasmHash(c.wasmHash)
		if err != nil {
			return rpc.GetLedgerEntriesResponse{}, err
		}
		keySym := xdr.ScSymbol("key")
		entry := xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeContractData,
				ContractData: &xdr.ContractDataEntry{
					Contract:   key.ContractData.Contract,
					Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &keySym},
					Durability: xdr.ContractDataDurabilityPersistent,
					Val:        scvalMap([]xdr.ScMapEntry{{Key: scvalSymbol("wasm_hash"), Val: scvalBytes(wasmHashBytes)}}),
				},
			},
		}
		xdrStr, err := xdr.MarshalBase64(entry)
		if err != nil {
			return rpc.GetLedgerEntriesResponse{}, err
		}
		return rpc.GetLedgerEntriesResponse{Entries: []rpc.LedgerEntryResult{{XDR: xdrStr}}}, nil
	}
}

// TestEnricher_ConcurrentMissesFetchOnce is the direct regression test for
// the "cache miss fetches once, even under concurrent requests" requirement.
// Without Enricher.fetchGroup, N concurrent lookups for the same uncached
// contract would each call FetchSpec independently; with it, they collapse
// into a single fetch and every waiter observes the same result.
func TestEnricher_ConcurrentMissesFetchOnce(t *testing.T) {
	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	const concurrency = 25

	wasmBytes := buildMinimalWasmWithEmptySpecSection(t)
	hash32 := bytes.Repeat([]byte{0x42}, 32)
	rpcClient := &fetchCountingRPCClient{
		wasmHash:  base64.StdEncoding.EncodeToString(hash32),
		wasmBytes: wasmBytes,
		delay:     20 * time.Millisecond,
	}
	fetcher := NewFetcher(rpcClient)
	cache := NewCache(nil)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	enricher := NewEnricher(fetcher, cache, log)

	events := []store.Event{{
		ID:         "evt",
		ContractID: contractID,
		Topics:     json.RawMessage(`[{"symbol":"transfer"}]`),
	}}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			<-start
			enricher.EnrichEvents(context.Background(), events)
		}()
	}
	close(start)
	wg.Wait()

	// Two ledger-entry round trips per fetch (wasm_hash, then the code
	// blob) — the count must not scale with concurrency. Without
	// Enricher.fetchGroup this would be ~2*concurrency instead.
	assert.LessOrEqual(t, rpcClient.calls.Load(), int64(4),
		"concurrent misses for the same contract must collapse into one fetch, not %d", concurrency)
}

// TestEnricher_CacheHitDoesNotRefetch locks in that once a spec is cached,
// subsequent enrichment for the same contract never touches the fetcher —
// a regression here would silently turn every enrichment into an RPC call.
func TestEnricher_CacheHitDoesNotRefetch(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cache := NewCache(nil)
	require.NoError(t, cache.Set(context.Background(), &ContractSpec{
		WasmHash:   "hash",
		ContractID: "CDLZ...",
		Events:     []EventSpec{{Name: "transfer"}},
	}))

	rpcClient := &fetchCountingRPCClient{}
	enricher := NewEnricher(NewFetcher(rpcClient), cache, log)

	events := []store.Event{{
		ID:         "evt",
		ContractID: "CDLZ...",
		Topics:     json.RawMessage(`[{"symbol":"transfer"}]`),
	}}
	for i := 0; i < 5; i++ {
		enricher.EnrichEvents(context.Background(), events)
	}

	assert.Zero(t, rpcClient.calls.Load(), "a cache hit must never reach the fetcher")
}

// TestEnricher_DoesNotMutateStoredEvent proves that enrichment is read-only
// on its input: the returned EnrichedEvent carries a decoded projection
// alongside the event, but the original Event value (and the bytes backing
// its JSON fields) must come back unchanged.
func TestEnricher_DoesNotMutateStoredEvent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cache := NewCache(nil)
	require.NoError(t, cache.Set(context.Background(), &ContractSpec{
		WasmHash:   "hash",
		ContractID: "CDLZ...",
		Events: []EventSpec{{
			Name:       "transfer",
			TopicSpecs: []FieldSpec{{Name: "from", Type: "address"}},
			ValueSpec:  &FieldSpec{Name: "amount", Type: "i128"},
		}},
	}))
	enricher := NewEnricher(nil, cache, log)

	original := store.Event{
		ID:         "evt1",
		ContractID: "CDLZ...",
		Topics:     json.RawMessage(`[{"symbol":"transfer"},{"address":"GA...FROM"}]`),
		Value:      json.RawMessage(`{"i128":"5000"}`),
	}
	// Keep an independent copy of the byte slices to compare against after
	// enrichment, since a mutation through a shared backing array wouldn't
	// show up by comparing the (unchanged) slice headers alone.
	wantTopics := append([]byte(nil), original.Topics...)
	wantValue := append([]byte(nil), original.Value...)

	enriched := enricher.EnrichEvents(context.Background(), []store.Event{original})
	require.Len(t, enriched, 1)
	assert.True(t, enriched[0].Decoded)

	assert.Equal(t, original, enriched[0].Event,
		"the enriched event's embedded Event must equal the input unchanged")
	assert.Equal(t, wantTopics, []byte(original.Topics), "enrichment must not mutate the input event's Topics bytes")
	assert.Equal(t, wantValue, []byte(original.Value), "enrichment must not mutate the input event's Value bytes")
}
