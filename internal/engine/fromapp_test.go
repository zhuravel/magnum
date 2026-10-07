package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// TestFromAppDryRunOnce wires the real components (hermetically: scripted
// subprocesses, no herdr socket, a closed MySQL port) and runs one dry-run
// tick end to end.
func TestFromAppDryRunOnce(t *testing.T) {
	home := t.TempDir()
	cfgText := strings.ReplaceAll(testConfigTOML, "HOME", home)
	cfgText = strings.Replace(cfgText, "[herdr]\n", "[herdr]\nsocket = \""+filepath.Join(home, "no.sock")+"\"\n", 1)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home}
	cfg, err := config.Load(layout, filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// A real registry the dry run must leave alone.
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	real, err := store.Open(layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := real.SetKV(context.Background(), "probe", "untouched"); err != nil {
		t.Fatal(err)
	}
	real.Close()

	var logs bytes.Buffer
	fake := &execx.Fake{}
	a, err := app.New(cfg, layout, app.Options{DryRun: true, Runner: fake, Stderr: &logs,
		MySQLDSN: "root:@tcp(127.0.0.1:1)/?timeout=1s"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	e := FromApp(a)
	if e.d.Runner != a.Runner {
		t.Fatal("FromApp does not hand the app's command runner to the engine: the triage command would never run")
	}
	if err := e.Run(context.Background(), Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Calls {
		if c.Mutates {
			t.Fatalf("mutating command ran in a dry run: %s", c.String())
		}
	}
	if len(fake.CallsWithPrefix("gh", "api", "graphql")) == 0 {
		t.Fatalf("the dry run did not poll GitHub: %v", fake.Calls)
	}
	if _, err := os.Stat(layout.Pid()); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a pidfile")
	}
	real, err = store.Open(layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	if v, _, _ := real.GetKV(context.Background(), "probe"); v != "untouched" {
		t.Fatalf("real registry changed: %q", v)
	}
	if _, ok, _ := real.GetKV(context.Background(), kvLastTick); ok {
		t.Fatal("dry run wrote daemon state into the real registry")
	}
	if !strings.Contains(logs.String(), "would provision") {
		t.Fatalf("expected a planned provision (pool below min):\n%s", logs.String())
	}
}
