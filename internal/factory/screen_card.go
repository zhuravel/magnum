package factory

// A PR's own lines on its card (tui.CardPR), your PR's or a published
// task's: what the PR is, as magnum's registry tells it, beside what it
// waits on. Words only: the CI, findings and approvals cells are the
// board's, which the painter draws from the same data as home's columns.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// cardPR is open PR st's lines on its card at c; nil for a PR magnum's
// registry does not have or that is no longer open.
func cardPR(st PRState, c prClock) *tui.CardPR {
	if !st.Found || st.GHState != store.GHOpen {
		return nil
	}
	cells := cellsOf(st)
	return &tui.CardPR{State: prLine(st, c), CI: cells.ci, Review: reviewLine(st, c), Findings: cells.findings,
		Approvals: cells.approvals, Reviewers: reviewersLine(st, c), Threads: threadsLine(st), FixLoop: fixLoopLine(st, c)}
}

// prLine is the PR line: open or a draft, its head, its branches and its
// last activity ("open · head a1b2c3d · zhuravel-coupon → master · last
// activity 08:15").
func prLine(st PRState, c prClock) string {
	parts := []string{"open"}
	if st.Draft {
		parts[0] = "a draft"
	}
	if st.Head != "" {
		parts = append(parts, "head "+textx.ShortSHA(st.Head))
	}
	switch {
	case st.HeadRef != "" && st.BaseRef != "":
		parts = append(parts, st.HeadRef+" → "+st.BaseRef)
	case st.BaseRef != "":
		parts = append(parts, "base "+st.BaseRef)
	}
	if !st.ActivityAt.IsZero() {
		parts = append(parts, "last activity "+when(st.ActivityAt, c.now))
	}
	return strings.Join(parts, " · ")
}

// reviewLine is magnum's last posted review of st in words: what it posted
// at which head and when, what it kept of the earlier findings, and that
// the head moved since; "" for none.
func reviewLine(st PRState, c prClock) string {
	r := st.Review
	if r == nil {
		return ""
	}
	verb := "magnum reviewed"
	switch r.Event {
	case "APPROVE", "APPROVED":
		verb = "magnum approved"
	case "REQUEST_CHANGES", "CHANGES_REQUESTED":
		verb = "magnum asked for changes"
	case "COMMENT", "COMMENTED":
		verb = "magnum commented"
	}
	line := verb + " at " + textx.ShortSHA(r.SHA)
	if !r.At.IsZero() {
		line += ", " + when(r.At, c.now)
	}
	var earlier []string
	for _, e := range []struct {
		n    int
		what string
	}{{r.Fixed, "fixed"}, {r.Open, "open"}, {r.Answered, "answered"}} {
		if e.n > 0 {
			earlier = append(earlier, fmt.Sprintf("%d %s", e.n, e.what))
		}
	}
	if len(earlier) > 0 {
		line += " · earlier findings: " + strings.Join(earlier, ", ")
	}
	if r.Simplifications > 0 {
		line += " · " + plural(r.Simplifications, "simplification")
	}
	if r.SHA != st.Head {
		line += " · the head moved since"
	}
	return line
}

// reviewersLine is the people's reviews of st in words: who approved
// (marking an approval of an older commit), who asked for changes, and
// whom it asks with how long ago; "" before magnum read the gate and with
// nobody to name.
func reviewersLine(st PRState, c prClock) string {
	var parts []string
	if g := st.Gate; g != nil {
		var approved, changes []string
		for _, o := range g.Opinions {
			switch {
			case o.Login == "":
			case o.State == "APPROVED" && o.CommitSHA != st.Head:
				approved = append(approved, o.Login+" (an older commit)")
			case o.State == "APPROVED":
				approved = append(approved, o.Login)
			case o.State == "CHANGES_REQUESTED":
				changes = append(changes, o.Login)
			}
		}
		if len(approved) > 0 {
			parts = append(parts, "approved by "+andList(approved))
		}
		if len(changes) > 0 {
			parts = append(parts, "changes asked by "+andList(changes))
		}
	}
	if len(st.Asked) > 0 {
		names := make([]string, len(st.Asked))
		for i, a := range st.Asked {
			names[i] = a.Who
			if !a.At.IsZero() {
				names[i] += " " + shortAge(max(c.now.Sub(a.At), 0)) + " ago"
			}
		}
		parts = append(parts, "asked "+andList(names))
	}
	return strings.Join(parts, " · ")
}

// threadsLine is the replies to magnum's reviews its judge has not read,
// by whom (each once, in order, marked when one is in magnum's thread);
// "" for none.
func threadsLine(st PRState) string {
	if len(st.Replies) == 0 {
		return ""
	}
	var who []string
	thread := map[string]bool{}
	for _, r := range st.Replies {
		by := cmp.Or(r.By, "a ghost")
		if !slices.Contains(who, by) {
			who = append(who, by)
		}
		thread[by] = thread[by] || r.Thread
	}
	for i, w := range who {
		if thread[w] {
			who[i] += " (in magnum's thread)"
		}
	}
	return textx.Count(len(st.Replies), "reply", "replies") + " magnum's judge has not read: " + andList(who)
}

// fixLoopLine is magnum's fix loop on st: the loop at work (its cycle or
// re-review, its fixer and since when, and K), fix off (F turns it on),
// else what the watch's autofix does with its findings; "" without a
// watch.
func fixLoopLine(st PRState, c prClock) string {
	switch {
	case st.Fixing:
		what := "fixing"
		switch {
		case st.Fix.Phase == engine.FixPhaseReviewing:
			what = "re-review of " + textx.ShortSHA(st.Fix.Head)
		case st.Fix.Cycle > 0:
			what = fmt.Sprintf("cycle %d of %d", st.Fix.Cycle, st.Fix.Cap)
		}
		if st.Fix.Role != "" {
			what += " by " + st.Fix.Role
		}
		if !st.Fix.StartedAt.IsZero() {
			what += " since " + when(st.Fix.StartedAt, c.now)
		}
		return what + " · K stops it"
	case st.FixOff:
		return "off for this PR · F turns it on"
	case st.Autofix == config.AutofixOwn || st.Autofix == config.AutofixOpen:
		return fmt.Sprintf("on: magnum's fixer takes its findings (autofix %q) · F turns it off", st.Autofix)
	case st.Autofix == config.AutofixNever:
		return fmt.Sprintf("magnum fixes nothing by itself (autofix %q) · f fixes what is open", st.Autofix)
	}
	return ""
}
