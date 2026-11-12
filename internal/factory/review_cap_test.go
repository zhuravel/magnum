package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// capFinding is finding i of a cap test at severity sev: its own title and
// line, so no two of them share a fingerprint (none comes back as a doubt).
func capFinding(i int, sev string) evalrun.LocalFinding {
	return evalrun.LocalFinding{Severity: sev, Title: fmt.Sprintf("The merger maps winner %d to the wrong arm", i), Path: "app/merger.rb",
		Line: 100 + 20*i, Body: "The mapping misses this case."}
}

// capFindings are findings from..to (inclusive) of a cap test, at P2 but
// the ones p1 names.
func capFindings(from, to int, p1 ...int) []evalrun.LocalFinding {
	var out []evalrun.LocalFinding
	for i := from; i <= to; i++ {
		sev := "P2"
		if slices.Contains(p1, i) {
			sev = "P1"
		}
		out = append(out, capFinding(i, sev))
	}
	return out
}

// withVerdict is answer with its judge's verdict set to v.
func withVerdict(v string, answer func(evalrun.LocalCase) evalrun.LocalResult) func(evalrun.LocalCase) evalrun.LocalResult {
	return func(c evalrun.LocalCase) evalrun.LocalResult {
		r := answer(c)
		r.Verdict = v
		return r
	}
}

// toCap runs the loop of task through its writer's fixes of rounds 1 to
// n-1 and on until the task leaves the live states (round n stops it).
func (r *reviewRig) toCap(task registry.Task, n int) {
	r.t.Helper()
	for k := 1; k < n; k++ {
		r.run(8, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(k)) && len(r.rt.promptsTo("fx-writer")) >= k
		})
		r.writerFixes(task, k, fixedCredit(k), nil)
	}
	r.run(12, func() bool { return !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		r.t.Fatalf("task %s waits %q after %d rounds, want it at the cap", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
}

// The cap round's blocking findings are written to review-<n>.json, as a
// fixing round's are, and your "go on" at the head the cap round reviewed
// hands them to the writer first: no round runs before the writer's fix,
// and the fresh loop's first round reviews the writer's new head. Task 9's
// "go on" spent a 50-minute round reviewing a head whose 5 proven findings
// the factory already held.
func TestARetryAtTheCapsHeadHandsItsFindingsToTheWriterFirst(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 3, 1)...), result(capFindings(4, 5, 4)...), result(capFindings(6, 6, 6)...), result())
	task, _ := r.reviewTask("PR-12394")
	r.toCap(task, 3)
	cs := r.cycles(task.ID)
	if len(cs) != 3 || cs[2].Exit != registry.ExitCap {
		t.Fatalf("cycles %+v, want 3 with the last at the cap", cs)
	}
	var rf ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(cs[2].N))), &rf); err != nil {
		t.Fatalf("the cap round's %s: %v", reviewFile(cs[2].N), err)
	}
	if len(rf.Findings) != 1 || rf.Findings[0].Title != capFinding(6, "P1").Title || rf.Head != cs[2].HeadBefore {
		t.Fatalf("%s = %+v, want the cap round's finding at its head", reviewFile(cs[2].N), rf)
	}
	prompts := len(r.rt.promptsTo("fx-writer"))

	r.letGoOn(task.ID)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > prompts })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != prompts+1 || !strings.Contains(ps[len(ps)-1].Text, reviewFile(cs[2].N)) {
		t.Fatalf("after your go on the writer got %d prompts (%d before), want its fix turn naming %s", len(ps), prompts, reviewFile(cs[2].N))
	}
	if n := len(r.rv.calls()); n != 3 {
		t.Fatalf("%d rounds before the writer's fix, want the 3 of the stopped loop", n)
	}
	r.writerFixes(task, cs[2].N, fixedCredit(3), nil)
	fixed := r.git(r.copyDir, "rev-parse", "HEAD")
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	calls := r.rv.calls()
	if len(calls) != 4 || calls[3].Round != 1 || calls[3].HeadSHA != fixed || calls[3].HeadSHA == cs[2].HeadBefore {
		t.Fatalf("rounds %+v, want a fresh loop's round 1 at the writer's head %s", calls, fixed)
	}
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q, want past the review", got.State, got.Stage, got.WaitsWhat)
	}
	if f := r.findings(task.ID)[Fingerprint(capFinding(6, "P1"))]; f.Outcome != registry.FindingFixed || f.FixCommit != fixed {
		t.Fatalf("the cap round's finding %+v, want it fixed by %s", f, fixed)
	}
}

// A stop at the cap that an older build wrote has no review-<n>.json and a
// review-stop.json of its line alone (task 9's): your "go on" at its head
// reads the cap round's result again and hands its findings to the writer
// all the same, before any round.
func TestARetryOfAnOlderBuildsCapStopHandsTheRoundsFindingsToTheWriter(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 3, 1)...), result(capFindings(4, 5, 4)...), result(capFindings(6, 7, 6)...), result())
	task, _ := r.reviewTask("PR-12400")
	r.toCap(task, 3)
	cs := r.cycles(task.ID)
	dir := r.layout.FactoryTaskDir(task.ID)
	if err := os.Remove(filepath.Join(dir, reviewFile(cs[2].N))); err != nil {
		t.Fatal(err)
	}
	r.taskFile(task.ID, reviewStopFile, fmt.Sprintf(`{"cycle": %d, "why": "2 blocking findings after 3 review rounds"}`, cs[2].ID))
	prompts := len(r.rt.promptsTo("fx-writer"))
	r.letGoOn(task.ID)
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > prompts })
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != prompts+1 || !strings.Contains(ps[len(ps)-1].Text, reviewFile(cs[2].N)) {
		t.Fatalf("after your go on the writer got %d prompts (%d before), want its fix turn naming %s", len(ps), prompts, reviewFile(cs[2].N))
	}
	var rf ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewFile(cs[2].N))), &rf); err != nil || len(rf.Findings) != 2 ||
		rf.Findings[0].Title != capFinding(6, "P1").Title || rf.Head != cs[2].HeadBefore {
		t.Fatalf("%s = %+v (%v), want the cap round's two findings", reviewFile(cs[2].N), rf, err)
	}
	if n := len(r.rv.calls()); n != 3 {
		t.Fatalf("%d rounds before the writer's fix, want 3", n)
	}
}

// A retry after a new head (a send back's, the base merged, your commit)
// starts the fresh loop at once, as before: the cap round reviewed another
// head, so the writer gets nothing of it.
func TestARetryAfterANewHeadStartsAFreshLoopAtOnce(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 3, 1)...), result(capFindings(4, 5, 4)...), result(capFindings(6, 6, 6)...), result())
	task, _ := r.reviewTask("PR-12395")
	r.toCap(task, 3)
	prompts := len(r.rt.promptsTo("fx-writer"))
	r.clock.Add(time.Minute)
	head := r.commit(task.JiraKey+" your fix", fixedCredit(9))
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("head_sha", head) }); err != nil {
		t.Fatal(err)
	}
	r.letGoOn(task.ID)
	r.run(10, func() bool { return len(r.rv.calls()) == 4 })
	calls := r.rv.calls()
	if len(calls) != 4 || calls[3].Round != 1 || calls[3].HeadSHA != head {
		t.Fatalf("rounds %+v, want a fresh loop's round 1 at the new head %s", calls, head)
	}
	if n := len(r.rt.promptsTo("fx-writer")); n != prompts {
		t.Fatalf("the writer got %d prompts after your go on, want none", n-prompts)
	}
}

// A stop at the cap tells you the cap round's blocking findings (priority,
// title, path), the trend of the blocking count and of the P1 count over
// the loop's rounds and the judge's verdict: the NEEDS YOU line names them,
// and review-stop.json keeps them for the card.
func TestACapStopNamesItsFindingsTheTrendAndTheVerdict(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 3, 1, 2)...), result(capFindings(4, 5, 4)...), result(capFindings(6, 6, 6)...))
	task, _ := r.reviewTask("PR-12396")
	r.toCap(task, 3)
	got := r.reload(task.ID)
	head := "1 blocking finding after 3 review rounds (blocking 3 → 2 → 1, P1 2 → 1 → 1; verdict blocking)"
	if want := head + ": P1 " + capFinding(6, "P1").Title + " (app/merger.rb)"; got.WaitsWhat != want {
		t.Fatalf("the NEEDS YOU line %q, want %q", got.WaitsWhat, want)
	}
	var s reviewStop
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewStopFile)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Why != got.WaitsWhat || s.Head != head || len(s.Findings) != 1 || s.Findings[0].Severity != "P1" || s.Findings[0].Path != "app/merger.rb" {
		t.Fatalf("%s = %+v, want the line, its head and the finding", reviewStopFile, s)
	}
}

// The task card of a task stopped at the cap lists the cap round's blocking
// findings under WAITING FOR, each with its priority, title and path, and
// its WAITING FOR line keeps the count, the trend and the verdict; a task
// that waits on you for another cause since gets none of them.
func TestTheCardOfACapStopListsItsFindings(t *testing.T) {
	r := newScreenRig(t)
	head := "6 blocking findings after 3 review rounds (blocking 11 → 8 → 6, P1 5 → 3 → 0; verdict non_blocking)"
	var items []ReviewItem
	for i := 1; i <= 6; i++ {
		items = append(items, ReviewItem{ID: fmt.Sprintf("F%d", i), Severity: "P2", Title: fmt.Sprintf("The merger maps winner %d wrong", i),
			Path: "app/models/merger.rb", Line: 10 * i})
	}
	why := head + ": P2 The merger maps winner 1 wrong (app/models/merger.rb); and 5 more"
	stopped := r.task("PR-27708", "feature", "A/B winners", registry.TaskNeedsYou, "local_review", waitsYou, why)
	b, _ := json.Marshal(reviewStop{Cycle: 3, Why: why, Head: head, Findings: items})
	r.file(stopped.ID, reviewStopFile, string(b))
	other := r.task("PR-27709", "feature", "Since then", registry.TaskNeedsYou, "local_review", waitsYou, "review unavailable (timeout)")
	r.file(other.ID, reviewStopFile, string(b))

	d := r.data(screenNow)
	c := d.Cards[stopped.ID].Task
	if c.WaitingFor != waitsYou+": "+head {
		t.Errorf("WAITING FOR %q, want the stop's count, trend and verdict", c.WaitingFor)
	}
	if len(c.Findings) != 6 || c.Findings[0] != "P2 The merger maps winner 1 wrong · app/models/merger.rb" {
		t.Errorf("the card's findings %q, want the six, priority, title and path", c.Findings)
	}
	if o := d.Cards[other.ID].Task; len(o.Findings) != 0 || o.WaitingFor != waitsYou+": review unavailable (timeout)" {
		t.Errorf("a task that waits on you for another cause: WAITING FOR %q, findings %q", o.WaitingFor, o.Findings)
	}
}

// A round at the cap of a loop that converges goes on by itself: no P0 or
// P1 among its findings, fewer blocking findings than the round before, and
// its judge's verdict does not block. The writer fixes its findings and one
// more round runs; task 9 (blocking 11, 8, 5, P1 5, 3, 0, the judge
// non_blocking) waited 776 minutes on you instead.
func TestAConvergingRoundAtTheCapGoesOn(t *testing.T) {
	r := newReviewRig(t, result(capFindings(1, 4, 1)...), result(capFindings(5, 7)...), result(capFindings(8, 9)...), result())
	task, _ := r.reviewTask("PR-12397")
	for k := 1; k <= 3; k++ {
		r.run(8, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(k)) && len(r.rt.promptsTo("fx-writer")) >= k
		})
		if got := r.reload(task.ID); !isLive(got) {
			t.Fatalf("round %d: task %s waits %q, want the loop to go on", k, got.State, got.WaitsWhat)
		}
		r.writerFixes(task, k, fixedCredit(k), nil)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	cs := r.cycles(task.ID)
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(cs) != 4 || cs[2].Exit != "" || cs[3].Exit != registry.ExitClean {
		t.Fatalf("task %s at %s waits %q; cycles %+v, want a 4th round that ends the loop clean", got.State, got.Stage, got.WaitsWhat, cs)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "review"), func(s string) bool { return strings.Contains(s, "extra round 1 of 2") }) {
		t.Errorf("review events %q, want the extra round said", r.eventTexts(task.ID, "review"))
	}
}

// A round at the cap stops the loop when the loop does not converge: a P1
// among its findings, as many blocking findings as the round before, or a
// judge's verdict that blocks. With cap_go_on off, every such stop is for
// you, as before (a safe stop's go-on: TestASafeStopAtTheCapGoesOnByItself).
func TestARoundAtTheCapThatDoesNotConvergeStops(t *testing.T) {
	for _, tc := range []struct {
		name  string
		third func(evalrun.LocalCase) evalrun.LocalResult
	}{
		{"a P1", result(capFindings(8, 8, 8)...)},
		{"as many as before", result(capFindings(8, 10)...)},
		{"a blocking verdict", withVerdict(store.VerdictBlocking, result(capFindings(8, 8)...))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReviewRig(t, result(capFindings(1, 4, 1)...), result(capFindings(5, 7)...), tc.third, result())
			r.cfg.Factory.LocalReview.CapGoOn = 0
			task, _ := r.reviewTask("PR-12398")
			r.toCap(task, 3)
			if cs := r.cycles(task.ID); len(cs) != 3 || cs[2].Exit != registry.ExitCap || len(r.rv.calls()) != 3 {
				t.Fatalf("cycles %+v after %d rounds, want the loop stopped at its cap", cs, len(r.rv.calls()))
			}
		})
	}
}

// The extra rounds stop at [factory.local_review] extra_cycles: a loop that
// keeps converging stops after cycles + extra_cycles rounds (for you, with
// cap_go_on off), and 0 turns them off.
func TestTheExtraRoundsStopAtExtraCycles(t *testing.T) {
	for _, tc := range []struct{ extra, rounds int }{{2, 5}, {0, 3}} {
		t.Run(fmt.Sprint(tc.extra), func(t *testing.T) {
			var answers []func(evalrun.LocalCase) evalrun.LocalResult
			from := 1
			for n := 7; n >= 1; n-- { // 7, 6, 5, ... blocking P2s
				answers = append(answers, result(capFindings(from, from+n-1)...))
				from += n
			}
			r := newReviewRig(t, answers...)
			r.cfg.Factory.LocalReview.ExtraCycles, r.cfg.Factory.LocalReview.CapGoOn = tc.extra, 0
			task, _ := r.reviewTask("PR-12399")
			r.toCap(task, tc.rounds)
			cs := r.cycles(task.ID)
			if len(cs) != tc.rounds || cs[len(cs)-1].Exit != registry.ExitCap || len(r.rv.calls()) != tc.rounds {
				t.Fatalf("cycles %+v after %d rounds, want %d rounds, the last at the cap", cs, len(r.rv.calls()), tc.rounds)
			}
		})
	}
}
