package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// The PR board and the factory screen keep one owner scope in magnum's
// registry: each opens in the one the other left, and writes every change
// of it (O, or the board's fallback to every owner when its owner's last
// row left). The board's tab round-trip to the dashboard keeps it too.
func TestTheBoardAndTheFactoryScreenShareTheOwnerScope(t *testing.T) {
	storetest.Serial(t)
	f := newInspFixture(t)
	prsSeed(t, f)
	st := f.store()
	ctx := context.Background()
	onScreen(t)
	var opened []string // the owner each screen opened in, "board" or "factory" first
	var change []string // what each screen does with OwnerChanged, in turn
	oldDash, oldBoard, oldFactory := tuiDashboard, tuiPRBoard, tuiFactory
	t.Cleanup(func() { tuiDashboard, tuiPRBoard, tuiFactory = oldDash, oldBoard, oldFactory })
	next := func(changed func(string)) {
		t.Helper()
		if len(change) == 0 {
			return
		}
		switch o := change[0]; {
		case o == "-":
		case changed == nil:
			t.Errorf("the screen hears no change of the owner scope (it was to change to %q)", o)
		default:
			changed(o)
		}
		change = change[1:]
	}
	tuiPRBoard = func(_ context.Context, _ tui.PRBoardSource, _ tui.DashboardActions, o tui.PRBoardOptions) error {
		opened = append(opened, "board "+o.DefaultOwner)
		next(o.OwnerChanged)
		return nil
	}
	tuiFactory = func(_ context.Context, _ tui.FactorySource, _ tui.FactoryActions, o tui.FactoryOptions) error {
		opened = append(opened, "factory "+o.DefaultOwner)
		next(o.OwnerChanged)
		return nil
	}
	run := func(name string, args ...string) {
		t.Helper()
		if code := f.run(name, args...); code != 0 {
			t.Fatalf("%s: exit %d, stderr: %s", name, code, f.Err.String())
		}
		if f.Err.Len() > 0 {
			t.Errorf("%s said: %s", name, f.Err.String())
		}
	}
	change = []string{"example", "alice", "-", "", "-"}
	run("tui")     // O on the board: example
	run("factory") // opens in example; O: alice
	run("tui")     // opens in alice
	run("factory") // opens in alice; its fallback: every owner
	run("tui")     // opens in every owner
	if want := []string{"board ", "factory example", "board alice", "factory alice", "board "}; !slices.Equal(opened, want) {
		t.Fatalf("the screens opened in %q, want %q", opened, want)
	}
	if v, ok, err := st.GetKV(ctx, kvScreenOwner); err != nil || !ok || v != "" {
		t.Errorf("the kept owner scope is %q (%v, %v), want every owner", v, ok, err)
	}

	// tab to the dashboard and back keeps the owner the board left.
	opened, change = nil, []string{"talkable"}
	tuiDashboard = func(context.Context, tui.DashboardSource, tui.DashboardActions, tui.DashboardOptions) error {
		return tui.ErrSwitchToBoard
	}
	tuiPRBoard = func(_ context.Context, _ tui.PRBoardSource, _ tui.DashboardActions, o tui.PRBoardOptions) error {
		opened = append(opened, "board "+o.DefaultOwner)
		next(o.OwnerChanged)
		if len(opened) == 1 {
			return tui.ErrSwitchToDashboard
		}
		return nil
	}
	run("tui")
	if want := []string{"board ", "board talkable"}; !slices.Equal(opened, want) {
		t.Errorf("a tab round-trip opened the board in %q, want %q", opened, want)
	}
	if v, _, _ := st.GetKV(ctx, kvScreenOwner); v != "talkable" {
		t.Errorf("the kept owner scope is %q, want talkable", v)
	}
}

// A factory screen that cannot read or write magnum's registry (none yet,
// or a file that is no database) opens with every owner, takes O as ever
// and says nothing of it; it creates no registry.
func TestTheFactoryScreenWithoutMagnumsRegistryRunsWithEveryOwner(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	onScreen(t)
	var opened []string
	old := tuiFactory
	tuiFactory = func(_ context.Context, _ tui.FactorySource, _ tui.FactoryActions, o tui.FactoryOptions) error {
		opened = append(opened, o.DefaultOwner)
		if o.OwnerChanged == nil {
			t.Error("the screen has no OwnerChanged")
		} else {
			o.OwnerChanged("example")
		}
		return nil
	}
	t.Cleanup(func() { tuiFactory = old })
	db := dt.ctx.Layout.DB()
	if code := dt.run("factory"); code != 0 || dt.stderr.Len() > 0 || fileExists(db) {
		t.Fatalf("no registry: exit %d, stderr %q, registry made %v", code, dt.stderr, fileExists(db))
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory"); code != 0 || dt.stderr.Len() > 0 {
		t.Fatalf("a registry that is no database: exit %d, stderr %q", code, dt.stderr)
	}
	if !slices.Equal(opened, []string{"", ""}) {
		t.Errorf("the screens opened in %q, want every owner", opened)
	}
}
