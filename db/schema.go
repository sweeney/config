package db

import (
	"database/sql"
	"errors"
	"fmt"
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

// ensurePublicReadRole brings config_namespaces to a shape whose read_role
// CHECK permits 'public'. It is a no-op when the table is absent (migrations
// own creating it) or already wide enough, so it is safe to call on every Open.
func ensurePublicReadRole(sqlDB *sql.DB) error {
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
		return nil
	}

	return rebuildNamespacesTable(sqlDB)
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
func rebuildNamespacesTable(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin %s rebuild: %w", namespacesTable, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	stmts := []string{
		"DROP TABLE IF EXISTS " + rebuildTable,
		fmt.Sprintf(newNamespacesDDL, rebuildTable),
		"INSERT INTO " + rebuildTable + `
		   (name, read_role, write_role, document, updated_at, updated_by, created_at)
		 SELECT name, read_role, write_role, document, updated_at, updated_by, created_at
		   FROM ` + namespacesTable,
		"DROP TABLE " + namespacesTable,
		"ALTER TABLE " + rebuildTable + " RENAME TO " + namespacesTable,
		"CREATE INDEX IF NOT EXISTS " + readRoleIndex +
			" ON " + namespacesTable + "(read_role)",
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rebuild %s: %w", namespacesTable, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s rebuild: %w", namespacesTable, err)
	}
	return nil
}
