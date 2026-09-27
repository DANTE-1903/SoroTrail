package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func benchStore(b *testing.B) *Postgres {
	b.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		b.Skip("TEST_DATABASE_URL not set; skipping Postgres store benchmarks")
	}

	if err := Migrate(dbURL); err != nil {
		b.Fatalf("failed to migrate database: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		b.Fatalf("failed to open pgxpool: %v", err)
	}
	b.Cleanup(pool.Close)

	return NewPostgres(pool)
}

// Generate a slice of dummy events for insertion benchmarks.
func generateDummyEvents(startID int, count int) []Event {
	events := make([]Event, count)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		idNum := startID + i
		ledger := int64(10000 + idNum/20)
		events[i] = Event{
			ID:               fmt.Sprintf("%019d-%010d", ledger, idNum%20),
			ContractID:       fmt.Sprintf("C%055d", idNum%50),
			Ledger:           ledger,
			Type:             "contract",
			TxHash:           fmt.Sprintf("%064x", idNum),
			TxIndex:          int32((idNum % 20) / 2),
			OpIndex:          int32(idNum % 2),
			InSuccessfulCall: true,
			Topics:           json.RawMessage(`[{"symbol":"transfer"},{"address":"CCW67TSBWVENNVMTQPEXNGXYL6G5CZWKW563CYCPBQR27XMTC2AFAXXT"}]`),
			Value:            json.RawMessage(`{"u64":100000}`),
			CreatedAt:        baseTime.Add(time.Duration(ledger) * time.Second),
			RawTopicXDR:      []string{"AAAAEAAAAAHRcmFuc2Zlcg=="},
			RawValueXDR:      "AAAAAwAAAAAABad0",
		}
	}
	return events
}

func BenchmarkUpsertEvents_Batch100(b *testing.B) {
	benchmarkUpsertEventsBatchSize(b, 100)
}

func BenchmarkUpsertEvents_Batch500(b *testing.B) {
	benchmarkUpsertEventsBatchSize(b, 500)
}

func BenchmarkUpsertEvents_Batch1000(b *testing.B) {
	benchmarkUpsertEventsBatchSize(b, 1000)
}

func BenchmarkUpsertEvents_Batch2500(b *testing.B) {
	benchmarkUpsertEventsBatchSize(b, 2500)
}

func BenchmarkUpsertEvents_Batch5000(b *testing.B) {
	benchmarkUpsertEventsBatchSize(b, 5000)
}

func benchmarkUpsertEventsBatchSize(b *testing.B, batchSize int) {
	st := benchStore(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	startID := 9000000
	for i := 0; i < b.N; i++ {
		events := generateDummyEvents(startID, batchSize)
		startID += batchSize
		_, err := st.UpsertEvents(ctx, events)
		if err != nil {
			b.Fatalf("UpsertEvents batch %d failed: %v", batchSize, err)
		}
	}
}

// Hot Filter Paths Benchmarks

func BenchmarkQueryEvents_Unfiltered(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{Limit: 50}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_ContractID(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{
		ContractID: fmt.Sprintf("C%055d", 5),
		Limit:      50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_Type(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{
		Types: []string{"contract"},
		Limit: 50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_TopicContains(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{
		TopicContains: json.RawMessage(`[{"symbol":"transfer"}]`),
		Limit:         50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_LedgerRange(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{
		FromLedger: 100100,
		ToLedger:   100500,
		Limit:      50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_CursorPagination(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	// Fetch first page to get valid cursor
	events, cursor, err := st.QueryEvents(ctx, EventFilter{Limit: 50})
	if err != nil || len(events) == 0 || cursor == "" {
		b.Skip("Insufficient data for cursor benchmark")
	}

	filter := EventFilter{
		Cursor: cursor,
		Limit:  50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryEvents_OrderByLedger(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	filter := EventFilter{
		OrderBy: OrderByLedger,
		Order:   "desc",
		Limit:   50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, err := st.QueryEvents(ctx, filter)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// benchDataset describes one seeded dataset the QueryEvents benchmarks
// run against. Rows reuse the id/ledger layout of generateDummyEvents so
// the seeded fixtures and the insertion benchmarks stay comparable.
type benchDataset struct {
	name      string
	events    int
	contracts int
}

// seedQueryEventsDataset writes n events spread over numContracts contracts
// and a contiguous ledger range, mirroring cmd/seed's distribution shape.
// It is idempotent per (name, n): the same ledger window is rewritten with
// identical rows, so re-running the benchmark neither duplicates data nor
// grows the table without bound.
func seedQueryEventsDataset(b *testing.B, st *Postgres, name string, n, numContracts int) {
	b.Helper()
	events := generateDummyEvents(startIDForDataset(name), n)
	for i := range events {
		events[i].ContractID = fmt.Sprintf("C%055d", i%numContracts)
	}
	if _, err := st.UpsertEvents(context.Background(), events); err != nil {
		b.Fatalf("seeding %s dataset (%d events): %v", name, n, err)
	}
}

// startIDForDataset maps a dataset name to a stable starting event ID so
// repeated runs upsert the same rows instead of inserting fresh ones.
func startIDForDataset(name string) int {
	switch name {
	case "queryevents-small":
		return 2000000
	case "queryevents-large":
		return 3000000
	default:
		return 1000000
	}
}

// BenchmarkQueryEvents_Seeded measures QueryEvents against datasets seeded
// by the benchmark itself, so the numbers reflect a known data volume and
// contract cardinality rather than whatever the database happens to hold.
//
// Like every benchmark in this file it is gated on TEST_DATABASE_URL: a
// plain `go test ./...` skips it, and CI enables it via the Postgres
// service (see CONTRIBUTING.md). Each sub-benchmark seeds its dataset
// first (idempotently — see seedQueryEventsDataset) and then exercises the
// hot filter paths over that data with a wildcard scope, which is what a
// single-tenant deployment issues.
func BenchmarkQueryEvents_Seeded(b *testing.B) {
	datasets := []benchDataset{
		{name: "queryevents-small", events: 1000, contracts: 10},
		{name: "queryevents-large", events: 20000, contracts: 50},
	}

	for _, ds := range datasets {
		b.Run(ds.name, func(b *testing.B) {
			st := benchStore(b)
			ctx := context.Background()
			seedQueryEventsDataset(b, st, ds.name, ds.events, ds.contracts)

			// Representative filters: unfiltered hot path, equality on the
			// contract list, a topic-contains lookup that leans on the GIN
			// index, a ledger window, and one full cursor walk. All run with
			// a wildcard scope — the tenant boundary itself is exercised
			// elsewhere — so the measurements isolate the query path.
			contract := fmt.Sprintf("C%055d", 0)
			scenarios := []struct {
				name   string
				filter EventFilter
			}{
				{"unfiltered", EventFilter{Limit: 50}},
				{"by-contract", EventFilter{ContractID: contract, Limit: 50}},
				{"topic-contains", EventFilter{
					TopicContains: json.RawMessage(`[{"symbol":"transfer"}]`),
					Limit:         50,
				}},
				{"ledger-window", EventFilter{FromLedger: 100000, ToLedger: 100900, Limit: 50}},
				{"desc-ledger", EventFilter{OrderBy: OrderByLedger, Order: "desc", Limit: 50}},
			}

			for _, sc := range scenarios {
				b.Run(sc.name, func(b *testing.B) {
					f := sc.filter
					f.Scope = WildcardScope()
					b.ResetTimer()
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if _, _, err := st.QueryEvents(ctx, f); err != nil {
							b.Fatal(err)
						}
					}
				})
			}

			// Cursor walk: fetch pages until the store reports exhaustion,
			// timing the whole traversal rather than a single page.
			b.Run("cursor-walk", func(b *testing.B) {
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					cursor := ""
					pages := 0
					b.StartTimer()
					for {
						page, next, err := st.QueryEvents(ctx, EventFilter{
							Scope:  WildcardScope(),
							Cursor: cursor,
							Limit:  100,
						})
						if err != nil {
							b.Fatal(err)
						}
						pages++
						if next == "" || len(page) == 0 {
							break
						}
						cursor = next
						if pages > ds.events/100+10 {
							b.Fatalf("cursor walk did not terminate after %d pages", pages)
						}
					}
				}
			})
		})
	}
}
