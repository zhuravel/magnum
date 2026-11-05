package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// `magnum factory` with no subcommand opens the factory's screen when it
// runs in a terminal, with the terminal's options from the config, and
// reads the factory's tasks into home's rows; anywhere else it prints its
// usage, as before.
func TestFactoryWithoutASubcommandOpensTheScreen(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	cfg.Terminal.Mouse, cfg.Terminal.Icons = false, "ascii"
	dt.ctx.Config = &cfg
	l := dt.ctx.Layout
	registrytest.Seed(t, l.FactoryDB())
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12340", Kind: "bug", Repo: "example/shop", Title: "Points vanish",
		State: registry.TaskNeedsYou, Stage: "questions", WaitsWho: "you", WaitsWhat: "2 questions before coding"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTask(ctx, registry.Task{Prompt: "rename the helper", Kind: "chore", Repo: "example/shop", Title: "Rename"}); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	// Not a terminal: the usage.
	if code := dt.run("factory"); code != 2 || !strings.Contains(dt.stderr.String(), "which factory command?") {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}

	onScreen(t)
	var opts tui.FactoryOptions
	var data tui.FactoryData
	var actions tui.FactoryActions
	old := tuiFactory
	tuiFactory = func(ctx context.Context, src tui.FactorySource, act tui.FactoryActions, o tui.FactoryOptions) error {
		opts, actions = o, act
		var err error
		data, err = src.Frame(ctx)
		return err
	}
	t.Cleanup(func() { tuiFactory = old })
	dt.stderr.Reset()
	if code := dt.run("factory"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if !opts.NoMouse || opts.Icons != tui.IconsASCII || opts.Now == nil {
		t.Errorf("options %+v: want the config's mouse off and ascii icons", opts)
	}
	if _, ok := actions.(*factoryActions); !ok {
		t.Errorf("actions %T, want the factory's requests", actions)
	}
	if opts.Settings == nil || opts.Settings.Source == nil || opts.Settings.Runtime == nil {
		t.Errorf("settings %+v, want `,` to open the settings screen", opts.Settings)
	}
	if seen, err := factory.ReadSeen(dt.ctx.Layout); err != nil || seen.IsZero() {
		t.Errorf("seen %v (%v) after the screen closed, want its time", seen, err)
	}
	if len(data.Rows) != 2 {
		t.Fatalf("rows %+v, want the 2 tasks", data.Rows)
	}
	first, second := data.Rows[0], data.Rows[1]
	if first.Key != "PR-12340" || first.Group != tui.GroupNeedsYou || len(first.WaitsOn) != 1 || first.WaitsOn[0].Who != "you" ||
		!first.WaitsOn[0].OnYou || first.WaitsOn[0].What != "2 questions before coding" {
		t.Errorf("first row %+v", first)
	}
	if second.Key != "task 2" || second.Group != tui.GroupWaits || second.Title != "Rename" {
		t.Errorf("second row %+v", second)
	}
	if card := data.Cards[first.ID].Task; card.Key != "PR-12340" || card.Kind != "bug" || card.Title != "Points vanish" {
		t.Errorf("the first task's card %+v", card)
	}
}

// Without the factory's registry the screen opens on a home with no task,
// and creates nothing.
func TestTheFactoryScreenWithoutARegistryShowsNoTask(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	onScreen(t)
	old := tuiFactory
	var rows int
	tuiFactory = func(ctx context.Context, src tui.FactorySource, _ tui.FactoryActions, _ tui.FactoryOptions) error {
		d, err := src.Frame(ctx)
		rows = len(d.Rows)
		return err
	}
	t.Cleanup(func() { tuiFactory = old })
	if code := dt.run("factory"); code != 0 || rows != 0 {
		t.Fatalf("exit %d rows %d, stderr: %s", code, rows, dt.stderr)
	}
	if fileExists(dt.ctx.Layout.FactoryDB()) {
		t.Error("opening the screen created the factory's registry")
	}
}
