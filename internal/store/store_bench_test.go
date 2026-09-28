package store

import (
	"encoding/json"
	"testing"
)

func generateDummyStoreEvents(startLedger int64, count int) []Event {
	events := make([]Event, count)
	for i := 0; i < count; i++ {
		events[i] = Event{
			Ledger: startLedger + int64(i),
		}
	}
	return events
}

func buildQuery(f EventFilter) (string, []any, error) {
	query := "SELECT * FROM events WHERE 1=1"
	var args []any
	if f.ContractID != "" {
		query += " AND contract_id = ?"
		args = append(args, f.ContractID)
	}
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return query, args, nil
}

func BenchmarkQueryConstruction_SimpleFilter(b *testing.B) {
	filter := EventFilter{
		ContractID: "C0000000000000000000000000000000000000000000000000000001",
		Limit:      50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = buildQuery(filter)
	}
}

func BenchmarkQueryConstruction_ComplexFilter(b *testing.B) {
	filter := EventFilter{
		ContractID:    "C0000000000000000000000000000000000000000000000000000001",
		Types:         []string{"contract"},
		FromLedger:    1000,
		ToLedger:      2000,
		TopicContains: json.RawMessage(`[{"symbol":"transfer"}]`),
		Limit:         100,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = buildQuery(filter)
	}
}

func BenchmarkUpsertBatchMemory_100(b *testing.B) {
	benchmarkUpsertPayload(b, 100)
}

func BenchmarkUpsertBatchMemory_500(b *testing.B) {
	benchmarkUpsertPayload(b, 500)
}

func BenchmarkUpsertBatchMemory_1000(b *testing.B) {
	benchmarkUpsertPayload(b, 1000)
}

func benchmarkUpsertPayload(b *testing.B, size int) {
	events := generateDummyStoreEvents(int64(100000), size)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = events
	}
}
