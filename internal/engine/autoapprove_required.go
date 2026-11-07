package engine

// A failing check the repository's required_checks does not name does not
// hold an automatic approval (DECISIONS "The approval's CI gate counts only
// the checks required_checks names"): on talkable#12036 a path-filtered
// workflow's job failed on a flake while the CI workflow, the only one the
// repository's `required_checks = ["workflow:CI"]` names, passed, and the
// gate held the reviewed PR for a day, since the base tip never runs that
// workflow. Where the PR's [[repo]] block sets required_checks the head's
// stored checks decide by the runs that list names (store.RequiredRuns, as
// the board reads them), not GitHub's combined rollup: a required run that
// fails goes to the base-tip comparison (failsLikeTheBase), one that runs
// waits, and the checks outside the list neither refuse nor wait; the
// approval's events and the daemon's log name the failing ones it ignored.
// An entry that names no check on a head whose checks magnum read whole
// holds it as a required run that has not finished (DECISIONS "A required
// check that did not run at the head is never green"): the repository's CI
// may run only when a PR opens, so a later push gets no run of it, and
// GitHub will not merge that head. Where the stored checks cannot say (none
// of the head's, a list GitHub cut at 100), and without the key, the rollup
// decides as before.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// requiredCI is what a head's stored checks say of the checks required_checks
// names (requiredCIOf).
type requiredCI struct {
	failed  []store.CheckResult // the required runs that failed
	running bool                // a required run has not finished
	// missing are the entries that name no check on the head: it never ran
	// there, or has not started yet.
	missing []string
	ignored []store.CheckResult // the failed checks no entry names
}

// requiredCIOf reads pr's stored checks against required, its repository's
// required checks; ok is false when they cannot decide and the rollup does,
// as without a list: no list, no checks stored for the head or not every
// one (GitHub lists 100), or checks an earlier build kept by workflow name
// (CIStatus.ByWorkflowFile: a PR's own workflow of the base's name, or a
// status, may have replaced a run there). An entry that names no check on
// the head is missing there (CIStatus: the checks list every one, so it has
// no run on the head), never passed. A run two entries name counts once.
func requiredCIOf(pr store.PR, required []string) (requiredCI, bool) {
	ci := pr.CI
	if len(required) == 0 || ci == nil || ci.SHA != pr.HeadSHA || !ci.Complete || !ci.ByWorkflowFile {
		return requiredCI{}, false
	}
	var r requiredCI
	counted := map[store.CheckKey]bool{} // the stored checks hold one run of each
	for _, entry := range required {
		runs := store.RequiredRuns(ci.Checks, entry)
		if len(runs) == 0 {
			r.missing = append(r.missing, entry)
			continue
		}
		for _, run := range runs {
			k := run.Key()
			if counted[k] {
				continue
			}
			counted[k] = true
			switch run.State {
			case store.CheckFailed:
				r.failed = append(r.failed, run)
			case store.CheckPending:
				r.running = true
			}
		}
	}
	for _, ch := range ci.Checks {
		if ch.State == store.CheckFailed && !slices.ContainsFunc(required, func(entry string) bool { return store.RequiredNames(entry, ch) }) {
			r.ignored = append(r.ignored, ch)
		}
	}
	return r, true
}

// requiredCIRefusal is the CI gate's answer (ciGateOf) for c by r, its
// head's required checks: a failing one goes to the base-tip comparison
// (failsLikeTheBase, which waits for a required one that still runs or has
// no run yet), one with no run on the head waits for it (ciMissingRefusal),
// a running one waits (ciRunsRefusal), and every one passed or skipped lets
// it through, whatever the checks outside the list do.
func (e *Engine) requiredCIRefusal(ctx context.Context, c store.RepoPR, r requiredCI) ciAnswer {
	switch {
	case len(r.failed) > 0:
		return e.failsLikeTheBase(ctx, c, r.failed, r.running || len(r.missing) > 0)
	case len(r.missing) > 0:
		return ciAnswer{why: ciMissingRefusal, waits: true, state: pipeline.CIRunning}
	case r.running:
		return ciAnswer{why: ciRunsRefusal, waits: true, state: pipeline.CIRunning}
	}
	return ciAnswer{state: pipeline.CIGreen}
}

// ignoredChecksShown is how many failing checks an ignored-checks note
// names; it counts the rest.
const ignoredChecksShown = 5

// ignoredChecksNote is what the approval of pr says of the failing checks
// outside its repository's required_checks that the gate ignored
// (requiredCIOf), "" for none: "ignored 1 failing check outside
// required_checks: Terraform scripts / lint". It names a check as the board
// does (its workflow, " / ", its name) when that is plain text
// (shownCheckName), at most ignoredChecksShown of them, and counts the rest
// ("and 2 more"): a workflow a PR adds names its own checks. A check the
// repository's ignore_checks names is no failing check here either, as the
// gate reads them (seenPR): the note never names it.
func (e *Engine) ignoredChecksNote(full string, pr store.PR) string {
	r, ok := requiredCIOf(e.seenPR(full, pr), seenRequired(pr, e.cfg.RequiredChecks(full), e.cfg.IgnoreChecks(full)))
	if !ok || len(r.ignored) == 0 {
		return ""
	}
	var shown []string
	for _, ch := range r.ignored {
		name := ch.Name
		if ch.Workflow != "" {
			name = ch.Workflow + " / " + ch.Name
		}
		if len(shown) < ignoredChecksShown && shownCheckName(name) {
			shown = append(shown, name)
		}
	}
	note := "ignored " + textx.Count(len(r.ignored), "failing check", "failing checks") + " outside required_checks"
	if len(shown) == 0 {
		return note
	}
	note += ": " + strings.Join(shown, ", ")
	if rest := len(r.ignored) - len(shown); rest > 0 {
		note += fmt.Sprintf(" and %d more", rest)
	}
	return note
}

// shownCheckName reports whether name, a check's workflow and name, is
// plain text an event or the log may carry: ASCII letters, digits, spaces
// and "._-/+@:()[],#", at most 120 bytes, not blank.
func shownCheckName(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > 120 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune(" ._-/+@:()[],#", r):
		default:
			return false
		}
	}
	return true
}
