package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// doctor reports a migration the CLI refuses under a running daemon as a
// failed registry check whose fix is `magnum daemon-restart --drain`, never
// config.toml, through the same printing as every check: JSON under --json.
func TestDoctorNamesTheMigrationRefusedUnderARunningDaemon(t *testing.T) {
	const pid = 4242
	for _, asJSON := range []bool{true, false} {
		f := newInspFixture(t)
		olderRegistry(t, f)
		old := daemonSys
		t.Cleanup(func() { daemonSys = old })
		daemonSys.DaemonPID = func(paths.Layout) (int, error) { return pid, nil }
		daemonSys.Runner = &execx.Fake{Rules: []execx.Rule{
			daemonRuleOK([]string{"ps", "-p", strconv.Itoa(pid), "-o", "comm="}, "/repo/bin/magnum\n"),
			daemonRuleOK([]string{"ps", "-p", strconv.Itoa(pid), "-o", "args="}, "/repo/bin/magnum daemon\n"),
		}}
		var args []string
		if asJSON {
			args = append(args, "--json")
		}
		code := f.run("doctor", args...)
		out := f.Out.String()
		if code != 1 {
			t.Fatalf("json %v: exit %d, out %s", asJSON, code, out)
		}
		if strings.Contains(out, "config.toml") {
			t.Fatalf("json %v: doctor blames config.toml for the refused migration:\n%s", asJSON, out)
		}
		detail := fmt.Sprintf("the registry is at schema 0 and this build needs %d, but the daemon (pid %d) still runs on it", store.LatestSchemaVersion(), pid)
		fix := "run `magnum daemon-restart --drain`"
		if !asJSON {
			if !strings.Contains(out, "FAIL  "+detail) || !strings.Contains(out, "      fix: "+fix) || !strings.Contains(out, "0 PASS, 0 WARN, 1 FAIL") {
				t.Fatalf("text:\n%s", out)
			}
			continue
		}
		var got []doctorCheck
		if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil {
			t.Fatalf("--json prints no JSON (%v):\n%s", err, out)
		}
		if len(got) != 1 || got[0].Name != "registry" || got[0].Status != doctorFail ||
			!strings.HasPrefix(got[0].Detail, detail) || !strings.HasPrefix(got[0].Fix, fix) || strings.Contains(got[0].Detail, "fix:") {
			t.Fatalf("checks = %+v", got)
		}
	}
}

// Any other failure to open the config and the registry keeps the
// config.toml fix, in JSON under --json.
func TestDoctorJSONReportsABrokenConfig(t *testing.T) {
	f := newInspFixture(t)
	if err := os.WriteFile(filepath.Join(f.Home, "config.toml"), []byte("[daemon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("doctor", "--json"); code != 1 {
		t.Fatalf("exit %d: %s", code, f.Out.String())
	}
	var got []doctorCheck
	if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil {
		t.Fatalf("--json prints no JSON (%v):\n%s", err, f.Out.String())
	}
	if len(got) != 1 || got[0].Name != "config" || got[0].Status != doctorFail || !strings.Contains(got[0].Fix, "config.toml") {
		t.Fatalf("checks = %+v", got)
	}
}
