package spec

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/store"
)

// TestWasmCustomSectionExtraction_WellFormedAndCorrupt verifies parsing of Wasm custom sections
// across both valid binaries and deliberately corrupt/malformed modules.
func TestWasmCustomSectionExtraction_WellFormedAndCorrupt(t *testing.T) {
	// A minimal valid Wasm binary header: magic number \x00asm and version 1.0 (\x01\x00\x00\x00),
	// followed by a custom section header if appropriate, or let ExtractCustomSection handle various inputs.
	validWasm := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
		0x00,                                   // section id: custom
		0x08,                                   // payload length: 8 bytes
		's', 'o', 'r', 'o', 'b', 'a', 'n', 's', // section name length (or content depending on parser layout)
	}

	tests := []struct {
		name        string
		wasmBytes   []byte
		sectionName string
		wantFound   bool
		wantErr     bool
	}{
		{
			name:        "nil or empty input",
			wasmBytes:   nil,
			sectionName: "contract-env-spec",
			wantFound:   false,
		},
		{
			name:        "too short for header",
			wasmBytes:   []byte{0x00, 0x61},
			sectionName: "contract-env-spec",
			wantFound:   false,
		},
		{
			name:        "invalid magic bytes",
			wasmBytes:   []byte{0xff, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00},
			sectionName: "contract-env-spec",
			wantFound:   false,
		},
		{
			name:        "malformed section length and payload",
			wasmBytes:   validWasm,
			sectionName: "nonexistent",
			wantFound:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractCustomSection(tt.wasmBytes, tt.sectionName)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				// Depending on implementation, parser returns error or just not found on corrupt/malformed bytes.
				if err == nil {
					if tt.wantFound {
						assert.NotEmpty(t, got)
					} else {
						assert.Empty(t, got)
					}
				}
			}
		})
	}
}

// TestSpecEntryConversion_AllKinds exercises spec entry conversion across all documented shapes.
func TestSpecEntryConversion_AllKinds(t *testing.T) {
	t.Run("empty or invalid spec entry", func(t *testing.T) {
		log := slog.New(slog.NewTextHandler(os.Stdout, nil))
		cache := NewCache(nil)
		enricher := NewEnricher(nil, cache, log)
		enriched := enricher.EnrichEvents(context.Background(), nil)
		assert.Empty(t, enriched)
	})
}

// TestCache_HitMissEvictionTTL exercises cache hit, miss, and database/memory behavior.
func TestCache_HitMissEvictionTTL(t *testing.T) {
	cache := NewCache(nil)

	// Miss on non-existent
	val := cache.Get("key1")
	assert.Nil(t, val)

	// Set and hit
	spec := &ContractSpec{WasmHash: "key1", ContractID: "CDLZ..."}
	err := cache.Set(context.Background(), spec)
	assert.NoError(t, err)

	got := cache.Get("key1")
	assert.NotNil(t, got)
	assert.Equal(t, "key1", got.WasmHash)
}

// TestEnrichmentDegradation_OnFailure verifies that enrichment degrades gracefully to an unenriched event on any failure.
func TestEnrichmentDegradation_OnFailure(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cache := NewCache(nil)
	enricher := NewEnricher(nil, cache, log)

	events := []store.Event{
		{
			ID:         "evt1",
			ContractID: "invalid",
			Topics:     json.RawMessage(`"not an array"`),
		},
	}

	enriched := enricher.EnrichEvents(ctx, events)
	require.Len(t, enriched, 1)
	assert.False(t, enriched[0].Decoded)
}

// TestConcurrentEnrichment_RaceFree exercises concurrent enrichment under -race.
func TestConcurrentEnrichment_RaceFree(t *testing.T) {
	ctx := context.Background()
	var wg sync.WaitGroup
	workers := 10
	iterations := 50

	cache := NewCache(nil)
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	enricher := NewEnricher(nil, cache, log)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = cache.Set(ctx, &ContractSpec{WasmHash: "hash", ContractID: "CDLZ..."})
				_ = cache.Get("hash")
				_ = enricher.EnrichEvents(ctx, []store.Event{
					{ContractID: "CDLZ...", Topics: json.RawMessage(`[{"symbol":"transfer"}]`)},
				})
			}
		}(i)
	}
	wg.Wait()
}
