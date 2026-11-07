package cli

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The CLI's own connections to the registry keep their sorts and temp
// tables in memory, as store.Open does: a spill would write an etilqs_*
// file to $TMPDIR on the internal disk.
func TestTheRegistryDSNsKeepTempTablesInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	storetest.OpenFile(t, path)
	for name, dsn := range map[string]string{"read-only": registryReadOnlyDSN(path), "write": registryWriteDSN(path)} {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		var temp int
		if err := db.QueryRow("PRAGMA temp_store").Scan(&temp); err != nil || temp != 2 {
			t.Errorf("%s DSN: temp_store = %d, %v; want 2 (MEMORY)", name, temp, err)
		}
		db.Close()
	}
}
