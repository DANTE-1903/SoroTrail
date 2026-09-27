package ingester

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
)

// The per-cycle event cap (Options.MaxEventsPerCycle) is enforced by two
// helpers that decide how much a single cycle may fetch. effectivePageLimit
// sizes the first getEvents request of a cycle; newCycleBudget allocates the
// shared budget that windowSweep's request chains draw down. These tests pin
// the helpers' contracts directly, then drive sweepBatch to show the budget
// decrements across pages, ends an exhausted cycle on a page boundary, and is
// re-allocated fresh on the next cycle.

func TestEffectivePageLimit(t *testing.T) {
	tests := []struct {
		name              string
		pageLimit         uint
		maxEventsPerCycle uint
		want              uint
	}{
		{name: "unset cap falls back to the page limit", pageLimit: 1000, want: 1000},
		{name: "cap below the page limit wins", pageLimit: 1000, maxEventsPerCycle: 200, want: 200},
		{name: "cap above the page limit does not inflate the request", pageLimit: 1000, maxEventsPerCycle: 5000, want: 1000},
		{name: "cap equal to the page limit is a no-op", pageLimit: 500, maxEventsPerCycle: 500, want: 500},
		{name: "defaulted page limit is still clamped by the cap", maxEventsPerCycle: 100, want: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := newTestIngester(&mockRPC{}, newMockStore(), Options{
				PageLimit:         tt.pageLimit,
				MaxEventsPerCycle: tt.maxEventsPerCycle,
			})
			assert.Equal(t, tt.want, ing.effectivePageLimit(),
				"the first request of a cycle must not exceed the per-cycle cap")
		})
	}
}

func TestNewCycleBudget(t *testing.T) {
	t.Run("no cap allocates no budget", func(t *testing.T) {
		ing := newTestIngester(&mockRPC{}, newMockStore(), Options{PageLimit: 100})
		assert.Nil(t, ing.newCycleBudget(),
			"an unset cap must leave the budget nil so every batch pages to completion")
	})

	t.Run("cap allocates a full budget", func(t *testing.T) {
		ing := newTestIngester(&mockRPC{}, newMockStore(), Options{PageLimit: 100, MaxEventsPerCycle: 42})
		budget := ing.newCycleBudget()
		require.NotNil(t, budget)
		assert.Equal(t, int64(42), budget.Load())
	})

	t.Run("a new cycle does not inherit the previous cycle's spend", func(t *testing.T) {
		ing := newTestIngester(&mockRPC{}, newMockStore(), Options{PageLimit: 100, MaxEventsPerCycle: 10})
		first := ing.newCycleBudget()
		first.Add(-10)
		require.Equal(t, int64(0), first.Load())

		second := ing.newCycleBudget()
		assert.Equal(t, int64(10), second.Load(),
			"an exhausted budget is per-cycle state and must be re-allocated each cycle")
	})
}

// TestSweepBatch_DecrementsSharedBudgetAcrossPages pins the page-by-page
// draw-down: each request claims min(page limit, remaining budget) and the
// page's events are then subtracted, so a later page asks for strictly less
// once the budget runs low.
func TestSweepBatch_DecrementsSharedBudgetAcrossPages(t *testing.T) {
	page1 := make([]rpc.Event, 10)
	for i := range page1 {
		page1[i] = rpcEvent(fmt.Sprintf("e1-%02d", i), uint32(100+i))
	}
	page2 := make([]rpc.Event, 5)
	for i := range page2 {
		page2[i] = rpcEvent(fmt.Sprintf("e2-%02d", i), uint32(200+i))
	}
	client := newScriptedRPC(map[string]rpc.GetEventsResponse{
		"":   {Events: page1, LatestLedger: 500, Cursor: "c1"},
		"c1": {Events: page2, LatestLedger: 500, Cursor: "c2"},
	})
	st := newMockStore()
	ing := newTestIngester(client, st, Options{PageLimit: 10, MaxEventsPerCycle: 15})

	remaining := ing.newCycleBudget()
	require.NotNil(t, remaining)
	var capHit, ledgerOutOfRange atomic.Bool

	err := ing.sweepBatch(context.Background(), &ledgerOutOfRange, remaining, &capHit, 100, 500, nil)
	require.NoError(t, err, "hitting the budget is not an error")

	require.Len(t, client.calls, 2, "two pages fit inside the budget before it is spent")
	assert.Equal(t, uint(10), client.calls[0].Pagination.Limit,
		"the first page takes the full page limit while the budget covers it")
	assert.Equal(t, uint(5), client.calls[1].Pagination.Limit,
		"the second page claims the remaining budget, not the page limit")
	assert.Equal(t, int64(0), remaining.Load(), "15 events across two pages spend the budget exactly")
	assert.True(t, capHit.Load(), "the exhausted budget is reported to windowSweep")
}

// TestSweepBatch_ExhaustedBudgetEndsCycleCleanly shows the stop happens
// before a request is issued, not part-way through a page: with a budget
// smaller than one page the single request is clamped to the budget, and the
// next iteration stops instead of fetching past it.
func TestSweepBatch_ExhaustedBudgetEndsCycleCleanly(t *testing.T) {
	page := make([]rpc.Event, 3)
	for i := range page {
		page[i] = rpcEvent(fmt.Sprintf("e-%02d", i), uint32(100+i))
	}
	client := newScriptedRPC(map[string]rpc.GetEventsResponse{
		"": {Events: page, LatestLedger: 500, Cursor: "c1"},
	})
	ing := newTestIngester(client, newMockStore(), Options{PageLimit: 10, MaxEventsPerCycle: 3})

	remaining := ing.newCycleBudget()
	require.NotNil(t, remaining)
	var capHit, ledgerOutOfRange atomic.Bool

	err := ing.sweepBatch(context.Background(), &ledgerOutOfRange, remaining, &capHit, 100, 500, nil)
	require.NoError(t, err)

	require.Len(t, client.calls, 1, "no further request is issued once the budget is spent")
	assert.Equal(t, uint(3), client.calls[0].Pagination.Limit,
		"the request is clamped down to the remaining budget")
	assert.Equal(t, int64(0), remaining.Load())
	assert.True(t, capHit.Load())
	assert.False(t, ledgerOutOfRange.Load(), "an exhausted budget is not an out-of-range signal")
}
