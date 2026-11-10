package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// ciCompletion is talkable's required check Completion of ci.yml, in state.
func ciCompletion(state string) github.Check {
	return github.Check{Name: "Completion", State: state, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
}

// ciFixHarness is the operator's PR #2 of talkable/talkable, whose GitHub
// rulesets require Completion, reviewed clean at fixHead, where Completion
// failed in run ciRunID of ci.yml (setFailedJobs' jobs and logs); the
// base's tip ran Completion as base says. The PR's files are app code.
func ciFixHarness(t *testing.T, base string) (*harness, *fixGit) {
	t.Helper()
	return ciFixHarnessWith(t, base, findings(map[string]int{}, "COMMENTED"))
}

// ciFixHarnessWith is ciFixHarness whose reviews give what next gives.
func ciFixHarnessWith(t *testing.T, base string, next func(in pipeline.RoundInput) (map[string]int, string)) (*harness, *fixGit) {
	t.Helper()
	h, git := openHarness(t, next, requiredCompletion, func(h *harness) {
		h.gh.defaultFiles = []string{"app/models/offer.rb"}
	})
	h.openOwn(prSpec{head: fixHead, ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick() // reviewed clean
	h.setHeadRuns(fixHead, failedCIRun(fixHead, 1))
	h.setFailedJobs()
	h.setBaseCI(base)
	return h, git
}

// setBaseCI makes the base's tip run Completion as state.
func (h *harness) setBaseCI(state string) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.firstParentsAll = []github.CommitWithChecks{{Checks: []github.Check{ciCompletion(state)}}}
}

// ciRecordOf is PR #2's record of CI fixes.
func (h *harness) ciRecordOf() ciFixRecord {
	h.t.Helper()
	r, err := h.e.ciFixRecordOf(h.ctx, h.pr(2).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// cannotFixer is a CI fixer that fixes nothing and lists job 101 as class.
func cannotFixer(h *harness, git *fixGit, class string) {
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "0 threads", "- 101 ci: not fixed. "+class+": rspec passes when run again")
	})
}

// A failure that is red on the base too starts no CI fix and reads no log:
// the base's CI is read once per head and failed set (one GraphQL call over
// three looks), and a new head is read again.
func TestTheLookReadsTheBaseOncePerHeadAndFailedSet(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckFailed)
	h.pastSettle()
	h.tick()
	h.tick()
	h.tick()
	if calls := h.ghCalls("first_parents:"); len(calls) != 1 {
		t.Fatalf("base reads = %q, want one", calls)
	}
	v := h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a, and on main at ")
	if !strings.HasSuffix(v.Why, " too: magnum fixes only a check that passes on the base") || len(v.Needs) != 1 || v.Needs[0].Kind != "ci" {
		t.Errorf("verdict = %+v", v)
	}
	if n := len(h.ghCalls("job_log:")) + len(h.ghCalls("run_jobs:")); n != 0 || len(h.fixRuns(h.pr(2).ID)) != 0 {
		t.Errorf("a failure red on the base read %d jobs or logs, or started a fixer", n)
	}
	next := strings.Repeat("e", 40)
	h.openOwn(prSpec{head: next, ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick() // magnum reviews the new head
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("first_parents:"); len(calls) != 2 {
		t.Fatalf("base reads = %q, want one more for the new head", calls)
	}
}

// A failure green on the base starts a CI fix once: the look reads the
// failed jobs once (the head's runs, the run's jobs, two logs), writes
// ci-failures.json and its summary beside the fixer's report, records only
// the file's digest and counts, and names no check's or job's name in its
// events; the fixer gets the file and no external file, and fix.start
// names no review it worked on. A fixer that says the test is flaky ends
// the loop quietly (no needs attention), and the head needs you with its
// words, kind ci; nothing starts or reads again at the head.
func TestTheLookReadsTheFailedJobsOnceAndStartsACIFixThatEndsQuietly(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckPassed)
	cannotFixer(h, git, "flaky")
	h.pastSettle()
	h.tick()
	pr := h.pr(2)
	if runs := h.fixRuns(pr.ID); len(runs) != 1 {
		t.Fatalf("fix runs = %d, want the CI fix's", len(runs))
	}
	for prefix, want := range map[string]int{"first_parents:": 1, "head_runs:": 1, "run_jobs:": 1, "job_log:": 2} {
		if n := len(h.ghCalls(prefix)); n != want {
			t.Errorf("%s calls = %d, want %d", prefix, n, want)
		}
	}
	dir := h.d.Layout.ReviewDir("talkable", "talkable", 2, fixHead)
	for _, name := range []string{ciFailuresFile, ciFailuresMD} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	h.ag.mu.Lock()
	inputs := h.ag.fixInputs
	h.ag.mu.Unlock()
	if len(inputs) != 1 || !inputs[0].CIOnly || inputs[0].CIFile != filepath.Join(dir, ciFailuresFile) || inputs[0].ExternalFile != "" {
		t.Fatalf("fix inputs = %+v, want the CI file and no external file", inputs)
	}
	rec := h.ciRecordOf()
	if rec.Read.Digest == "" || rec.Read.Jobs != 2 || len(rec.Starts) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	raw, _ := kvValue(h, kvPRCIFix(pr.ID))
	start := h.dispatchData("fix.start")
	if start["mode"] != FixModeCI || start["review_run"] != nil || start["ci_jobs"] != float64(2) {
		t.Errorf("fix.start data = %v", start)
	}
	for _, kind := range []string{"fix.ci_read", "fix.start", "fix.ended", "autofix.open"} {
		ev, ok := h.fixEvent(kind)
		if !ok {
			t.Fatalf("no %s event", kind)
		}
		for _, name := range []string{"rspec", "capybara", "Run tests", "Failure line"} {
			if strings.Contains(ev.Message, name) || strings.Contains(string(ev.Data), name) || strings.Contains(raw, name) {
				t.Errorf("%s or the record names %q: %s %s", kind, name, ev.Message, ev.Data)
			}
		}
	}
	if s := rec.Starts[0]; s.Head != fixHead || s.Outcome != ciOutcomeCannot || s.Why != "flaky" || s.By != autofixBy ||
		strings.Join(s.Entries, ",") != "Completion" {
		t.Errorf("start = %+v, want a cannot of class flaky", s)
	}
	if ev, _ := h.fixEvent("fix.ended"); ev.Message != "the CI fix ends: the CI fix found a flaky test, not the PR's code" {
		t.Errorf("fix.ended = %q", ev.Message)
	}
	h.wantState(2, store.PRReviewed)
	h.tick()
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a: the CI fix found a flaky test, not the PR's code")
	if len(v.Needs) != 1 || v.Needs[0].Kind != "ci" {
		t.Errorf("needs = %+v", v.Needs)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 || len(h.ghCalls("job_log:")) != 2 || len(h.ghCalls("first_parents:")) != 1 {
		t.Errorf("after the CI fix: %d fix runs, %d log reads, %d base reads; want nothing again at the head", n,
			len(h.ghCalls("job_log:")), len(h.ghCalls("first_parents:")))
	}
}

// The CI fix is recorded before its start: a start the fix job refuses at
// once (here someone typed into the PR's panes) is marked unstarted and
// does not count; CI fixes recorded with no loop (a crash between the
// record and the start) count, so a restart never starts a fourth.
func TestTheCIFixRecordIsWrittenBeforeTheStart(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckPassed)
	h.pastSettle()
	pr := h.pr(2)
	typed := h.clock.Now()
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) { u.Set("human_active_at", typed) }); err != nil {
		t.Fatal(err)
	}
	h.tick()
	rec := h.ciRecordOf()
	if len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted || rec.count() != 0 {
		t.Fatalf("record = %+v, want one unstarted start", rec.Starts)
	}
	h.wantOpen(OpenWaits, "someone typed into its panes")
	if len(h.fixRuns(pr.ID)) != 0 {
		t.Fatal("a fixer ran")
	}
	lost := ciFixRecord{Read: rec.Read, Base: rec.Base}
	for _, head := range []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333"} {
		lost.Starts = append(lost.Starts, ciFixStart{Head: head, Entries: []string{"Completion"}, At: typed})
	}
	b, err := json.Marshal(lost)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, kvPRCIFix(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Hour) // past the cooldown
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a after 3 CI fixes: the CI is yours")
	if len(h.fixRuns(pr.ID)) != 0 || len(h.ciRecordOf().Starts) != 3 {
		t.Fatal("a fourth CI fix started")
	}
}

// A CI fix's push goes to its forced re-review like any fixer's push (the
// verdict says it is the CI fix's), its outcome is recorded at once, and
// the loop's next cycle, on the re-review's findings, fixes them in
// findings mode: the CI fix is the first cycle alone.
func TestACIFixPushGoesToItsForcedReReviewAndTheNextCycleFixesFindings(t *testing.T) {
	var mu sync.Mutex
	rounds := 0
	h, git := ciFixHarnessWith(t, github.CheckPassed, func(pipeline.RoundInput) (map[string]int, string) {
		mu.Lock()
		defer mu.Unlock()
		if rounds++; rounds == 2 {
			return map[string]int{"P2": 1}, "COMMENTED" // the CI fix's push brings a finding
		}
		return map[string]int{}, "COMMENTED"
	})
	cycles := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		if cycles++; cycles == 1 {
			return fixPushed, fixReport(run.ID, "b2b2b2b Keep the total in cents", "0 threads", "none")
		}
		return "", fixReport(run.ID, "nothing", fixReplied, "none")
	})
	h.pastSettle()
	h.tick() // the CI fix runs and pushes
	rec := h.ciRecordOf()
	if len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomePushed || rec.Starts[0].Pushed != fixPushed {
		t.Fatalf("record = %+v, want the push recorded", rec.Starts)
	}
	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Mode != FixModeCI {
		t.Fatalf("loop = %+v, %v; want the CI fix's push in its re-review", st, ok)
	}
	h.e.autofixOpen(h.ctx)
	h.wantOpen(OpenFixing, "fixing, re-review of b2b2b2b (the CI fix's push)")
	for range 4 {
		h.advance(time.Minute)
		h.tick()
	}
	h.ag.mu.Lock()
	inputs := slices.Clone(h.ag.fixInputs)
	h.ag.mu.Unlock()
	if len(inputs) != 2 || !inputs[0].CIOnly || inputs[1].CIOnly || inputs[1].CIFile != "" {
		t.Fatalf("fix inputs = %+v, want the CI fix, then a cycle on the re-review's findings", inputs)
	}
	if ev, ok := h.fixEventOf(2, "fix.start"); !ok || strings.Contains(string(ev.Data), `"mode"`) {
		t.Errorf("the second cycle's fix.start = %s, want no mode", ev.Data)
	}
	if rec := h.ciRecordOf(); rec.count() != 1 || rec.Starts[0].Outcome != ciOutcomePushed {
		t.Errorf("record = %+v: the loop's end keeps the CI fix's push", rec.Starts)
	}
}

// The fixer's ci entries under Needs you name the job and one class word:
// the settle reads only that word, never the reason.
func TestNeedsYouEntryReadsACIEntryAndItsClass(t *testing.T) {
	class, ci, rest := ciCannotOf([]string{"129341926468 ci: not fixed. infra: the runner lost its disk",
		"2101 P2 app/a.rb:4: not fixed. contested", "77 ci: not fixed. the log is cut"})
	if class != "infra" || !ci || len(rest) != 1 || !strings.HasPrefix(rest[0], "2101 P2") {
		t.Errorf("class %q, ci %v, rest %q", class, ci, rest)
	}
	for entry, want := range map[string]string{
		"5 ci: not fixed. flaky: passes again": "flaky", "5 ci: not fixed. secret: STRIPE_KEY": "secret", "5 ci: not fixed. config: a step": "config",
		"5 ci: not fixed. base: master fails too": "base", "5 ci: not fixed. whatever": "unfixed",
	} {
		if got, ok, _ := ciCannotOf([]string{entry}); got != want || !ok {
			t.Errorf("%q: class %q, %v; want %q", entry, got, ok, want)
		}
	}
	if m := needsYouEntryRe.FindStringSubmatch("2101 P1 app/models/offer.rb:42: not fixed. The author's call: a or b"); m == nil || m[7] == "" || m[8] != "" {
		t.Errorf("the author's call: %q", m)
	}
}

// The daemon's push of a merge of the parent (a loop's first cycle, a CI
// fix's included, merges a base tip the head lacks first) is magnum's, not
// the operator's: its head is the fixer's, so hands_off_after_push does
// not hold the next start on it with "you pushed".
func TestADaemonsMergeOfTheParentIsNoPushOfYours(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckPassed)
	repo := store.Repo{Owner: "talkable", Name: "talkable"}
	for kind, merge := range map[string]string{"fix.parent_merged": fixPushed, "fix.parent_pushed": strings.Repeat("d", 40)} {
		h.e.event(h.ctx, "info", prSubject(repo, 2), kind, "magnum merged main into the PR", map[string]any{"merge": merge, "head": fixHead})
	}
	l := openLedger{Since: h.clock.Now().Add(-time.Hour)}
	h.e.openLoopsEnded(h.ctx, repo, h.pr(2), &l, false)
	if !slices.Contains(l.FixerHeads, fixPushed) || !slices.Contains(l.FixerHeads, strings.Repeat("d", 40)) {
		t.Errorf("fixer heads = %q, want the daemon's merges", l.FixerHeads)
	}
}
