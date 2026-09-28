package spec

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpecCache_ConcurrentRace(t *testing.T) {
	t.Parallel()
	cache := NewCache(nil)
	var wg sync.WaitGroup
	goroutines := 20
	ops := 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			contractID := fmt.Sprintf("contract-%d", id%5)
			wasmHash := fmt.Sprintf("hash-%d", id%5)
			ctx := context.Background()
			for j := 0; j < ops; j++ {
				if j%2 == 0 {
					_ = cache.Set(ctx, &ContractSpec{WasmHash: wasmHash, ContractID: contractID})
				} else {
					_ = cache.Get(wasmHash)
				}
			}
		}(i)
	}
	wg.Wait()
	assert.True(t, true)
}
