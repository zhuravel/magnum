package engine

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// requiredCompletion makes GitHub's rulesets of talkable/talkable require
// Completion, a check of its CI workflow.
func requiredCompletion(h *harness) {
	h.gh.required = map[string][]string{"talkable/talkable": {"Completion"}}
}

// othersChecks are the checks of the other apps, passed: the rollup of a
// head with no run of CI is SUCCESS.
func othersChecks() []github.Check {
	return []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("", "Devin Review", github.CheckPassed)}
}

// openOwn has GitHub list the operator's PR #2 as spec, into main (the
// default branch, whose required checks GitHub's list is) unless spec
// names its base, beside the baseline.
func (h *harness) openOwn(spec prSpec) {
	h.t.Helper()
	spec.n, spec.author, spec.base = 2, "zhuravel", cmp.Or(spec.base, "main")
	h.open(prSpec{n: 1, head: "base1"}, spec)
}

// othersPassed opens PR #2 at head with the checks of the other apps
// passed and no run of CI: the rollup is SUCCESS.
func (h *harness) othersPassed(head string) {
	h.t.Helper()
	h.openOwn(prSpec{head: head, ci: "SUCCESS", checks: othersChecks()})
}

// The live case: an own PR whose repository requires Completion, and whose
// CI workflow runs only when a PR opens. GitHub lists the other apps'
// checks at the head, all passed, and the rollup says SUCCESS; the look
// called the PR idle ("nothing open") while GitHub blocked its merge. Once
// the head is older than settle_max it now says that the required check
// did not run there.
func TestTheLookNeverCallsAHeadIdleWhoseRequiredCheckDidNotRun(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: run its workflow on the PR's branch, or push")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Errorf("dispatched %q without ci_dispatch", calls)
	}
}

// ciDispatched is a harness for the look's start of the CI workflow:
// requiredCompletion, [[repo]] ci_dispatch = "ci.yml", and a head whose
// files are paths (Details' list), at which the other apps' checks passed.
func ciDispatched(t *testing.T, paths ...string) *harness {
	t.Helper()
	return ciDispatchedAt(t, prSpec{head: fixHead, ci: "SUCCESS", checks: othersChecks()}, paths...)
}

// ciDispatchedAt is ciDispatched with the operator's PR #2 opened as spec
// (openOwn), reviewed clean.
func ciDispatchedAt(t *testing.T, spec prSpec, paths ...string) *harness {
	t.Helper()
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = paths
	})
	h.openOwn(spec)
	h.tick() // reviewed clean
	return h
}

// dispatchData is the data of PR #2's newest event of kind.
func (h *harness) dispatchData(kind string) map[string]any {
	h.t.Helper()
	ev, ok := h.fixEvent(kind)
	if !ok {
		h.t.Fatalf("no %s event", kind)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		h.t.Fatal(err)
	}
	return data
}

// With ci_dispatch, the look starts the workflow that produces the
// required check on the PR's branch (as the watch's poll identity) once the
// check did not run at the head past settle_max, records it
// (fix.open_ci_dispatch) and waits for it: never twice for the head, and
// once settle_max passed again with no run, the PR needs you.
func TestTheLookStartsTheCIWorkflowOnceForAHeadWhoseRequiredCheckDidNotRun(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"dispatch:talkable/talkable:ci.yml@feature"}
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want %q", calls, want)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	data := h.dispatchData("fix.open_ci_dispatch")
	if data["head"] != fixHead || data["workflow"] != "ci.yml" {
		t.Errorf("fix.open_ci_dispatch data = %v", data)
	}
	h.advance(10 * time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	h.advance(11 * time.Minute)
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum started ci.yml at ")
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want one for the head", calls)
	}
}

// The run magnum started fills the check in: it runs (CI at the head
// waits), then passes, and the PR is idle.
func TestTheRunOfTheStartedCIWorkflowEndsTheWait(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	checks := []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("CI", "Completion", github.CheckPending)}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", ci: "PENDING", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a")
	checks[1].State = github.CheckPassed
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", ci: "SUCCESS", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// A PR that changes its own workflows or actions would run its own CI with
// the repository's secrets: magnum starts nothing, says why once
// (fix.open_ci_dispatch_refused), and the PR needs you.
func TestTheLookNeverStartsTheCIWorkflowOfAPRThatChangesItsWorkflows(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb", ".github/workflows/ci.yml")
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q for a PR that changes its workflows", calls)
	}
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum does not start ci.yml (the PR changes its own workflows or actions)")
	if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
		t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
	}
}

// A start GitHub refuses (a workflow without a workflow_dispatch trigger
// at the branch: HTTP 422) is recorded (fix.open_ci_dispatch_failed, with
// the status and never GitHub's message, which can name the branch), never
// tried again for the head, and the PR needs you.
func TestAFailedStartOfTheCIWorkflowNeedsYouAndIsNotRepeated(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.gh.mu.Lock()
	h.gh.dispatchErr = &github.APIError{Op: "dispatch", Status: 422, Message: "Workflow does not have 'workflow_dispatch' trigger"}
	h.gh.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.tick()
	h.advance(10 * time.Minute)
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want one for the head", n)
	}
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum's start of ci.yml failed (GitHub answered HTTP 422)")
	ev, ok := h.fixEvent("fix.open_ci_dispatch_failed")
	if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "HTTP 422") || strings.Contains(ev.Message, "workflow_dispatch") {
		t.Fatalf("fix.open_ci_dispatch_failed = %+v, %v", ev, ok)
	}
}

// The start waits for the end of `magnum pause` and for the GitHub budget
// of the identity it starts as; without either, the verdict asks the look
// to start it.
func TestTheStartOfTheCIWorkflowWaitsForThePauseAndTheBudget(t *testing.T) {
	f := idleFacts()
	f.ci = openCI{missing: []string{"Completion"}}
	f.ciWorkflow = "ci.yml"
	v, _, _ := decideOpen(f)
	if !v.dispatch || v.Verdict != OpenNeedsYou {
		t.Fatalf("verdict = %+v, want the start asked for", v)
	}
	f.pause, f.pauseUntil = "daemon paused", openNow.Add(time.Hour)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the pause ends")
	if v.dispatch || !v.Until.Equal(f.pauseUntil) {
		t.Errorf("dispatch %v, until %s under the pause", v.dispatch, v.Until)
	}
	f.pause, f.pauseUntil, f.low = "", time.Time{}, true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the GitHub budget allows")
	if v.dispatch {
		t.Error("asked to start it below the GitHub floor")
	}
}

// A required check its workflow creates only when the jobs it needs end
// (Completion `needs:` the other CI jobs) has no run at the head while that
// workflow runs, however long it takes: the look waits for it as CI at the
// head, past settle_max too, never says that it did not run and starts
// nothing; once it ran and passed the PR is idle.
func TestTheLookWaitsForARequiredCheckWhileTheHeadsChecksRun(t *testing.T) {
	passed, pending := github.CheckPassed, github.CheckPending
	h := ciDispatched(t, "app/models/order.rb")
	checks := append(othersChecks(), ciCheck("CI", "RSpec (3)", pending), ciCheck("CI", "Checks", passed))
	h.openOwn(prSpec{head: fixHead, ci: "PENDING", checks: checks})
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q while the head's CI ran", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion has not started")
	h.advance(30 * time.Minute) // a slow run
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion has not started")

	checks[2].State = passed
	checks = append(checks, ciCheck("CI", "Completion", pending))
	h.openOwn(prSpec{head: fixHead, ci: "PENDING", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a")
	checks[4].State = passed
	h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q", calls)
	}
}

// GitHub's list is the default branch's: a PR stacked on another branch
// merges without its checks, so one with no run at such a PR's head leaves
// rule 11 to the rollup, as before, and magnum starts nothing for it.
func TestTheLookLeavesTheRequiredChecksOfAStackedPRToTheRollup(t *testing.T) {
	h := ciDispatchedAt(t, prSpec{head: fixHead, base: "feature-a", ci: "SUCCESS", checks: othersChecks()}, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q for a stacked PR", calls)
	}
}

// A [[repo]] required_checks list GitHub does not enforce may name a
// workflow whose path filters skip a head (an infra-only PR gets only its
// plan checks, and GitHub merges it): without ci_dispatch an entry with no
// run at the head leaves rule 11 to the rollup, as before.
func TestTheLookLeavesAConfiguredCheckWithNoRunToTheRollupWithoutCIDispatch(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), withRequiredChecks("workflow:CI"))
	h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: []github.Check{ciCheck("Infra", "infra-plan", github.CheckPassed)}})
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
}

// A required check ignore_checks names ran at the head though magnum does
// not count it: it is never missing, whether GitHub's rulesets require it
// or a [[repo]] required_checks list of a repository that sets ci_dispatch
// names it, and the PR is idle once the rest passed.
func TestARequiredCheckIgnoreChecksNamesIsNotMissingWhereItRan(t *testing.T) {
	for name, mod := range map[string]func(*harness){
		"GitHub's rulesets": func(h *harness) { h.gh.required = map[string][]string{"talkable/talkable": {"Vendor Gates"}} },
		"required_checks":   withRequiredDispatch("Vendor Gates"),
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), mod, func(h *harness) {
				h.cfg.Watches[0].IgnoreChecks = []string{"Vendor Gates"}
			})
			h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: append(othersChecks(), vendorCheck(github.CheckPassed))})
			h.tick() // reviewed clean
			h.pastSettle()
			h.tick()
			h.tick()
			h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
			if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
				t.Fatalf("dispatched %q for a check that ran", calls)
			}
		})
	}
}

// The start reads the head's own file list: a stored list of an older head
// (the poll has not listed the new head's yet, and that head may add a
// workflow) starts nothing, and the look starts it once the head's list is
// stored. A new head after a recorded start is started again, once: the
// start of an older head is not this head's.
func TestTheLookStartsTheCIWorkflowOncePerHeadByThatHeadsFiles(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want one for a1a1a1a", n)
	}
	pr := h.pr(2)
	pr.HeadSHA = fixPushed // pushed, its files not listed yet
	if s, ok := h.e.openStartCI(h.ctx, h.repo(), h.cfg.Watches[0], pr, "ci.yml", "required check Completion"); ok || s != (ciStart{}) {
		t.Fatalf("start = %+v, %v with the files of the older head", s, ok)
	}
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d with the files of the older head", n)
	}

	h.othersPassed(fixPushed)
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"dispatch:talkable/talkable:ci.yml@feature", "dispatch:talkable/talkable:ci.yml@feature"}
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want one for each head", calls)
	}
	h.wantOpen(OpenWaits, "CI at b2b2b2b: magnum started ci.yml at ")
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 2 {
		t.Fatalf("dispatches = %d, want one for each head", n)
	}
}

// magnum starts the workflow only on a PR whose files GitHub listed whole
// (a file past the list may change a workflow) and whose branch it knows:
// it refuses any other once for the head (fix.open_ci_dispatch_refused),
// and the PR needs you.
func TestTheLookRefusesToStartTheCIWorkflowOfAHeadItCannotCheck(t *testing.T) {
	t.Run("files GitHub did not list whole", func(t *testing.T) {
		// a head of its own: the registry keeps the first list of a head
		h := ciDispatchedAt(t, prSpec{head: fixPushed, ci: "SUCCESS", checks: othersChecks(), filesTruncated: true}, "app/models/order.rb")
		h.pastSettle()
		h.tick()
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q", calls)
		}
		h.wantOpen(OpenNeedsYou, "did not run at b2b2b2b: magnum does not start ci.yml (GitHub did not list every file of the PR)")
		if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
			t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
		}
	})
	t.Run("a branch magnum does not know", func(t *testing.T) {
		h := ciDispatched(t, "app/models/order.rb")
		pr := h.pr(2)
		pr.HeadRef = nil
		s, ok := h.e.openStartCI(h.ctx, h.repo(), h.cfg.Watches[0], pr, "ci.yml", "required check Completion")
		if !ok || s.Refused != "the PR's branch is not known" || !s.At.IsZero() {
			t.Fatalf("start = %+v, %v; want refused", s, ok)
		}
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q", calls)
		}
		if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
			t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
		}
	})
}
