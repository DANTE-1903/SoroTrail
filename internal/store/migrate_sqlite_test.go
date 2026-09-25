package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTempSQLiteURL returns a sqlite DATABASE_URL backed by a fresh temp file so
// migration state survives across calls (needed for up/down/status sequences).
func newTempSQLiteURL(t *testing.T) string {
	t.Helper()
	return "sqlite:" + filepath.Join(t.TempDir(), "migrate.db")
}

// sqliteUpVersions returns the embedded SQLite up-migration versions in
// ascending order, so these tests stay valid as migrations are added.
func sqliteUpVersions(t *testing.T) []uint {
	t.Helper()
	files, err := sqliteMigrationFiles("up")
	require.NoError(t, err)
	require.NotEmpty(t, files, "the SQLite series must have at least one migration")
	versions := make([]uint, 0, len(files))
	for _, f := range files {
		versions = append(versions, uint(f.version))
	}
	return versions
}

func sqliteLatestVersion(t *testing.T) uint {
	t.Helper()
	versions := sqliteUpVersions(t)
	return versions[len(versions)-1]
}

func sqliteTableExists(t *testing.T, databaseURL, table string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", parseSQLiteDSN(databaseURL))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n))
	return n > 0
}

func TestMigrateStatusSQLite_ReportsPendingBeforeUp(t *testing.T) {
	url := newTempSQLiteURL(t)

	st, err := MigrateStatus(url)
	require.NoError(t, err)
	assert.Zero(t, st.Version, "a fresh database has no applied migrations")
	assert.False(t, st.Dirty)
	assert.Equal(t, sqliteUpVersions(t), st.Pending, "every migration should be pending")
}

func TestMigrateUpDownStatusSQLite(t *testing.T) {
	url := newTempSQLiteURL(t)
	versions := sqliteUpVersions(t)
	latest := versions[len(versions)-1]

	// up: applies every pending migration and clears the pending list.
	require.NoError(t, MigrateUp(url, 0))
	assert.True(t, sqliteTableExists(t, url, "events"), "up must create the schema")

	st, err := MigrateStatus(url)
	require.NoError(t, err)
	assert.Equal(t, latest, st.Version)
	assert.False(t, st.Dirty)
	assert.Empty(t, st.Pending, "nothing pending once applied")

	// up again is a no-op (idempotent).
	require.NoError(t, MigrateUp(url, 0))

	// down with steps=1 rolls back exactly the newest migration.
	require.NoError(t, MigrateDown(url, 1))
	st, err = MigrateStatus(url)
	require.NoError(t, err)
	assert.Equal(t, []uint{latest}, st.Pending, "only the newest migration is pending")
	assert.True(t, sqliteTableExists(t, url, "events"), "0001 is still applied")

	// down with steps=0 rolls back everything and drops the schema.
	require.NoError(t, MigrateDown(url, 0))
	st, err = MigrateStatus(url)
	require.NoError(t, err)
	assert.Zero(t, st.Version)
	assert.Equal(t, versions, st.Pending, "the rolled-back migrations are pending again")
	assert.False(t, sqliteTableExists(t, url, "events"), "down must drop the schema")

	// down again with nothing applied is a no-op.
	require.NoError(t, MigrateDown(url, 1))
}

func TestMigrateDownSQLite_ZeroStepsRollsBackEverything(t *testing.T) {
	url := newTempSQLiteURL(t)
	require.NoError(t, MigrateUp(url, 0))

	require.NoError(t, MigrateDown(url, 0))

	st, err := MigrateStatus(url)
	require.NoError(t, err)
	assert.Zero(t, st.Version)
	assert.Equal(t, sqliteUpVersions(t), st.Pending)
}

func TestMigrateLegacyEntryPointSQLite(t *testing.T) {
	url := newTempSQLiteURL(t)

	// Migrate is the startup alias for MigrateUp(url, 0); it must keep working.
	require.NoError(t, Migrate(url))
	st, err := MigrateStatus(url)
	require.NoError(t, err)
	assert.Equal(t, sqliteLatestVersion(t), st.Version)
	assert.Empty(t, st.Pending)

	// The historical GetMigrationStatus entry point must agree.
	legacy, err := GetMigrationStatus(url)
	require.NoError(t, err)
	assert.Equal(t, st, legacy)
}

func TestMigrateStatus_SQLiteStoreIsUsableAfterUp(t *testing.T) {
	url := newTempSQLiteURL(t)
	require.NoError(t, MigrateUp(url, 0))

	db, err := sql.Open("sqlite", parseSQLiteDSN(url))
	require.NoError(t, err)
	defer db.Close()

	st := NewSQLite(db)
	require.NoError(t, st.Ping(context.Background()))
	_, _, err = st.QueryEvents(context.Background(), EventFilter{Limit: 1})
	require.NoError(t, err)
}

func TestMigrateUp_UnsupportedScheme(t *testing.T) {
	err := MigrateUp("mysql://user@host/db", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database url scheme")
}

func TestMigrateStatus_ClickHouseUnsupported(t *testing.T) {
	// ClickHouse has an apply-only series, so status/down report that clearly
	// without attempting a connection.
	_, err := MigrateStatus("clickhouse://localhost:9000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clickhouse")

	err = MigrateDown("clickhouse://localhost:9000", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clickhouse")
}
