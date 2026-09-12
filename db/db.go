package db

import (
	"embed"

	commondb "github.com/sweeney/identity/common/db"
)

//go:embed migrations
var migrationsFS embed.FS

type Database = commondb.Database

func Open(path string) (*Database, error) {
	database, err := commondb.OpenWithMigrations(path, migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}

	// Schema steps that cannot be expressed as re-runnable SQL migrations.
	// See schema.go for why this lives here rather than in db/migrations/.
	if err := applySchemaSteps(database.DB(), path); err != nil {
		database.Close()
		return nil, err
	}

	return database, nil
}
