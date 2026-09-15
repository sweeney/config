//go:build integration

package db_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sweeney/config/db"
)

// common/v0.6.0 records applied migrations in a schema_migrations ledger
// instead of replaying every file on every boot. A database created by an
// earlier build has the full schema but no ledger, and the runner adopts it by
// re-running each migration once and recording the result.
//
// That re-run is the risk the tests below pin down. It is a no-op for this
// service only because every migration is either CREATE ... IF NOT EXISTS or a
// bare ADD COLUMN, which the runner tolerates. A future migration of any other
// shape — an unguarded CREATE INDEX, a seed INSERT — would execute a second
// time against every deployed database, so these tests are the place that
// catches it before a deploy does.

// migrationLedger returns the migration names recorded in the ledger.
func migrationLedger(t *testing.T, path string) []string {
	t.Helper()
	raw := rawOpen(t, path)
	rows, err := raw.Query(`SELECT name FROM schema_migrations ORDER BY name`)
	require.NoError(t, err, "schema_migrations should exist")
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// dropLedger turns a fully-migrated database into what a pre-v0.6.0 build left
// behind: the schema in place, and nothing recording how it got there.
func dropLedger(t *testing.T, path string) {
	t.Helper()
	raw := rawOpen(t, path)
	_, err := raw.Exec(`DROP TABLE schema_migrations`)
	require.NoError(t, err)
}

func TestOpen_RecordsMigrationsInLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	assert.Equal(t, []string{
		"001_init.sql",
		"002_config_audit.sql",
		"003_audit_actor_username.sql",
		"004_namespace_updated_by_username.sql",
	}, migrationLedger(t, path), "every shipped migration should be recorded")
}

// TestOpen_AdoptsPreLedgerDatabase is the upgrade boot: a database migrated by
// common/v0.5.0 must open cleanly and end up with a complete ledger.
func TestOpen_AdoptsPreLedgerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, insertNamespace(database.DB(), "mqtt", "user", "admin"))
	require.NoError(t, database.Close())

	dropLedger(t, path)

	adopted, err := db.Open(path)
	require.NoError(t, err, "adopting a pre-ledger database must not fail the boot")
	defer adopted.Close()

	assert.Len(t, migrationLedger(t, path), 4, "adoption should record every migration")

	var namespaces int
	require.NoError(t, adopted.DB().QueryRow(`SELECT COUNT(*) FROM config_namespaces`).Scan(&namespaces))
	assert.Equal(t, 1, namespaces, "adoption must not disturb existing rows")
}

// TestOpen_AdoptionPreservesUnbackfilledColumns guards the two ADD COLUMN
// migrations specifically. Both are deliberately not backfilled, so a re-run
// that actually executed would be visible as a column reset to NULL — or, if
// the runner stopped tolerating it, as a failed boot.
func TestOpen_AdoptionPreservesUnbackfilledColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, insertNamespace(database.DB(), "mqtt", "user", "admin"))
	_, err = database.DB().Exec(
		`UPDATE config_namespaces SET updated_by_username = 'martin' WHERE name = 'mqtt'`)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	dropLedger(t, path)

	adopted, err := db.Open(path)
	require.NoError(t, err)
	defer adopted.Close()

	var username string
	require.NoError(t, adopted.DB().QueryRow(
		`SELECT updated_by_username FROM config_namespaces WHERE name = 'mqtt'`).Scan(&username))
	assert.Equal(t, "martin", username, "the ADD COLUMN re-run must be skipped, not applied")
}

// TestOpen_SteadyStateSkipsAppliedMigrations proves the ledger is actually
// consulted: once recorded, a migration is never offered again.
func TestOpen_SteadyStateSkipsAppliedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	before := migrationLedger(t, path)

	second, err := db.Open(path)
	require.NoError(t, err)
	require.NoError(t, second.Close())

	assert.Equal(t, before, migrationLedger(t, path), "a second boot should add no rows")
}
