package db

import (
	"embed"

	commondb "github.com/sweeney/identity/common/db"
)

//go:embed migrations
var migrationsFS embed.FS

type Database = commondb.Database

func Open(path string) (*Database, error) {
	return commondb.OpenWithMigrations(path, migrationsFS, "migrations")
}
