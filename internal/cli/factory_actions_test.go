package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenActionsTest is the factory screen's source and actions over a
// daemon group test whose factory daemon (answer) answers each request on
// the CLI's first wait; it records every request and every kick.
type screenActionsTest struct {
	*daemonGroupTest
	src   *factoryScreenSource
	act   *factoryActions
	kicks int
	seen  []registry.Request
}

func newScreenActionsTest(t *testing.T, pid int, answer func(registry.Request) (state, result string)) *screenActionsTest {
	t.Helper()
	at := &screenActionsTest{}
	at.daemonGroupTest = newFactoryStartTest(t, pid, func(q registry.Request) (string, string) {
		at.seen = append(at.seen, q)
		if answer == nil {
			return registry.RequestDone, "ok " + q.Kind
		}
		return answer(q)
	})
	kick := daemonSys.FactoryKick
	daemonSys.FactoryKick = func(l paths.Layout) (int, error) {
		at.kicks++
		return kick(l)
	}
	at.src = &factoryScreenSource{c: at.ctx, now: func() time.Time { return screenActionsNow }}
	t.Cleanup(at.src.close)
	at.act = newFactoryActions(at.ctx, at.src, nil)
	return at
}

var screenActionsNow = time.Date(2026, 11, 4, 9, 5, 0, 0, time.Local)

// registry opens the test's factory registry.
func (at *screenActionsTest) registry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(at.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return reg
}

func payloadOf[T any](t *testing.T, q registry.Request) T {
	t.Helper()
	var p T
	if err := json.Unmarshal(q.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every action key of the screen is a factory request with the screen as
// its surface: the adapter queues it, wakes the factory daemon and returns
// its answer; the indexes of an answer go as the screen sends them, from 0,
// and a start carries "from your words only" and the repository.
func TestTheFactoryScreensActionsAreFactoryRequests(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	ctx := t.Context()
	bound := tui.ReadyBinding{HeadSHA: "ffff1111", ReportHash: "rh", DiffHash: "dh", Shown: screenActionsNow.Add(-5 * time.Minute),
		DiffOpened: screenActionsNow.Add(-4 * time.Minute), DiffDwell: 90 * time.Second}
	for _, tc := range []struct {
		name string
		do   func() (tui.ActionResult, error)
		kind string
		want any
		got  func(registry.Request) any
	}{
		{"start", func() (tui.ActionResult, error) {
			return at.act.Start(ctx, "PR-12399 the reminder", "bug", "example/app", true)
		},
			factory.ReqStart, factory.StartPayload{Text: "PR-12399 the reminder", Kind: "bug", Repo: "example/app", WordsOnly: true, Surface: "screen"},
			func(q registry.Request) any { return payloadOf[factory.StartPayload](t, q) }},
		{"answer", func() (tui.ActionResult, error) {
			return at.act.Answer(ctx, 7, 3, map[int]string{0: "Seven days", 2: "ask:rev-ann"}, map[int]string{1: "code-answers"})
		}, factory.ReqAnswer, "7 3 map[0:Seven days 2:ask:rev-ann] map[1:code-answers] screen",
			func(q registry.Request) any {
				p := payloadOf[factory.AnswerPayload](t, q)
				return strconv.FormatInt(p.TaskID, 10) + " " + strconv.Itoa(p.Revision) + " " + fmtMap(p.Answers) + " " + fmtMap(p.Needless) + " " + p.Surface
			}},
		{"decide", func() (tui.ActionResult, error) {
			return at.act.Decide(ctx, 7, tui.FactorySendBack, "too-much", "keep the copier", bound)
		}, factory.ReqDecide,
			factory.DecidePayload{TaskID: 7, Choice: factory.ChoiceSendBack, Reason: "too-much", Note: "keep the copier", HeadSHA: "ffff1111",
				ReportHash: "rh", DiffHash: "dh", Shown: bound.Shown, DiffOpened: bound.DiffOpened, DiffDwell: bound.DiffDwell, Surface: "screen"},
			func(q registry.Request) any { return payloadOf[factory.DecidePayload](t, q) }},
		{"stop", func() (tui.ActionResult, error) { return at.act.Stop(ctx, 7) }, factory.ReqStop,
			factory.StopPayload{TaskID: 7, Surface: "screen"}, func(q registry.Request) any { return payloadOf[factory.StopPayload](t, q) }},
		{"retry", func() (tui.ActionResult, error) { return at.act.Retry(ctx, 7) }, factory.ReqRetry,
			factory.RetryPayload{TaskID: 7, Surface: "screen"}, func(q registry.Request) any { return payloadOf[factory.RetryPayload](t, q) }},
	} {
		kicks, n := at.kicks, len(at.seen)
		res, err := tc.do()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(at.seen) != n+1 || at.kicks != kicks+1 {
			t.Fatalf("%s: %d requests and %d kicks, want one of each", tc.name, len(at.seen)-n, at.kicks-kicks)
		}
		q := at.seen[n]
		if q.Kind != tc.kind || res.Text != "ok "+tc.kind {
			t.Errorf("%s: request %s answered %q", tc.name, q.Kind, res.Text)
		}
		got := tc.got(q)
		if d, ok := got.(factory.DecidePayload); ok {
			w := tc.want.(factory.DecidePayload)
			if !d.Shown.Equal(w.Shown) || !d.DiffOpened.Equal(w.DiffOpened) {
				t.Errorf("%s: times %v %v, want %v %v", tc.name, d.Shown, d.DiffOpened, w.Shown, w.DiffOpened)
			}
			d.Shown, d.DiffOpened, w.Shown, w.DiffOpened = time.Time{}, time.Time{}, time.Time{}, time.Time{}
			got, tc.want = d, w
		}
		if got != tc.want {
			t.Errorf("%s: payload %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A refusal is the daemon's words, as an error.
	at = newScreenActionsTest(t, 250, func(registry.Request) (string, string) {
		return registry.RequestFailed, "PR-12360 is active, not waiting on you"
	})
	if _, err := at.act.Retry(ctx, 7); err == nil || err.Error() != "PR-12360 is active, not waiting on you" {
		t.Errorf("a refused retry: %v", err)
	}
	// Without a factory daemon the request waits for one, and the screen says so.
	at = newScreenActionsTest(t, 0, nil)
	res, err := at.act.Stop(ctx, 7)
	if err != nil || !strings.Contains(res.Text, "no factory daemon runs") || !strings.Contains(res.Text, "request 1") {
		t.Errorf("a stop without a daemon: %+v, %v", res, err)
	}
}

func fmtMap(m map[int]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, strconv.Itoa(k)+":"+m[k])
	}
	return "map[" + strings.Join(parts, " ") + "]"
}

// The preview is the daemon's new-task form of the text in the repository
// the screen names, decoded.
func TestTheScreensPreviewIsTheDaemonsForm(t *testing.T) {
	storetest.Serial(t)
	form := tui.NewTaskForm{Text: "PR-12399 words", Kind: "bug", Err: "PR-12399 is not in Jira", WordsOnly: true}
	b, err := json.Marshal(form)
	if err != nil {
		t.Fatal(err)
	}
	at := newScreenActionsTest(t, 250, func(registry.Request) (string, string) { return registry.RequestDone, string(b) })
	got, err := at.act.Preview(t.Context(), "PR-12399 words", "example/site")
	if err != nil || got.Text != form.Text || got.Err != form.Err || !got.WordsOnly || got.Kind != "bug" {
		t.Fatalf("preview %+v, %v", got, err)
	}
	if p := payloadOf[factory.PreviewPayload](t, at.seen[0]); at.seen[0].Kind != factory.ReqPreview || p.Text != "PR-12399 words" ||
		p.Repo != "example/site" {
		t.Errorf("request %s %+v", at.seen[0].Kind, p)
	}
}

// An answer the daemon refused because the questions changed reaches the
// screen typed, never as prose: the adapter reads the task, and its newer
// revision is the error's time, and the agent that asked it the first role
// that raised its first new question, a question being new by its text as
// the factory tells questions apart (factory.QuestionKey); a refusal for
// another reason is the daemon's words.
func TestAStaleAnswerReachesTheScreenAsTheQuestionsChanged(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, func(registry.Request) (string, string) {
		return registry.RequestFailed, "the questions changed at 14:20, nothing was sent"
	})
	reg := at.registry(t)
	ctx := t.Context()
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12345", Kind: "feature", Repo: "talkable/talkable", Title: "Credit expiry",
		State: registry.TaskNeedsYou, Stage: "questions", WaitsWho: "you", WaitsWhat: "1 question before coding"})
	if err != nil {
		t.Fatal(err)
	}
	q := registry.Question{Text: "When?", Recommended: -1, RaisedBy: "fx-judge"}
	if err := reg.ReplaceQuestions(ctx, task.ID, 1, []registry.Question{q}); err != nil {
		t.Fatal(err)
	}
	q2 := registry.Question{Text: "Which locale?", Recommended: -1, RaisedBy: "fx-cause, fx-points"} // two roles raised it: the first one says
	again := registry.Question{Text: "  when? ", Recommended: -1, RaisedBy: "fx-points"}             // revision 1's question, by another role
	if err := reg.ReplaceQuestions(ctx, task.ID, 2, []registry.Question{again, q2}); err != nil {
		t.Fatal(err)
	}
	qs, err := reg.Questions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer := qs[len(qs)-1].Asked

	_, err = at.act.Answer(ctx, task.ID, 1, map[int]string{0: "Seven days"}, nil)
	changed, ok := errors.AsType[*tui.QuestionsChangedError](err)
	if !ok || !errors.Is(err, tui.ErrQuestionsChanged) {
		t.Fatalf("error %v (%T), want the questions changed, typed", err, err)
	}
	if !changed.At.Equal(newer) || !strings.Contains(changed.Why, "finding the cause asked 1 new question.") {
		t.Errorf("changed at %v (%q), want the newer revision's time %v and the agent that asked", changed.At, changed.Why, newer)
	}

	// The same revision refused: the daemon's words, not "changed".
	_, err = at.act.Answer(ctx, task.ID, 2, map[int]string{0: "de"}, nil)
	if err == nil || errors.Is(err, tui.ErrQuestionsChanged) || err.Error() != "the questions changed at 14:20, nothing was sent" {
		t.Errorf("a refusal of the newest revision: %v", err)
	}
}

// Focus is a focus request with the screen's own pid and the event's time
// (the screen's, not the report's goroutine's), which records no input of
// yours; a blur keeps its time as the time you last saw home; with no
// factory registry yet nothing is queued.
func TestTheScreensFocusIsARequestWithItsPid(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	ctx := t.Context()
	focusAt, blurAt := screenActionsNow.Add(-2*time.Minute), screenActionsNow.Add(-time.Minute)
	if err := at.act.Focus(ctx, true, focusAt); err != nil {
		t.Fatal(err)
	}
	if err := at.act.Focus(ctx, false, blurAt); err != nil {
		t.Fatal(err)
	}
	if len(at.seen) != 2 {
		t.Fatalf("requests %+v, want two focus reports", at.seen)
	}
	for i, want := range []factory.FocusPayload{{Focused: true, PID: os.Getpid(), At: focusAt}, {PID: os.Getpid(), At: blurAt}} {
		p := payloadOf[factory.FocusPayload](t, at.seen[i])
		if at.seen[i].Kind != factory.ReqFocus || p.Focused != want.Focused || p.PID != want.PID || !p.At.Equal(want.At) {
			t.Errorf("report %d: %s %+v, want %+v", i, at.seen[i].Kind, p, want)
		}
	}
	if seen, err := factory.ReadSeen(at.ctx.Layout); err != nil || !seen.Equal(blurAt) {
		t.Errorf("seen %v (%v) after a blur, want its time %v", seen, err, blurAt)
	}

	bare := newDaemonGroupTest(t)
	src := &factoryScreenSource{c: bare.ctx, now: func() time.Time { return screenActionsNow }}
	defer src.close()
	if err := newFactoryActions(bare.ctx, src, nil).Focus(ctx, true, screenActionsNow); err != nil {
		t.Fatal(err)
	}
	if fileExists(bare.ctx.Layout.FactoryDB()) {
		t.Error("a focus report created the factory's registry")
	}
}

// Seen keeps the latest time you saw home, across screens and restarts:
// an earlier time changes nothing, and the next frame digests from it.
func TestSeenOnlyMovesForwardAcrossScreens(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	ctx := t.Context()
	later, earlier := screenActionsNow.Add(-time.Minute), screenActionsNow.Add(-time.Hour)
	if err := at.act.Seen(ctx, later); err != nil {
		t.Fatal(err)
	}
	other := newFactoryActions(at.ctx, &factoryScreenSource{c: at.ctx, now: time.Now}, nil)
	if err := other.Seen(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	if seen, err := factory.ReadSeen(at.ctx.Layout); err != nil || !seen.Equal(later) {
		t.Fatalf("seen %v (%v), want the later %v", seen, err, later)
	}
}

// The digest counts your absence from your last input: a screen left
// focused overnight, with no key since, digests in the morning; focus
// reports change nothing but a blur, which keeps its time.
func TestTheDigestCountsOnlyTheTimeYouWereAway(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	registrytest.Seed(t, dt.ctx.Layout.FactoryDB())
	daemonSys.FactoryKick = func(paths.Layout) (int, error) { return 0, nil } // no factory daemon: the reports wait queued
	night := screenActionsNow.Add(-14 * time.Hour)
	now := night
	src := &factoryScreenSource{c: dt.ctx, now: func() time.Time { return now }}
	defer src.close()
	act := newFactoryActions(dt.ctx, src, nil)
	ctx := t.Context()
	if err := act.Focus(ctx, true, night); err != nil { // at the screen, focused
		t.Fatal(err)
	}
	if err := act.Seen(ctx, night); err != nil { // your last key of the evening
		t.Fatal(err)
	}
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{Clock: func() time.Time { return night.Add(3 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12302", Kind: "chore", Repo: "talkable/talkable", Title: "Remove old sync job",
		State: registry.TaskActive, Stage: "publish"})
	if err == nil {
		err = reg.TransitionTask(ctx, task.ID, []string{registry.TaskActive}, registry.TaskDone, nil)
	}
	reg.Close()
	if err != nil {
		t.Fatal(err)
	}
	now = screenActionsNow // 14 hours on, the screen still focused, no key since
	d, err := src.Frame(ctx)
	if err != nil || d.Digest == nil || len(d.Digest.Done) != 1 || !d.Digest.Since.Equal(night) {
		t.Fatalf("frame %+v, %v: want the night's digest from the last key", d.Digest, err)
	}
	if err := act.Seen(ctx, now); err != nil { // the morning's first key
		t.Fatal(err)
	}
	if got := src.lastSeen(); !got.Equal(now) {
		t.Fatalf("last seen %v after a key, want its time", got)
	}
	blur := now.Add(time.Hour)
	if err := act.Focus(ctx, false, blur); err != nil {
		t.Fatal(err)
	}
	if got := src.lastSeen(); !got.Equal(blur) {
		t.Fatalf("last seen %v after a blur, want the blur's %v", got, blur)
	}
}

// Open steps into the task's pane: herdr focuses the agent that waits on
// you (else the working one) on the factory's session, and the terminal on
// that session comes to the front; it asks and records nothing.
func TestOpenStepsIntoTheTasksPaneOnTheFactorySession(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	cfg := *at.ctx.Config
	cfg.Factory.HerdrSession = "factory"
	at.ctx.Config = &cfg
	reg := at.registry(t)
	ctx := t.Context()
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12371", Kind: "bug", Repo: "talkable/talkable", Title: "Coupon CSV",
		State: registry.TaskActive, Stage: "investigate"})
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12372", Kind: "bug", Repo: "talkable/talkable", Title: "No agents"})
	if err != nil {
		t.Fatal(err)
	}
	storetest.Seed(t, at.ctx.Layout.FactoryAgentsDB())
	st, err := store.Open(at.ctx.Layout.FactoryAgentsDB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "factory:talkable", Owner: "factory", Name: "talkable", WatchOwner: "factory", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	title := "PR-12371"
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "factory:task:" + strconv.FormatInt(task.ID, 10),
		Number: int(task.ID), URL: "file:///x", HeadSHA: "t1", Title: &title, GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: "factory"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct{ role, status, name string }{{"fx-repro", "working", "factory-repro"}, {"fx-cause", "blocked", "factory-cause"}} {
		status, name := s.status, s.name
		if _, err := st.CreateSession(ctx, store.Session{PRID: up.PR.ID, Role: s.role, State: store.SessionLive, AgentStatus: &status,
			AgentName: &name}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	var focused []string
	var revealed int
	at.act = newFactoryActions(at.ctx, at.src, nil)
	at.act.focusPane = func(_ context.Context, target string) error {
		focused = append(focused, target)
		return nil
	}
	at.act.revealSession = func(context.Context) (string, error) {
		revealed++
		return "brought the terminal to the front", nil
	}
	res, err := at.act.Open(ctx, task.ID)
	if err != nil || !slices.Equal(focused, []string{"factory-cause"}) || revealed != 1 || !strings.Contains(res.Text, "finding the cause") {
		t.Fatalf("open: %+v, %v; focused %q, revealed %d", res, err, focused, revealed)
	}
	if _, err := at.act.Open(ctx, quiet.ID); err == nil || !strings.Contains(err.Error(), "no pane") {
		t.Errorf("open of a task with no agent: %v", err)
	}
	if len(at.seen) != 0 {
		t.Errorf("open sent requests %+v", at.seen)
	}
	if ins, err := reg.Inputs(ctx, task.ID); err != nil || len(ins) != 0 {
		t.Errorf("open recorded inputs %+v (%v)", ins, err)
	}
}

// Diff is the pager on the ready card's diff in the task's copy, its files
// in the card's order; it only builds the command, and a task with no
// ready card has none.
func TestDiffPagesTheReadyCardsDiffInTheTasksCopy(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	reg := at.registry(t)
	ctx := t.Context()
	ready, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12360", Kind: "bug", Repo: "talkable/talkable", Title: "Points",
		State: registry.TaskNeedsYou, Stage: "ready", WaitsWho: "you", WaitsWhat: "ready to push (card 1): PR-12360 Points"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12361", Kind: "bug", Repo: "talkable/talkable", Title: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	card := factory.ReadyCardFile{Revision: 1, Head: "ffff1111", Base: "base2222", CopyPath: "/copies/talkable.factory3", Copy: "talkable.factory3",
		Files: []factory.CardFile{{Path: "Gemfile.lock", InPlan: true, Flagged: true}, {Path: "app/a.rb", InPlan: true}}}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	dir := at.ctx.Layout.FactoryTaskDir(ready.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready-card.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if at.act.Diff(ready.ID) != nil {
		t.Error("a diff before the screen read the card")
	}
	d, err := at.src.Frame(ctx)
	if err != nil || d.Cards[ready.ID].Ready == nil {
		t.Fatalf("frame %+v, %v: want the ready card", d.Cards[ready.ID], err)
	}
	// A card with no order file (drawn by an older build, or its file gone)
	// pages in git's order: git refuses an order file it cannot read.
	t.Setenv("LESS", "FRX")
	cmd := at.act.Diff(ready.ID)
	if cmd == nil {
		t.Fatal("no diff for the ready card")
	}
	if want := append([]string{"git"}, factory.DiffArgs(card, "")...); !slices.Equal(cmd.Args, want) || filepath.Base(cmd.Path) != "git" {
		t.Errorf("diff %q, want %q", cmd.Args, want)
	}
	// Whatever your LESS says, the pager does not quit at once on a short
	// diff (which the card would count as read).
	if !slices.Contains(cmd.Env, "LESS=FRX -+F") {
		t.Errorf("the pager's LESS %q, want yours with -+F", slices.DeleteFunc(slices.Clone(cmd.Env), func(e string) bool { return !strings.HasPrefix(e, "LESS=") }))
	}
	order := factory.DiffOrderPath(at.ctx.Layout, ready.ID)
	if err := os.WriteFile(order, []byte("Gemfile.lock\napp/a.rb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cmd := at.act.Diff(ready.ID); cmd == nil || !slices.Equal(cmd.Args, append([]string{"git"}, factory.DiffArgs(card, order)...)) {
		t.Errorf("diff with the order file %v", cmd)
	}
	if at.act.Diff(other.ID) != nil {
		t.Error("a diff for a task with no ready card")
	}

	// [factory] diff_pager pages it with your viewer (GIT_PAGER, which git
	// prefers over core.pager), keeping the card's order; with none, git's
	// own pager.
	pager := func(cmd *exec.Cmd) string {
		got := ""
		for _, e := range cmd.Env {
			if v, ok := strings.CutPrefix(e, "GIT_PAGER="); ok {
				got = v // the last one wins
			}
		}
		return got
	}
	if got := pager(at.act.Diff(ready.ID)); got == "hunk pager" {
		t.Errorf("GIT_PAGER %q with no diff_pager", got)
	}
	at.ctx.Config.Factory.DiffPager = "hunk pager"
	cmd = at.act.Diff(ready.ID)
	if got := pager(cmd); got != "hunk pager" || !slices.Equal(cmd.Args, append([]string{"git"}, factory.DiffArgs(card, order)...)) {
		t.Errorf("diff with diff_pager: GIT_PAGER %q, args %q; want hunk pager over the same git diff", got, cmd.Args)
	}
}

// An agents store the screen cannot read is said on home, with why, and the
// rest of home is drawn: never an empty AGENTS with no word.
func TestTheScreenSaysWhyItShowsNoAgents(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	reg := at.registry(t)
	if _, err := reg.CreateTask(t.Context(), registry.Task{JiraKey: "PR-12371", Kind: "bug", Repo: "talkable/talkable", Title: "Coupon CSV",
		State: registry.TaskActive, Stage: "investigate"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(at.ctx.Layout.FactoryAgentsDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at.ctx.Layout.FactoryAgentsDB(), []byte("not a database, not at all, not even close"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := at.src.Frame(t.Context())
	if err != nil || len(d.Rows) != 1 {
		t.Fatalf("frame %+v, %v: want the task's row", d.Rows, err)
	}
	if !slices.Contains(d.Blocking, "agents not read") || !slices.ContainsFunc(d.BlockingLong, func(l string) bool {
		return strings.HasPrefix(l, "agents not read (") && len(l) > len("agents not read ()")
	}) {
		t.Errorf("blocking %q / %q, want the agents store's failure named", d.Blocking, d.BlockingLong)
	}
}

// OpenBrowser, the screen's b and t, opens the page in the browser with
// the board's opener (`open <url>`), sends the factory no request and
// records no input.
func TestOpenBrowserOpensThePageAsTheBoardDoes(t *testing.T) {
	storetest.Serial(t)
	at := newScreenActionsTest(t, 250, nil)
	const page = "https://github.com/talkable/talkable/pull/4527"
	at.fake.Rules = append(at.fake.Rules, daemonRuleOK([]string{"open", page}, ""))
	if err := at.act.OpenBrowser(t.Context(), page); err != nil {
		t.Fatal(err)
	}
	if opened := at.fake.CallsWithPrefix("open"); len(opened) != 1 || !slices.Equal(opened[0].Args, []string{page}) || !opened[0].Mutates {
		t.Errorf("browser calls %+v, want one open of %s", opened, page)
	}
	if len(at.seen) != 0 || at.kicks != 0 {
		t.Errorf("OpenBrowser sent requests %+v and %d kicks", at.seen, at.kicks)
	}
}
