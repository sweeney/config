//go:build integration

package db_test

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// --- migration safety ---
//
// The rebuild is the one genuinely destructive thing this service does to its
// own data. These tests are about what happens when it goes wrong, which the
// happy-path tests above deliberately do not cover.

func snapshotFiles(t *testing.T, dbPath string) []string {
	t.Helper()
	matches, err := filepath.Glob(dbPath + ".pre-public-rebuild-*")
	require.NoError(t, err)
	return matches
}

// TestOpen_RebuildWritesRestorableSnapshot: the snapshot has to be a usable
// database, not just a file that exists. It is written with VACUUM INTO
// rather than copied because the database is in WAL mode, where a plain file
// copy can miss committed data still sitting in the write-ahead log.
func TestOpen_RebuildWritesRestorableSnapshot(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "houses", "admin", "admin"))
	require.NoError(t, insertNamespace(raw, "mqtt", "user", "admin"))
	require.NoError(t, raw.Close())

	database, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	snaps := snapshotFiles(t, path)
	require.Len(t, snaps, 1, "exactly one pre-rebuild snapshot")

	// The snapshot must still be the OLD schema — it is the thing you restore
	// to undo the migration, so it has to predate it.
	snap := rawOpen(t, snaps[0])
	var ddl string
	require.NoError(t, snap.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	assert.NotContains(t, ddl, "'public'", "the snapshot is the pre-migration schema")

	var names []string
	rows, err := snap.Query("SELECT name FROM config_namespaces ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	assert.Equal(t, []string{"houses", "mqtt"}, names, "every row is in the snapshot")
}

// TestOpen_FreshDatabaseWritesNoSnapshot: a fresh install rebuilds an empty
// table, and snapshotting nothing is just litter in the data directory.
func TestOpen_FreshDatabaseWritesNoSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	database, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	assert.Empty(t, snapshotFiles(t, path), "nothing to lose, nothing to snapshot")
}

// TestOpen_AbortsWhenSnapshotCannotBeWritten is the ordering guarantee: if we
// cannot take the backup, we must not perform the migration. A database left
// un-migrated is recoverable; one migrated with no way back is not.
func TestOpen_AbortsWhenSnapshotCannotBeWritten(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "houses", "admin", "admin"))
	require.NoError(t, raw.Close())

	// Pin the clock so the snapshot path is predictable, then occupy that
	// path with a directory so VACUUM INTO cannot create the file.
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	db.SetSnapshotClock(t, at)
	blocked := path + ".pre-public-rebuild-" + at.Format("20060102T150405Z")
	require.NoError(t, os.Mkdir(blocked, 0o755))

	_, err := db.Open(path)
	require.Error(t, err, "Open must fail rather than migrate without a snapshot")
	assert.Contains(t, err.Error(), "snapshot")

	// And the database must be exactly as it was.
	after := rawOpen(t, path)
	var ddl string
	require.NoError(t, after.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	assert.NotContains(t, ddl, "'public'", "schema untouched")
	var n int
	require.NoError(t, after.QueryRow("SELECT COUNT(*) FROM config_namespaces").Scan(&n))
	assert.Equal(t, 1, n, "data untouched")
}

// TestOpen_RebuildRollsBackOnFailure: the whole rebuild runs in one
// transaction so a failure part-way leaves the database wholly un-migrated
// rather than half-migrated. The failure is injected with a view occupying
// the rebuild table's name — DROP TABLE refuses to drop a view — which is an
// arbitrary fault, but the point is that ANY fault mid-rebuild is survivable.
func TestOpen_RebuildRollsBackOnFailure(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "houses", "admin", "admin"))
	require.NoError(t, insertNamespace(raw, "mqtt", "user", "admin"))
	_, err := raw.Exec(
		"CREATE VIEW config_namespaces_rebuild_public AS SELECT 1 AS x")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = db.Open(path)
	require.Error(t, err, "a failed rebuild must surface, not be swallowed")

	after := rawOpen(t, path)
	var ddl string
	require.NoError(t, after.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	assert.NotContains(t, ddl, "'public'", "schema rolled back")

	var names []string
	rows, qerr := after.Query("SELECT name FROM config_namespaces ORDER BY name")
	require.NoError(t, qerr)
	defer rows.Close()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	assert.Equal(t, []string{"houses", "mqtt"}, names, "no rows lost to the failed rebuild")
}

// --- probe misdiagnosis ---
//
// The rebuild is destructive and shape-flattening: it recreates a hard-coded
// seven-column table. So "does this database accept read_role='public'?" must
// never answer "no" for a reason that has nothing to do with the CHECK, and
// the answer must be recorded rather than re-derived on every boot.

func userVersion(t *testing.T, d *sql.DB) int {
	t.Helper()
	var v int
	require.NoError(t, d.QueryRow("PRAGMA user_version").Scan(&v))
	return v
}

// TestOpen_FreshInstallDoesNotRebuild: 001_init.sql now creates the table
// already wide enough, so a new database never runs the destructive path.
// The stored DDL is the tell — only the migration says IF NOT EXISTS; a
// rebuilt table carries the text from newNamespacesDDL instead.
func TestOpen_FreshInstallDoesNotRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	database, err := db.Open(path)
	require.NoError(t, err)
	defer database.Close()

	var ddl string
	require.NoError(t, database.DB().QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	// A rebuilt table has been through ALTER TABLE ... RENAME, which makes
	// SQLite store its name quoted. The migration's own CREATE does not.
	assert.Contains(t, ddl, "CREATE TABLE config_namespaces",
		"a fresh install should keep the migration's table, not a rebuilt one")
	assert.NotContains(t, ddl, `"config_namespaces"`, "an unquoted name means it was never renamed into place")
	assert.Contains(t, ddl, "'public'")
	assert.Equal(t, 1, userVersion(t, database.DB()), "the completed state is recorded")
}

// TestOpen_SkipsOnceRecorded: with the version recorded, a later Open must
// not re-derive anything. A column added by some future migration proves it —
// the rebuild would drop it.
func TestOpen_SkipsOnceRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	first, err := db.Open(path)
	require.NoError(t, err)
	_, err = first.DB().Exec(`ALTER TABLE config_namespaces ADD COLUMN owner TEXT NOT NULL DEFAULT 'alice'`)
	require.NoError(t, err)
	_, err = first.DB().Exec(`CREATE UNIQUE INDEX uq_owner ON config_namespaces(owner)`)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := db.Open(path)
	require.NoError(t, err, "a recorded version means there is nothing left to decide")
	defer second.Close()

	var n int
	require.NoError(t, second.DB().QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('config_namespaces') WHERE name='owner'").Scan(&n))
	assert.Equal(t, 1, n, "the added column must survive")
	require.NoError(t, second.DB().QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uq_owner'").Scan(&n))
	assert.Equal(t, 1, n, "the added index must survive")
}

// TestOpen_ProbeFailureUnrelatedToCheckIsFatal: anything other than the
// read_role CHECK rejecting the probe row is an unexpected database, and
// rebuilding one of those flattens whatever made it unexpected. Fail loudly.
func TestOpen_ProbeFailureUnrelatedToCheckIsFatal(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "houses", "admin", "admin"))
	_, err := raw.Exec(`CREATE TRIGGER no_inserts BEFORE INSERT ON config_namespaces
	                    BEGIN SELECT RAISE(ABORT, 'inserts are blocked'); END`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = db.Open(path)
	require.Error(t, err, "an unexplained probe failure must not be read as 'needs rebuilding'")
	assert.NotContains(t, err.Error(), "snapshot", "it should fail before taking any action")

	after := rawOpen(t, path)
	var ddl string
	require.NoError(t, after.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces'").Scan(&ddl))
	assert.NotContains(t, ddl, "'public'", "nothing was rebuilt")
	assert.Empty(t, snapshotFiles(t, path), "and nothing was written next to the database")
}

// TestOpen_RefusesRebuildWithUnexpectedColumns: the rebuild copies a
// hard-coded column list, so a table carrying anything else would be silently
// amputated — and the row-count check cannot see it, because the counts match.
func TestOpen_RefusesRebuildWithUnexpectedColumns(t *testing.T) {
	path := newOldSchemaDB(t)
	raw := rawOpen(t, path)
	require.NoError(t, insertNamespace(raw, "houses", "admin", "admin"))
	_, err := raw.Exec(`ALTER TABLE config_namespaces ADD COLUMN owner TEXT NOT NULL DEFAULT 'alice'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = db.Open(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner", "the error should name what it refused to drop")

	after := rawOpen(t, path)
	var n int
	require.NoError(t, after.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('config_namespaces') WHERE name='owner'").Scan(&n))
	assert.Equal(t, 1, n, "the column is still there")
}
