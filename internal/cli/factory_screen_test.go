package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
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

// Each frame checks the factory daemon's pidfile with kill -0 (no ps): with
// no pidfile, or one whose process is gone, home's blocker line says the
// daemon is down and how to start it; a live one draws the holds it saved.
func TestTheFactoryScreenSaysWhenTheDaemonIsDown(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	registrytest.Seed(t, dt.ctx.Layout.FactoryDB())
	pid := 0
	alive := true
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return pid, nil }
	var probes []string
	daemonSys.Kill = func(p int, sig syscall.Signal) error {
		probes = append(probes, fmt.Sprintf("%d %d", p, sig))
		if !alive {
			return syscall.ESRCH
		}
		return nil
	}
	src := &factoryScreenSource{c: dt.ctx, now: time.Now}
	frame := func() tui.FactoryData {
		t.Helper()
		d, err := src.Frame(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	down := func(d tui.FactoryData) bool {
		return len(d.Blocking) == 1 && strings.Contains(d.Blocking[0], "factory daemon down") &&
			len(d.BlockingLong) == 1 && strings.Contains(d.BlockingLong[0], "magnum factory install")
	}
	if d := frame(); !down(d) {
		t.Fatalf("no pidfile: blocking %q %q, want the daemon down", d.Blocking, d.BlockingLong)
	}
	pid = 4242
	if d := frame(); down(d) || len(probes) != 1 || probes[0] != "4242 0" {
		t.Fatalf("a live daemon: blocking %q, probes %q, want no line and one kill -0", d.Blocking, probes)
	}
	alive = false
	if d := frame(); !down(d) {
		t.Fatalf("a pidfile of a process that is gone: blocking %q, want the daemon down", d.Blocking)
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

// ownOnlyMagnum is magnum's registry with your open PRs own and nothing
// else.
type ownOnlyMagnum struct{ own []factory.PRState }

func (m ownOnlyMagnum) Busy(context.Context) (bool, string, error) { return false, "", nil }

func (m ownOnlyMagnum) PRStates(context.Context, []factory.PRRef) (map[factory.PRRef]factory.PRState, error) {
	return map[factory.PRRef]factory.PRState{}, nil
}

func (m ownOnlyMagnum) OwnPRs(context.Context) ([]factory.PRState, error) { return m.own, nil }

func (m ownOnlyMagnum) FixRecords(context.Context, []factory.PRRef) (map[factory.PRRef]factory.FixRecord, error) {
	return map[factory.PRRef]factory.FixRecord{}, nil
}

// Without the factory's registry (no task was ever started) the screen
// still lists your own open PRs from magnum's registry, and creates
// nothing: with [board] own_prs = "factory" the board leaves them out, so
// they would show nowhere. Before, the screen drew an empty home.
func TestTheFactoryScreenWithoutARegistryListsYourPRs(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	cfg.Board.OwnPRs = config.OwnPRsFactory
	dt.ctx.Config = &cfg
	own := factory.PRState{Ref: factory.PRRef{Repo: "example/shop", Number: 12}, ID: 5, Title: "Coupon rounding",
		URL: "https://github.com/example/shop/pull/12", Found: true, DaemonUp: true, Watched: true, GHState: store.GHOpen,
		State: store.PRReviewed, AuthorLogin: "zhuravel"}
	src := &factoryScreenSource{c: dt.ctx, now: time.Now, magnum: ownOnlyMagnum{own: []factory.PRState{own}}}
	defer src.close()
	d, err := src.Frame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 1 || d.Rows[0].ID != -5 || d.Rows[0].PR == nil || d.Rows[0].PR.Ref != "example/shop#12" {
		t.Fatalf("rows %+v, want your PR example/shop#12", d.Rows)
	}
	if fileExists(dt.ctx.Layout.FactoryDB()) {
		t.Error("the frame created the factory's registry")
	}
}

// The screen reads the published tasks' PRs from magnum's registry, which
// it never migrates or writes: a published task's row says its PR's
// verdict there, here that it needs you, with the needs as the card's
// links, where it said "magnum: review of #N" before.
func TestTheFactoryScreenShowsAPublishedTasksPRVerdict(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", Autofix: config.AutofixOpen}}
	dt.ctx.Config = &cfg
	l := dt.ctx.Layout
	ctx := context.Background()
	registrytest.Seed(t, l.FactoryDB())
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12341", Kind: "bug", Repo: "example/shop", Title: "Coupon rounding",
		State: registry.TaskActive, Stage: "publish", Branch: "zhuravel-PR-12341"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.LinkPR(ctx, registry.PRLink{TaskID: task.ID, Repo: "example/shop", Number: 4530, Branch: task.Branch}); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 4530) }); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	st := storetest.OpenFile(t, l.DB())
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_shop", Owner: "example", Name: "shop", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a1", 20)
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_4530", Number: 4530, URL: "https://github.com/example/shop/pull/4530",
		HeadSHA: head, GHState: store.GHOpen, InitialState: store.PRReviewed, Identity: "zhuravel"})
	if err != nil {
		t.Fatal(err)
	}
	link := "https://github.com/example/shop/pull/4530#discussion_r1"
	v := engine.OpenVerdict{Verdict: engine.OpenNeedsYou, Why: "1 item needs you: 1 thread of a person", Head: head, At: time.Now().Add(time.Minute),
		Needs: []engine.OpenNeed{{Kind: "thread", URL: link}}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVPROpen(up.PR.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, store.KVDaemonLastTick, store.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.Pid()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Pid(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	onScreen(t)
	var data tui.FactoryData
	old := tuiFactory
	tuiFactory = func(ctx context.Context, src tui.FactorySource, _ tui.FactoryActions, _ tui.FactoryOptions) error {
		var err error
		data, err = src.Frame(ctx)
		return err
	}
	t.Cleanup(func() { tuiFactory = old })
	if code := dt.run("factory"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if len(data.Rows) != 1 {
		t.Fatalf("rows %+v, want the published task", data.Rows)
	}
	row := data.Rows[0]
	if row.Group != tui.GroupNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0].What != v.Why || !row.WaitsOn[0].OnYou {
		t.Errorf("the published task's row %+v, want NEEDS YOU with the verdict's words", row)
	}
	if card := data.Cards[row.ID].Task; !slices.Equal(card.Links, []string{link}) {
		t.Errorf("its card's links %q, want %q", card.Links, link)
	}
}

// statesMagnum is magnum's registry as the factory reads it, with your
// open PRs own and the published tasks' PRs states.
type statesMagnum struct {
	ownOnlyMagnum
	states map[factory.PRRef]factory.PRState
}

func (m statesMagnum) PRStates(context.Context, []factory.PRRef) (map[factory.PRRef]factory.PRState, error) {
	return m.states, nil
}

// The rows' PRs carry what r, R, z, M and U go by (tui.FactoryPRFacts),
// read from magnum's registry, read only: magnum's state and GitHub's,
// the head and the reviewed head, the mute, a snooze that holds, the
// Codex flag, and whether the PR's watch runs autofix; for your PR's row
// and for a published task's alike. A PR the registry has no row of, and
// a snooze that has ended, give nothing. Before, the screen read none of
// it, and had no key that needed it.
func TestTheFactoryScreenReadsWhatThePRKeysGoBy(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"shop"}, Identity: "zhuravel", Autofix: config.AutofixOpen},
		{Owner: "example", Include: []string{"widget"}, Identity: "zhuravel"}}
	dt.ctx.Config = &cfg
	l := dt.ctx.Layout
	ctx := context.Background()
	registrytest.Seed(t, l.FactoryDB())
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12341", Kind: "bug", Repo: "example/shop", Title: "Coupon rounding",
		State: registry.TaskActive, Stage: "publish", Branch: "zhuravel-PR-12341"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionTask(ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 4530) }); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	st := storetest.OpenFile(t, l.DB())
	pr := func(owner, name string, n int, head string) store.PR {
		t.Helper()
		repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_" + name, Owner: owner, Name: name, Mode: store.RepoModePerPR})
		if err != nil {
			t.Fatal(err)
		}
		up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: fmt.Sprintf("PR_%d", n), Number: n,
			URL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, name, n), HeadSHA: head, GHState: store.GHOpen,
			InitialState: store.PRReviewed, Identity: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		return up.PR
	}
	headTask, headOwn := strings.Repeat("a1", 20), strings.Repeat("b2", 20)
	taskPR, own, gone := pr("example", "shop", 4530, headTask), pr("example", "widget", 12, headOwn), pr("example", "widget", 13, headOwn)
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := st.UpdatePR(ctx, taskPR.ID, func(u *store.PRUpdate) { u.Set("muted", true); u.Set("reviewed_sha", headTask) }); err != nil {
		t.Fatal(err)
	}
	for key, v := range map[string]string{
		engine.KVPRSnooze(taskPR.ID):   fmt.Sprintf(`{"until":%q,"at":%q,"by":"the board"}`, until.Format(time.RFC3339), until.Format(time.RFC3339)),
		store.KVPRCodexFlag(own.ID):    `{"kind":"codex","at":"2026-11-01T10:00:00Z"}`,
		engine.KVPRSnooze(gone.ID):     `{"until":"2020-01-01T00:00:00Z","at":"2020-01-01T00:00:00Z"}`,
		engine.KVPRSnooze(own.ID + 99): `{"until":"2099-01-01T00:00:00Z","at":"2020-01-01T00:00:00Z"}`,
	} {
		if err := st.SetKV(ctx, key, v); err != nil {
			t.Fatal(err)
		}
	}
	state := func(repo string, n int, id int64) factory.PRState {
		return factory.PRState{Ref: factory.PRRef{Repo: repo, Number: n}, ID: id, Title: "a PR", Found: true, DaemonUp: true, Watched: true,
			GHState: store.GHOpen, State: store.PRReviewed, AuthorLogin: "zhuravel"}
	}
	src := &factoryScreenSource{c: dt.ctx, now: time.Now, magnum: statesMagnum{
		ownOnlyMagnum: ownOnlyMagnum{own: []factory.PRState{state("example/widget", 12, own.ID), state("example/widget", 13, gone.ID),
			state("example/widget", 14, own.ID+99)}},
		states: map[factory.PRRef]factory.PRState{{Repo: "example/shop", Number: 4530}: state("example/shop", 4530, taskPR.ID)}}}
	defer src.close()
	d, err := src.Frame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]*tui.FactoryPRFacts{}
	for _, r := range d.Rows {
		if r.PR != nil {
			facts[r.PR.Ref] = r.PR.Facts
		}
	}
	want := map[string]*tui.FactoryPRFacts{
		"example/shop#4530": {State: store.PRReviewed, GHState: store.GHOpen, Head: headTask, Reviewed: headTask, Muted: true,
			SnoozedUntil: until, Autofix: true},
		"example/widget#12": {State: store.PRReviewed, GHState: store.GHOpen, Head: headOwn, CodexFlag: "Codex flagged · never reviewed again"},
		"example/widget#13": {State: store.PRReviewed, GHState: store.GHOpen, Head: headOwn},
		"example/widget#14": nil, // no row of it in magnum's registry
	}
	if len(facts) != len(want) {
		t.Fatalf("rows with a PR %v, want %d", facts, len(want))
	}
	for ref, w := range want {
		got, ok := facts[ref]
		switch {
		case !ok:
			t.Errorf("no row of %s", ref)
		case w == nil && got != nil:
			t.Errorf("%s: facts %+v, want none", ref, *got)
		case w != nil && (got == nil || !got.SnoozedUntil.Equal(w.SnoozedUntil)):
			t.Errorf("%s: facts %+v, want %+v", ref, got, *w)
		case w != nil:
			g := *got
			g.SnoozedUntil = w.SnoozedUntil
			if g != *w {
				t.Errorf("%s: facts %+v, want %+v", ref, g, *w)
			}
		}
	}
}
