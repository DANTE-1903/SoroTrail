package store

import (
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/clickhouse/*.sql
var clickhouseMigrationsFS embed.FS

// clickhouseMigrationsDir is the embedded path migration files live under.
const clickhouseMigrationsDir = "migrations/clickhouse"

// clickhouseMigration is one versioned, embedded DDL file.
type clickhouseMigration struct {
	version uint
	name    string
	sql     string
}

// loadClickHouseMigrations reads and orders the embedded ClickHouse DDL
// files. It is a plain function over the embedded FS (no network access)
// so the ordering and parsing logic is unit-testable without a live
// ClickHouse server.
func loadClickHouseMigrations() ([]clickhouseMigration, error) {
	entries, err := clickhouseMigrationsFS.ReadDir(clickhouseMigrationsDir)
	if err != nil {
		return nil, fmt.Errorf("reading embedded clickhouse migrations: %w", err)
	}

	migrations := make([]clickhouseMigration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		version, err := clickHouseMigrationVersion(e.Name())
		if err != nil {
			return nil, fmt.Errorf("parsing migration filename %q: %w", e.Name(), err)
		}
		content, err := clickhouseMigrationsFS.ReadFile(clickhouseMigrationsDir + "/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", e.Name(), err)
		}
		migrations = append(migrations, clickhouseMigration{version: version, name: e.Name(), sql: string(content)})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// clickHouseMigrationVersion extracts the leading numeric prefix from a
// migration filename, e.g. "0001_init.up.sql" -> 1.
func clickHouseMigrationVersion(name string) (uint, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("expected NNNN_name.up.sql, got %q", name)
	}
	version, err := strconv.ParseUint(prefix, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("expected numeric version prefix, got %q: %w", prefix, err)
	}
	return uint(version), nil
}

// CountEmbeddedClickHouseMigrations returns the number of embedded
// ClickHouse migration versions. Mirrors CountEmbeddedMigrations for the
// Postgres series, used by schema-inspect tooling and staleness checks.
func CountEmbeddedClickHouseMigrations() (int, error) {
	migrations, err := loadClickHouseMigrations()
	if err != nil {
		return 0, err
	}
	return len(migrations), nil
}

// splitStatements splits a migration file's contents on statement-ending
// semicolons. The embedded schema files never put a semicolon inside a
// string literal, so a plain split is sufficient and avoids pulling in a
// SQL parser for DDL we control ourselves.
func splitStatements(script string) []string {
	raw := strings.Split(script, ";")
	statements := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		statements = append(statements, s)
	}
	return statements
}

// clickhouseExecutor runs DDL/DML statements against ClickHouse over its
// HTTP interface. Native TCP (the port clickhouse:// URLs default to)
// requires a client library this module doesn't otherwise depend on; the
// HTTP interface needs nothing beyond net/http, so it's what the migration
// path and the stub-filling work in #586 both use.
type clickhouseExecutor struct {
	cfg    clickHouseConfig
	client *http.Client
}

func newClickHouseExecutor(cfg clickHouseConfig) *clickhouseExecutor {
	return &clickhouseExecutor{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

// exec runs a single statement against the given database ("" uses the
// server's default database, which is required for CREATE DATABASE itself
// since the target database doesn't exist yet to select it).
func (e *clickhouseExecutor) exec(ctx context.Context, database, statement string) error {
	scheme := "http"
	if e.cfg.ssl {
		scheme = "https"
	}
	endpoint := fmt.Sprintf("%s://%s:%d/", scheme, e.cfg.host, e.cfg.httpPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(statement))
	if err != nil {
		return fmt.Errorf("building clickhouse request: %w", err)
	}
	if database != "" {
		q := req.URL.Query()
		q.Set("database", database)
		req.URL.RawQuery = q.Encode()
	}
	if e.cfg.username != "" {
		req.SetBasicAuth(e.cfg.username, e.cfg.password)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clickhouse returned %d: %s (statement: %s)", resp.StatusCode, strings.TrimSpace(string(body)), truncateForError(statement))
	}
	return nil
}

// query runs a statement expected to return a single scalar value in
// ClickHouse's default TSV output, and returns the raw trimmed result.
func (e *clickhouseExecutor) query(ctx context.Context, database, statement string) (string, error) {
	scheme := "http"
	if e.cfg.ssl {
		scheme = "https"
	}
	endpoint := fmt.Sprintf("%s://%s:%d/", scheme, e.cfg.host, e.cfg.httpPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(statement))
	if err != nil {
		return "", fmt.Errorf("building clickhouse request: %w", err)
	}
	if database != "" {
		q := req.URL.Query()
		q.Set("database", database)
		req.URL.RawQuery = q.Encode()
	}
	if e.cfg.username != "" {
		req.SetBasicAuth(e.cfg.username, e.cfg.password)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("clickhouse request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading clickhouse response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clickhouse returned %d: %s (statement: %s)", resp.StatusCode, strings.TrimSpace(string(body)), truncateForError(statement))
	}
	return strings.TrimSpace(string(body)), nil
}

// queryInt64 runs a statement expected to return a single integer scalar
// (e.g. a count(*)) and parses it. Used by CountEvents, CountContracts,
// Stats and the retention methods in clickhouse.go, all of which need
// exactly this shape.
func (e *clickhouseExecutor) queryInt64(ctx context.Context, database, statement string) (int64, error) {
	out, err := e.query(ctx, database, statement+" FORMAT TabSeparated")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing scalar result %q: %w", out, err)
	}
	return v, nil
}

// queryColumn runs a statement expected to return a single string column
// and returns one entry per row. Used by DeleteEventsBefore to select the
// batch of ids to delete.
func (e *clickhouseExecutor) queryColumn(ctx context.Context, database, statement string) ([]string, error) {
	out, err := e.query(ctx, database, statement+" FORMAT TabSeparated")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		values = append(values, line)
	}
	return values, nil
}

func truncateForError(s string) string {
	const max = 200
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// migrateClickHouse applies the embedded ClickHouse DDL series to the
// database addressed by databaseURL. It is idempotent: every statement is
// CREATE ... IF NOT EXISTS, and already-applied versions (tracked in
// schema_migrations) are skipped. After applying, it re-checks that the
// tracked version matches the embedded set, which is what turns a
// partially-applied or out-of-band-modified schema into a clear startup
// error instead of the previous silent no-op.
func migrateClickHouse(databaseURL string) error {
	cfg, err := parseClickHouseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parsing clickhouse url: %w", err)
	}

	migrations, err := loadClickHouseMigrations()
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("no embedded clickhouse migrations found")
	}

	exec := newClickHouseExecutor(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	database := cfg.database
	if database == "" {
		database = "default"
	}

	// The target database must exist before any statement can select it,
	// so this one runs against the server's default database.
	if database != "default" {
		if err := exec.exec(ctx, "", fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", quoteIdentifier(database))); err != nil {
			return fmt.Errorf("creating clickhouse database %q: %w", database, err)
		}
	}

	applied, err := appliedClickHouseVersions(ctx, exec, database)
	if err != nil {
		return fmt.Errorf("reading applied clickhouse migrations: %w", err)
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		for _, stmt := range splitStatements(m.sql) {
			if err := exec.exec(ctx, database, stmt); err != nil {
				return fmt.Errorf("applying clickhouse migration %s: %w", m.name, err)
			}
		}
		insert := fmt.Sprintf("INSERT INTO schema_migrations (version) VALUES (%d)", m.version)
		if err := exec.exec(ctx, database, insert); err != nil {
			return fmt.Errorf("recording clickhouse migration %s: %w", m.name, err)
		}
	}

	// Re-read what's actually recorded and compare against the embedded
	// set. A mismatch here means the schema is stale or was modified
	// out-of-band (e.g. schema_migrations rows were deleted, or a
	// concurrent deploy is mid-migration) — surface that loudly rather
	// than letting the app start against an incomplete schema.
	final, err := appliedClickHouseVersions(ctx, exec, database)
	if err != nil {
		return fmt.Errorf("verifying clickhouse schema: %w", err)
	}
	for _, m := range migrations {
		if !final[m.version] {
			return fmt.Errorf("clickhouse schema is stale: migration %s (version %d) is not recorded as applied", m.name, m.version)
		}
	}

	return nil
}

// appliedClickHouseVersions queries schema_migrations for the set of
// versions already recorded. It tolerates the table not existing yet
// (fresh database) by creating it first — schema_migrations' own
// CREATE TABLE IF NOT EXISTS is itself the first embedded migration
// statement, so this only matters before that first migration has run.
func appliedClickHouseVersions(ctx context.Context, exec *clickhouseExecutor, database string) (map[uint]bool, error) {
	bootstrap := `CREATE TABLE IF NOT EXISTS schema_migrations (version UInt32, applied_at DateTime64(3) DEFAULT now64(3)) ENGINE = ReplacingMergeTree(applied_at) ORDER BY version`
	if err := exec.exec(ctx, database, bootstrap); err != nil {
		return nil, fmt.Errorf("bootstrapping schema_migrations: %w", err)
	}

	out, err := exec.query(ctx, database, "SELECT DISTINCT version FROM schema_migrations FINAL ORDER BY version FORMAT TabSeparated")
	if err != nil {
		return nil, err
	}

	applied := make(map[uint]bool)
	if out == "" {
		return applied, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.ParseUint(line, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing schema_migrations row %q: %w", line, err)
		}
		applied[uint(v)] = true
	}
	return applied, nil
}

// quoteIdentifier backtick-quotes a ClickHouse identifier. Database names
// here come from operator-controlled DATABASE_URL config, not end-user
// input, but quoting is nearly free and avoids relying on that.
func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
