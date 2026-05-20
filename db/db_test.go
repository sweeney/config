//go:build integration

package db_test

import (
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
