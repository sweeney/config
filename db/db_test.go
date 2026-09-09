//go:build integration

package db_test

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sweeney/config/db"
)

func TestOpen_CreatesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	_, err = os.Stat(path)
	assert.NoError(t, err, "database file should exist on disk")
}

func TestOpen_RunsMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var name string
	err = database.DB().QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='config_namespaces'",
	).Scan(&name)
	require.NoError(t, err, "config_namespaces table should exist after migrations")
	assert.Equal(t, "config_namespaces", name)

	var idxName string
	err = database.DB().QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_config_namespaces_read_role'",
	).Scan(&idxName)
	require.NoError(t, err, "read_role index should exist after migrations")
	assert.Equal(t, "idx_config_namespaces_read_role", idxName)
}

func TestOpen_MigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	for i := range 2 {
		database, err := db.Open(path)
		require.NoError(t, err, "open attempt %d should succeed", i+1)
		database.Close()
	}
}

func TestOpen_WALModeEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var mode string
	err = database.DB().QueryRow("PRAGMA journal_mode").Scan(&mode)
	require.NoError(t, err)
	assert.Equal(t, "wal", mode)
}

func TestOpen_FilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0600), info.Mode().Perm(),
		"database file should be owner-only (0600), got %04o", info.Mode().Perm())
}

func TestOpen_ForeignKeysEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var fkEnabled int
	err = database.DB().QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled)
	require.NoError(t, err)
	assert.Equal(t, 1, fkEnabled)
}

// --- read_role='public' schema rebuild -------------------------------------

// oldSchemaDDL is the schema as it shipped in db/migrations/001_init.sql
// before 'public' became a legal read_role. Tests build databases from this
// directly so they exercise the same upgrade path a production database (or a
// restored R2 backup) takes.
const oldSchemaDDL = `
CREATE TABLE IF NOT EXISTS config_namespaces (
    name        TEXT PRIMARY KEY,
    read_role   TEXT NOT NULL,
    write_role  TEXT NOT NULL,
    document    TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    CHECK (read_role IN ('admin', 'user')),
    CHECK (write_role IN ('admin', 'user'))
);
CREATE INDEX IF NOT EXISTS idx_config_namespaces_read_role
    ON config_namespaces(read_role);
`

// rawOpen opens a SQLite file without going through db.Open, so tests can set
// up and inspect a database without triggering the rebuild. The "sqlite"
// driver is registered transitively by github.com/sweeney/config/db.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })
	return raw
}

// newOldSchemaDB creates a database file carrying the pre-'public' schema and
// returns its path.
func newOldSchemaDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	raw := rawOpen(t, path)
	_, err := raw.Exec(oldSchemaDDL)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	return path
}

func insertNamespace(execer interface {
	Exec(string, ...any) (sql.Result, error)
}, name, readRole, writeRole string) error {
	_, err := execer.Exec(
		`INSERT INTO config_namespaces
		   (name, read_role, write_role, document, updated_at, updated_by, created_at)
		 VALUES (?, ?, ?, '{}', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z')`,
		name, readRole, writeRole,
	)
	return err
}

// TestOldSchema_RejectsPublicReadRole is the baseline: it proves the CHECK
// constraint in the shipped migration really does reject 'public', so the
// green tests below are meaningful rather than vacuous.
func TestOldSchema_RejectsPublicReadRole(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)

	err := insertNamespace(raw, "ns-public", "public", "admin")
	require.Error(t, err, "old schema must reject read_role='public'")
	assert.Contains(t, err.Error(), "CHECK constraint failed")
}

// TestOpen_RebuildAllowsPublicReadRole proves db.Open upgrades an existing
// old-schema database in place so 'public' becomes insertable.
func TestOpen_RebuildAllowsPublicReadRole(t *testing.T) {
	path := newOldSchemaDB(t)

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	err = insertNamespace(database.DB(), "ns-public", "public", "admin")
	assert.NoError(t, err, "read_role='public' should be accepted after rebuild")
}

// TestOpen_RebuildPreservesExistingRows is the data-loss guard: every column of
// every pre-existing row must survive the table rebuild byte-identically.
func TestOpen_RebuildPreservesExistingRows(t *testing.T) {
	type row struct {
		name, readRole, writeRole, document, updatedAt, updatedBy, createdAt string
	}
	seed := []row{
		{"alpha", "admin", "admin", `{"a":1}`, "2020-01-01T00:00:00Z", "alice@example.com", "2019-01-01T00:00:00Z"},
		{"beta", "user", "admin", `{"b":[1,2,3],"nested":{"k":"v"}}`, "2021-02-02T02:02:02Z", "bob@example.com", "2020-02-02T02:02:02Z"},
		{"delta", "admin", "user", `{}`, "2023-04-04T04:04:04Z", "dave@example.com", "2022-04-04T04:04:04Z"},
		{"gamma", "user", "user", `{"unicode":"héllo → 世界","quote":"it's"}`, "2022-03-03T03:03:03Z", "carol@example.com", "2021-03-03T03:03:03Z"},
	}

	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	for _, r := range seed {
		_, err := raw.Exec(
			`INSERT INTO config_namespaces
			   (name, read_role, write_role, document, updated_at, updated_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.name, r.readRole, r.writeRole, r.document, r.updatedAt, r.updatedBy, r.createdAt,
		)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	rows, err := database.DB().Query(
		`SELECT name, read_role, write_role, document, updated_at, updated_by, created_at
		   FROM config_namespaces ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()

	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.name, &r.readRole, &r.writeRole,
			&r.document, &r.updatedAt, &r.updatedBy, &r.createdAt))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, seed, got, "every column of every row must survive the rebuild")
}

// TestOpen_RebuildRecreatesReadRoleIndex proves the index dropped along with
// the old table is put back.
func TestOpen_RebuildRecreatesReadRoleIndex(t *testing.T) {
	path := newOldSchemaDB(t)

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var idxName string
	err = database.DB().QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_config_namespaces_read_role'",
	).Scan(&idxName)
	require.NoError(t, err, "read_role index should exist after rebuild")
	assert.Equal(t, "idx_config_namespaces_read_role", idxName)
}

// TestOpen_RebuildIsIdempotent is the critical test: common/db has no
// migration ledger, so Open runs on every boot. The second Open must detect
// the schema is already correct and do nothing. A sentinel index (which a
// DROP TABLE would take with it) is the tell-tale for an unnecessary rebuild.
func TestOpen_RebuildIsIdempotent(t *testing.T) {
	const sentinelIndex = "idx_rebuild_sentinel"

	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "survivor", "user", "admin"))
	require.NoError(t, raw.Close())

	first, err := db.Open(path)
	require.NoError(t, err)
	_, err = first.DB().Exec(
		"CREATE INDEX " + sentinelIndex + " ON config_namespaces(updated_by)")
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := db.Open(path)
	require.NoError(t, err)
	defer second.Close()

	var idxName string
	err = second.DB().QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name=?", sentinelIndex,
	).Scan(&idxName)
	require.NoError(t, err,
		"sentinel index is gone: the second Open rebuilt the table unnecessarily")

	var count int
	require.NoError(t, second.DB().QueryRow(
		"SELECT COUNT(*) FROM config_namespaces WHERE name='survivor'").Scan(&count))
	assert.Equal(t, 1, count, "pre-existing data must survive a second Open")

	require.NoError(t, insertNamespace(second.DB(), "ns-public", "public", "admin"),
		"schema must still permit 'public' after the second Open")
}

// TestOpen_FreshDatabaseAllowsPublicReadRole covers the no-prior-file path:
// migrations create the old-shape table, the rebuild then widens it.
func TestOpen_FreshDatabaseAllowsPublicReadRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var ddl string
	require.NoError(t, database.DB().QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	assert.Contains(t, ddl, "'public'", "fresh database should end up with the widened DDL")

	assert.NoError(t, insertNamespace(database.DB(), "ns-public", "public", "admin"))
}

// TestOpen_WriteRoleStillRejectsPublic pins the asymmetry: a namespace may be
// readable without a token, but nothing is ever anonymously writable.
func TestOpen_WriteRoleStillRejectsPublic(t *testing.T) {
	path := newOldSchemaDB(t)

	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	err = insertNamespace(database.DB(), "ns-bad", "admin", "public")
	require.Error(t, err, "write_role='public' must remain illegal")
	assert.Contains(t, err.Error(), "CHECK constraint failed")
}
