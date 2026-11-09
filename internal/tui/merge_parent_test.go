package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/zhuravel/magnum/internal/textx"
)

// mergePlanAsk is what the fake MergeParentPlan asks: the daemon's plan
// names the base, its tip, the branch and the bot's login.
func mergePlanAsk(ref string) string {
	return "Merge the parent of " + ref + ": magnum merges master at c0c0c0c into feature as fixer-app[bot], with no conflict?"
}

// mergePlanAsked is what the fake plan's question asked about: the PR's
// head, master at c0c0c0c, no conflict.
var mergePlanAsked = MergeAsked{Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Base: "master",
	Tip: "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0", At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}

// mergeCall is how the fakes record a merge of ref's parent bound to a:
// "merge-parent talkable/talkable#5 bound to master at c0c0c0c, head
// a1a1a1a, 0 conflicts, planned 10:00".
func mergeCall(ref string, a MergeAsked) string {
	return fmt.Sprintf("merge-parent %s bound to %s at %s, head %s, %d conflicts, planned %s", ref, a.Base, textx.ShortSHA(a.Tip),
		textx.ShortSHA(a.Head), a.Conflicts, a.At.UTC().Format("15:04"))
}

func (f *fakeActions) MergeParentPlan(_ context.Context, ref string) (ActionResult, error) {
	res, err := f.record("merge-parent --dry-run " + ref)
	if err == nil {
		asked := mergePlanAsked
		res.Ask, res.Parent = mergePlanAsk(ref), &asked
	}
	return res, err
}

func (f *fakeActions) MergeParent(_ context.Context, ref string, asked MergeAsked) (ActionResult, error) {
	return f.record(mergeCall(ref, asked))
}

func (a *fakeFactoryActions) MergeParentPlan(_ context.Context, ref string) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plans = append(a.plans, ref)
	asked := mergePlanAsked
	return ActionResult{Text: "dry run", Ask: mergePlanAsk(ref), Parent: &asked}, nil
}

func (a *fakeFactoryActions) MergeParent(_ context.Context, ref string, asked MergeAsked) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.merges = append(a.merges, mergeCall(ref, asked))
	return ActionResult{Text: "merging the parent of " + ref}, nil
}

// P merges the PR's parent as the fixer App's bot. Its y/N names who
// merges which tip of which base into which branch, and as whom, which only
// the daemon knows: P first asks it in a dry run (it changes nothing),
// then puts the daemon's plan as the question; only y sends the merge, and
// it sends it bound to what the plan asked about (the head, the base, its
// tip, the conflicts, when), which the daemon checks again: the bug was a y
// that bound nothing, so the daemon merged whatever tip the base had then.
func TestPAsksWithTheDaemonsPlanBeforeMergingTheParent(t *testing.T) {
	m, act := fixBoard(t, nil)
	m, _ = boardAct(t, m, "P")
	if !slices.Equal(act.calls, []string{"merge-parent --dry-run talkable/talkable#5"}) {
		t.Fatalf("P called %v, want the plan alone", act.calls)
	}
	want := mergePlanAsk("talkable/talkable#5")
	if m.confirm == nil || m.confirm.question != want {
		t.Fatalf("P asked %+v, want %q", m.confirm, want)
	}
	mustContain(t, viewOf(m), "y confirms, any other key cancels")
	if n, _ := send(t, m, keyMsg("n")); n.confirm != nil || len(act.calls) != 1 {
		t.Errorf("n acted or left the question up: %v", act.calls)
	}
	m, _ = boardAct(t, m, "y")
	if !slices.Equal(act.calls, []string{"merge-parent --dry-run talkable/talkable#5", mergeCall("talkable/talkable#5", mergePlanAsked)}) {
		t.Errorf("y called %v, want the merge bound to its plan", act.calls)
	}
	if m.confirm != nil {
		t.Errorf("a question is left up: %+v", m.confirm)
	}
}

// P refuses, before it asks the daemon anything, a PR that has nothing to
// merge into or a round in flight; a refusal of the daemon's plan shows as
// any failed action, with no question.
func TestPRefusesWhatHasNothingToMerge(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(r *PRBoardRow)
		want string
	}{
		{"merged", func(r *PRBoardRow) { r.GHState = "MERGED" }, "talkable#5 is merged: nothing to merge"},
		{"a round", func(r *PRBoardRow) { r.State = "reviewing" }, "talkable#5: round in progress (reviewing); K kills it"},
		{"a fix loop", func(r *PRBoardRow) { r.FixLoop = true }, "talkable#5: a fix loop works on it; K stops it"},
	} {
		m, act := fixBoard(t, c.edit)
		m, _ = boardAct(t, m, "P")
		if len(act.calls) != 0 || m.confirm != nil {
			t.Errorf("%s: P called %v, asked %+v", c.name, act.calls, m.confirm)
		}
		mustContain(t, viewOf(m), c.want)
	}
}

// f names the login the fixer pushes and replies as, not "you": a fixer
// App's bot since the App switch.
func TestFAsksAsTheFixersLogin(t *testing.T) {
	m, _ := fixBoard(t, func(r *PRBoardRow) { r.Fixer = "fixer-app[bot]" })
	m, _ = send(t, m, keyMsg("f"))
	if want := "Fix the findings of talkable#5 as fixer-app[bot]?"; m.confirm == nil || m.confirm.question != want {
		t.Fatalf("f asked %+v, want %q", m.confirm, want)
	}
}

// The dashboard's P is the board's: the plan, then its question, then the merge.
func TestDashboardPMergesTheParentAfterThePlan(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
	m, _, act := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	m, _ = dashAct(t, m, "j", "j", "P")
	if want := mergePlanAsk("talkable#7"); m.confirm == nil || m.confirm.question != want {
		t.Fatalf("P asked %+v (calls %v), want %q", m.confirm, act.calls, want)
	}
	if _, _ = dashAct(t, m, "y"); !slices.Equal(act.calls, []string{"merge-parent --dry-run talkable#7", mergeCall("talkable#7", mergePlanAsked)}) {
		t.Errorf("calls %v, want the plan and the merge bound to it", act.calls)
	}
}

// The factory screen's P on your PR's row asks the daemon's plan, puts it
// as the question, and only y merges, bound to the plan.
func TestFactoryPOnAnOwnPRRowMergesTheParentAfterThePlan(t *testing.T) {
	h := testYours(t, 140, 40)
	h.press("g")
	h.selectRow("#4602")
	h.press("P")
	mustContain(t, h.view(), mergePlanAsk("example/talkable#4602")+" y/N")
	h.press("n")
	h.act.mu.Lock()
	plans, merges := slices.Clone(h.act.plans), slices.Clone(h.act.merges)
	h.act.mu.Unlock()
	if !slices.Equal(plans, []string{"example/talkable#4602"}) || len(merges) != 0 {
		t.Fatalf("plans %v, merges %v after n", plans, merges)
	}
	h.press("P", "y")
	h.act.mu.Lock()
	merges = slices.Clone(h.act.merges)
	h.act.mu.Unlock()
	if !slices.Equal(merges, []string{mergeCall("example/talkable#4602", mergePlanAsked)}) {
		t.Errorf("merges %v, want the row's PR bound to its plan", merges)
	}
}

// Home's help names P beside f F K, and P on a row without a PR in
// magnum's registry starts nothing and says which keys act on one there.
func TestFactoryHomeNamesPInItsHelpAndOnARowWithoutAPR(t *testing.T) {
	h := testYours(t, 140, 40)
	home := helpSection(t, ansi.Strip(strings.Join(h.m.helpContent(240), "\n")), "Home")
	if !slices.ContainsFunc(home, func(l string) bool {
		return strings.HasPrefix(l, "f F K P ") && strings.Contains(l, "merge its parent") && strings.Contains(l, "y/N")
	}) {
		t.Errorf("the help's Home lacks f F K P with the merge of the parent:\n%s", strings.Join(home, "\n"))
	}
	h.press("g")
	h.selectRow("PR-12371")
	h.press("P")
	v := h.view()
	mustContain(t, v, "f, F, K and P act on a PR in magnum's registry: PR-12371 has none there")
	mustNotContain(t, v, "y/N")
}

// P's question on the board and the dashboard wraps over the footer's
// lines, as E's does, so that a narrow screen still shows all that the
// plan names (the tip, the bot, whether it conflicts) before y merges.
func TestPsQuestionShowsThePlanWholeOnANarrowScreen(t *testing.T) {
	whole := func(t *testing.T, view string, w int, want string) {
		t.Helper()
		for i, l := range strings.Split(view, "\n") {
			if ansi.StringWidth(l) > w {
				t.Errorf("%d columns: line %d is %d cells wide: %q", w, i, ansi.StringWidth(l), l)
			}
		}
		if got := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(view), "─", " ")), " "); !strings.Contains(got, want+" y/N") {
			t.Errorf("%d columns: the question is not shown whole, want %q in:\n%s", w, want, view)
		}
	}
	for _, w := range []int{80, 120} {
		m, _, act := newBoard(t, w, 40, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(nil)}})
		m, _ = boardAct(t, m, "P")
		if m.confirm == nil {
			t.Fatalf("%d columns: no question (calls %v)", w, act.calls)
		}
		whole(t, viewOf(m), w, mergePlanAsk("talkable/talkable#5"))
		if lines := strings.Split(viewOf(m), "\n"); len(lines) != 40 {
			t.Errorf("%d columns: the board is %d lines high, want 40", w, len(lines))
		}

		data := dashData()
		data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
		d, _, dact := newDash(t, w, 40)
		d, _ = send(t, d, dashDataMsg{data: data})
		d, _ = dashAct(t, d, "j", "j", "P")
		if d.confirm == nil {
			t.Fatalf("%d columns: no question on the dashboard (calls %v)", w, dact.calls)
		}
		whole(t, viewOf(d), w, mergePlanAsk("talkable#7"))
	}
}
