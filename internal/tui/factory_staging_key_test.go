package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The factory screen's E (a staging deploy of a row's PR) is the board's:
// the chooser of the PR's staging envs, the daemon's plan of the chosen
// ones as one y/N, and on y the deploy bound to that plan, through
// magnum's own request path (FactoryActions.StagingPlan, StagingDeploy),
// never the factory daemon.

func (a *fakeFactoryActions) StagingPlan(_ context.Context, ref string, envs []string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stagingPlans = append(a.stagingPlans, ref+" "+strings.Join(envs, ","))
	if a.stagingPlanErr != nil {
		return ActionResult{Text: "dry run"}, a.stagingPlanErr
	}
	ask := a.stagingAsk
	if ask == "" {
		ask = stagingAsk(ref, envs)
	}
	return ActionResult{Text: "dry run", Ask: ask, Staging: &StagingAsked{Envs: envs, Head: "abcdef1234",
		Seen: []StagingSeen{{Env: envs[0], Migrations: []string{"db/migrate/1_a.rb"}}}}}, nil
}

func (a *fakeFactoryActions) StagingDeploy(_ context.Context, ref string, asked StagingAsked) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stagingDeploys = append(a.stagingDeploys, ref+" "+strings.Join(asked.Envs, ",")+" at "+asked.Head+" seen "+asked.Seen[0].Migrations[0])
	if a.stagingPending {
		return ActionResult{Text: "the daemon (pid 4242) is running, so the work was queued as request 7 (staging_deploy); it has not " +
			"answered yet: follow it with `magnum logs request:7 -f`", Requests: []Request{{ID: 7, Kind: "staging_deploy", State: RequestPending}}}, nil
	}
	return ActionResult{Text: "deploying " + ref + " at abcdef1 to " + strings.Join(asked.Envs, " and ") + " as fixer-app[bot]",
		Requests: []Request{{ID: 7, Kind: "staging_deploy", State: RequestDone}}}, nil
}

// stagingCalls are the plans and the deploys the screen asked for.
func (a *fakeFactoryActions) stagingCalls() (plans, deploys []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.stagingPlans), slices.Clone(a.stagingDeploys)
}

// testStaged is the published tasks' screen at w x h, PR-12320's row (its
// PR example/talkable#4530) and card with three staging servers, as
// factory.ScreenData gives both: void has its head, bart has it on the
// staging branch, waiting for CI, bastion has none of it.
func testStaged(t *testing.T, w, h int) *factoryHarness {
	t.Helper()
	hs := testPublished(t, w, h)
	hs.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "PR-12320" {
				d.Rows[i].Stagings = stagingCells("latest", "ci", "none")
				c := d.Cards[r.ID]
				c.Task.Stagings = d.Rows[i].Stagings
				d.Cards[r.ID] = c
			}
		}
	})
	hs.press("ctrl+r")
	return hs
}

// wantStaging checks the plans and the deploys the screen asked for.
func (h *factoryHarness) wantStaging(plans, deploys []string) {
	h.t.Helper()
	gotPlans, gotDeploys := h.act.stagingCalls()
	if !slices.Equal(gotPlans, plans) || !slices.Equal(gotDeploys, deploys) {
		h.t.Errorf("plans %q and deploys %q, want %q and %q", gotPlans, gotDeploys, plans, deploys)
	}
}

const stagedDeploy = "example/talkable#4530 void at abcdef1234 seen db/migrate/1_a.rb"

// E on a home row with a PR and staging servers opens the board's chooser
// of the row's envs, each with its state; enter with none chosen says to
// choose one first; 1 and enter ask the daemon's plan of void (a dry run)
// and put its question as the y/N; n deploys nothing, y deploys bound to
// the plan's envs, head and what it named. Before, E was no key of the
// factory screen: a task's PR went to staging from the board alone.
func TestFactoryEOnAHomeRowChoosesTheEnvsThenDeploysAfterThePlan(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E")
	mustContain(t, h.view(), "Deploy PR-12320's #4530 to: 1 [ ] void latest · 2 [ ] bart CI · 3 [ ] bastion none", "enter asks")
	h.press("enter")
	mustContain(t, h.view(), "choose an env first")
	h.wantStaging(nil, nil)

	h.press("1")
	mustContain(t, h.view(), "1 [x] void", "2 [ ] bart") // the toggle is drawn: the chooser is part of the frame
	h.press("enter")
	h.wantStaging([]string{"example/talkable#4530 void"}, nil)
	mustContain(t, h.view(), stagingAsk("example/talkable#4530", []string{"void"})+" y/N")
	h.press("n")
	v := h.view()
	mustContain(t, v, "cancelled, nothing was run")
	mustNotContain(t, v, "y/N")
	h.wantStaging([]string{"example/talkable#4530 void"}, nil)

	h.press("E", "1", "enter", "y")
	h.wantStaging([]string{"example/talkable#4530 void", "example/talkable#4530 void"}, []string{stagedDeploy})
	mustContain(t, h.view(), "deploying example/talkable#4530 at abcdef1 to void as fixer-app[bot]")
	if h.act.calls() != 0 {
		t.Errorf("E ran %d other actions", h.act.calls())
	}
}

// E on a task's card is home's: the chooser on the card's rule, the
// plan, its y/N in the card's footer, and the deploy on y.
func TestFactoryEOnATaskCardDeploysAfterThePlan(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.open("PR-12320")
	if h.m.view != fvTask || h.m.task != 1 {
		t.Fatalf("enter on PR-12320 opened view %v of %d, want its task card", h.m.view, h.m.task)
	}
	h.press("E")
	mustContain(t, h.view(), "Deploy PR-12320's #4530 to: 1 [ ] void latest")
	h.press("1", "2")
	mustContain(t, h.view(), "1 [x] void", "2 [x] bart")
	h.press("enter")
	h.wantStaging([]string{"example/talkable#4530 void,bart"}, nil)
	mustContain(t, h.view(), stagingAsk("example/talkable#4530", []string{"void", "bart"})+" y/N")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4530 void,bart"}, []string{"example/talkable#4530 void,bart at abcdef1234 seen db/migrate/1_a.rb"})
	if h.m.view != fvTask {
		t.Errorf("the deploy left the card for view %v", h.m.view)
	}
}

// E refuses, before any chooser and with no request: a row without a PR
// in magnum's registry (in the words the fix keys use), a PR whose
// repository has no staging servers in the config (no cells, and no
// FactoryPR.StagingEnvs), a merged PR, and a screen without actions.
func TestFactoryERefusesWhatItCannotDeploy(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	for _, c := range []struct{ row, want string }{
		{"PR-12302", "E acts on a PR in magnum's registry: PR-12302 has none there"},
		{"PR-12324", "PR-12324's #4534's repository has no staging envs ([[factory.repo.staging]] in config.toml)"},
	} {
		h.selectRow(c.row)
		h.press("E")
		v := h.view()
		mustContain(t, v, c.want)
		mustNotContain(t, v, "Deploy PR-", "y/N")
		h.press("g")
	}
	h.facts("PR-12320", FactoryPRFacts{State: "merged", GHState: "MERGED"})
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E")
	mustContain(t, h.view(), "PR-12320's #4530 is merged: nothing to deploy")
	h.wantStaging(nil, nil)

	y := testYours(t, 160, 40)
	y.press("g")
	y.selectRow("#4602")
	y.press("E")
	v := y.view()
	mustContain(t, v, "#4602's repository has no staging envs ([[factory.repo.staging]] in config.toml)")
	mustNotContain(t, v, "Deploy #4602 to")
	y.wantStaging(nil, nil)

	n := testStaged(t, 160, 40)
	n.m.act = nil
	n.press("g")
	n.selectRow("PR-12320")
	n.press("E")
	v = n.view()
	mustContain(t, v, noActions)
	mustNotContain(t, v, "Deploy PR-")
}

// Any key but a digit, a and enter closes the chooser with a note and
// asks nothing; a toggles every env, then none.
func TestFactoryEsChooserCancelsOnAnyOtherKey(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "a")
	mustContain(t, h.view(), "1 [x] void", "2 [x] bart", "3 [x] bastion")
	h.press("a")
	mustContain(t, h.view(), "1 [ ] void", "2 [ ] bart", "3 [ ] bastion")
	h.press("x")
	v := h.view()
	mustContain(t, v, "Deploy PR-12320's #4530 to staging: cancelled, nothing was run")
	mustNotContain(t, v, "[ ] void")
	h.wantStaging(nil, nil)
	if h.m.bar.choose != nil {
		t.Error("the chooser is still up")
	}
}

// A plan with nothing to deploy is the plan's failure in the footer: no
// question is asked, and nothing is deployed.
func TestFactoryEsPlanErrorAsksNothing(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.act.stagingPlanErr = errors.New("nothing to deploy: void: already there (staging-void holds PR #4530's head)")
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1", "enter")
	v := h.view()
	mustContain(t, v, "failed: nothing to deploy: void: already there")
	mustNotContain(t, v, "y/N")
	h.press("y")
	h.wantStaging([]string{"example/talkable#4530 void"}, nil)
}

// A deploy the daemon has not answered yet is queued: the factory screen
// follows no request, so the footer says that `magnum staging <the PR>`
// follows it, with the PR's own reference.
func TestFactoryEsPendingDeploySaysHowToFollowIt(t *testing.T) {
	h := testStaged(t, 200, 40)
	h.act.stagingPending = true
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1", "enter", "y")
	h.wantStaging([]string{"example/talkable#4530 void"}, []string{stagedDeploy})
	mustContain(t, h.view(), "the staging deploy of PR-12320's #4530 is queued: `magnum staging example/talkable#4530` follows it")
}

// planQuestion's lines as a screen shows them, read back as one: from the
// line that starts it to the one that ends in y/N, the rule's glyphs and
// the keys dropped, a line broken after a hyphen joined without a space.
func askedOn(t *testing.T, view string, w int) string {
	t.Helper()
	asked := ""
	for i, l := range strings.Split(view, "\n") {
		if ansi.StringWidth(l) > w {
			t.Errorf("line %d is %d cells wide, past %d: %q", i, ansi.StringWidth(l), w, l)
		}
		if asked == "" && !strings.Contains(l, "Deploy talkable#5's head") {
			continue
		}
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "─"))
		l, _, _ = strings.Cut(l, "  y confirms")
		if asked != "" && !strings.HasSuffix(asked, "-") {
			asked += " "
		}
		asked += l
		if strings.HasSuffix(l, "y/N") {
			break
		}
	}
	return asked
}

// E's y/N says all the daemon's plan names, on home and on the card, at
// the widths the screen runs at: its words wrap over the footer's lines
// instead of being cut at the screen's edge, "y/N" ending the last, and
// the screen keeps its height.
func TestFactoryEsQuestionShowsTheMigrationsAndTheWarningWhole(t *testing.T) {
	for _, w := range []int{100, 140, 200} {
		h := testStaged(t, w, 40)
		h.act.stagingAsk = planQuestion
		h.press("g")
		h.selectRow("PR-12320")
		h.press("E", "1", "enter")
		v := h.view()
		if got := askedOn(t, v, w); got != planQuestion+" y/N" {
			t.Errorf("%d columns: home asks\n%q\nwant\n%q\n%s", w, got, planQuestion+" y/N", v)
		}
		if n := len(strings.Split(v, "\n")); n != 40 {
			t.Errorf("%d columns: home is %d lines high, want 40", w, n)
		}
		h.press("n", "enter", "E", "1", "enter")
		if h.m.view != fvTask {
			t.Fatalf("%d columns: enter opened view %v, want the task card", w, h.m.view)
		}
		v = h.view()
		if got := askedOn(t, v, w); got != planQuestion+" y/N" {
			t.Errorf("%d columns: the card asks\n%q\nwant\n%q\n%s", w, got, planQuestion+" y/N", v)
		}
		if n := len(strings.Split(v, "\n")); n != 40 {
			t.Errorf("%d columns: the card is %d lines high, want 40", w, n)
		}
	}
}

// The plan's question is the daemon's text: it reaches the footer cleaned
// of escape sequences, as every text from the daemon does.
func TestFactoryEsQuestionIsCleaned(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.act.stagingAsk = "Deploy \x1b]8;;https://evil.example\x07#4530\x1b]8;;\x07 to void\x1b[31m now?"
	h.press("g")
	h.selectRow("PR-12320")
	h.press("E", "1", "enter")
	raw := h.m.View().Content
	if strings.Contains(raw, "evil.example") || strings.Contains(raw, "\x1b[31m now") || strings.Contains(raw, "\x07") {
		t.Errorf("the question reached the screen uncleaned:\n%q", raw)
	}
	mustContain(t, h.view(), "Deploy #4530 to void now? y/N")
}

// stagedDeploys gives the PRs of PR-12320 and PR-12324 the staging
// deploys magnum's registry has of them: a deploy to void running at its
// push (whose words carry an escape sequence) on #4530, a failed one to
// bart on #4534.
func stagedDeploys(d *FactoryData) {
	for i, r := range d.Rows {
		if r.PR == nil {
			continue
		}
		pr := *r.PR
		switch r.Key {
		case "PR-12320":
			pr.StagingDeploys = []StagingDeploy{{Effect: "void-4530-20261103t140600-abcd", Env: "void", Step: "push", Phase: "begin",
				State: "running", Live: true, Text: "push of 6b6b6b6 to \x1b[31mstaging-void", At: factoryAgo(factoryNow, 3*time.Minute)}}
		case "PR-12324":
			pr.StagingDeploys = []StagingDeploy{{Effect: "bart-4534-20261103t120000-abcd", Env: "bart", Step: "watch", Phase: "fail",
				State: "failed", Why: "ci.yml failed on the merge", At: factoryAgo(factoryNow, 2*time.Hour)}}
		}
		d.Rows[i].PR = &pr
	}
}

// The task card shows its PR's staging deploys under STAGINGS, as the
// board's card does, from the records magnum's registry has: the effect,
// its state, its last event's step and phase and how long ago, and that
// event's words, cleaned. Another PR's deploy is not its card's, and
// home's rows draw none.
func TestTheTaskCardShowsItsPRsStagingDeploys(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.src.change(stagedDeploys)
	h.press("ctrl+r", "g")
	mustNotContain(t, h.view(), "deploy void-4530", "deploy bart-4534")
	h.open("PR-12320")
	v := h.view()
	mustContain(t, v, "STAGINGS", "deploy void-4530-20261103t140600-abcd: running · push begin · 3m ago", "push of 6b6b6b6 to staging-void")
	mustNotContain(t, v, "bart-4534", "ci.yml failed on the merge")
	if raw := h.m.View().Content; strings.Contains(raw, "\x1b[31mstaging-void") {
		t.Errorf("the deploy's words reached the card uncleaned:\n%q", raw)
	}
	if i, j := strings.Index(v, "STAGINGS"), strings.Index(v, "deploy void-4530"); j < i {
		t.Errorf("the deploy is not under STAGINGS:\n%s", v)
	}
}

// The help names E on home and on the task card, in the board's words.
func TestTheFactoryHelpNamesE(t *testing.T) {
	h := testStaged(t, 160, 40)
	help := ansi.Strip(strings.Join(h.m.helpContent(240), "\n"))
	for _, sec := range []string{"Home", "Task card"} {
		if !slices.ContainsFunc(helpSection(t, help, sec), func(l string) bool {
			return strings.HasPrefix(l, "E ") && strings.Contains(l, "deploy to staging (choose, y/N)")
		}) {
			t.Errorf("the help's %s lacks E:\n%s", sec, strings.Join(helpSection(t, help, sec), "\n"))
		}
	}
}

// configEnvs gives the PR of the row keyed key the staging envs the config
// names for its repository (FactoryPR.StagingEnvs, the cli source's
// config.Stagings), and reads the factory again.
func (h *factoryHarness) configEnvs(key string, envs ...string) {
	h.t.Helper()
	found := false
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == key && r.PR != nil {
				pr := *r.PR
				pr.StagingEnvs = envs
				d.Rows[i].PR, found = &pr, true
			}
		}
	})
	if !found {
		h.t.Fatalf("no row %s with a PR", key)
	}
	h.press("ctrl+r")
}

// A row with a PR but no staging cells yet (the factory daemon has not
// written them, or never reads them: your own PR's row), whose repository
// has staging envs in the config, opens the chooser on the config's envs,
// each not read yet: the review daemon plans them anyway. Only a
// repository with none in the config says it has none. Before, every such
// row said its repository had no staging envs, and your own PR's that the
// screen read none of it.
func TestFactoryEOnARowWithoutCellsChoosesTheConfigsEnvs(t *testing.T) {
	h := testStaged(t, 160, 40)
	h.configEnvs("PR-12324", "void", "bart\x1b[31m")
	h.press("g")
	h.selectRow("PR-12324")
	h.press("E")
	mustContain(t, h.view(), "Deploy PR-12324's #4534 to: 1 [ ] void not read yet · 2 [ ] bart not read yet")
	h.press("2", "enter")
	h.wantStaging([]string{"example/talkable#4534 bart"}, nil)
	mustContain(t, h.view(), stagingAsk("example/talkable#4534", []string{"bart"})+" y/N")

	y := testYours(t, 160, 40)
	y.configEnvs("#4602", "void")
	y.press("g")
	y.selectRow("#4602")
	y.press("E")
	mustContain(t, y.view(), "Deploy #4602 to: 1 [ ] void not read yet")
	y.press("1", "enter", "y")
	y.wantStaging([]string{"example/talkable#4602 void"}, []string{"example/talkable#4602 void at abcdef1234 seen db/migrate/1_a.rb"})
}

// E's chooser says each server's state in the factory screen's own words,
// as the row's cell and the card's STAGINGS line say it (CI with its
// count, CI ✗, deploying), without the cell's age; none and unknown in
// words.
func TestFactoryEsChooserKeepsTheFactorysStateWords(t *testing.T) {
	h := testStaged(t, 200, 40)
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "PR-12320" {
				d.Rows[i].Stagings = []StagingCell{{Name: "void", State: "ci", Count: "12/41", Seen: factoryNow}, {Name: "bart", State: "ci-fail"},
					{Name: "bastion", State: "deploying"}, {Name: "rc", State: "outdated"}, {Name: "demo", State: "unknown"}}
			}
		}
	})
	h.press("ctrl+r", "g")
	h.selectRow("PR-12320")
	h.press("E")
	mustContain(t, h.view(), "1 [ ] void CI 12/41 · 2 [ ] bart CI ✗ · 3 [ ] bastion deploying · 4 [ ] rc outdated · 5 [ ] demo unknown")
}

// E's chooser lists the factory's cells in the board's states, the way
// back of the factory's own mapping (a head on the staging branch is
// pushed, whether its CI runs, failed or passed), and claims nothing of a
// state it does not know; the env's name is cleaned before it is drawn or
// sent.
func TestFactoryEsChooserSaysTheCellsInTheBoardsStates(t *testing.T) {
	cells := []StagingCell{{Name: "void", State: "latest"}, {Name: "bart", State: "ci-fail"}, {Name: "bastion", State: "deploying"},
		{Name: "rc", State: "outdated"}, {Name: "qa\x1b[31m1", State: "merging"}, {Name: "demo", State: "none"}}
	got := stagingInfosOf(cells)
	want := []StagingInfo{{Env: "void", State: "latest"}, {Env: "bart", State: "pushed"}, {Env: "bastion", State: "pushed"},
		{Env: "rc", State: "outdated"}, {Env: "qa1", State: "unknown"}, {Env: "demo", State: "none"}}
	if !slices.Equal(got, want) {
		t.Errorf("the chooser's envs %+v, want %+v", got, want)
	}
}

// The right-click menu offers E, "deploy to staging", on home's row and on
// the task card, dimmed where E refuses (no PR, no staging envs), and its
// run opens the chooser, as the key does: a click never deploys. Home's
// hints offer E on a row it deploys, when the line has room for it.
func TestTheMenuAndTheHintsOfferE(t *testing.T) {
	h := testStaged(t, 200, 40)
	h.press("g")
	item := func(items []menuItem) (menuItem, bool) {
		i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == "E" })
		if i < 0 {
			return menuItem{}, false
		}
		return items[i], true
	}
	for _, c := range []struct {
		row string
		ok  bool
	}{{"PR-12320", true}, {"PR-12324", false}, {"PR-12302", false}} {
		h.selectRow(c.row)
		it, found := item(h.m.menuItems())
		if !found || it.label != "deploy to staging" || it.ok != c.ok {
			t.Errorf("%s: the menu's E %+v (found %v), want it live %v", c.row, it, found, c.ok)
		}
		h.press("g")
	}
	h.selectRow("PR-12320")
	if hints := strings.Split(h.view(), "\n"); !strings.Contains(hints[len(hints)-1], "E staging") {
		t.Errorf("home's hints lack E on a row it deploys: %q", hints[len(hints)-1])
	}
	h.selectRow("PR-12324")
	if hints := strings.Split(h.view(), "\n"); strings.Contains(hints[len(hints)-1], "E staging") {
		t.Errorf("home's hints offer E on a row it refuses: %q", hints[len(hints)-1])
	}
	h.press("g")
	h.selectRow("PR-12320")
	h.m.menu = ctxMenu{open: true, rowKey: h.m.menuOwner()}
	h.press("E")
	if h.m.bar.choose == nil || h.m.menu.open {
		t.Fatalf("the menu's E opened no chooser:\n%s", h.view())
	}
	h.press("x")
	h.wantStaging(nil, nil)

	h.open("PR-12320")
	if it, found := item(h.m.menuItems()); !found || !it.ok {
		t.Errorf("the task card's menu: E %+v (found %v), want it live", it, found)
	}
}

// The task card's deploy lines are the board card's: while the resolver
// works on a deploy's conflict, the card says what it does, naming the
// agent's pane, cleaned.
func TestTheTaskCardSaysWhatTheResolverDoesForItsPRsDeploy(t *testing.T) {
	h := testStaged(t, 200, 40)
	h.src.change(func(d *FactoryData) {
		stagedDeploys(d)
		for i, r := range d.Rows {
			if r.Key == "PR-12320" {
				pr := *r.PR
				pr.StagingDeploys[0].Resolver = "try 1: the agent resolves 1 conflicted path in slot review1: pane \x1b[31mw3:p1"
				d.Rows[i].PR = &pr
			}
		}
	})
	h.press("ctrl+r")
	h.open("PR-12320")
	mustContain(t, h.view(), "resolver try 1: the agent resolves 1 conflicted path in slot review1: pane w3:p1")
}
