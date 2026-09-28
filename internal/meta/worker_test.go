package meta

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRPCClient simulates RPC responses for metadata resolution.
type mockRPCClient struct {
	fn func(ctx context.Context, contractID string) (Metadata, error)
}

func (m *mockRPCClient) GetMetadata(ctx context.Context, contractID string) (Metadata, error) {
	return m.fn(ctx, contractID)
}

// mockMetadataStore implements MetadataStore with an in-memory map.
type mockMetadataStore struct {
	mu      sync.Mutex
	storage map[string]storedMeta
}

type storedMeta struct {
	meta Metadata
	time time.Time
}

func newMockMetadataStore() *mockMetadataStore {
	return &mockMetadataStore{
		storage: make(map[string]storedMeta),
	}
}

func (s *mockMetadataStore) Get(contractID string) (Metadata, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok := s.storage[contractID]
	if !ok {
		return Metadata{}, false
	}
	return val.meta, true
}

func (s *mockMetadataStore) Put(contractID string, m Metadata, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storage[contractID] = storedMeta{
		meta: m,
		time: t,
	}
}

func TestResolveMetadata_Cases(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		initialStore map[string]Metadata
		rpcMeta      Metadata
		rpcErr       error
		useFallback  bool
		useSingle    bool
		wantMeta     Metadata
		wantStorePut bool
		wantErr      bool
	}{
		{
			name: "successful simulation decoding to expected metadata",
			rpcMeta: Metadata{
				Name:     "USDC",
				Symbol:   "USDC",
				Decimals: 7,
				IsToken:  true,
			},
			wantMeta: Metadata{
				Name:     "USDC",
				Symbol:   "USDC",
				Decimals: 7,
				IsToken:  true,
			},
			wantStorePut: true,
		},
		{
			name: "non-token contract cached as negative result",
			rpcMeta: Metadata{
				IsToken: false,
			},
			wantMeta: Metadata{
				IsToken: false,
			},
			wantStorePut: true,
		},
		{
			name:         "cached result bypasses rpc simulation",
			initialStore: map[string]Metadata{"C_CACHED": {Name: "DAI", Symbol: "DAI", Decimals: 18, IsToken: true}},
			rpcMeta:      Metadata{IsToken: false}, // Should not be called
			wantMeta:     Metadata{Name: "DAI", Symbol: "DAI", Decimals: 18, IsToken: true},
			wantStorePut: false,
		},
		{
			name:         "failed simulation not overwriting existing metadata when fallback enabled",
			useFallback:  true,
			initialStore: map[string]Metadata{"C_EXISTING": {Name: "OLD", Symbol: "OLD", Decimals: 6, IsToken: true}},
			rpcErr:       errors.New("rpc timeout"),
			wantMeta:     Metadata{Name: "OLD", Symbol: "OLD", Decimals: 6, IsToken: true},
		},
		{
			name:    "failed simulation returns error when no fallback",
			rpcErr:  errors.New("rpc error"),
			wantErr: true,
		},
		{
			name:         "malformed simulation output rejected without panicking",
			rpcMeta:      Metadata{Name: "\x00malformed", IsToken: true},
			wantMeta:     Metadata{Name: "\x00malformed", IsToken: true},
			wantStorePut: true,
		},
		{
			name:         "single flight resolution works for concurrent requests",
			useSingle:    true,
			rpcMeta:      Metadata{Name: "SF", Symbol: "SF", Decimals: 7, IsToken: true},
			wantMeta:     Metadata{Name: "SF", Symbol: "SF", Decimals: 7, IsToken: true},
			wantStorePut: true,
		},
	}

	for _, tt := range tests {
		data := tt
		t.Run(data.name, func(t *testing.T) {
			store := newMockMetadataStore()
			for k, v := range data.initialStore {
				store.Put(k, v, time.Now())
			}

			rpcCalls := 0
			rpc := &mockRPCClient{
				fn: func(ctx context.Context, contractID string) (Metadata, error) {
					rpcCalls++
					if data.rpcErr != nil {
						return Metadata{}, data.rpcErr
					}
					return data.rpcMeta, nil
				},
			}

			contractID := "C_TEST"
			if len(data.initialStore) > 0 {
				for k := range data.initialStore {
					contractID = k
				}
			}

			var got Metadata
			var err error

			if data.useFallback {
				got, err = ResolveMetadataWithFallbacks(ctx, rpc, store, contractID)
			} else if data.useSingle {
				got, err = ResolveMetadataCachedSingleFlight(ctx, rpc, store, contractID)
			} else {
				got, err = ResolveMetadata(ctx, rpc, store, contractID)
			}

			if data.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, data.wantMeta, got)
				if data.wantStorePut {
					stored, ok := store.Get(contractID)
					assert.True(t, ok)
					assert.Equal(t, data.wantMeta, stored)
				}
			}
		})
	}
}
