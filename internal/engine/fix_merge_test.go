package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// fixInputsOf are the inputs the fake fix prompt was rendered with so far.
func (h *harness) fixInputsOf() []agents.FixPromptInput {
	h.t.Helper()
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	return slices.Clone(h.ag.fixInputs)
}

// fixMergeHead is the merge commit a fixer makes of the base's tip.
const fixMergeHead = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"

// On talkable#12079 GitHub showed the PR as CONFLICTING and the fixer did
// not resolve it: the skill forbade any merge, a textual conflict is no
// finding, and magnum never looked. Now a conflict with the base is work
// for the fix job: `magnum fix` on a conflicting PR starts even after a
// review that found nothing (no other reviewer waits either), the answer
// says the fixer merges the base, and the prompt names the base branch, the
// tip magnum fetched and the conflicting paths. The request fetches the
// base and asks `git merge-tree` in the slot; the guard asks again on the
// slot the fixer works in. A dry run fetches nothing and says what would run.
func TestAHandStartedFixOnAConflictingPRWhoseReviewFoundNothingStartsAndNamesTheBase(t *testing.T) {
	const what = "claude-fix merges master at c0c0c0c (2 conflicting files) in review1 (magnum's review of a1a1a1a found nothing)"
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	slot := h.slot("review1")
	git.conflict("app/models/offer.rb", "db/schema.rb")
	fixer(h, git, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: `bin/rspec spec/models/offer_spec.rb` passed", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})

	dry := h.fix(FixPayload{DryRun: true})
	h.handle()
	if res := h.wantDone(dry); !strings.Contains(res, "dry run: would start fixing talkable/talkable#2: "+what+", cycle 1 of 3") {
		t.Errorf("dry run = %q, want it to say %q", res, what)
	}
	if fetches, _ := git.calls(); len(fetches) != 0 {
		t.Errorf("the dry run fetched %v", fetches)
	}

	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, what+", cycle 1 of 3") {
		t.Errorf("answer = %q, want it to say %q", res, what)
	}
	fetches, merges := git.calls()
	if want := slot.MainClone + ":master"; len(fetches) != 2 || fetches[0] != want || fetches[1] != want {
		t.Errorf("fetches = %v, want %s at the request and at the guard", fetches, want)
	}
	if want := slot.Path + ":" + fixHead + ":" + fixBaseTip; len(merges) != 3 || merges[2] != want {
		t.Errorf("merge checks = %v, want the guard's last: %s", merges, want)
	}
	inputs := h.fixInputsOf()
	if len(inputs) != 1 {
		t.Fatalf("fix prompts = %d, want 1", len(inputs))
	}
	in := inputs[0]
	if in.ConflictBase != "master" || in.ConflictSHA != fixBaseTip || !slices.Equal(in.ConflictPaths, []string{"app/models/offer.rb", "db/schema.rb"}) || !in.NoFindings {
		t.Errorf("the prompt's conflict = %q %q %q (no findings %v), want master, %s and both paths", in.ConflictBase, in.ConflictSHA, in.ConflictPaths, in.NoFindings, fixBaseTip)
	}
	ev, ok := h.fixEvent("fix.start")
	if !ok || !strings.Contains(ev.Message, what+" (run ") {
		t.Errorf("fix.start = %q, %v; want it to say %q", ev.Message, ok, what)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["conflict_base"] != "master" || data["conflict_sha"] != fixBaseTip || data["conflict_files"] != 2.0 {
		t.Errorf("fix.start data = %s, want the base, its tip and 2 files", ev.Data)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Head != fixMergeHead {
		t.Errorf("loop = %+v, %v; want it waiting for the re-review of the merge %s", st, ok, fixMergeHead)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want 1", n)
	}
}

// Autofix starts a loop only after a review with findings: a review that
// found nothing on a PR that conflicts with its base starts none, and
// nothing fetches its base.
func TestAutofixDoesNotStartForAConflictAlone(t *testing.T) {
	h, git := autofixHarness(t, "zhuravel", findings(map[string]int{}, "COMMENTED"))
	git.conflict("app/models/offer.rb")
	fixer(h, git, fixMergeHead, func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop started for the conflict alone")
	}
	if fetches, merges := git.calls(); len(fetches)+len(merges) != 0 {
		t.Errorf("autofix looked at the base: fetches %v, merge checks %v", fetches, merges)
	}
}

// The fixer's report names the merge commit among its pushed commits, first
// or alone: the settle takes the pushed head (the merge itself when nothing
// else was pushed) as it takes any listed commit, and the loop goes on to
// the forced re-review of it.
func TestAPushWhoseFirstCommitMergesTheBaseSettles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings map[string]int
		head     string
		pushed   string
	}{
		{name: "the merge and a fix", findings: map[string]int{"P2": 1}, head: fixPushed,
			pushed: "d3d3d3d Merge c0c0c0c: `bin/rspec spec/models/offer_spec.rb` passed\nb2b2b2b Fix the total (review comment 4100000001)"},
		{name: "the merge alone", findings: map[string]int{}, head: fixMergeHead,
			pushed: "d3d3d3d Merge c0c0c0c: not run: environment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", tc.findings)
			git.conflict("app/models/offer.rb")
			fixer(h, git, tc.head, func(run store.Run) string {
				return fixReport(run.ID, tc.pushed, "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
			})
			id := h.fix(FixPayload{})
			h.handle()
			h.wantDone(id)
			if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Head != tc.head {
				t.Errorf("loop = %+v, %v; want it waiting for the re-review of %s", st, ok, tc.head)
			}
			if ev, ok := h.fixEvent("fix.pushed"); !ok || !strings.Contains(ev.Message, "the head is "+tc.head[:7]) {
				t.Errorf("fix.pushed = %q, %v", ev.Message, ok)
			}
			if _, ok := h.fixEvent("fix.failed"); ok {
				t.Error("the settle failed the push")
			}
		})
	}
}

// The review focus of the conflict check: a base that cannot be fetched
// never blocks a fix with findings. The fixer starts without the merge step
// (no conflict in its prompt, no merge-tree asked) and a
// fix.conflict_unchecked warning says why. A review that found nothing, on
// a PR no other reviewer waits on, is still refused, with the cause.
func TestAFailedBaseFetchRunsTheFixWithoutTheMergeStep(t *testing.T) {
	const fetchErr = "gitx: fetch master: exit 128: fatal: unable to access the remote"
	t.Run("findings", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		git.fetchErr = errors.New(fetchErr)
		git.conflict("app/models/offer.rb")
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		id := h.fix(FixPayload{})
		h.handle()
		h.wantDone(id)
		if n := len(h.fixRuns(pr.ID)); n != 1 {
			t.Fatalf("fix runs = %d, want the fixer's", n)
		}
		ev, ok := h.fixEvent("fix.conflict_unchecked")
		want := "could not check talkable/talkable#2 for a conflict with its base master: " + fetchErr + "; claude-fix starts without the merge step"
		if !ok || ev.Level != "warn" || ev.Message != want {
			t.Errorf("fix.conflict_unchecked = %q (%s), %v\nwant %q", ev.Message, ev.Level, ok, want)
		}
		inputs := h.fixInputsOf()
		if len(inputs) != 1 || inputs[0].ConflictBase != "" || inputs[0].ConflictSHA != "" || inputs[0].ConflictPaths != nil {
			t.Errorf("the fix prompt names a conflict: %+v", inputs)
		}
		if _, merges := git.calls(); len(merges) != 0 {
			t.Errorf("merge checks = %v, want none on a base that was not fetched", merges)
		}
		if _, ok := h.fixEvent("fix.replied"); !ok {
			t.Error("the fix did not run to its settle")
		}
	})
	t.Run("found nothing", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{})
		git.fetchErr = errors.New(fetchErr)
		id := h.fix(FixPayload{})
		h.handle()
		want := othersOnly + "; its merge with master could not be checked: " + fetchErr
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Errorf("%s %q\nwant failed %q", r.State, deref(r.Result), want)
		}
		if len(h.fixRuns(pr.ID)) != 0 {
			t.Error("a refused request started the fixer")
		}
	})
}

// A look at the base that fails after the fetch never blocks the fix either:
// a rev-parse of origin/<base> or a merge-tree that git refuses leaves the
// fixer without the merge step, and the warning says why.
func TestAFailedConflictCheckAfterTheFetchRunsTheFixWithoutTheMergeStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(g *fixGit)
		why  string
	}{
		{name: "rev-parse", set: func(g *fixGit) { g.baseErr = errors.New("gitx: rev-parse refs/remotes/origin/master: no such ref") },
			why: "gitx: rev-parse refs/remotes/origin/master: no such ref"},
		{name: "merge-tree", set: func(g *fixGit) {
			g.mergeErr = errors.New("gitx: merge-tree: exit 128: fatal: refusing to merge unrelated histories")
		},
			why: "gitx: merge-tree: exit 128: fatal: refusing to merge unrelated histories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			git.conflict("app/models/offer.rb")
			tc.set(git)
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
			id := h.fix(FixPayload{})
			h.handle()
			h.wantDone(id)
			want := "could not check talkable/talkable#2 for a conflict with its base master: " + tc.why + "; claude-fix starts without the merge step"
			if ev, ok := h.fixEvent("fix.conflict_unchecked"); !ok || ev.Message != want {
				t.Errorf("fix.conflict_unchecked = %q, %v\nwant %q", ev.Message, ok, want)
			}
			if inputs := h.fixInputsOf(); len(inputs) != 1 || inputs[0].ConflictSHA != "" {
				t.Errorf("the fix prompt names a conflict: %+v", inputs)
			}
			if _, ok := h.fixEvent("fix.replied"); !ok {
				t.Error("the fix did not run to its settle")
			}
		})
	}
}

// A fix that starts for the merge alone keeps the tip its request fetched
// when the guard's own fetch fails moments later: the fixer still merges
// it, and the warning says so.
func TestAMergeOnlyFixWhoseGuardFetchFailsMergesTheTipTheRequestFetched(t *testing.T) {
	const fetchErr = "gitx: fetch master: exit 128: fatal: unable to access the remote"
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{})
	git.conflict("app/models/offer.rb")
	git.fetchErrs = []error{nil, errors.New(fetchErr)}
	fixer(h, git, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	want := "could not check talkable/talkable#2 for a conflict with its base master: " + fetchErr + "; claude-fix merges the tip c0c0c0c that the request fetched"
	if ev, ok := h.fixEvent("fix.conflict_unchecked"); !ok || ev.Message != want {
		t.Errorf("fix.conflict_unchecked = %q, %v\nwant %q", ev.Message, ok, want)
	}
	if inputs := h.fixInputsOf(); len(inputs) != 1 || inputs[0].ConflictSHA != fixBaseTip || !slices.Equal(inputs[0].ConflictPaths, []string{"app/models/offer.rb"}) {
		t.Errorf("the fix prompt = %+v, want the conflict the request found", inputs)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Head != fixMergeHead {
		t.Errorf("loop = %+v, %v; want it waiting for the re-review of the merge", st, ok)
	}
}

// A fix that starts for the merge alone has nothing left to do when the
// guard finds that the head merges its base cleanly after all (the base
// moved in between): the loop ends quietly, with an info fix.ended event
// and toast, and nobody is called.
func TestAMergeOnlyFixWhoseConflictIsGoneEndsQuietly(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	git.mergeAnswers = [][]string{{"app/models/offer.rb"}, nil}
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	ev, ok := h.fixEvent("fix.ended")
	if want := "the fix loop ends: nothing to fix: the head a1a1a1a merges master cleanly now, and the review on a1a1a1a found nothing"; !ok ||
		ev.Level != "info" || ev.Message != want {
		t.Errorf("fix.ended = %q (%s), %v\nwant %q", ev.Message, ev.Level, ok, want)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed = %q: the loop calls the operator", ev.Message)
	}
	if cur := h.pr(pr.Number); cur.State == store.PRNeedsAttention {
		t.Errorf("the PR needs attention: %q", deref(cur.LastError))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived its end")
	}
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer started with nothing to do")
	}
	h.flush()
	h.wantToasts("needs attention", 0)
}

// `magnum fix` is answered in the daemon's tick, so the fetch of the base
// it makes for a review that found nothing is bounded (fixRequestFetch): a
// slow remote counts as a failed fetch instead of stalling the poll. A
// request held for a drain looks at a freshly fetched base too, not at the
// last one fetched (a stacked PR's parent may never have been fetched), and
// the guard's own fetch runs on the job's goroutine with git's bound alone.
func TestARequestFetchesTheBaseWithinItsBound(t *testing.T) {
	t.Run("a drain", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{})
		git.conflict("app/models/offer.rb")
		h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
		id := h.fix(FixPayload{})
		h.handle()
		if res := h.wantDone(id); !strings.HasPrefix(res, "held; the fix of talkable/talkable#2 starts after the restart") {
			t.Errorf("answer = %q, want held for the restart", res)
		}
		if b := git.bounds(); len(b) != 1 || b[0] <= 0 || b[0] > fixRequestFetch {
			t.Errorf("fetch bounds = %v, want one fetch within %s", b, fixRequestFetch)
		}
	})
	t.Run("a start", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{})
		git.conflict("app/models/offer.rb")
		fixer(h, git, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
		})
		id := h.fix(FixPayload{})
		h.handle()
		h.wantDone(id)
		if b := git.bounds(); len(b) != 2 || b[0] <= 0 || b[0] > fixRequestFetch || b[1] != 0 {
			t.Errorf("fetch bounds = %v, want the request's within %s, then the guard's without one", b, fixRequestFetch)
		}
	})
}

// A stop during the guard's fetch of the base (`magnum abort`) ends the job
// as any stop does: no warning that the base could not be checked, no
// fixer, no failure.
func TestAStopDuringTheBaseFetchStopsTheFixQuietly(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	git.mu.Lock()
	git.fetchGate, git.fetchStarted = make(chan struct{}), make(chan struct{}, 1)
	started := git.fetchStarted
	git.mu.Unlock()
	id := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.wantDone(id)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the guard never fetched the base")
	}
	abort := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(abort); r.State != store.RequestDone {
		t.Errorf("abort: %s %q", r.State, deref(r.Result))
	}
	if _, ok := h.fixEvent("pr.aborted"); !ok {
		t.Error("no pr.aborted event")
	}
	if done, err := h.st.StepDone(h.ctx, "pr:talkable/talkable#2", "fix.guard"); err != nil || done {
		t.Errorf("the guard step done = %v, %v; want it ended by the stop", done, err)
	}
	for _, kind := range []string{"fix.conflict_unchecked", "fix.failed"} {
		if ev, ok := h.fixEvent(kind); ok {
			t.Errorf("%s = %q after a stop", kind, ev.Message)
		}
	}
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer started after the abort")
	}
}

// A conflicting PR whose review found nothing, and whose other reviewers'
// read GitHub refused for the rate limit, is held as the floor holds a fix,
// not started for the merge: the GitHub budget is spent, and the held fix
// looks at the base when it starts. Nothing fetches the base meanwhile.
func TestAConflictDoesNotStartAFixWhoseOtherReviewersReadHitTheRateLimit(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	git.conflict("app/models/offer.rb")
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	until := h.clock.Now().Add(30 * time.Minute)
	rateLimitedThreads(h, until)
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "held; the fix of talkable/talkable#2 starts once the GitHub budget of zhuravel recovers") {
		t.Errorf("answer = %q, want the fix held for the budget", res)
	}
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Errorf("fix runs = %d, want none while the budget is spent", n)
	}
	if fetches, merges := git.calls(); len(fetches)+len(merges) != 0 {
		t.Errorf("the held fix looked at the base: fetches %v, merge checks %v", fetches, merges)
	}
}
