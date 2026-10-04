package cli

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// olderRegistry leaves an empty registry at schema 0 in f's layout, standing
// in for one an older daemon opened: this build would migrate it.
func olderRegistry(t *testing.T, f *inspFixture) {
	t.Helper()
	if err := f.Ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.Ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE marker (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
}

func registryVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestCLIRefusesToMigrateUnderARunningDaemon: a CLI command of a newer build
// does not migrate the registry while a magnum daemon runs on it (the daemon
// would exit mid-round); it names daemon-restart --drain. A pidfile naming a
// process that is not a magnum daemon does not block, and with no daemon the
// CLI migrates as before.
func TestCLIRefusesToMigrateUnderARunningDaemon(t *testing.T) {
	const pid = 4242
	for _, tc := range []struct {
		name    string
		pid     int
		comm    string
		refused bool
	}{
		{"daemon running", pid, "/repo/bin/magnum", true},
		{"recycled pid", pid, "/usr/bin/vim", false},
		{"no daemon", 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInspFixture(t)
			olderRegistry(t, f)
			old := daemonSys
			t.Cleanup(func() { daemonSys = old })
			daemonSys.DaemonPID = func(paths.Layout) (int, error) { return tc.pid, nil }
			daemonSys.Runner = &execx.Fake{Rules: []execx.Rule{
				daemonRuleOK([]string{"ps", "-p", strconv.Itoa(pid), "-o", "comm="}, tc.comm+"\n"),
				daemonRuleOK([]string{"ps", "-p", strconv.Itoa(pid), "-o", "args="}, tc.comm+" daemon\n"),
			}}
			a, err := inspOpenApp(f.Ctx, false)
			if tc.refused {
				if err == nil {
					a.Close()
					t.Fatal("the CLI migrated the registry under a running daemon")
				}
				if !strings.Contains(err.Error(), "daemon-restart --drain") || !strings.Contains(err.Error(), "pid 4242") {
					t.Fatalf("error lacks the fix: %v", err)
				}
				if v := registryVersion(t, f.Ctx.Layout.DB()); v != 0 {
					t.Fatalf("registry at schema %d after a refused migration", v)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a.Close()
			if v := registryVersion(t, f.Ctx.Layout.DB()); v != store.LatestSchemaVersion() {
				t.Fatalf("registry at schema %d, want %d", v, store.LatestSchemaVersion())
			}
		})
	}
}

// A registry already at this build's schema opens without consulting the
// guard: a running daemon of the same build is the normal case.
func TestCLIOpensACurrentRegistryUnderARunningDaemon(t *testing.T) {
	f := newInspFixture(t)
	f.store()
	old := daemonSys
	t.Cleanup(func() { daemonSys = old })
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return os.Getpid(), nil }
	daemonSys.Runner = &execx.Fake{} // any ps call fails the test's expectations loudly
	a, err := inspOpenApp(f.Ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if calls := daemonSys.Runner.(*execx.Fake).Calls; len(calls) != 0 {
		t.Fatalf("the guard ran for a current registry: %+v", calls)
	}
}
