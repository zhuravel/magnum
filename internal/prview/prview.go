// Package prview is a PR of magnum's registry as magnum's screens draw it:
// the head's CI (CI) and the findings of magnum's last posted review
// (Findings, Held), so the PR board's CI and FINDINGS columns and the
// factory screen's say the same of a PR. It reads the registry and writes
// nothing; the painters are internal/tui's.
package prview

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// CI is a PR's CIInfo at head, by its repository's required checks req
// (store.Store.RequiredChecks): ci is the head's checks as magnum's readers
// see them (store.SeenCI, the checks ignore_checks names left out); nil
// when magnum has no checks for the PR yet. A required check is matched by
// name across workflows (store.RequiredRuns), one that never ran on the
// head is "missing", and the checks of an older commit than head leave
// every required check that did not fail pending. The failed checks keep
// their pages (Links), and GitHub's merge state of the read says, with m,
// why it blocks the merge (Blocked, mergeBlock); it never makes a check
// green.
func CI(ci *store.CIStatus, head string, req store.RequiredChecks, m Merge) *tui.CIInfo {
	if ci == nil {
		return nil
	}
	out := &tui.CIInfo{Total: ci.Total, Passed: ci.Passed, Failed: ci.Failed, Pending: ci.Pending, Skipped: ci.Skipped,
		Stale: ci.SHA != "" && head != "" && ci.SHA != head, RequiredSource: req.Source}
	out.State = ciState(ci.Failed, ci.Pending, ci.AllSkipped, len(ci.Checks))
	byWorkflow := map[string]*tui.WorkflowCI{}
	var order []string
	for _, c := range ci.Checks {
		if c.State == store.CheckFailed {
			name := c.Name
			if c.Workflow != "" {
				name = c.Workflow + " / " + c.Name
			}
			out.Failing = append(out.Failing, name)
			if c.URL != "" {
				out.Links = append(out.Links, tui.CheckLink{Name: name, URL: c.URL})
			}
		}
		w, ok := byWorkflow[c.Workflow]
		if !ok {
			w = &tui.WorkflowCI{Name: c.Workflow}
			byWorkflow[c.Workflow] = w
			order = append(order, c.Workflow)
		}
		w.Total++
		switch c.State {
		case store.CheckPassed:
			w.Passed++
		case store.CheckFailed:
			w.Failed++
		case store.CheckPending:
			w.Pending++
		}
	}
	slices.Sort(order)
	for _, name := range order {
		w := byWorkflow[name]
		w.State = ciState(w.Failed, w.Pending, w.Passed+w.Failed+w.Pending == 0, w.Total)
		out.Workflows = append(out.Workflows, *w)
	}
	for _, pattern := range req.Checks {
		c := requiredCheck(ci.Checks, pattern)
		if out.Stale && c.State != store.CheckFailed {
			// The checks are an older commit's: the head's are not known yet.
			c.State, c.Done, c.Total = store.CheckPending, 0, 0
		}
		out.Required = append(out.Required, c)
	}
	out.Blocked = mergeBlock(ci.Merge, out, m)
	return out
}

// mergeBlock is why GitHub blocks the merge, in words, as GitHub's merge
// state (store.CIStatus.Merge) of out's read says it: a conflict, a branch
// behind its base, or a block (BLOCKED, DRAFT) that magnum's facts explain
// as far as they go (a draft, the required checks that failed, did not
// run or have not finished, the reviews GitHub's gate still wants), else
// a rule magnum does not read (conversations to resolve, a deployment,
// signed commits: what the PR's merge box lists); "" for a merge GitHub
// does not block or has not computed (UNKNOWN), and for an older commit's
// read.
func mergeBlock(state string, out *tui.CIInfo, m Merge) string {
	if out.Stale {
		return ""
	}
	var why []string
	switch strings.ToUpper(state) {
	case "DIRTY":
		why = []string{"it conflicts with its base"}
	case "BEHIND":
		why = []string{"its branch is behind its base"}
	case "BLOCKED", "DRAFT":
		if m.Draft || strings.EqualFold(state, "DRAFT") {
			why = append(why, "a draft")
		}
		for _, s := range []struct{ state, one, many string }{
			{store.CheckFailed, "failed", "failed"},
			{"missing", "did not run", "did not run"},
			{store.CheckPending, "has not finished", "have not finished"},
		} {
			var names []string
			for _, r := range out.Required {
				if r.State == s.state {
					names = append(names, cmp.Or(r.Label, r.Name))
				}
			}
			if len(names) > 0 {
				why = append(why, andList(names)+" "+textx.Plural(len(names), s.one, s.many))
			}
		}
		switch m.Decision {
		case "REVIEW_REQUIRED":
			why = append(why, "a required approval is missing")
		case "CHANGES_REQUESTED":
			why = append(why, "changes are requested")
		}
		if len(why) == 0 {
			why = []string{"a rule magnum does not read"}
		}
	default:
		return ""
	}
	return "GitHub blocks the merge: " + strings.Join(why, ", ")
}

// andList joins names as a sentence does: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Merge is what CI reads of a PR beside its checks to say why GitHub
// blocks its merge (tui.CIInfo.Blocked): whether it is a draft, and
// GitHub's review decision (store.ReviewGate.Decision: APPROVED,
// CHANGES_REQUESTED or REVIEW_REQUIRED; "" when its base requires no
// review or magnum has not read the gate).
type Merge struct {
	Draft    bool
	Decision string
}

// ciState folds counts into a board state: a failure wins, then anything
// running, then "every check skipped" (nothing ran), then none or passed.
func ciState(failed, pending int, allSkipped bool, checks int) string {
	switch {
	case failed > 0:
		return "failed"
	case pending > 0:
		return "pending"
	case checks == 0:
		return "none"
	case allSkipped:
		return "skipped"
	}
	return "passed"
}

// requiredCheck is one required check on the head: a name glob
// ("Completion", "rspec*") matched across workflows, the latest check run
// of each matching name counting and, apart, the latest commit status of
// it, or "workflow:<glob>" for every check of the workflows whose file or
// declared name it matches (store.RequiredRuns). State "missing" when
// nothing matched.
func requiredCheck(checks []store.CheckResult, pattern string) tui.CheckState {
	out := tui.CheckState{Name: pattern, Label: CheckLabel(pattern)}
	matched := store.RequiredRuns(checks, pattern)
	out.Total = len(matched)
	if out.Total == 0 {
		out.State = "missing"
		return out
	}
	var failed, pending, skipped int
	for _, c := range matched {
		switch c.State {
		case store.CheckFailed:
			failed++
		case store.CheckPending:
			pending++
		case store.CheckSkipped:
			skipped++
		}
	}
	out.Done = out.Total - pending
	switch {
	case failed > 0:
		out.State = store.CheckFailed
	case pending > 0:
		out.State = store.CheckPending
	case skipped == len(matched):
		out.State = store.CheckSkipped
	default:
		out.State = store.CheckPassed
	}
	return out
}

// CheckLabel is a required check's short name on the screens: without the
// "workflow:" prefix, and a glob without its trailing wildcards and the
// separators before them ("ci / *" → "ci", "rspec*" → "rspec"); the pattern
// itself when nothing would be left ("*").
func CheckLabel(pattern string) string {
	s := strings.TrimPrefix(pattern, store.RequiredWorkflowPrefix)
	if t := strings.TrimRight(s, "*?"); t != s {
		s = strings.TrimRight(t, " /:-_")
	}
	if s == "" {
		return pattern
	}
	return s
}

// Findings is magnum's posted review s as the FINDINGS columns say it;
// held says the review held its approval (Held).
func Findings(s store.ReviewSummary, held bool) *tui.FindingsInfo {
	return &tui.FindingsInfo{Counts: s.Counts, Simplifications: s.Simplifications, Fixed: s.Fixed, Open: s.Open,
		OpenProblems: s.OpenProblems(), OpenCounts: s.OpenCounts, Answered: s.Answered, Verdict: s.Verdict, Posted: s.Event,
		SHA: s.SHA, Held: held}
}

// Held reports whether s, PR prID's latest posted review, held its
// approval ([pipeline] approve_after_full_pass): the approval hold record
// (engine.KVPRApprovalHold) names that very review, as the engine's
// heldReviewOf requires; one it left behind for an older review holds
// nothing. Only a clean verdict holds, so no other reads the record; a
// clean comment without one is a comment-only repository's. A registry
// that cannot say holds nothing.
func Held(ctx context.Context, st *store.Store, prID int64, s store.ReviewSummary) bool {
	if s.Verdict != store.VerdictClean || s.ReviewID == 0 {
		return false
	}
	v, ok, err := st.GetKV(ctx, engine.KVPRApprovalHold(prID))
	if err != nil || !ok {
		return false
	}
	id, ok := engine.HeldReviewID(v)
	return ok && id == s.ReviewID
}
