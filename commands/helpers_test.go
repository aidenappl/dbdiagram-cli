package commands

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// runSQL creates a SQLite database at path and runs stmts.
func runSQL(t *testing.T, path string, stmts ...string) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
