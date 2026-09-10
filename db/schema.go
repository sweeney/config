package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// The read_role CHECK constraint on config_namespaces has to gain 'public'.
// SQLite cannot ALTER a CHECK constraint, so widening it means the full
// table-rebuild dance, and that cannot live in db/migrations/: common/db's
// migration runner keeps no ledger — it Execs every migration file on every
// startup and relies on CREATE TABLE IF NOT EXISTS for idempotence. A rebuild
// expressed as SQL there would therefore re-run on every boot.
//
// It also cannot be a one-off run by hand. `config-server --restore-backup`
// pulls an older SQLite file down from R2 and drops it into place; a restore of
// a pre-migration backup would silently revert the schema and every public
// namespace would start failing at the CHECK. Running the rebuild inside Open,
// guarded by an "is it already done?" probe, makes restores self-healing.
//
// Note the asymmetry: write_role deliberately does NOT gain 'public'. A
// namespace may be readable without a token; nothing is ever anonymously
// writable.

const (
	namespacesTable = "config_namespaces"
	rebuildTable    = "config_namespaces_rebuild_public"
	readRoleIndex   = "idx_config_namespaces_read_role"

	// probeNamespace is the sentinel row name used to ask the database
	// whether it accepts read_role='public'. It is always rolled back.
	probeNamespace = "__config_public_read_role_probe__"
)

// newNamespacesDDL is the target shape. %s is the table name so the same text
// serves both the rebuild table and (after RENAME) the real one.
const newNamespacesDDL = `CREATE TABLE %s (
    name        TEXT PRIMARY KEY,
    read_role   TEXT NOT NULL,
    write_role  TEXT NOT NULL,
    document    TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    CHECK (read_role IN ('admin', 'user', 'public')),
    CHECK (write_role IN ('admin', 'user'))
)`

// snapshotClock names pre-rebuild snapshots. A variable so tests can pin it.
var snapshotClock = func() time.Time { return time.Now().UTC() }

// snapshotBeforeRebuild writes a consistent copy of the database next to it
// and returns the path.
//
// VACUUM INTO rather than a file copy: the database runs in WAL mode, where
// committed transactions may still be sitting in the -wal file, so copying
// the main database file alone can silently lose them.
//
// The name carries a UTC timestamp so a second attempt can never overwrite
// the snapshot from the first — the older one is the more original, and
// clobbering it is exactly the mistake you cannot undo.
func snapshotBeforeRebuild(sqlDB *sql.DB, dbPath string) (string, error) {
	target := fmt.Sprintf("%s.pre-public-rebuild-%s",
		dbPath, snapshotClock().Format("20060102T150405Z"))
	if _, err := sqlDB.Exec("VACUUM INTO ?", target); err != nil {
		return "", fmt.Errorf("write pre-rebuild snapshot to %s: %w", target, err)
	}
	return target, nil
}

// ensurePublicReadRole brings config_namespaces to a shape whose read_role
// CHECK permits 'public'. It is a no-op when the table is absent (migrations
// own creating it) or already wide enough, so it is safe to call on every Open.
func ensurePublicReadRole(sqlDB *sql.DB, dbPath string) error {
	var existingDDL string
	err := sqlDB.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", namespacesTable,
	).Scan(&existingDDL)
	if errors.Is(err, sql.ErrNoRows) {
		// No table: nothing to rebuild.
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", namespacesTable, err)
	}

	permitted, err := permitsPublicReadRole(sqlDB)
	if err != nil {
		return err
	}
	if permitted {
		// Logged on every boot, deliberately: without it, "did the rebuild
		// run?" is unanswerable from the journal, because a silent skip and
		// a version that never checked look identical.
		log.Printf("config db: schema check — read_role already permits 'public', no rebuild needed")
		return nil
	}

	var rows int
	if err := sqlDB.QueryRow("SELECT COUNT(*) FROM " + namespacesTable).Scan(&rows); err != nil {
		return fmt.Errorf("count %s before rebuild: %w", namespacesTable, err)
	}

	// Snapshot only when there is something to lose. A fresh install rebuilds
	// an empty table on first boot, and snapshotting nothing is just litter.
	if rows > 0 {
		snapshot, err := snapshotBeforeRebuild(sqlDB, dbPath)
		if err != nil {
			// Deliberately fatal. A database left un-migrated is recoverable;
			// one migrated with no way back is not.
			return err
		}
		log.Printf("config db: schema rebuild required for %d row(s); snapshot written to %s",
			rows, snapshot)
	} else {
		log.Printf("config db: schema rebuild required; table is empty, no snapshot taken")
	}

	// time.Now() rather than snapshotClock: the latter is pinned by tests and
	// UTC-normalised, which discards the monotonic reading a duration needs.
	started := time.Now()
	if err := rebuildNamespacesTable(sqlDB, rows); err != nil {
		return err
	}
	log.Printf("config db: schema rebuild complete — %d row(s) migrated in %s",
		rows, time.Since(started).Round(time.Microsecond))
	return nil
}

// permitsPublicReadRole asks the database itself, rather than pattern-matching
// the stored DDL: it attempts a fully valid insert whose only unusual value is
// read_role='public', inside a transaction that is always rolled back. This
// tests the exact semantics the rebuild exists to establish, so it cannot be
// fooled by whitespace, comment or quoting differences in the DDL text.
//
// The DELETE ahead of the INSERT removes any same-named row so a primary key
// collision cannot be mistaken for a rejected CHECK. Both statements are
// discarded by the rollback.
func permitsPublicReadRole(sqlDB *sql.DB) (bool, error) {
	tx, err := sqlDB.Begin()
	if err != nil {
		return false, fmt.Errorf("begin read_role probe: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // the probe must never commit

	if _, err := tx.Exec(
		"DELETE FROM "+namespacesTable+" WHERE name = ?", probeNamespace,
	); err != nil {
		return false, fmt.Errorf("read_role probe cleanup: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO `+namespacesTable+`
		   (name, read_role, write_role, document, updated_at, updated_by, created_at)
		 VALUES (?, 'public', 'admin', '{}', '', '', '')`,
		probeNamespace,
	)
	return err == nil, nil
}

// rebuildNamespacesTable performs the SQLite table-rebuild dance in one
// transaction: build the new shape alongside, copy every column across, drop
// the old table and rename the new one into place. Dropping a table drops its
// indexes too, so the read_role index is recreated afterwards.
//
// PRAGMA foreign_keys is deliberately left alone — there are no foreign keys
// on this table, and common/db turns it on for a reason.
func rebuildNamespacesTable(sqlDB *sql.DB, expectedRows int) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin %s rebuild: %w", namespacesTable, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Copy first, verify, and only then destroy anything. The verification
	// has to sit between the two: after the RENAME the rebuild table no
	// longer exists to be counted.
	copyStmts := []string{
		"DROP TABLE IF EXISTS " + rebuildTable,
		fmt.Sprintf(newNamespacesDDL, rebuildTable),
		"INSERT INTO " + rebuildTable + `
		   (name, read_role, write_role, document, updated_at, updated_by, created_at)
		 SELECT name, read_role, write_role, document, updated_at, updated_by, created_at
		   FROM ` + namespacesTable,
	}
	for _, stmt := range copyStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rebuild %s: %w", namespacesTable, err)
		}
	}

	// Belt and braces before the point of no return. INSERT ... SELECT is
	// all-or-nothing, so a mismatch should be impossible — but this is the
	// one operation where "should be impossible" is not good enough, and the
	// check turns a silent loss into a rollback with the original intact.
	var copied int
	if err := tx.QueryRow("SELECT COUNT(*) FROM " + rebuildTable).Scan(&copied); err != nil {
		return fmt.Errorf("verify %s rebuild: %w", namespacesTable, err)
	}
	if copied != expectedRows {
		return fmt.Errorf(
			"refusing to complete %s rebuild: copied %d row(s), expected %d",
			namespacesTable, copied, expectedRows)
	}

	swapStmts := []string{
		"DROP TABLE " + namespacesTable,
		"ALTER TABLE " + rebuildTable + " RENAME TO " + namespacesTable,
		"CREATE INDEX IF NOT EXISTS " + readRoleIndex +
			" ON " + namespacesTable + "(read_role)",
	}
	for _, stmt := range swapStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rebuild %s: %w", namespacesTable, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s rebuild: %w", namespacesTable, err)
	}
	return nil
}
