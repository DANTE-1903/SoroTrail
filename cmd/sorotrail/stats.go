package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/sorotrail/sorotrail/internal/store"
)

// runStats implements `sorotrail stats`: prints the store's stats as an
// aligned two-column table so an operator can see ingestion progress and
// storage footprint without curling the API or querying SQL.
func runStats(args []string) error {
	return runStatsTo(os.Stdout, args)
}

// runStatsTo is runStats with an explicit output writer so tests can
// assert on the emitted table without touching stdout. Tests exercise
// renderStatsTable directly for the DB-backed flow; this wrapper only
// needs to prove flag/argument handling, which never reaches the DB.
// statsUsage is the help text for the stats subcommand.
const statsUsage = `usage: sorotrail stats

Prints store stats as a table: stored event counts, ledger progress
(ingested / verified / chain head / lag), watched contract counts, and
the on-disk size of the events table. Read-only — no writes are performed.

Only database-backed stats are shown; process-local counters (RPC errors,
decode failures, guarded-store query errors) belong to the running indexer
and are reported by its /stats endpoint instead.
`

func runStatsTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), statsUsage)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // usage already printed
		}
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("unexpected argument %q (stats takes no arguments)", fs.Args()[0])
	}

	ctx := context.Background()
	st, cleanup, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	stats, err := st.Stats(ctx, store.SystemScope())
	if err != nil {
		return fmt.Errorf("reading store stats: %w", err)
	}
	return renderStatsTable(w, stats)
}

// statsRow is one metric/value pair of the stats table.
type statsRow struct {
	metric string
	value  string
}

// statsRows converts a Stats struct into the ordered rows of the table.
// Optional fields (chain head, ingest lag, last poll) render as "-"
// / "never" rather than fake zeroes when the backend hasn't reported them.
func statsRows(s store.Stats) []statsRow {
	rows := []statsRow{
		{"total events", strconv.FormatInt(s.TotalEvents, 10)},
		{"last ingested ledger", strconv.FormatInt(s.LastIngestedLedger, 10)},
		{"verified through ledger", strconv.FormatInt(s.VerifiedThroughLedger, 10)},
		{"oldest stored ledger", strconv.FormatInt(s.OldestStoredLedger, 10)},
		{"chain head ledger", formatOptionalInt(s.ChainHeadLedger)},
		{"ingest lag", formatIngestLag(s.IngestLagLedgers)},
		{"last successful poll", formatLastPoll(s.LastSuccessfulPoll)},
		{"contracts", strconv.FormatInt(s.ContractCount, 10)},
		{"watched contracts", strconv.FormatInt(s.WatchedContracts, 10)},
		{"events table size", humanSize(s.TableSizeBytes)},
	}
	return rows
}

// renderStatsTable writes the stats as an aligned METRIC/VALUE table.
func renderStatsTable(w io.Writer, s store.Stats) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "METRIC\tVALUE"); err != nil {
		return err
	}
	for _, r := range statsRows(s) {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r.metric, r.value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatOptionalInt renders a nullable ledger as "-" when absent.
func formatOptionalInt(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

// formatIngestLag renders the lag with its unit, or "-" when the
// backend cannot compute a chain head to measure against.
func formatIngestLag(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d ledgers", *p)
}

// formatLastPoll renders the last successful poll timestamp in the same
// wall-clock format `apikey list` uses; "never" before the first pass.
func formatLastPoll(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Format("2006-01-02 15:04:05")
}
