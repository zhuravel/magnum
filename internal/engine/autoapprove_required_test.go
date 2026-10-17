package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
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

// reviewedWithChecks opens PR #2 on head b1 with rollup ci and checks after
// a first poll, and lets magnum's clean review of it post.
func reviewedWithChecks(h *harness, ci string, checks ...github.Check) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: ci, checks: checks})
	h.tick()
	h.reviewTo(2, "b1")
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

// Without [[repo]] required_checks every check counts as before, the list
// GitHub's rulesets declare included (it does not change the gate): the
// 12036 layout is held for the unrequired job, and a running one waits. A
// list whose entry names no check on the head (one that never ran there)
// cannot say what the head's checks are, so every check counts there too.
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
		{name: "required_checks names no check on the head", ci: "FAILURE", checks: layout, want: ciFailsWords,
			mods: []func(*harness){withRequiredChecks("workflow:CI", "Deploy preview")}},
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
