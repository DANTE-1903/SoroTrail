package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/sorotrail/sorotrail/internal/config"
	"github.com/sorotrail/sorotrail/internal/store"
)

// runMigrate implements `sorotrail migrate <up|down|status>`: it wraps the
// golang-migrate operations the startup path already performs behind an
// explicit subcommand, so an operator can apply, roll back, or inspect the
// schema without starting the full indexer.
//
// The database comes from the same DATABASE_URL the server uses. Postgres is
// the production target; SQLite URLs are supported for local development.
func runMigrate(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			migrateUsage()
			return nil
		}
	}
	if len(args) == 0 {
		migrateUsage()
		return errors.New("migrate requires an action: up, down, or status")
	}

	action := args[0]
	switch action {
	case "up", "down", "status":
	default:
		migrateUsage()
		return fmt.Errorf("unknown migrate action %q (want up, down, or status)", action)
	}

	fs := flag.NewFlagSet("migrate "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = migrateUsage
	steps := fs.Int("steps", defaultMigrateSteps(action),
		"migrations to apply (up) or roll back (down); 0 = all")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *steps < 0 {
		return fmt.Errorf("--steps must be >= 0, got %d", *steps)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel, cfg.LogFormat)

	switch action {
	case "up":
		if err := store.MigrateUp(cfg.DatabaseURL, *steps); err != nil {
			return err
		}
		log.Info("migrations applied", "direction", "up", "steps", *steps)
		return nil
	case "down":
		if err := store.MigrateDown(cfg.DatabaseURL, *steps); err != nil {
			return err
		}
		log.Info("migrations rolled back", "direction", "down", "steps", *steps)
		return nil
	default: // status
		st, err := store.MigrateStatus(cfg.DatabaseURL)
		if err != nil {
			return err
		}
		printMigrationStatus(st)
		return nil
	}
}

// defaultMigrateSteps keeps `migrate down` safe by default: a bare invocation
// rolls back exactly one migration, while `migrate up` applies everything
// pending (the historical startup behavior).
func defaultMigrateSteps(action string) int {
	if action == "down" {
		return 1
	}
	return 0
}

func printMigrationStatus(st store.MigrationStatus) {
	if st.Version == 0 {
		fmt.Printf("migration status: no migrations applied (%d pending)\n", len(st.Pending))
		return
	}
	dirty := "clean"
	if st.Dirty {
		dirty = "DIRTY — manual repair required"
	}
	fmt.Printf("migration status: version=%d (%s), %d pending\n", st.Version, dirty, len(st.Pending))
	if len(st.Pending) > 0 {
		fmt.Printf("  pending versions: %v\n", st.Pending)
	}
}

func migrateUsage() {
	fmt.Fprint(os.Stderr, `usage: sorotrail migrate <up|down|status> [--steps N]

Wrap the golang-migrate operations behind a subcommand.

actions:
  up      apply pending migrations (--steps 0 = all, the default)
  down    roll back migrations (--steps N; 0 = all; default 1)
  status  print the current schema version, dirty flag, and pending versions

The database is taken from DATABASE_URL. Postgres is the production target;
sqlite URLs are supported for local development.

flags:
  --steps N   migrations to apply/roll back; 0 = all
`)
}
