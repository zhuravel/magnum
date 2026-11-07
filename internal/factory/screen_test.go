package factory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenNow is the clock of the screen's tests: the morning after a night
// away.
var screenNow = time.Date(2026, 11, 4, 9, 5, 0, 0, time.Local)

// screenRig is a factory registry and an agents store on one clock, the
// default config with one repository, and the tasks' directories: what
// ScreenData reads.
type screenRig struct {
	t      *testing.T
	ctx    context.Context
	clock  *storetest.Clock
	reg    *registry.Registry
	agents *store.Store
	cfg    *config.Config
	layout paths.Layout
	repo   int64 // the owner repository of the agents store, once made
	// cards are the ready cards the last data drew (ScreenOptions.ReadyCards).
	cards map[int64]ReadyCardFile
	// magnum is magnum's registry as the data reads it (ScreenOptions.Magnum);
	// nil: the data reads none.
	magnum *fakeMagnum
}

func newScreenRig(t *testing.T) *screenRig {
	t.Helper()
	storetest.Parallel(t)
	clock := storetest.NewClock(screenNow)
	cfg := config.Defaults()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/talkable", Base: "master", MainClone: t.TempDir()}}
	return &screenRig{t: t, ctx: t.Context(), clock: clock, reg: registrytest.OpenWith(t, registry.Options{Clock: clock.Now}),
		agents: storetest.Open(t, filepath.Join(t.TempDir(), "agents.db")), cfg: cfg, layout: paths.Layout{Home: t.TempDir()}}
}

// at sets the clock to d before screenNow.
func (r *screenRig) at(d time.Duration) { r.clock.Set(screenNow.Add(-d)) }

// task creates a task of kind at the clock's time.
func (r *screenRig) task(key, kind, title, state, stage, who, what string) registry.Task {
	r.t.Helper()
	t, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: key, Kind: kind, Repo: "example/talkable", Title: title, State: state,
		Stage: stage, WaitsWho: who, WaitsWhat: what, Branch: "zhuravel-" + key + "-work"})
	if err != nil {
		r.t.Fatal(err)
	}
	return t
}

// move moves task t to state with its wait, at the clock's time.
func (r *screenRig) move(t registry.Task, state, who, what string, set ...func(*registry.TaskUpdate)) registry.Task {
	r.t.Helper()
	if err := r.reg.TransitionTask(r.ctx, t.ID, []string{t.State}, state, func(u *registry.TaskUpdate) {
		u.Set("waits_who", who)
		u.Set("waits_what", what)
		for _, f := range set {
			f(u)
		}
	}); err != nil {
		r.t.Fatal(err)
	}
	got, err := r.reg.TaskByID(r.ctx, t.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// published publishes task t as PR number, opened at the clock's time.
func (r *screenRig) published(t registry.Task, number int) registry.Task {
	r.t.Helper()
	if err := r.reg.LinkPR(r.ctx, registry.PRLink{TaskID: t.ID, Repo: "example/talkable", Number: number, Branch: t.Branch}); err != nil {
		r.t.Fatal(err)
	}
	return r.move(t, registry.TaskPublished, "", "", func(u *registry.TaskUpdate) { u.Set("pr_number", number) })
}

// questions asks qs as task t's next revision, at the clock's time.
func (r *screenRig) questions(t registry.Task, qs ...registry.Question) registry.Task {
	r.t.Helper()
	cur, err := r.reg.TaskByID(r.ctx, t.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.ReplaceQuestions(r.ctx, t.ID, cur.QuestionRev+1, qs); err != nil {
		r.t.Fatal(err)
	}
	if cur, err = r.reg.TaskByID(r.ctx, t.ID); err != nil {
		r.t.Fatal(err)
	}
	return cur
}

func question(text string, options ...string) registry.Question {
	q := registry.Question{Text: text, Recommended: -1, RaisedBy: "fx-judge"}
	for _, o := range options {
		q.Options = append(q.Options, registry.Option{Label: o})
	}
	if len(options) > 0 {
		q.Recommended, q.Why = 0, "the ticket says so"
	}
	return q
}

// file writes a file of task id's directory.
func (r *screenRig) file(id int64, name, text string) {
	r.t.Helper()
	dir := r.layout.FactoryTaskDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// card writes task id's ready card.
func (r *screenRig) card(id int64, c ReadyCardFile) {
	r.t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		r.t.Fatal(err)
	}
	r.file(id, readyCardFile, string(b))
}

// session starts a session of role for task t in the agents store, live
// with the agent status given ("working", "idle", "blocked"), its status
// changed at changed.
func (r *screenRig) session(t registry.Task, role, status string, started, changed time.Time) store.Session {
	r.t.Helper()
	if r.repo == 0 {
		repo, err := r.agents.UpsertRepo(r.ctx, store.Repo{NodeID: "factory:talkable", Owner: "factory", Name: "talkable", WatchOwner: "factory",
			Mode: store.RepoModePerPR})
		if err != nil {
			r.t.Fatal(err)
		}
		r.repo = repo.ID
	}
	title := TaskKey(t)
	up, err := r.agents.UpsertPRFromGitHub(r.ctx, store.GitHubPR{RepoID: r.repo, NodeID: "factory:task:" + strconv.FormatInt(t.ID, 10),
		Number: int(t.ID), URL: "file://" + r.layout.FactoryTaskDir(t.ID), HeadSHA: "t" + strconv.FormatInt(t.ID, 10), Title: &title,
		GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: "factory"})
	if err != nil {
		r.t.Fatal(err)
	}
	name, pane := "factory-"+role+"-"+strconv.FormatInt(t.ID, 10), "w1:p"+strconv.FormatInt(t.ID, 10)+role
	s, err := r.agents.CreateSession(r.ctx, store.Session{PRID: up.PR.ID, Role: role, State: store.SessionLive, AgentStatus: &status,
		AgentStatusAt: &changed, StartedAt: started, AgentName: &name, HerdrPaneID: &pane})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// verified records role as verified in task t's newest attempt (one begun
// at stage when there is none).
func (r *screenRig) verified(t registry.Task, stage string, roles ...string) {
	r.t.Helper()
	as, err := r.reg.Attempts(r.ctx, t.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(as) == 0 {
		a, err := r.reg.BeginAttempt(r.ctx, t.ID, stage)
		if err != nil {
			r.t.Fatal(err)
		}
		as = append(as, a)
	}
	for _, role := range roles {
		if err := r.reg.RecordRoleRun(r.ctx, registry.RoleRun{AttemptID: as[len(as)-1].ID, Role: role, Verified: r.clock.Now(),
			Outcome: "verified"}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// data is ScreenData at screenNow for an operator last seen at lastSeen.
func (r *screenRig) data(lastSeen time.Time) tui.FactoryData {
	r.t.Helper()
	r.cards = map[int64]ReadyCardFile{}
	o := ScreenOptions{Config: r.cfg, Layout: r.layout, ReadyCards: func(id int64, c ReadyCardFile) { r.cards[id] = c }}
	if r.magnum != nil {
		o.Magnum = r.magnum
	}
	d, err := ScreenData(r.ctx, r.reg, r.agents, screenNow, lastSeen, o)
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func rowOf(t *testing.T, d tui.FactoryData, key string) tui.FactoryRow {
	t.Helper()
	for _, row := range d.Rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("no row %s in %+v", key, d.Rows)
	return tui.FactoryRow{}
}

func hasRow(d tui.FactoryData, key string) bool {
	return slices.ContainsFunc(d.Rows, func(r tui.FactoryRow) bool { return r.Key == key })
}

// Home groups the tasks by who they wait on: a task that needs you is in
// NEEDS YOU, with you as its blocker and your move; one that waits on an
// agent, the memory, a person or magnum's review of its PR waits on others,
// each blocker naming who first; a stopped task is not on home.
func TestScreenDataGroupsByWhoTheTaskWaitsOn(t *testing.T) {
	r := newScreenRig(t)
	r.at(41 * time.Minute)
	asks := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	asks = r.questions(asks, question("When does the reminder go out?", "Seven days before it expires", "One day before"),
		question("Who gets the reminder?", "Advocates only"), question("Can a merchant turn it off?"))
	r.move(asks, registry.TaskNeedsYou, waitsYou, "3 questions before coding")
	r.at(18 * time.Minute)
	r.task("PR-12388", "chore", "Weekly gem upgrades", registry.TaskQueued, "", waitMemory, "full, starts below 80% swap")
	r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskWaiting, "questions", "rev-ann", "a question in Jira")
	stopped := r.task("PR-12340", "bug", "Stopped one", registry.TaskActive, "investigate", "", "")
	r.move(stopped, registry.TaskStopped, "", "")
	r.at(30 * time.Hour)
	old := r.task("PR-12302", "chore", "Remove old sync job", registry.TaskActive, "publish", "", "")
	r.published(old, 4519)

	d := r.data(screenNow.Add(-10 * time.Minute))
	if hasRow(d, "PR-12340") {
		t.Errorf("a stopped task is on home: %+v", d.Rows)
	}
	row := rowOf(t, d, "PR-12345")
	if row.Group != tui.GroupNeedsYou || row.Mark != tui.MarkNeedsYou || len(row.WaitsOn) != 1 {
		t.Fatalf("the task that needs you: %+v", row)
	}
	if w := row.WaitsOn[0]; w.Who != tui.WaitsOnYou || !w.OnYou || w.What != "3 questions before coding" || w.Short != "3 questions" {
		t.Errorf("its blocker %+v, want you: 3 questions before coding (short: 3 questions)", w)
	}
	if row.Move != "answer 3 questions" || row.MoveShort != "answer them" || !row.Since.Equal(screenNow.Add(-41*time.Minute)) {
		t.Errorf("its move %q/%q since %v", row.Move, row.MoveShort, row.Since)
	}
	if !strings.Contains(row.Help, `first: "When does the reminder go out?"`) {
		t.Errorf("its help %q names no first question", row.Help)
	}
	for key, want := range map[string]tui.WaitsOn{
		"PR-12388": {Who: "memory", What: "full, starts below 80% swap", Short: "full"},
		"PR-12330": {Who: "rev-ann", What: "a question in Jira"},
		"PR-12302": {Who: "magnum", What: "review of #4519"},
	} {
		row := rowOf(t, d, key)
		if row.Group != tui.GroupWaits || row.Mark != tui.MarkWaits || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want || row.Move != "" {
			t.Errorf("%s: %+v, want it waiting on others with %+v and no move", key, row, want)
		}
	}
	if got := rowOf(t, d, "PR-12302"); got.GitHub != "#4519" || got.Since.IsZero() {
		t.Errorf("a PR published yesterday: %+v, want its number and its wait's start", got)
	}
	if !slices.Equal(d.Repos, []string{"example/talkable"}) {
		t.Errorf("repos %q", d.Repos)
	}
	if card := d.Cards[asks.ID].Task; card.Key != "PR-12345" || card.WaitingFor != "you: 3 questions before coding" || card.Kind != "feature" {
		t.Errorf("the task card %+v", card)
	}
}

// A task that needs you while one of its agents still works has both
// blockers on its one line, you first; it stays still (nothing in NEEDS YOU
// moves).
func TestScreenDataShowsTwoBlockersOnOneLine(t *testing.T) {
	r := newScreenRig(t)
	r.at(2 * time.Hour)
	task := r.task("PR-12310", "bug", "Deploy list on campaigns", registry.TaskActive, "investigate", "", "")
	r.session(task, "fx-history", "working", screenNow.Add(-10*time.Minute), screenNow.Add(-3*time.Second))
	task = r.questions(task, question("Archived campaigns too?", "Live only"))
	r.move(task, registry.TaskNeedsYou, waitsYou, "1 question before investigate goes on")

	row := rowOf(t, r.data(screenNow), "PR-12310")
	want := []tui.WaitsOn{
		{Who: tui.WaitsOnYou, What: "1 question before investigate goes on", Short: "1 question", OnYou: true,
			Long: `1 question before investigate goes on · first: "Archived campaigns too?"`},
		{Who: "agent", What: "reading the history"},
	}
	if !slices.Equal(row.WaitsOn, want) {
		t.Fatalf("blockers %+v, want %+v", row.WaitsOn, want)
	}
	if row.Mark != tui.MarkNeedsYou || row.Group != tui.GroupNeedsYou {
		t.Errorf("row %+v, want a still NEEDS YOU row", row)
	}
	if got := r.data(screenNow).Cards[task.ID].Task.WaitingFor; got != "you: 1 question before investigate goes on · agent: reading the history" {
		t.Errorf("waiting for %q", got)
	}
}

// Tasks and decisions are counted apart: three tasks need you, and they owe
// five decisions (three questions, a ready card, a retry): the chips read
// "3 tasks need you, 5 decisions". A question asked of someone else and a
// task that waits on them owe you nothing.
func TestScreenDataCountsTasksAndDecisionsApart(t *testing.T) {
	r := newScreenRig(t)
	asks := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	asks = r.questions(asks, question("When?", "Seven days"), question("Who?", "Advocates"), question("Off switch?"))
	r.move(asks, registry.TaskNeedsYou, waitsYou, "3 questions before coding")
	ready := r.task("PR-12360", "bug", "Custom activities lose points on copy", registry.TaskNeedsYou, "ready", waitsYou,
		"ready to push (card 1): PR-12360 Custom activities lose points on copy")
	r.card(ready.ID, ReadyCardFile{Revision: 1, Head: "abc", Base: "def", Copy: "talkable.factory3"})
	r.task("PR-12393", "bug", "Referral export misses friends' emails", registry.TaskNeedsYou, "local_review", waitsYou,
		"review unavailable (timeout)")
	other := r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "questions", "", "")
	other = r.questions(other, question("Which locale?"))
	r.file(other.ID, asksFile, `[{"raised_by": "fx-judge", "text": "Which locale?", "login": "rev-ann", "comment": "comment-1-0.md"}]`)
	r.move(other, registry.TaskWaiting, "rev-ann", "a question in Jira")

	d := r.data(screenNow)
	needs := 0
	for _, row := range d.Rows {
		if row.Group == tui.GroupNeedsYou {
			needs++
		}
	}
	if needs != 3 || d.Decisions != 5 {
		t.Fatalf("%d tasks need you, %d decisions; want 3 and 5 (%+v)", needs, d.Decisions, d.Rows)
	}
	if row := rowOf(t, d, "PR-12393"); row.Move != "retry" || row.WaitsOn[0].Short != "review unavailable" {
		t.Errorf("the review unavailable row %+v, want the move retry", row)
	}
}

// DONE TODAY keeps the tasks done today (merged); a task done yesterday is
// gone, and a published one, of today or yesterday, waits on magnum's
// review: its PR is open (TestAPublishedTaskIsNoLongerDoneOnItsDay).
func TestScreenDataKeepsTheTasksDoneToday(t *testing.T) {
	r := newScreenRig(t)
	r.at(30 * time.Hour)
	old := r.task("PR-12301", "chore", "Done yesterday", registry.TaskActive, "publish", "", "")
	r.move(old, registry.TaskDone, "", "")
	yesterday := r.task("PR-12303", "chore", "Published yesterday", registry.TaskActive, "publish", "", "")
	r.published(yesterday, 4520)
	r.clock.Set(time.Date(2026, 11, 4, 8, 10, 0, 0, time.Local))
	merged := r.task("PR-12302", "chore", "Remove old sync job", registry.TaskActive, "publish", "", "")
	r.clock.Set(time.Date(2026, 11, 4, 8, 20, 0, 0, time.Local))
	r.move(merged, registry.TaskDone, "", "")
	pushed := r.task("PR-12305", "bug", "Coupon CSV header names", registry.TaskActive, "publish", "", "")
	r.clock.Set(time.Date(2026, 11, 4, 8, 40, 0, 0, time.Local))
	r.published(pushed, 4519)

	d := r.data(screenNow.Add(-time.Minute))
	if hasRow(d, "PR-12301") {
		t.Errorf("a task done yesterday is on home")
	}
	want := tui.WaitsOn{Who: "nothing", What: "merged 08:20"}
	if row := rowOf(t, d, "PR-12302"); row.Group != tui.GroupDone || row.Mark != tui.MarkDone || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want || !row.Since.IsZero() {
		t.Errorf("PR-12302: %+v, want DONE TODAY with %+v", row, want)
	}
	for key, n := range map[string]int{"PR-12303": 4520, "PR-12305": 4519} {
		row := rowOf(t, d, key)
		if want := (tui.WaitsOn{Who: "magnum", What: "review of #" + strconv.Itoa(n)}); row.Group != tui.GroupWaits || row.WaitsOn[0] != want {
			t.Errorf("%s, published: %+v, want it waiting with %+v", key, row, want)
		}
	}
	if card := d.Cards[pushed.ID].Task; card.PR != "#4519" {
		t.Errorf("the pushed task's card names PR %q", card.PR)
	}
}

// The agents are named by what they do (the role's doing: fx-cause is
// "finding the cause"), with why they exist, their state, how long they
// worked and when their pane last showed something; the row's blocker is
// the agents still at work, with how many of the stage's are done.
func TestScreenDataNamesAgentsByWhatTheyDo(t *testing.T) {
	r := newScreenRig(t)
	r.at(9 * time.Minute)
	task := r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "investigate", "", "")
	r.session(task, "fx-repro", "idle", screenNow.Add(-9*time.Minute), screenNow.Add(-4*time.Minute))
	r.session(task, "fx-history", "idle", screenNow.Add(-9*time.Minute), screenNow.Add(-5*time.Minute))
	r.session(task, "fx-cause", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-20*time.Second))
	r.verified(task, "investigate", "fx-repro", "fx-history")

	d := r.data(screenNow)
	row := rowOf(t, d, "PR-12371")
	if row.Mark != tui.MarkWorking || len(row.WaitsOn) != 1 || row.WaitsOn[0] != (tui.WaitsOn{Who: "agent", What: "finding the cause, 2 of 3 done",
		Short: "finding the cause"}) {
		t.Fatalf("row %+v, want a working row: agent: finding the cause, 2 of 3 done", row)
	}
	agents := d.Cards[task.ID].Task.Agents
	byName := map[string]tui.CardAgent{}
	for _, a := range agents {
		byName[a.Name] = a
	}
	cause := byName["finding the cause"]
	if cause.State != "working" || cause.Why != "find the cause on its own, apart from the others" || cause.For != 8*time.Minute ||
		!cause.Output.Equal(screenNow.Add(-20*time.Second)) {
		t.Errorf("the cause finder %+v", cause)
	}
	if repro := byName["reproducing it"]; repro.State != "done" || repro.Why != "show the bug with a failing spec" {
		t.Errorf("the reproducer %+v (agents %+v)", repro, agents)
	}
	if len(agents) != 3 || agents[0].Name != "finding the cause" {
		t.Errorf("agents %+v, want the three, newest first", agents)
	}
	stages := d.Cards[task.ID].Task.Stages
	if len(stages) == 0 || stages[0].Name != "investigate" || stages[0].State != "working" || stages[len(stages)-1].State != "todo" {
		t.Errorf("stages %+v, want investigate working and the rest to do", stages)
	}
}

// A role whose prompt you let go of (you looked at its pane, and the
// prompt is never sent again) has that prompt's first line on its task
// card's WAITING FOR line, so you see what it may not have received; the
// agent's own line is left as it is (the card draws it only while it works).
func TestScreenDataShowsWhatALetGoPromptAsked(t *testing.T) {
	r := newScreenRig(t)
	task := r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "investigate", "", "")
	r.session(task, "fx-cause", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-time.Minute))
	a, err := r.reg.BeginAttempt(r.ctx, task.ID, "investigate")
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.reg.AppendLedger(r.ctx, registry.LedgerEntry{TaskID: task.ID, AttemptID: a.ID, Role: "fx-cause", Hash: "h1",
		Text: "Find the cause of PR-12371 on your own.\nRead the export code first."})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.MarkDelivered(r.ctx, e.ID, registry.DeliveredAbandoned); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reg.RecordInput(r.ctx, registry.Input{TaskID: task.ID, Kind: registry.InputRetry, Surface: "screen", Stage: "investigate",
		Role: "fx-cause"}); err != nil {
		t.Fatal(err)
	}
	card := r.data(screenNow).Cards[task.ID].Task
	if want := "agent: finding the cause · let go: Find the cause of PR-12371 on your own."; card.WaitingFor != want {
		t.Errorf("waiting for %q, want %q", card.WaitingFor, want)
	}
	if len(card.Agents) != 1 || card.Agents[0].Now != "" {
		t.Errorf("agents %+v, want the cause finder's line left empty", card.Agents)
	}
}

// The ready card carries the factory's card: its FLAGS line, the local
// review's leftovers (the KNOWN row: leftovers, declined findings with
// their risks, doubts, reviewers a round went without), every changed file
// unread with the flagged ones first, what the push frees (the copy, and
// the oldest queued task that then starts in it), the binding of a
// decision, and push recommended after a clean review.
func TestScreenDataFillsTheFlagsAndTheLeftovers(t *testing.T) {
	r := newScreenRig(t)
	r.at(12 * time.Minute)
	ready := r.task("PR-12360", "bug", "Custom activities lose points on copy", registry.TaskNeedsYou, "ready", waitsYou,
		"ready to push (card 2): PR-12360 Custom activities lose points on copy")
	r.task("PR-12395", "bug", "Advocate dashboard loads slowly", registry.TaskQueued, "", waitCopy, "all 3 busy")
	flags := []string{"not in the plan: spec/factories/reward_rules.rb", "sensitive: Gemfile.lock"}
	r.card(ready.ID, ReadyCardFile{Revision: 2, Head: "ffff1111", Base: "base2222", ReportHash: "rh", DiffHash: "dh",
		Branch: "zhuravel-PR-12360-points", Remote: "zhuravel-PR-12360-points", Copy: "talkable.factory3", CopyPath: "/copies/talkable.factory3",
		Flags: flags, Review: ReviewSummary{Exit: registry.ExitClean, Rounds: 2,
			Leftovers: []ReviewItem{{Severity: "P3", Path: "app/services/campaign_copier.rb", Line: 18, Title: "the name copy_rules hides a step"}},
			Declined:  []ReviewItem{{Severity: "P2", Path: "spec/copier_spec.rb", Line: 40, Title: "one spec per rule", Risk: "3 slower specs"}},
			Doubts:    []ReviewItem{{Severity: "P2", Title: "archived rules may be skipped"}},
			Missing:   []string{"codex-review"}},
		Files: []CardFile{
			{Path: "Gemfile.lock", Added: 2, Removed: 2, InPlan: true, Flagged: true},
			{Path: "spec/factories/reward_rules.rb", Added: 2},
			{Path: "app/services/campaign_copier.rb", Added: 31, Removed: 6, InPlan: true},
		}})
	r.file(ready.ID, reportFile, "## Architecture decisions\nnone\n\n## Cause\ncopying a campaign copies its activities first\n\n"+
		"## Proof\nReproduced: yes\n\n## Plan\n- copy the reward rules first\n\n## Doubts\n- archived campaigns: not tested\n")

	d := r.data(screenNow)
	cards := d.Cards[ready.ID]
	c := cards.Ready
	if c == nil {
		t.Fatalf("no ready card: %+v", cards)
	}
	if !slices.Equal(c.Flags, flags) {
		t.Errorf("flags %q, want the card's %q", c.Flags, flags)
	}
	wantKnown := []string{
		"P3 app/services/campaign_copier.rb:18: the name copy_rules hides a step",
		"declined: P2 spec/copier_spec.rb:40: one spec per rule. The risk of the suggested change: 3 slower specs",
		"a doubt: P2 archived rules may be skipped (it came back twice after its fix)",
		"a round of the review went without codex-review's report",
	}
	if !slices.Equal(c.Leftovers, wantKnown) {
		t.Errorf("leftovers %q, want %q", c.Leftovers, wantKnown)
	}
	wantUnread := []string{"Gemfile.lock", "spec/factories/reward_rules.rb", "app/services/campaign_copier.rb"}
	if !slices.Equal(c.Unread, wantUnread) {
		t.Errorf("unread %q, want every file, the flagged first: %q", c.Unread, wantUnread)
	}
	if len(c.Files) != 3 || c.Files[0] != (tui.CardFile{Path: "Gemfile.lock", Added: 2, Removed: 2, InPlan: true, Flagged: true}) ||
		c.Files[1].InPlan {
		t.Errorf("files %+v", c.Files)
	}
	if c.Frees != "talkable.factory3; PR-12395 starts in it" || c.Copy != "talkable.factory3" || c.Branch != "zhuravel-PR-12360-points" {
		t.Errorf("frees %q, copy %q, branch %q", c.Frees, c.Copy, c.Branch)
	}
	if c.Recommended != 0 || c.Why == "" || c.NoPush != "" || len(c.Failed) != 0 || c.Waited != 12*time.Minute {
		t.Errorf("recommended %d (%q), no push %q, failed %q, waited %v", c.Recommended, c.Why, c.NoPush, c.Failed, c.Waited)
	}
	if c.Cause != "copying a campaign copies its activities first" || c.Fix != "copy the reward rules first" ||
		!slices.Equal(c.Doubts, []string{"archived campaigns: not tested"}) {
		t.Errorf("cause %q, fix %q, doubts %q", c.Cause, c.Fix, c.Doubts)
	}
	if cards.Bind != (tui.ReadyBinding{HeadSHA: "ffff1111", ReportHash: "rh", DiffHash: "dh"}) {
		t.Errorf("binding %+v", cards.Bind)
	}
	// The card handed back for the pager is the one drawn, read once.
	if got, ok := r.cards[ready.ID]; !ok || got.Head != "ffff1111" || got.CopyPath != "/copies/talkable.factory3" || len(r.cards) != 1 {
		t.Errorf("the ready cards handed back %+v, want the drawn card alone", r.cards)
	}
	row := rowOf(t, d, "PR-12360")
	if w := row.WaitsOn[0]; w.What != "ready to push · 3 files +35 −8" || w.Short != "ready to push" || row.Move != "read the diff, then push" {
		t.Errorf("row %+v", row)
	}

	// A card whose conflict with the base sent the task back offers no push.
	conflict := r.task("PR-12399", "bug", "Conflicting one", registry.TaskNeedsYou, "ready", waitsYou, "ready to push (card 1): PR-12399")
	r.card(conflict.ID, ReadyCardFile{Revision: 1, Head: "a", Base: "b", Copy: "talkable.factory2",
		Flags: []string{"conflicts with the base: app/models/campaign.rb"}})
	if c := r.data(screenNow).Cards[conflict.ID].Ready; c == nil || c.NoPush == "" || c.Recommended == 0 {
		t.Errorf("a conflict card %+v, want push not offered and not recommended", c)
	}
}

// A queued row names what frees it: the oldest queued task waits for the
// push of a task at its ready card, the others for it to start first.
func TestScreenDataNamesWhatFreesAQueuedTask(t *testing.T) {
	r := newScreenRig(t)
	ready := r.task("PR-12360", "bug", "Custom activities lose points on copy", registry.TaskNeedsYou, "ready", waitsYou,
		"ready to push (card 1): PR-12360")
	r.card(ready.ID, ReadyCardFile{Revision: 1, Copy: "talkable.factory3"})
	r.task("PR-12395", "bug", "Advocate dashboard loads slowly", registry.TaskQueued, "", waitCopy, "all 3 busy")
	r.task("PR-12396", "chore", "Later", registry.TaskQueued, "", waitCopy, "all 3 busy")

	d := r.data(screenNow)
	first := rowOf(t, d, "PR-12395")
	if first.Frees != "frees when PR-12360 is pushed" || first.WaitsOn[0] != (tui.WaitsOn{Who: "copy", What: "all 3 busy", Short: "after PR-12360"}) {
		t.Errorf("the oldest queued row %+v", first)
	}
	if !strings.Contains(first.Help, "the push of PR-12360 frees talkable.factory3 for it") {
		t.Errorf("its help %q", first.Help)
	}
	if second := rowOf(t, d, "PR-12396"); second.Frees != "starts after PR-12395" {
		t.Errorf("the second queued row frees %q", second.Frees)
	}
}

// After more than an hour away, home digests the tasks that finished since
// you were last seen, newest first, and the chip says how long you were
// away, how many finished and how many came to need you meanwhile; after a
// short absence, or with no time seen yet, there is no digest.
func TestScreenDataDigestsWhatFinishedWhileYouWereAway(t *testing.T) {
	r := newScreenRig(t)
	seen := time.Date(2026, 11, 3, 18, 40, 0, 0, time.Local)
	r.clock.Set(seen.Add(-2 * time.Hour))
	before := r.task("PR-12304", "chore", "Finished before", registry.TaskActive, "publish", "", "")
	r.move(before, registry.TaskDone, "", "")
	merged := r.task("PR-12302", "chore", "Remove old sync job", registry.TaskActive, "publish", "", "")
	pushed := r.task("PR-12318", "chore", "Seed data into the copy's own databases", registry.TaskActive, "publish", "", "")
	r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskNeedsYou, "questions", waitsYou, "1 question before coding")
	r.clock.Set(time.Date(2026, 11, 3, 18, 52, 0, 0, time.Local))
	r.published(pushed, 4541)
	r.clock.Set(time.Date(2026, 11, 3, 21, 15, 0, 0, time.Local))
	r.move(merged, registry.TaskDone, "", "")
	r.clock.Set(time.Date(2026, 11, 3, 23, 0, 0, 0, time.Local))
	r.task("PR-12360", "bug", "Custom activities lose points on copy", registry.TaskNeedsYou, "ready", waitsYou, "ready to push (card 1)")

	d := r.data(seen)
	if d.Digest == nil {
		t.Fatal("no digest after a night away")
	}
	want := tui.Digest{Since: seen, Chip: "away 14h: 2 done, 1 new for you", Done: []string{
		"PR-12302 Remove old sync job · merged 21:15",
		"PR-12318 Seed data into the copy's own databases · you pushed 18:52, PR #4541",
	}}
	if !d.Digest.Since.Equal(want.Since) || d.Digest.Chip != want.Chip || !slices.Equal(d.Digest.Done, want.Done) {
		t.Errorf("digest %+v, want %+v", *d.Digest, want)
	}
	if d := r.data(screenNow.Add(-50 * time.Minute)); d.Digest != nil {
		t.Errorf("a digest after 50 minutes away: %+v", d.Digest)
	}
	if d := r.data(time.Time{}); d.Digest != nil {
		t.Errorf("a digest with no time seen: %+v", d.Digest)
	}
}

// A hold of the machine is a blocker line of home (memory full: new tasks
// wait), short and with its reason on a wide screen, as the daemon last saw
// it; the tasks it holds name it as their blocker.
func TestScreenDataShowsAHoldAsABlocker(t *testing.T) {
	r := newScreenRig(t)
	holds := []Hold{{Kind: HoldInfra, Reason: "SSH key refused", Since: screenNow.Add(-5 * time.Minute)},
		{Kind: HoldMemory, Reason: "swap 91% used, launches wait below 80%", Since: screenNow.Add(-time.Hour)}}
	b, err := json.Marshal(holds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryHolds()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.layout.FactoryHolds(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.task("PR-12388", "chore", "Weekly gem upgrades", registry.TaskQueued, "", waitMemory, "full, starts below 80% swap")
	r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "investigate", HoldMemory, "swap 91% used, launches wait below 80%")

	d := r.data(screenNow)
	if want := []string{"infra down: every stage waits", "memory full: new tasks wait"}; !slices.Equal(d.Blocking, want) {
		t.Errorf("blocking %q, want %q", d.Blocking, want)
	}
	if want := []string{"infra down: every stage waits (SSH key refused)", "memory full: new tasks wait (swap 91% used, launches wait below 80%)"}; !slices.Equal(d.BlockingLong, want) {
		t.Errorf("blocking long %q, want %q", d.BlockingLong, want)
	}
	if row := rowOf(t, d, "PR-12371"); row.WaitsOn[0] != (tui.WaitsOn{Who: "memory", What: "swap 91% used, launches wait below 80%", Short: "full"}) ||
		row.Mark != tui.MarkWaits {
		t.Errorf("the held task %+v", row)
	}

	// No hold: no blocker line.
	if err := os.WriteFile(r.layout.FactoryHolds(), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := r.data(screenNow); len(d.Blocking) != 0 || len(d.BlockingLong) != 0 {
		t.Errorf("blocking %q %q with no hold", d.Blocking, d.BlockingLong)
	}
}

// With the factory daemon down nothing moves, and home says so: one blocker
// line names it and how to start it, holds.json (the last daemon's) is not
// read, and no working mark turns, since no agent's state is read live.
func TestScreenDataWithTheDaemonDownSaysSoAndMovesNothing(t *testing.T) {
	r := newScreenRig(t)
	b, err := json.Marshal([]Hold{{Kind: HoldInfra, Reason: "SSH key refused", Since: screenNow.Add(-5 * time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryHolds()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.layout.FactoryHolds(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	r.at(9 * time.Minute)
	task := r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "investigate", "", "")
	r.session(task, "fx-cause", "working", screenNow.Add(-8*time.Minute), screenNow.Add(-20*time.Second))

	d, err := ScreenData(r.ctx, r.reg, r.agents, screenNow, screenNow, ScreenOptions{Config: r.cfg, Layout: r.layout, DaemonDown: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{daemonDownShort}; !slices.Equal(d.Blocking, want) {
		t.Errorf("blocking %q, want %q", d.Blocking, want)
	}
	if len(d.BlockingLong) != 1 || !strings.Contains(d.BlockingLong[0], "magnum factory install") || strings.Contains(d.BlockingLong[0], "SSH") {
		t.Errorf("blocking long %q, want the daemon's line naming how to start it, and no hold", d.BlockingLong)
	}
	if row := rowOf(t, d, "PR-12371"); row.Mark != tui.MarkWaits {
		t.Errorf("row %+v, want it waiting, not working", row)
	}
	if up := r.data(screenNow); rowOf(t, up, "PR-12371").Mark != tui.MarkWorking || len(up.Blocking) != 1 || !strings.HasPrefix(up.Blocking[0], "infra") {
		t.Errorf("with the daemon up: blocking %q, row %+v", up.Blocking, rowOf(t, up, "PR-12371"))
	}
}

// The task card offers r exactly when a retry goes on: the check is the
// retry's own (retryRefusal), so a task that waits for your answers, or
// does not wait on you, offers none, and one whose questions wait on the
// person you asked does.
func TestTheTaskCardOffersRetryExactlyWhenARetryWouldGoOn(t *testing.T) {
	r := newScreenRig(t)
	unavailable := r.task("PR-12393", "bug", "Review unavailable", registry.TaskNeedsYou, "local_review", waitsYou, "review unavailable (timeout)")
	answers := r.task("PR-12345", "feature", "Questions on you", registry.TaskActive, "questions", "", "")
	answers = r.questions(answers, question("When?", "Seven days"))
	answers = r.move(answers, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	asked := r.task("PR-12330", "bug", "Asked of rev-ann", registry.TaskActive, "questions", "", "")
	asked = r.questions(asked, question("Which locale?"))
	r.file(asked.ID, asksFile, `[{"raised_by": "fx-judge", "text": "Which locale?", "login": "rev-ann", "comment": "c.md"}]`)
	asked = r.move(asked, registry.TaskNeedsYou, waitsYou, "the questions changed")
	working := r.task("PR-12371", "bug", "Working", registry.TaskActive, "investigate", "", "")

	d := r.data(screenNow)
	for _, tc := range []struct {
		task registry.Task
		want bool
	}{{unavailable, true}, {answers, false}, {asked, true}, {working, false}} {
		got := d.Cards[tc.task.ID].Task.Retry
		guard := retryRefusal(r.ctx, r.reg, r.layout, tc.task)
		if got != tc.want || got != (guard == nil) {
			t.Errorf("%s: retry %v (the retry's guard: %v), want %v", TaskKey(tc.task), got, guard, tc.want)
		}
	}
}

// The question card is the newest revision's questions with their options,
// the factory's recommendation and why, your own words as the last option,
// the answers the factory has (the screen sends none of them again), and a
// question asked of someone with whom and the drafted comment.
func TestScreenDataFillsTheQuestionCard(t *testing.T) {
	r := newScreenRig(t)
	r.at(41 * time.Minute)
	task := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	who := question("Who gets the reminder?", "Advocates only", "Advocates and friends")
	when := question("When does the reminder go out?", "Seven days before it expires")
	when.Context, when.Example, when.Options[0].Means = "The ticket names no day.", "a credit that expires on Fri 14 Nov.", "Fri 7 Nov"
	off := question("Can a merchant turn it off?")
	task = r.questions(task, who, when, off)
	r.at(30 * time.Minute)
	if err := r.reg.AnswerBatch(r.ctx, task.ID, task.QuestionRev, map[int]string{0: "Advocates only"}); err != nil {
		t.Fatal(err)
	}
	r.file(task.ID, asksFile, `[{"raised_by": "fx-judge", "text": "Can a merchant turn it off?", "login": "rev-ann", "comment": "comment-1-2.md"}]`)
	r.file(task.ID, "comment-1-2.md", "rev-ann, a question on PR-12345: Can a merchant turn it off?\n")
	task = r.move(task, registry.TaskNeedsYou, waitsYou, "1 question before coding")

	q := r.data(screenNow).Cards[task.ID].Question
	if q == nil {
		t.Fatal("no question card")
	}
	if q.TaskKey != "PR-12345" || q.Revision != task.QuestionRev || !q.Needless || !q.Asked.Equal(screenNow.Add(-41*time.Minute)) ||
		!q.AsOf.Equal(q.Asked) || len(q.Questions) != 3 {
		t.Fatalf("card %+v", q)
	}
	first, second, third := q.Questions[0], q.Questions[1], q.Questions[2]
	if first.Answer != "Advocates only" || !first.TookRecommendation || !first.AnsweredAt.Equal(screenNow.Add(-30*time.Minute)) {
		t.Errorf("the answered question %+v", first)
	}
	if second.Context != "The ticket names no day." || second.Example == "" || second.Recommended != 0 || second.Why != "the ticket says so" ||
		second.Answer != "" || second.Options[0] != (tui.CardOption{Label: "Seven days before it expires", Means: "Fri 7 Nov"}) {
		t.Errorf("the open question %+v", second)
	}
	if last := second.Options[len(second.Options)-1]; !last.Own || last.Label != "Type your own answer" {
		t.Errorf("its last option %+v, want your own answer", last)
	}
	if third.Asks != "rev-ann" || third.Answer != "ask rev-ann in Jira" || !strings.Contains(third.Comment, "Can a merchant turn it off?") ||
		!slices.ContainsFunc(third.Options, func(o tui.CardOption) bool { return o.Send == "ask:rev-ann" }) {
		t.Errorf("the asked question %+v", third)
	}
	if row := rowOf(t, r.data(screenNow), "PR-12345"); row.Move != "answer 1 question" || row.MoveShort != "answer it" {
		t.Errorf("row %+v", row)
	}
}

// `!` lists the alert events, newest first, each with its time; other
// events are not alerts.
func TestScreenDataListsTheAlertsNewestFirst(t *testing.T) {
	r := newScreenRig(t)
	task := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	for i, text := range []string{"PR-12345: you: 3 questions before coding", "factory: you: infra: SSH key refused, held since 08:00"} {
		r.at(time.Duration(2-i) * time.Hour)
		id := task.ID
		if i == 1 {
			id = 0
		}
		if err := r.reg.AppendEvent(r.ctx, id, "alert", registry.PhaseInfo, text); err != nil {
			t.Fatal(err)
		}
		if err := r.reg.AppendEvent(r.ctx, id, "stage", registry.PhaseInfo, "not an alert"); err != nil {
			t.Fatal(err)
		}
	}
	want := []tui.FactoryAlert{
		{At: screenNow.Add(-time.Hour), Text: "factory: you: infra: SSH key refused, held since 08:00"},
		{At: screenNow.Add(-2 * time.Hour), Text: "PR-12345: you: 3 questions before coding"},
	}
	got := r.data(screenNow).Alerts
	if len(got) != len(want) {
		t.Fatalf("alerts %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].Text != want[i].Text {
			t.Errorf("alert %d %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The daemon saves the holds in force for the screen on its first tick and
// whenever they change, and a hold that lifts leaves the file empty.
func TestTheDaemonSavesItsHoldsForTheScreen(t *testing.T) {
	var critical atomic.Bool
	critical.Store(true)
	h := newHarness(t, func(d *Deps) {
		d.Memory = func(context.Context) engine.MemorySample {
			if critical.Load() {
				return engine.MemorySample{Level: engine.MemoryCritical}
			}
			return engine.MemorySample{Level: engine.MemoryNormal}
		}
	})
	h.tick()
	hs, err := ReadHolds(h.layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].Kind != HoldMemory || !strings.HasPrefix(hs[0].Reason, "critical pressure") {
		t.Fatalf("holds %+v, want the memory hold", hs)
	}
	critical.Store(false)
	h.tick()
	if hs, err = ReadHolds(h.layout); err != nil || len(hs) != 0 {
		t.Fatalf("holds %+v (%v), want none once it lifted", hs, err)
	}
}

// The daemon keeps the screen's latest focus report where the alerts'
// delivery reads it: a report that arrives late after a newer one changes
// nothing, and a screen whose process ended holds nothing.
func TestTheDaemonKeepsTheScreensLatestFocus(t *testing.T) {
	h := newHarness(t)
	focus := func(focused bool, at time.Time) registry.Request {
		t.Helper()
		id, err := h.reg.EnqueueRequest(h.ctx, ReqFocus, FocusPayload{Focused: focused, PID: os.Getpid(), At: at})
		if err != nil {
			t.Fatal(err)
		}
		h.tick()
		q, err := h.reg.RequestByID(h.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if q := focus(true, t0.Add(time.Minute)); q.State != registry.RequestDone {
		t.Fatalf("focus request %+v", q)
	}
	if q := focus(false, t0); q.State != registry.RequestDone {
		t.Fatalf("a late blur %+v", q)
	}
	f, err := ReadScreenFocus(h.layout)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Focused || f.PID != os.Getpid() || !f.At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("kept %+v, want the newer focus", f)
	}
	if (FocusPayload{Focused: true, PID: 1 << 22, At: t0}).Live() {
		t.Error("the focus of a pid that runs no process holds")
	}
	if (FocusPayload{Focused: false, PID: os.Getpid(), At: t0}).Live() {
		t.Error("a blurred screen holds")
	}
	if ins, err := h.reg.Inputs(h.ctx, 0); err != nil || len(ins) != 0 {
		t.Errorf("a focus report recorded inputs %+v (%v)", ins, err)
	}
}

// The status rule's news is the daemon's answer to your newest request, read
// typed from the requests: the screen's own focus reports and previews
// after it never push it out, and it goes once it is older than flashFor.
func TestScreenDataFlashesYourNewestAnswer(t *testing.T) {
	r := newScreenRig(t)
	r.at(3 * time.Minute)
	id, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: "PR-12345"})
	if err == nil {
		err = r.reg.CompleteRequest(r.ctx, id, registry.RequestDone, "started PR-12345 as task 1: feature in example/talkable on b\nmore")
	}
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		r.at(2*time.Minute - time.Duration(i)*time.Second)
		for _, kind := range []string{ReqFocus, ReqPreview} {
			id, err := r.reg.EnqueueRequest(r.ctx, kind, nil)
			if err == nil {
				err = r.reg.CompleteRequest(r.ctx, id, registry.RequestDone, "ok")
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, want := r.data(screenNow).Flash, clock(screenNow.Add(-3*time.Minute))+" started PR-12345 as task 1: feature in example/talkable on b"; got != want {
		t.Errorf("flash %q, want %q", got, want)
	}
	d, err := ScreenData(r.ctx, r.reg, r.agents, screenNow.Add(flashFor), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil || d.Flash != "" {
		t.Errorf("flash %q (%v) after %s, want none", d.Flash, err, flashFor)
	}
}

// One task the screen cannot read (its asks.json is not JSON) is a row
// that says so, as a wait on you, and the rest of home is drawn.
func TestOneUnreadableTaskLeavesTheRestOfHome(t *testing.T) {
	r := newScreenRig(t)
	broken := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	broken = r.questions(broken, question("When?", "Seven days"))
	r.move(broken, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	r.file(broken.ID, asksFile, "{not json")
	r.task("PR-12371", "bug", "Coupon CSV export times out", registry.TaskActive, "investigate", "", "")

	d := r.data(screenNow)
	row := rowOf(t, d, "PR-12345")
	if row.Group != tui.GroupNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0].Who != tui.WaitsOnYou || !row.WaitsOn[0].OnYou ||
		!strings.HasPrefix(row.WaitsOn[0].What, "the screen cannot read it: ") || !strings.Contains(row.WaitsOn[0].What, asksFile) {
		t.Errorf("the unreadable task's row %+v", row)
	}
	if card := d.Cards[broken.ID].Task; card.Key != "PR-12345" || !strings.Contains(card.WaitingFor, "the screen cannot read it") {
		t.Errorf("its card %+v", card)
	}
	if !hasRow(d, "PR-12371") {
		t.Errorf("the other task is not on home: %+v", d.Rows)
	}
}

// The screen's own requests do not grow the registry: a preview forgets
// your draft once answered, and the daemon prunes the focus reports and the
// previews answered more than a day ago, with their events; your other
// requests stay.
func TestTheDaemonForgetsTheScreensOwnRequests(t *testing.T) {
	h := newHarness(t)
	enqueue := func(kind string, payload any) int64 {
		t.Helper()
		id, err := h.reg.EnqueueRequest(h.ctx, kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	preview := enqueue(ReqPreview, PreviewPayload{Text: "PR-12345 my pasted draft"})
	focus := enqueue(ReqFocus, FocusPayload{Focused: true, PID: os.Getpid(), At: t0})
	stop := enqueue(ReqStop, StopPayload{TaskID: 999, Surface: "screen"})
	h.tick()
	q, err := h.reg.RequestByID(h.ctx, preview)
	if err != nil || q.State != registry.RequestDone || string(q.Payload) != "{}" {
		t.Fatalf("the answered preview %+v (%v), want its payload forgotten", q, err)
	}
	for _, ev := range h.events(0, "request") {
		if strings.Contains(ev.Text, "my pasted draft") {
			t.Errorf("a preview's event carries the draft: %q", ev.Text)
		}
	}
	h.clock.Add(25 * time.Hour)
	h.tick()
	for id, kept := range map[int64]bool{preview: false, focus: false, stop: true} {
		if _, err := h.reg.RequestByID(h.ctx, id); (err == nil) != kept {
			t.Errorf("request %d kept %v, want %v", id, err == nil, kept)
		}
	}
	for _, ev := range h.events(0, "request") {
		if strings.Contains(ev.Text, "(preview)") || strings.Contains(ev.Text, "(focus)") {
			t.Errorf("a pruned request's event stays: %q", ev.Text)
		}
	}
}

// Every question of a ticket task offers to ask the ticket's reporter in
// Jira (kept beside its ticket, on one line and bounded); a question asked
// of someone already keeps that name, and a task of your words or a ticket
// with no reporter offers no ask.
func TestTheQuestionCardOffersToAskTheTicketsReporter(t *testing.T) {
	r := newScreenRig(t)
	ticket := r.task("PR-12345", "feature", "Credit expiry reminder email", registry.TaskActive, "questions", "", "")
	ticket = r.questions(ticket, question("When does the reminder go out?", "Seven days"), question("Can a merchant turn it off?"))
	r.file(ticket.ID, reporterFile, "rev-\x1b[31mann\n")
	r.file(ticket.ID, asksFile, `[{"raised_by": "fx-judge", "text": "Can a merchant turn it off?", "login": "alice", "comment": "c.md"}]`)
	r.move(ticket, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	words, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "rename the helper", Kind: "chore", Repo: "example/talkable", Title: "Rename",
		State: registry.TaskActive, Stage: "questions"})
	if err != nil {
		t.Fatal(err)
	}
	words = r.questions(words, question("Which helper?"))
	r.move(words, registry.TaskNeedsYou, waitsYou, "1 question before coding")
	none := r.task("PR-12346", "bug", "No reporter", registry.TaskActive, "questions", "", "")
	none = r.questions(none, question("Which page?"))
	r.move(none, registry.TaskNeedsYou, waitsYou, "1 question before coding")

	d := r.data(screenNow)
	qs := d.Cards[ticket.ID].Question.Questions
	ask := tui.CardOption{Label: "Ask rev- [31mann in Jira (the task waits)", Send: "ask:rev- [31mann"}
	if got := qs[0].Options; len(got) != 3 || got[1] != ask || !got[2].Own {
		t.Errorf("the open question's options %+v, want its own, then the reporter's ask, then your words", got)
	}
	if got := qs[1].Options; !slices.ContainsFunc(got, func(o tui.CardOption) bool { return o.Send == "ask:alice" }) ||
		slices.ContainsFunc(got, func(o tui.CardOption) bool { return o.Send == ask.Send }) {
		t.Errorf("the asked question's options %+v, want alice's ask only", got)
	}
	for _, id := range []int64{words.ID, none.ID} {
		if got := d.Cards[id].Question.Questions[0].Options; slices.ContainsFunc(got, func(o tui.CardOption) bool { return o.Send != "" }) {
			t.Errorf("task %d offers an ask: %+v", id, got)
		}
	}
	long := strings.Repeat("ü", 100)
	r.file(ticket.ID, reporterFile, long)
	if got := r.data(screenNow).Cards[ticket.ID].Question.Questions[0].Options[1].Send; len(got) > len(askPrefix)+maxReporter {
		t.Errorf("a long reporter's ask %q, want it bounded", got)
	}
}

// An ask of the ticket's reporter takes their whole display name, spaces
// included, as the card offers it; the task then waits on them, and the
// comment drafted for them greets them by it.
func TestAnAskOfTheTicketsReporterTakesTheirWholeName(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12345", registry.TaskActive, "questions")
	if err := h.reg.ReplaceQuestions(h.ctx, task.ID, 1, []registry.Question{question("Can a merchant turn it off?")}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.TransitionTask(h.ctx, task.ID, []string{registry.TaskActive}, registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
		u.Set("waits_who", waitsYou)
		u.Set("waits_what", "1 question before coding")
	}); err != nil {
		t.Fatal(err)
	}
	dir := h.layout.FactoryTaskDir(task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reporterFile), []byte("Rev Ann\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply, err := h.e.answerRequest(h.ctx, AnswerPayload{TaskID: task.ID, Revision: 1, Answers: map[int]string{0: "ask:Rev Ann"}, Surface: "screen"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "question 1 asked of Rev Ann:") { // the reply reads the ask as the files do
		t.Errorf("the reply %q, want it to name the reporter whole", reply)
	}
	asks, err := readAsksIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := asks[QuestionKey("Can a merchant turn it off?")]
	if !ok || a.Login != "Rev Ann" {
		t.Fatalf("asks %+v, want the reporter asked by their whole name", asks)
	}
	b, err := os.ReadFile(filepath.Join(dir, a.Comment))
	if err != nil || !strings.HasPrefix(string(b), "Rev Ann, a question on PR-12345") {
		t.Errorf("the drafted comment %q (%v)", b, err)
	}
	if got := h.reload(task.ID); got.WaitsWho != "Rev Ann" || got.WaitsWhat != "a question in Jira" {
		t.Errorf("task waits on %q: %q, want the reporter", got.WaitsWho, got.WaitsWhat)
	}
}

// A ticket's reporter is kept beside its ticket when the ticket is read,
// and ticket.md, which the agents and a round's judge read, stays as it was.
func TestAStartKeepsTheTicketsReporterBesideItsTicket(t *testing.T) {
	r := newIntakeRig(t)
	b, err := json.Marshal(map[string]any{"key": "PR-12347", "fields": map[string]any{
		"summary": "Coupon CSV header names", "issuetype": map[string]string{"name": "Bug"}, "status": map[string]string{"name": "To Do"},
		"description": "The header names are wrong.", "reporter": map[string]string{"displayName": "rev-ann", "emailAddress": "rev-ann@example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.site.issues["PR-12347"] = string(b)
	task, err := r.start("PR-12347")
	if err != nil {
		t.Fatal(err)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	if got, err := os.ReadFile(filepath.Join(dir, reporterFile)); err != nil || string(got) != "rev-ann\n" {
		t.Errorf("%s: %q (%v)", reporterFile, got, err)
	}
	if md, err := os.ReadFile(filepath.Join(dir, ticketFile)); err != nil || strings.Contains(string(md), "rev-ann") {
		t.Errorf("ticket.md carries the reporter (%v):\n%s", err, md)
	}
	if got := readReporter(dir); got != "rev-ann" {
		t.Errorf("readReporter = %q", got)
	}
}
