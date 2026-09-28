package sqlite

import "database/sql"

// openRaw opens a database with no pragmas and no migrations, so a test can
// build a deliberately wrong database to check that Open rejects it.
func openRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}
