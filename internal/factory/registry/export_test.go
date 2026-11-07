package registry

import "database/sql"

// DBForTest gives the external test package the raw handle, for the schema
// checks that no typed method can reach.
func (r *Registry) DBForTest() *sql.DB { return r.db }

// MigrationSQLForTest is the text of the embedded migration with the given
// version, for a test that builds a database as an older binary left it.
func MigrationSQLForTest(version int) string { return migrations[version-1].SQL }

// OpenDBOutsideATestBinary is OpenDB as a production binary runs it, which
// refuses: a test binary cannot see that refusal through OpenDB.
func OpenDBOutsideATestBinary(db *sql.DB, opts Options) (*Registry, error) {
	return openDB(db, opts, false)
}
