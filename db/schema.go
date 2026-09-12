package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
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

// schemaVersionPublicReadRole is written to PRAGMA user_version once the
// table is known to accept read_role='public'.
//
// Recording it matters more than it looks. Without it the answer is
// re-derived from scratch on every Open, which means a probe that fails for
// some unrelated reason sends a perfectly healthy database into a
// destructive rebuild — repeatedly, writing a fresh full-size snapshot each
// boot. With it, the question is asked once.
const schemaVersionPublicReadRole = 1

// expectedNamespaceColumns is the exact column set the rebuild knows how to
// carry across. Anything else and it must refuse: newNamespacesDDL is
// hard-coded and the copy names these columns explicitly, so an unrecognised
// column would be dropped silently — and the row-count check cannot see it,
// because the counts still match.
var expectedNamespaceColumns = []string{
	"created_at", "document", "name", "read_role", "updated_at", "updated_by", "write_role",
}

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
	var recorded int
	if err := sqlDB.QueryRow("PRAGMA user_version").Scan(&recorded); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if recorded >= schemaVersionPublicReadRole {
		// Logged on every boot, deliberately. The fast path makes this a true
		// no-op, but silence would leave "checked and settled" and "a binary
		// that never checked" looking identical in the journal, which is the
		// question an operator actually has after a deploy.
		log.Printf("config db: schema check — public read role settled (schema version %d), nothing to do",
			recorded)
		return nil
	}

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
		log.Printf("config db: schema check — read_role already permits 'public', no rebuild needed")
		return recordSchemaVersion(sqlDB)
	}

	// The rebuild flattens the table to expectedNamespaceColumns. If this
	// database carries anything else, refuse rather than amputate it.
	if err := assertNoUnexpectedColumns(sqlDB); err != nil {
		return err
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
	// Confirm the rebuild actually achieved what it was for. If the probe had
	// misdiagnosed something, this turns a silent amputation into one failed
	// start.
	permitted, err = permitsPublicReadRole(sqlDB)
	if err != nil {
		return err
	}
	if !permitted {
		return fmt.Errorf(
			"rebuilt %s but it still rejects read_role='public' — refusing to continue",
			namespacesTable)
	}

	log.Printf("config db: schema rebuild complete — %d row(s) migrated in %s",
		rows, time.Since(started).Round(time.Microsecond))
	return recordSchemaVersion(sqlDB)
}

// recordSchemaVersion marks the public-read-role migration as settled.
func recordSchemaVersion(sqlDB *sql.DB) error {
	// PRAGMA does not accept a bound parameter, and the value is a constant.
	stmt := fmt.Sprintf("PRAGMA user_version = %d", schemaVersionPublicReadRole)
	if _, err := sqlDB.Exec(stmt); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

// assertNoUnexpectedColumns refuses to rebuild a table whose shape this code
// does not recognise.
func assertNoUnexpectedColumns(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query("SELECT name FROM pragma_table_info(?)", namespacesTable)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", namespacesTable, err)
	}
	defer rows.Close()

	var unexpected []string
	known := make(map[string]bool, len(expectedNamespaceColumns))
	for _, c := range expectedNamespaceColumns {
		known[c] = true
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan %s columns: %w", namespacesTable, err)
		}
		if !known[name] {
			unexpected = append(unexpected, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s columns: %w", namespacesTable, err)
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return fmt.Errorf(
			"refusing to rebuild %s: unrecognised column(s) %s would be dropped by the rebuild",
			namespacesTable, strings.Join(unexpected, ", "))
	}
	return nil
}

// permitsPublicReadRole asks the database itself, rather than
// pattern-matching the stored DDL: it attempts a fully valid insert whose
// only unusual value is read_role='public', inside a transaction that is
// always rolled back.
//
// It runs a positive control first — the identical insert with a role that
// is valid under both the old and the new CHECK. The two attempts differ in
// exactly one value, so a control that succeeds where the real probe fails
// isolates the read_role constraint as the thing that rejected it, whatever
// the driver's error message happens to say.
//
// That differential matters more than it looks. Classifying by error text
// worked only because the CHECK is unnamed and SQLite renders the expression
// (which contains "read_role"); a named constraint reports its name instead,
// and a driver upgrade could reword the message. Either would make an
// ordinary rejection look like an unexplained failure and refuse the boot —
// on the code path that runs exactly once per host, in production, during
// the upgrade this change exists for. Reading semantics instead of text is
// the same argument that made this a probe rather than a DDL match.
func permitsPublicReadRole(sqlDB *sql.DB) (bool, error) {
	tx, err := sqlDB.Begin()
	if err != nil {
		return false, fmt.Errorf("begin read_role probe: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // the probe must never commit

	if err := probeInsertRole(tx, "user"); err != nil {
		return false, fmt.Errorf(
			"read_role probe control failed on %s: the table rejects a probe row even with an "+
				"unambiguously valid role, so this says nothing about the read_role CHECK: %w",
			namespacesTable, err)
	}
	if err := probeInsertRole(tx, ConfigRolePublicLiteral); err != nil {
		// The control succeeded moments ago and only the role differs, so the
		// read_role constraint is what refused it.
		return false, nil
	}
	return true, nil
}

// ConfigRolePublicLiteral is the role value the probe tests for. Spelled out
// here rather than imported so this file stays free of domain dependencies;
// db/migrations owns the same literal.
const ConfigRolePublicLiteral = "public"

// probeInsertRole writes the sentinel row with the given read role, clearing
// any previous attempt first so a primary-key collision can never be mistaken
// for a rejected CHECK.
//
// The DELETE belongs inside this function, not hoisted around the pair of
// calls in permitsPublicReadRole. Both attempts write the same primary key,
// so a surviving control row would collide with the real probe and every
// database would read as "not permitted" — rebuilding unconditionally, on
// every boot, including databases that are already correct.
func probeInsertRole(tx *sql.Tx, readRole string) error {
	if _, err := tx.Exec(
		"DELETE FROM "+namespacesTable+" WHERE name = ?", probeNamespace,
	); err != nil {
		return fmt.Errorf("read_role probe cleanup: %w", err)
	}
	_, err := tx.Exec(
		`INSERT INTO `+namespacesTable+`
		   (name, read_role, write_role, document, updated_at, updated_by, created_at)
		 VALUES (?, ?, 'admin', '{}', '', '', '')`,
		probeNamespace, readRole,
	)
	return err
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
