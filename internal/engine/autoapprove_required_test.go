package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// ciCheck is a check of workflow that finished (or started) before every
// review.
func ciCheck(workflow, name, state string) github.Check {
	return github.Check{Name: name, State: state, Workflow: workflow, At: ciBefore}
}

// withRequiredChecks sets talkable/talkable's [[repo]] required_checks.
func withRequiredChecks(checks ...string) func(*harness) {
	return func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", RequiredChecks: checks})
	}
}

// approvedEvents are the review.auto_approved events' messages.
func (h *harness) approvedEvents() []string {
	h.t.Helper()
	var out []string
	for _, ev := range h.events("review.auto_approved") {
		out = append(out, ev.Message)
	}
	return out
}

// withRequiredDispatch sets talkable/talkable's [[repo]] required_checks
// and ci_dispatch = "ci.yml": the operator says the repository's CI may not
// run at a head, so an entry with no run there is missing.
func withRequiredDispatch(checks ...string) func(*harness) {
	return func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", RequiredChecks: checks, CIDispatch: "ci.yml"})
	}
}

// reviewedWithChecks opens PR #2 on head b1 with rollup ci and checks after
// a first poll, and lets magnum's clean review of it post.
func reviewedWithChecks(h *harness, ci string, checks ...github.Check) {
	h.t.Helper()
	reviewedSpec(h, prSpec{n: 2, head: "b1", ci: ci, checks: checks})
}

// reviewedSpec opens PR #2 as spec after a first poll, and lets magnum's
// clean review of its head post.
func reviewedSpec(h *harness, spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, spec)
	h.tick()
	h.reviewTo(2, spec.head)
	h.flush()
	h.flush()
}

// On talkable#12036 the CI workflow (64 checks, Completion included)
// passed, a path-filtered workflow's job (Terraform scripts / lint) failed
// on a flake, and the repository's required_checks = ["workflow:CI"] named
// what GitHub requires to merge; the gate still held magnum's approval as
// the operator for the unrequired job, which the base tip never runs, for a
// day. A failing check required_checks does not name no longer holds the
// approval: it posts with its body unchanged, review.auto_approved and the
// daemon's log name the check it ignored, and the base tip is never read
// for it. An unrequired check that still runs is not waited for either,
// and a required check that fails on the base tip too goes through as
// before while the unrequired failure is ignored beside it.
func TestAFailingCheckOutsideRequiredChecksDoesNotHoldTheApproval(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	const note = "ignored 1 failing check outside required_checks: Terraform scripts / lint"
	for _, tc := range []struct {
		name     string
		ci       string
		checks   []github.Check
		base     []github.Check
		note     string
		baseRead bool
	}{
		{name: "the 12036 layout", ci: "FAILURE", note: note, checks: []github.Check{ciCheck("CI", "rspec", passed),
			ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)}},
		{name: "an unrequired check still runs", ci: "PENDING", checks: []github.Check{ciCheck("CI", "rspec", passed),
			ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", pending)}},
		{name: "a required check fails on the base too", ci: "FAILURE", note: note, baseRead: true,
			checks: []github.Check{ciCheck("CI", "audit", failed), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)},
			base:   []github.Check{ciCheck("CI", "audit", failed), ciCheck("CI", "Completion", passed)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			h, _ := newAutoHarness(t, withRequiredChecks("workflow:CI"), withBaseChecks(tc.base...),
				func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
			reviewedWithChecks(h, tc.ci, tc.checks...)
			got := h.created()
			if len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q, want the approval of b1", got)
			}
			if strings.Contains(got[0], "Terraform") || strings.Contains(got[0], "required_checks") {
				t.Fatalf("the approval's body names the ignored check: %q", got[0])
			}
			if r := h.autoRefused(2); r != "" {
				t.Fatalf("held back for %q", r)
			}
			evs := h.approvedEvents()
			if len(evs) != 1 {
				t.Fatalf("review.auto_approved events = %q, want one", evs)
			}
			if tc.note == "" {
				if strings.Contains(evs[0], "ignored") || strings.Contains(logs.String(), "outside required_checks") {
					t.Fatalf("an approval that ignored no failing check says it did: %q\n%s", evs[0], logs.String())
				}
			} else {
				if !strings.HasSuffix(evs[0], "; "+tc.note) {
					t.Fatalf("review.auto_approved = %q, want it to end with %q", evs[0], tc.note)
				}
				if !strings.Contains(logs.String(), tc.note) {
					t.Fatalf("the daemon's log does not say %q:\n%s", tc.note, logs.String())
				}
			}
			reads := len(h.ghCalls("commit_checks:"))
			baseEvents := h.events(ciBaseEvent)
			if tc.baseRead {
				want := "talkable/talkable#2: its head's 1 failing check fails on master's tip base-ti too; the approval goes ahead"
				if reads != 1 || len(baseEvents) != 1 || baseEvents[0].Message != want {
					t.Fatalf("base tip read %d times, %s events = %+v; want one read and %q", reads, ciBaseEvent, baseEvents, want)
				}
			} else if reads != 0 || len(baseEvents) != 0 {
				t.Fatalf("base tip read %d times, %s events = %+v, for checks required_checks does not name", reads, ciBaseEvent, baseEvents)
			}
		})
	}
}

// A check required_checks names still holds the approval as it did, with
// the same refusal: a failing one (a whole workflow's, or one a name glob
// names in another workflow's suite) whose base tip passes it, and a
// running one, which waits however the unrequired checks beside it end.
func TestAFailingRequiredCheckStillHoldsTheApproval(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	for _, tc := range []struct {
		name     string
		required []string
		ci       string
		checks   []github.Check
		want     string
	}{
		{name: "a workflow's check fails", required: []string{"workflow:CI"}, ci: "FAILURE", want: ciFailsWords,
			checks: []github.Check{ciCheck("CI", "rspec", failed), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)}},
		{name: "a named check fails", required: []string{"Completion"}, ci: "FAILURE", want: ciFailsWords,
			checks: []github.Check{ciCheck("CI", "rspec", passed), ciCheck("Dispatch", "Completion", failed)}},
		{name: "a required check still runs", required: []string{"workflow:CI"}, ci: "FAILURE", want: ciRunsWords,
			checks: []github.Check{ciCheck("CI", "rspec", pending), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, withRequiredChecks(tc.required...),
				withBaseChecks(ciCheck("CI", "rspec", passed), ciCheck("Dispatch", "Completion", passed)))
			reviewedWithChecks(h, tc.ci, tc.checks...)
			h.wantRefused(2, tc.want)
			if got := h.autoRefused(2); got != tc.want {
				t.Fatalf("held back for %q, want exactly %q", got, tc.want)
			}
			if evs := h.approvedEvents(); len(evs) != 0 {
				t.Fatalf("review.auto_approved events = %q", evs)
			}
		})
	}
}

// ciMissingWords is the CI gate's refusal of a head a required check did
// not run on.
const ciMissingWords = "its head's required checks have not run (magnum approves it once they pass)"

// A required check with no run on a head whose checks magnum read whole
// holds the approval as the operator, however the other checks read: the
// repository's CI runs only when a PR opens, so a later push gets no
// Completion, and GitHub will not merge the head. It holds where GitHub's
// rulesets require it on the default branch the PR merges into (without
// [[repo]] required_checks, where the rollup decides the rest), and where
// required_checks names it in a repository that sets ci_dispatch. Before,
// such an entry let the rollup decide, and the SUCCESS of the other checks
// approved the head. The refusal waits, as for checks that run (magnum
// approves once the check ran and passed), and a required check that
// fails as on the base tip waits for it too, where it would go through.
func TestARequiredCheckThatNeverRanOnTheHeadHoldsTheApproval(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	others := []github.Check{ciCheck("Pronto", "pronto", passed), ciCheck("", "Devin Review", passed)}
	layout := []github.Check{ciCheck("CI", "rspec", passed), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)}
	redLikeTheBase := []github.Check{ciCheck("Pronto", "pronto", passed), ciCheck("CI", "rspec", failed)}
	for _, tc := range []struct {
		name   string
		mod    func(*harness)
		ci     string
		checks []github.Check
		want   string
	}{
		{name: "GitHub's rulesets", mod: requiredCompletion, ci: "SUCCESS", checks: others, want: ciMissingWords},
		{name: "a named check", mod: withRequiredDispatch("Completion"), ci: "SUCCESS", checks: others, want: ciMissingWords},
		{name: "a workflow", mod: withRequiredDispatch("workflow:CI"), ci: "SUCCESS", checks: others[:1], want: ciMissingWords},
		{name: "one of two, beside a failing unrequired check", mod: withRequiredDispatch("workflow:CI", "Deploy preview"), ci: "FAILURE",
			checks: layout, want: ciMissingWords},
		{name: "beside a required check that fails as on the base tip", mod: withRequiredDispatch("workflow:CI", "Completion"), ci: "FAILURE",
			checks: redLikeTheBase, want: ciRunsWords},
		{name: "GitHub's, beside a check that fails as on the base tip", mod: requiredCompletion, ci: "FAILURE",
			checks: redLikeTheBase, want: ciRunsWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, tc.mod, withBaseChecks(ciCheck("CI", "rspec", failed)))
			reviewedSpec(h, prSpec{n: 2, head: "b1", base: "main", ci: tc.ci, checks: tc.checks})
			h.wantRefused(2, tc.want)
			if got := h.autoRefused(2); got != tc.want {
				t.Fatalf("held back for %q, want exactly %q", got, tc.want)
			}
			if evs := h.approvedEvents(); len(evs) != 0 {
				t.Fatalf("review.auto_approved events = %q", evs)
			}
		})
	}
}

// A required check with no run at the head that nothing makes required
// there leaves the gate to the rollup, as before: GitHub's list is the
// default branch's, and a PR stacked on another branch merges without it;
// a [[repo]] required_checks list GitHub does not enforce may name a
// workflow whose path filters skip the head (an infra-only PR gets only its
// plan checks), unless the repository sets ci_dispatch. The rollup then
// approves a head whose other checks passed, and holds one where a check
// outside the list fails.
func TestARequiredCheckWithNoRunThatNothingEnforcesLeavesTheRollupToDecide(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	for _, tc := range []struct {
		name   string
		mod    func(*harness)
		base   string
		ci     string
		checks []github.Check
		want   string // "" = approved
	}{
		{name: "a PR stacked on another branch", mod: requiredCompletion, base: "feature-a", ci: "SUCCESS",
			checks: []github.Check{ciCheck("Pronto", "pronto", passed), ciCheck("", "Devin Review", passed)}},
		{name: "a workflow its path filters skip", mod: withRequiredChecks("workflow:CI"), base: "main", ci: "SUCCESS",
			checks: []github.Check{ciCheck("Infra", "infra-plan", passed)}},
		{name: "beside a failing unrequired check", mod: withRequiredChecks("workflow:CI", "Deploy preview"), base: "main", ci: "FAILURE",
			checks: []github.Check{ciCheck("CI", "rspec", passed), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)},
			want:   ciFailsWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, tc.mod, withBaseChecks(ciCheck("CI", "rspec", passed)))
			reviewedSpec(h, prSpec{n: 2, head: "b1", base: tc.base, ci: tc.ci, checks: tc.checks})
			if tc.want != "" {
				h.wantRefused(2, tc.want)
				if got := h.autoRefused(2); got != tc.want {
					t.Fatalf("held back for %q, want exactly %q", got, tc.want)
				}
				return
			}
			if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q, want the approval of b1 (held back for %q)", got, h.autoRefused(2))
			}
		})
	}
}

// A required check with no run waits as checks that run do, in their
// class: the status comment says that the approval waits for the head's
// checks, magnum approves the head once the check ran there and passed,
// and a standing approval of an earlier head is not withdrawn for it.
func TestARequiredCheckWithNoRunWaitsAsChecksThatRun(t *testing.T) {
	passed := github.CheckPassed
	without := []github.Check{ciCheck("Pronto", "pronto", passed)}
	with := []github.Check{ciCheck("Pronto", "pronto", passed), ciCheck("CI", "Completion", passed)}
	h, _ := newAutoHarness(t, approvesAsAlice, withRequiredDispatch("Completion"))
	h.reviewedFor(prSpec{head: "b1", ci: "SUCCESS", checks: without})
	h.wantApprovalLine(aliceCIRuns)
	h.moveTo(prSpec{head: "b1", ci: "SUCCESS", checks: with})
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1 once Completion ran", got)
	}

	h.moveTo(prSpec{head: "b2", ci: "SUCCESS", checks: without})
	h.reviewTo(2, "b2")
	h.flush()
	h.flush()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("withdrawn while Completion has not run: %q", d)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.HeadSHA != "b1" {
		t.Fatalf("approval = %+v", a)
	}
}

// Without [[repo]] required_checks every check counts as before, the list
// GitHub's rulesets declare included (it changes the gate only where one of
// its checks has no run at the head): the 12036 layout is held for the
// unrequired job, and a running one waits.
func TestNoRequiredChecksKeepsEveryCheckCounting(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	layout := []github.Check{ciCheck("CI", "rspec", passed), ciCheck("CI", "Completion", passed), ciCheck("Terraform scripts", "lint", failed)}
	for _, tc := range []struct {
		name   string
		mods   []func(*harness)
		ci     string
		checks []github.Check
		want   string
	}{
		{name: "no list", ci: "FAILURE", checks: layout, want: ciFailsWords},
		{name: "GitHub's rulesets require CI", ci: "FAILURE", checks: layout, want: ciFailsWords,
			mods: []func(*harness){func(h *harness) { h.gh.required = map[string][]string{"talkable/talkable": {"Completion"}} }}},
		{name: "an unrequired check still runs", ci: "PENDING", want: ciRunsWords,
			checks: []github.Check{ciCheck("CI", "rspec", passed), ciCheck("Terraform scripts", "lint", pending)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, append([]func(*harness){withBaseChecks(ciCheck("CI", "rspec", passed))}, tc.mods...)...)
			reviewedWithChecks(h, tc.ci, tc.checks...)
			h.wantRefused(2, tc.want)
			if got := h.autoRefused(2); got != tc.want {
				t.Fatalf("held back for %q, want exactly %q", got, tc.want)
			}
		})
	}
}
