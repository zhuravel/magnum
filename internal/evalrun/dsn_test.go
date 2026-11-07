package evalrun

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The replay's read-only connection to the registry keeps its sorts and
// temp tables in memory, as store.Open does.
func TestTheReadOnlyDSNKeepsTempTablesInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magnum.db")
	storetest.OpenFile(t, path)
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var temp int
	if err := db.QueryRow("PRAGMA temp_store").Scan(&temp); err != nil || temp != 2 {
		t.Fatalf("temp_store = %d, %v; want 2 (MEMORY)", temp, err)
	}
}
