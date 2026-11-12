package factory

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// shortCap is a loop of one round at the cap and no extra round, so each
// round of a cap test is a stop at the cap.
func (r *reviewRig) shortCap(goOns int) {
	r.cfg.Factory.LocalReview.Cycles, r.cfg.Factory.LocalReview.ExtraCycles, r.cfg.Factory.LocalReview.CapGoOn = 1, 0, goOns
}

// fixPrompts are the writer's prompts so far.
func (r *reviewRig) fixPrompts() int { return len(r.rt.promptsTo("fx-writer")) }

// toFixPrompt ticks until the writer got its n-th prompt, and fails unless
// it did and names the review file of the task's newest cycle.
func (r *reviewRig) toFixPrompt(task registry.Task, n int) registry.Cycle {
	r.t.Helper()
	r.run(12, func() bool { return r.fixPrompts() >= n })
	cs := r.cycles(task.ID)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != n || !strings.Contains(ps[n-1].Text, reviewFile(cs[len(cs)-1].N)) {
		got := r.reload(task.ID)
		r.t.Fatalf("the writer got %d prompts, want its fix turn %d naming %s (task %s waits %q)", len(ps), n, reviewFile(cs[len(cs)-1].N),
			got.State, got.WaitsWhat)
	}
	return cs[len(cs)-1]
}

// goOnsOf are the task's go-ons at the cap as review-go-on.json keeps them.
func (r *reviewRig) goOnsOf(id int64) []capGoOn {
	r.t.Helper()
	gs, err := readGoOns(r.layout.FactoryTaskDir(id))
	if err != nil {
		r.t.Fatal(err)
	}
	return gs
}

// A stop at the cap that is safe (no P0 or P1, a verdict that does not
// block, nothing the writer declined in the loop) goes on by itself, as your
// retry would: the writer gets the cap round's findings, and a fresh loop
// reviews its head. Task 9 (PR-27708) stopped so three times on 2026-11-12,
// each round all P2 and non_blocking, and waited hours for your retry each
// time.
func TestASafeStopAtTheCapGoesOnByItself(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 5)...), result(capFindings(6, 8)...), result())
	task, _ := r.reviewTask("PR-12501")
	for k := 1; k <= 2; k++ {
		r.toFixPrompt(task, k)
		r.writerFixes(task, k, fixedCredit(k), nil)
	}
	cy := r.toFixPrompt(task, 3) // round 3 at the cap: 3 P2s after 3, so no extra round
	if got := r.reload(task.ID); !isLive(got) || got.WaitsWho == waitsYou {
		t.Fatalf("task %s waits %s %q, want it at work", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if cs := r.cycles(task.ID); len(cs) != 3 || cs[2].Exit != registry.ExitCap || cy.ID != cs[2].ID {
		t.Fatalf("cycles %+v, want 3, the last ended at the cap", cs)
	}
	const alert = "PR-12501: went on by itself at the cap (1 of 2): 3 P2 findings to the writer"
	if got := r.eventTexts(task.ID, "alert"); !slices.Equal(got, []string{alert}) {
		t.Fatalf("alerts %q, want %q", got, alert)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "review"), func(s string) bool { return strings.Contains(s, "went on by itself at the cap (1 of 2)") }) {
		t.Errorf("review events %q, want the go-on said", r.eventTexts(task.ID, "review"))
	}
	if gs := r.goOnsOf(task.ID); len(gs) != 1 || gs[0].Cycle != cy.ID || gs[0].N != 1 || gs[0].Pass != nil {
		t.Errorf("go-ons %+v, want the one of cycle %d", gs, cy.ID)
	}
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(d.Rows, func(row tui.FactoryRow) bool { return row.ID == task.ID }); i < 0 || d.Rows[i].Group == tui.GroupNeedsYou {
		t.Errorf("rows %+v, want the task at work, not in NEEDS YOU", d.Rows)
	}

	r.writerFixes(task, cy.N, fixedCredit(3), nil)
	fixed := r.git(r.copyDir, "rev-parse", "HEAD")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if len(calls) != 4 || calls[3].Round != 1 || calls[3].HeadSHA != fixed {
		t.Fatalf("rounds %+v, want a fresh loop's round 1 at the writer's head %s", calls, fixed)
	}
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q, want past the review", got.State, got.Stage, got.WaitsWhat)
	}
	if f := r.findings(task.ID)[Fingerprint(capFinding(6, "P2"))]; f.Outcome != registry.FindingFixed || f.FixCommit != fixed {
		t.Errorf("the cap round's finding %+v, want it fixed by %s", f, fixed)
	}
}

// Once a task went on by itself cap_go_on times, its next safe stop does
// not wait for you either: the writer fixes the cap round's findings, the
// factory's checks run on its fix, and the stage passes with no round more.
// The ready card's summary and the PR body name the findings fixed after
// the last round, which no round reviewed.
func TestAfterItsGoOnsASafeStopHasTheWriterFixThenPasses(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...))
	r.shortCap(1)
	task, _ := r.reviewTask("PR-12502")
	first := r.toFixPrompt(task, 1)
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	last := r.toFixPrompt(task, 2) // the fresh loop's round 1, at the cap again: the one go-on is spent
	if got := r.reload(task.ID); !isLive(got) {
		t.Fatalf("task %s waits %q, want it at work", got.State, got.WaitsWhat)
	}
	wantAlerts := []string{"PR-12502: went on by itself at the cap (1 of 1): 2 P2 findings to the writer",
		"PR-12502: passes the cap after its fix (1 go-on spent): 2 P2 findings to the writer, not reviewed again"}
	if got := r.eventTexts(task.ID, "alert"); !slices.Equal(got, wantAlerts) {
		t.Fatalf("alerts %q, want %q", got, wantAlerts)
	}
	ps := r.rt.promptsTo("fx-writer")
	if !strings.Contains(ps[0].Text, "the next round reviews it") || strings.Contains(ps[0].Text, "No review round follows this fix") {
		t.Errorf("the go-on's fix turn should say the next round reviews it:\n%s", ps[0].Text)
	}
	if strings.Contains(ps[1].Text, "the next round reviews it") || !strings.Contains(ps[1].Text, "No review round follows this fix") {
		t.Errorf("the fix turn that passes the stage should say no review round follows it:\n%s", ps[1].Text)
	}
	r.writerFixes(task, last.N, fixedCredit(2), nil)
	fixed := r.git(r.copyDir, "rev-parse", "HEAD")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || len(r.rv.calls()) != 2 || got.HeadSHA != fixed {
		t.Fatalf("task %s at %s (head %s) after %d rounds, want past the review at the fix %s with no round more",
			got.State, got.Stage, short(got.HeadSHA), len(r.rv.calls()), short(fixed))
	}
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != exitCapFixed || sum.Head != fixed || sum.Rounds != 1 || len(sum.FixedAfter) != 2 ||
		sum.FixedAfter[0].Title != capFinding(3, "P2").Title || sum.FixedAfter[1].Path != "app/merger.rb" {
		t.Fatalf("summary %+v, want exit cap_fixed at the fix with the two findings fixed after the last round", sum)
	}
	if f := r.findings(task.ID)[Fingerprint(capFinding(4, "P2"))]; f.Outcome != registry.FindingFixed || f.FixCommit != fixed {
		t.Errorf("the last round's finding %+v, want it fixed by %s", f, fixed)
	}

	r.taskFile(task.ID, reportFile, "## Cause\nthe merger maps the winners\n\n## Plan\n- map each winner\n")
	r.taskFile(task.ID, prBodyFile, "## Summary\n\nThe change.\n")
	if err := r.e.appendKnown(r.ctx, got); err != nil {
		t.Fatal(err)
	}
	body := r.readTask(task.ID, prBodyFile)
	for _, want := range []string{"Fixed after the local review's last round, not reviewed again",
		"- P2 `app/merger.rb:160`: " + capFinding(3, "P2").Title, "- P2 `app/merger.rb:180`: " + capFinding(4, "P2").Title} {
		if !strings.Contains(body, want) {
			t.Errorf("pr-body.md lacks %q:\n%s", want, body)
		}
	}
	if _, why := r.e.readyProblem(got); why != "" {
		t.Errorf("the ready card is refused: %s", why)
	}
}

// The writer's fix turn that passes the stage (LastFix) never says that the
// next round reviews its commit: no round does. It says so, asks the writer
// to run the specs of every file it changes and to check each fix itself,
// and says that the PR's review checks the result after publish; a decline
// without a risk is left for the ready card. Every other fix turn reads as
// before.
func TestTheLastFixTurnSaysNoRoundReviewsIt(t *testing.T) {
	d := fullPromptData()
	text := renderFactory(t, config.Defaults(), ReviewFixPrompt, d)
	for _, want := range []string{"the next round reviews it", "the next round of the review finds it again"} {
		if !strings.Contains(text, want) {
			t.Errorf("a fix turn before the cap lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "no review round follows") {
		t.Errorf("a fix turn before the cap says no round follows:\n%s", text)
	}
	d.LastFix = true
	text = renderFactory(t, config.Defaults(), ReviewFixPrompt, d)
	for _, want := range []string{"No review round follows this fix", "run the specs of every file you change", "check each fix yourself",
		"the PR's review checks the result after it is published", "the ready card lists it as left open"} {
		if !strings.Contains(text, want) {
			t.Errorf("the last fix turn lacks %q:\n%s", want, text)
		}
	}
	for _, never := range []string{"the next round reviews it", "the next round of the review"} {
		if strings.Contains(text, never) {
			t.Errorf("the last fix turn says %q:\n%s", never, text)
		}
	}
}

// A stop at the cap that is not safe still stops for you: a P1 among the
// cap round's findings, a judge's verdict that blocks, or a finding the
// writer declined in the loop. Nothing went on by itself.
func TestAnUnsafeStopAtTheCapStillStopsForYou(t *testing.T) {
	for _, tc := range []struct {
		name     string
		second   func(evalrun.LocalCase) evalrun.LocalResult
		declined map[string]string
	}{
		{"a P1", result(capFindings(3, 4, 4)...), nil},
		{"a blocking verdict", withVerdict(store.VerdictBlocking, result(capFindings(3, 4)...)), nil},
		{"a declined finding", result(capFindings(3, 4)...), map[string]string{"F1": "a second query on every page load"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, result(capFindings(1, 2)...), tc.second, result())
			r.cfg.Factory.LocalReview.Cycles = 2
			task, _ := r.reviewTask("PR-12503")
			r.toFixPrompt(task, 1)
			r.writerFixes(task, 1, fixedCredit(1), tc.declined)
			r.run(12, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			if cs := r.cycles(task.ID); got.State != registry.TaskNeedsYou || len(cs) != 2 || cs[1].Exit != registry.ExitCap {
				t.Fatalf("task %s waits %q; cycles %+v, want it stopped for you at the cap", got.State, got.WaitsWhat, cs)
			}
			if gs := r.goOnsOf(task.ID); len(gs) != 0 || r.fixPrompts() != 1 {
				t.Errorf("go-ons %+v and %d writer prompts, want none past the first round's", gs, r.fixPrompts())
			}
			if slices.ContainsFunc(r.eventTexts(task.ID, "alert"), func(s string) bool { return strings.Contains(s, "by itself") }) {
				t.Errorf("alerts %q, want none of a go-on", r.eventTexts(task.ID, "alert"))
			}
		})
	}
}

// cap_go_on = 0 turns the go-on off: a safe stop at the cap waits for you,
// as before.
func TestCapGoOnZeroStopsForYouAsBefore(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result())
	r.shortCap(0)
	task, _ := r.reviewTask("PR-12504")
	r.toCap(task, 1)
	if gs := r.goOnsOf(task.ID); len(gs) != 0 || r.fixPrompts() != 0 {
		t.Errorf("go-ons %+v and %d writer prompts, want none", gs, r.fixPrompts())
	}
}

// A daemon that stops between a safe stop's end of its cycle and the
// go-on goes on once at its next start: the writer gets the cap round's
// findings once, the alert is said once, and the count is kept, so the next
// safe stop is the second of two.
func TestARestartBetweenASafeStopAndItsGoOnGoesOnOnce(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result(capFindings(3, 4)...), result())
	r.shortCap(2)
	task, _ := r.reviewTask("PR-12505")
	r.e.beforeStop = func() error { return daemonStops }
	r.run(8, func() bool { cs := r.cycles(task.ID); return len(cs) == 1 && !cs[0].Ended.IsZero() })
	if cs := r.cycles(task.ID); len(cs) != 1 || cs[0].Exit != registry.ExitCap || r.fixPrompts() != 0 {
		t.Fatalf("cycles %+v, %d writer prompts: the daemon did not stop between the cap's end and the go-on", cs, r.fixPrompts())
	}
	r.restart()
	first := r.toFixPrompt(task, 1)
	r.run(3, nil)
	if n := r.fixPrompts(); n != 1 {
		t.Fatalf("the writer got %d prompts after the restart, want its one fix turn", n)
	}
	const one = "PR-12505: went on by itself at the cap (1 of 2): 2 P2 findings to the writer"
	if got := r.eventTexts(task.ID, "alert"); !slices.Equal(got, []string{one}) {
		t.Fatalf("alerts %q, want %q once", got, one)
	}
	// A daemon that stops before the cycle ends decides the round again: its
	// go-on is the one recorded, never a second.
	if g, err := r.e.recordGoOn(r.layout.FactoryTaskDir(task.ID), first, nil, ReviewSummary{}); err != nil || g.N != 1 || len(r.goOnsOf(task.ID)) != 1 {
		t.Fatalf("cycle %d's go-on recorded again: %+v (%v), %d go-ons; want the one recorded", first.N, g, err, len(r.goOnsOf(task.ID)))
	}
	r.writerFixes(task, first.N, fixedCredit(1), nil)
	r.toFixPrompt(task, 2)
	if gs := r.goOnsOf(task.ID); len(gs) != 2 || gs[0].N != 1 || gs[1].N != 2 {
		t.Fatalf("go-ons %+v, want the first and the second", gs)
	}
	const two = "PR-12505: went on by itself at the cap (2 of 2): 2 P2 findings to the writer"
	if got := r.eventTexts(task.ID, "alert"); !slices.Equal(got, []string{one, two}) {
		t.Fatalf("alerts %q, want %q then %q", got, one, two)
	}
}

// A go-on is said once: a pass of the review long after it (the task back
// from QA or the ready card, hundreds of events later, its cap cycle still
// the newest) says nothing again.
func TestAGoOnIsSaidOnceHoweverLongAfter(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 2)...), result())
	r.shortCap(2)
	task, _ := r.reviewTask("PR-12508")
	cy := r.toFixPrompt(task, 1)
	for i := range 250 {
		r.e.event(r.ctx, task.ID, "stage", registry.PhaseInfo, fmt.Sprintf("a later step %d", i))
	}
	if told, err := r.e.toldOfStop(r.ctx, r.reload(task.ID), cy); err != nil || !told {
		t.Fatalf("told %v (%v), want the go-on to go on", told, err)
	}
	if got := r.eventTexts(task.ID, "alert"); len(got) != 1 {
		t.Fatalf("alerts %q, want the go-on's once", got)
	}
}

// Your retry at a stop that was not safe goes on as before and spends
// none of the go-ons: the next safe stop is the first of two.
func TestYourRetryAtAnUnsafeStopSpendsNoGoOn(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 1, 1)...), result(capFindings(2, 3)...), result())
	r.shortCap(2)
	task, _ := r.reviewTask("PR-12506")
	r.toCap(task, 1)
	r.letGoOn(task.ID)
	stopped := r.toFixPrompt(task, 1)
	r.writerFixes(task, stopped.N, fixedCredit(1), nil)
	r.toFixPrompt(task, 2)
	const alert = "PR-12506: went on by itself at the cap (1 of 2): 2 P2 findings to the writer"
	if got := r.eventTexts(task.ID, "alert"); !slices.Contains(got, alert) {
		t.Fatalf("alerts %q, want %q", got, alert)
	}
	if gs := r.goOnsOf(task.ID); len(gs) != 1 || gs[0].N != 1 {
		t.Fatalf("go-ons %+v, want only the one after your retry", gs)
	}
}

// The ready card of a task whose local review passed at the cap after the
// writer's fix names the findings fixed after the last round, each its
// priority, place and title, and its recommendation says no round reviewed
// them.
func TestTheReadyCardNamesTheFindingsFixedAfterTheLastRound(t *testing.T) {
	r := newScreenRig(t)
	ready := r.task("PR-27708", "feature", "A/B winners", registry.TaskNeedsYou, "ready", waitsYou, "ready to push (card 1): PR-27708 A/B winners")
	r.card(ready.ID, ReadyCardFile{Revision: 1, Head: "ffff1111", Base: "base2222", ReportHash: "rh", DiffHash: "dh",
		Branch: "zhuravel-PR-27708-winners", Copy: "talkable.factory3", CopyPath: "/copies/talkable.factory3",
		Review: ReviewSummary{Exit: exitCapFixed, Rounds: 3, Head: "ffff1111",
			Leftovers: []ReviewItem{{Severity: "P3", Path: "app/a.rb", Line: 4, Title: "a stale comment"}},
			FixedAfter: []ReviewItem{{Severity: "P2", Path: "app/models/merger.rb", Line: 10, Title: "The merger maps winner 1 wrong"},
				{Severity: "P2", Path: "app/models/merger.rb", Line: 20, Title: "The merger maps winner 2 wrong"}}}})
	c := r.data(screenNow).Cards[ready.ID].Ready
	if c == nil {
		t.Fatal("no ready card")
	}
	want := []string{"P2 app/models/merger.rb:10: The merger maps winner 1 wrong", "P2 app/models/merger.rb:20: The merger maps winner 2 wrong"}
	if !slices.Equal(c.FixedAfter, want) || len(c.Leftovers) != 1 {
		t.Errorf("fixed after %q, KNOWN %q; want %q and the one leftover", c.FixedAfter, c.Leftovers, want)
	}
	if c.Recommended != 0 || !strings.Contains(c.Why, "no round reviewed") {
		t.Errorf("recommendation %d (%q), want push, saying no round reviewed the fix", c.Recommended, c.Why)
	}
}

// The PR body's Known list names the findings fixed after the local
// review's last round under their own heading, apart from what was left
// open; a summary without them reads as before.
func TestThePRBodyNamesTheFindingsFixedAfterTheLastRound(t *testing.T) {
	sum := ReviewSummary{Exit: exitCapFixed, Rounds: 3,
		FixedAfter: []ReviewItem{{Severity: "P2", Path: "app/models/merger.rb", Line: 10, Title: "The merger maps winner 1 wrong"}}}
	got := knownList(sum)
	for _, want := range []string{"<summary>nothing the local review left open, 1 finding fixed after its last round</summary>",
		"**Fixed after the local review's last round, not reviewed again**", "- P2 `app/models/merger.rb:10`: The merger maps winner 1 wrong"} {
		if !strings.Contains(got, want) {
			t.Errorf("the Known list lacks %q:\n%s", want, got)
		}
	}
	sum.Leftovers = []ReviewItem{{Severity: "P3", Path: "app/a.rb", Title: "a stale comment"}}
	if got := knownList(sum); !strings.Contains(got, "<summary>1 note the local review left open, 1 finding fixed after its last round</summary>") {
		t.Errorf("the Known list with a leftover:\n%s", got)
	}
	sum.FixedAfter = nil
	if got := knownList(sum); strings.Contains(got, "Fixed after") || !strings.Contains(got, "<summary>1 note the local review left open</summary>") {
		t.Errorf("the Known list without findings fixed after:\n%s", got)
	}
}

// A go-on's alert reaches you as a toast, though nothing waits on you: it
// counts nobody among those who need you, and nothing drops it as "no
// longer waits on you" while its task is at work.
func TestAGoOnsAlertIsAToastThatCountsNobodyAsWaitingOnYou(t *testing.T) {
	r := newAlertRig(t)
	task := r.task("PR-12507", registry.TaskActive, "local_review")
	r.e.alert(r.ctx, task.ID, "PR-12507: "+fmt.Sprintf(goOnLine, 1, 2)+": 3 P2 findings to the writer")
	r.tick()
	r.after(2 * time.Minute)
	r.wantToasts("after the 2 minutes", shownToast{"factory: nothing needs you", "PR-12507 went on by itself at the cap"})
	if got := r.texts(task.ID, alertDropped); len(got) != 0 {
		t.Errorf("drops %q, want none", got)
	}
}
