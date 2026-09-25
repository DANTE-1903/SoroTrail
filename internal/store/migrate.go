package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var postgresMigrationsFS embed.FS

//go:embed migrations/sqlite/*.sql
var sqliteMigrationsFS embed.FS

// Migrate applies all pending migrations to the database at databaseURL.
// It detects the dialect from the URL scheme and runs the appropriate
// migration series. Safe to call on every startup; up-to-date schema is a no-op.
//
// It is the startup-path alias for MigrateUp(url, 0) and is kept so existing
// callers (main, replay, backfill, …) are unchanged.
func Migrate(databaseURL string) error {
	return MigrateUp(databaseURL, 0)
}

// MigrateUp applies pending migrations. steps > 0 applies at most that many
// migrations; steps <= 0 applies every pending migration.
//
// ClickHouse runs the embedded ClickHouse series (apply-only). SQLite runs the
// embedded SQLite series; Postgres runs the embedded Postgres series via
// golang-migrate (and honors steps).
func MigrateUp(databaseURL string, steps int) error {
	if strings.HasPrefix(databaseURL, "clickhouse://") {
		return migrateClickHouse(databaseURL)
	}
	if isSQLiteURL(databaseURL) {
		return migrateSQLiteUp(databaseURL, steps)
	}
	if !isPostgresURL(databaseURL) {
		return fmt.Errorf("unsupported database url scheme")
	}

	m, err := newPostgresMigrate(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if steps > 0 {
		if err := m.Steps(steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("applying %d migration step(s): %w", steps, err)
		}
		return nil
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back migrations. steps > 0 rolls back at most that many
// migrations; steps <= 0 rolls back everything (a destructive full teardown,
// which is why `migrate down` defaults to a single step at the CLI layer).
func MigrateDown(databaseURL string, steps int) error {
	if strings.HasPrefix(databaseURL, "clickhouse://") {
		return errors.New("clickhouse migrations are apply-only; nothing to roll back")
	}
	if isSQLiteURL(databaseURL) {
		return migrateSQLiteDown(databaseURL, steps)
	}
	if !isPostgresURL(databaseURL) {
		return fmt.Errorf("unsupported database url scheme")
	}

	m, err := newPostgresMigrate(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if steps > 0 {
		if err := m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("rolling back %d migration step(s): %w", steps, err)
		}
		return nil
	}
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("rolling back migrations: %w", err)
	}
	return nil
}

// MigrateStatus reports the current migration version, dirty state, and the
// versions still pending, without mutating the database.
func MigrateStatus(databaseURL string) (MigrationStatus, error) {
	if strings.HasPrefix(databaseURL, "clickhouse://") {
		return MigrationStatus{}, errors.New("clickhouse migration status is not supported")
	}
	if isSQLiteURL(databaseURL) {
		return migrateSQLiteStatus(databaseURL)
	}
	if !isPostgresURL(databaseURL) {
		return MigrationStatus{}, fmt.Errorf("unsupported database url scheme")
	}

	m, err := newPostgresMigrate(databaseURL)
	if err != nil {
		return MigrationStatus{}, err
	}
	defer func() { _, _ = m.Close() }()

	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return MigrationStatus{}, fmt.Errorf("reading migration version: %w", err)
	}
	if errors.Is(err, migrate.ErrNilVersion) {
		version, dirty = 0, false // nothing applied yet
	}

	src, err := iofs.New(postgresMigrationsFS, "migrations")
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("opening migrations: %w", err)
	}
	defer func() { _ = src.Close() }()
	pending, err := migrationVersions(src, version)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("reading available migrations: %w", err)
	}
	return MigrationStatus{Version: version, Dirty: dirty, Pending: pending}, nil
}

func isPostgresURL(databaseURL string) bool {
	return strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://")
}

func isSQLiteURL(databaseURL string) bool {
	return strings.HasPrefix(databaseURL, "sqlite:") || strings.HasPrefix(databaseURL, "file:")
}

// newPostgresMigrate builds a golang-migrate instance over the embedded
// Postgres migrations. Closing the returned *migrate.Migrate closes the
// underlying database connection as well, so callers only defer m.Close().
func newPostgresMigrate(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(postgresMigrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("loading embedded migrations: %w", err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening migration connection: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing migrations: %w", err)
	}
	return m, nil
}

// --- SQLite migration series ---

// sqliteMigrationFile is one embedded SQLite migration file with its parsed
// leading version number.
type sqliteMigrationFile struct {
	version int
	name    string
}

// sqliteMigrationFiles returns the up- or down-migration files sorted by
// ascending version. Files whose name does not start with a version are
// skipped, mirroring the historical loader.
func sqliteMigrationFiles(direction string) ([]sqliteMigrationFile, error) {
	entries, err := sqliteMigrationsFS.ReadDir("migrations/sqlite")
	if err != nil {
		return nil, fmt.Errorf("reading sqlite migrations: %w", err)
	}
	suffix := "." + direction + ".sql"
	var out []sqliteMigrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(e.Name(), "%d", &version); err != nil || version == 0 {
			continue
		}
		out = append(out, sqliteMigrationFile{version: version, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func openSQLiteForMigration(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", parseSQLiteDSN(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("opening sqlite for migration: %w", err)
	}
	return db, nil
}

// ensureSQLiteMigrationTable creates the migration tracking table used by the
// SQLite series (one row per applied version).
func ensureSQLiteMigrationTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, dirty INTEGER NOT NULL DEFAULT 0)`); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}
	return nil
}

// migrateSQLiteUp applies pending SQLite migrations. steps > 0 bounds how many
// files are applied; steps <= 0 applies all pending files.
func migrateSQLiteUp(databaseURL string, steps int) error {
	db, err := openSQLiteForMigration(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := ensureSQLiteMigrationTable(db); err != nil {
		return err
	}
	files, err := sqliteMigrationFiles("up")
	if err != nil {
		return err
	}

	applied := 0
	for _, f := range files {
		if steps > 0 && applied >= steps {
			break
		}
		var existing int
		err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = ?`, f.version).Scan(&existing)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking migration %s: %w", f.name, err)
		}

		content, err := sqliteMigrationsFS.ReadFile("migrations/sqlite/" + f.name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", f.name, err)
		}
		if _, err := db.Exec(string(content)); err != nil {
			return fmt.Errorf("applying migration %s: %w", f.name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, dirty) VALUES (?, 0)`, f.version); err != nil {
			return fmt.Errorf("recording migration %s: %w", f.name, err)
		}
		applied++
	}
	return nil
}

// migrateSQLiteDown rolls back applied SQLite migrations newest-first.
// steps > 0 bounds how many files are rolled back; steps <= 0 rolls back all.
func migrateSQLiteDown(databaseURL string, steps int) error {
	db, err := openSQLiteForMigration(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := ensureSQLiteMigrationTable(db); err != nil {
		return err
	}

	down, err := sqliteMigrationFiles("down")
	if err != nil {
		return err
	}
	downByName := make(map[int]string, len(down))
	for _, f := range down {
		downByName[f.version] = f.name
	}

	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version DESC`)
	if err != nil {
		return fmt.Errorf("listing applied migrations: %w", err)
	}
	var appliedVersions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning applied migration: %w", err)
		}
		appliedVersions = append(appliedVersions, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("listing applied migrations: %w", err)
	}
	_ = rows.Close()
	sort.Sort(sort.Reverse(sort.IntSlice(appliedVersions)))

	rolledBack := 0
	for _, version := range appliedVersions {
		if steps > 0 && rolledBack >= steps {
			break
		}
		name, ok := downByName[version]
		if !ok {
			return fmt.Errorf("no down migration for version %d", version)
		}
		content, err := sqliteMigrationsFS.ReadFile("migrations/sqlite/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}
		if _, err := db.Exec(string(content)); err != nil {
			return fmt.Errorf("rolling back migration %s: %w", name, err)
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, version); err != nil {
			return fmt.Errorf("recording rollback of migration %s: %w", name, err)
		}
		rolledBack++
	}
	return nil
}

// migrateSQLiteStatus reads the highest recorded version without creating the
// tracking table (status must not mutate) and lists the still-pending up
// migrations.
func migrateSQLiteStatus(databaseURL string) (MigrationStatus, error) {
	db, err := openSQLiteForMigration(databaseURL)
	if err != nil {
		return MigrationStatus{}, err
	}
	defer func() { _ = db.Close() }()

	var st MigrationStatus
	var tables int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`,
	).Scan(&tables); err != nil {
		return MigrationStatus{}, fmt.Errorf("inspecting sqlite schema: %w", err)
	}
	if tables > 0 {
		var version int
		var dirty bool
		err := db.QueryRow(`SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &dirty)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// table exists but nothing recorded yet
		case err != nil:
			return MigrationStatus{}, fmt.Errorf("reading migration version: %w", err)
		default:
			st.Version = uint(version)
			st.Dirty = dirty
		}
	}

	files, err := sqliteMigrationFiles("up")
	if err != nil {
		return MigrationStatus{}, err
	}
	for _, f := range files {
		if uint(f.version) > st.Version {
			st.Pending = append(st.Pending, uint(f.version))
		}
	}
	return st, nil
}

// parseSQLiteDSN strips the URL scheme, leaving the file path (or :memory:)
// that modernc.org/sqlite expects. Prefixes are checked longest-first so
// "sqlite://" is not left with a stray leading slash by the "sqlite:" case.
func parseSQLiteDSN(databaseURL string) string {
	for _, p := range []string{"sqlite3://", "sqlite://", "sqlite3:", "sqlite:", "file:"} {
		if strings.HasPrefix(databaseURL, p) {
			return strings.TrimPrefix(databaseURL, p)
		}
	}
	return databaseURL
}
