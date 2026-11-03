package registry

import "database/sql"

// DBForTest gives the external test package the raw handle, for the schema
// checks that no typed method can reach.
func (r *Registry) DBForTest() *sql.DB { return r.db }
