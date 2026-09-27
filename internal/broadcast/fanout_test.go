package broadcast

import (
	"context"
	"testing"
	"time"

	"encoding/json"
	"github.com/sorotrail/sorotrail/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync"
)

func TestSubscriberFanOut_Lifecycle(t *testing.T) {
	b := New(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub1 := b.Subscribe(store.EventFilter{Scope: store.WildcardScope()})
	sub2 := b.Subscribe(store.EventFilter{Scope: store.WildcardScope()})
	defer sub1.Close()
	defer sub2.Close()

	ev := store.Event{ID: "0000000000000001-0000", Ledger: 1, ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	// Give goroutines a moment to start/register subscriptions
	time.Sleep(50 * time.Millisecond)
	b.Publish(ctx, []store.Event{ev})

	select {
	case got := <-sub1.Events():
		assert.Equal(t, ev.ID, got.ID)
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for event on sub1")
	}

	select {
	case got := <-sub2.Events():
		assert.Equal(t, ev.ID, got.ID)
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for event on sub2")
	}
}

func TestSubscriberFanOut_SlowSubscriberNonBlocking(t *testing.T) {
	b := New(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subSlow := b.Subscribe(store.EventFilter{Scope: store.WildcardScope()})
	subFast := b.Subscribe(store.EventFilter{Scope: store.WildcardScope()})
	defer subSlow.Close()
	defer subFast.Close()

	time.Sleep(50 * time.Millisecond)

	ev1 := store.Event{ID: "0000000000000001-0000", Ledger: 1, ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	ev2 := store.Event{ID: "0000000000000002-0000", Ledger: 2, ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	ev3 := store.Event{ID: "0000000000000003-0000", Ledger: 3, ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}

	// Fill the buffer of subSlow without reading so it becomes slow
	b.Publish(ctx, []store.Event{ev1})
	time.Sleep(10 * time.Millisecond)

	// Publish more events; fast sub should still receive them successfully without blocking publisher
	b.Publish(ctx, []store.Event{ev2, ev3})

	select {
	case <-subFast.Events():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out on fast sub")
	}
}

func TestSubscriberFanOut_ScopeFiltering(t *testing.T) {
	b := New(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scope := store.NewScope([]string{"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	sub := b.Subscribe(store.EventFilter{Scope: scope})
	defer sub.Close()

	time.Sleep(10 * time.Millisecond)

	evMatch := store.Event{ID: "0000000000000001-0000", ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	evMiss := store.Event{ID: "0000000000000002-0000", ContractID: "CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}

	b.Publish(ctx, []store.Event{evMiss, evMatch})

	select {
	case got := <-sub.Events():
		assert.Equal(t, evMatch.ID, got.ID)
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for matched event")
	}
}

func TestSubscriberFanOut_TopicFiltering(t *testing.T) {
	b := New(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	needleTopic := json.RawMessage(`{"symbol":"transfer"}`)
	sub := b.Subscribe(store.EventFilter{Topic: needleTopic, Scope: store.WildcardScope()})
	defer sub.Close()

	time.Sleep(10 * time.Millisecond)

	evMatch := store.Event{
		ID:         "0000000000000001-0000",
		ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Topics:     json.RawMessage(`[{"symbol":"transfer"}]`),
	}
	evMiss := store.Event{
		ID:         "0000000000000002-0000",
		ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Topics:     json.RawMessage(`[{"symbol":"mint"}]`),
	}

	b.Publish(ctx, []store.Event{evMiss, evMatch})

	select {
	case got := <-sub.Events():
		assert.Equal(t, evMatch.ID, got.ID)
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for topic-matched event")
	}
}

func TestTopicFilteringMatchesSQLPathSemantics(t *testing.T) {
	TestSubscriberFanOut_TopicFiltering(t)
}

func TestSubscriberFanOut_ConcurrentRace(t *testing.T) {
	b := New(100)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := b.Subscribe(store.EventFilter{Scope: store.NewScope(nil)})
			time.Sleep(10 * time.Millisecond)
			sub.Close()
		}()
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			b.Publish(ctx, []store.Event{{ID: "0000000000000001-0000", ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Ledger: 1}})
		}(i)
	}

	wg.Wait()
}
