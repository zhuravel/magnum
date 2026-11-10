package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/textx"
)

// ciFixAsk is what the fake FixCIPlan asks: the daemon's plan names the
// fixer, the failed check, the head, the failed jobs, the slot, the login
// and the count.
func ciFixAsk(ref string) string {
	return "Fix the failed CI of " + ref + ": claude-fix fixes required check Completion at abcdef1 (2 failed jobs) in review3 as fixer-app[bot], CI fix 1 of 3?"
}

// ciFixAsked is what the fake plan's question asked about.
var ciFixAsked = CIFixAsked{Head: "abcdef1234", Entries: []string{"Completion"}, Count: 0, At: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}

// ciFixCall is how the fakes record a CI fix of ref bound to a.
func ciFixCall(ref string, a CIFixAsked) string {
	return fmt.Sprintf("fix --ci %s bound to %s at %s, %d CI fixes, planned %s", ref, strings.Join(a.Entries, ","), textx.ShortSHA(a.Head), a.Count,
		a.At.UTC().Format("15:04"))
}

// errNothingFailed is the daemon's refusal of a CI fix where no required
// check failed, which the fake plan answers for a ref its refuse names.
var errNothingFailed = errors.New("nothing to fix: no required check failed at abcdef1")

func (f *fakeActions) FixCIPlan(_ context.Context, ref string) (ActionResult, error) {
	if f.ciRefuse == ref {
		return f.recordErr("fix --ci --dry-run "+ref, errNothingFailed)
	}
	res, err := f.record("fix --ci --dry-run " + ref)
	if err == nil {
		asked := ciFixAsked
		res.Ask, res.CIFix = ciFixAsk(ref), &asked
	}
	return res, err
}

func (f *fakeActions) FixCI(_ context.Context, ref string, asked CIFixAsked) (ActionResult, error) {
	return f.record(ciFixCall(ref, asked))
}

// recordErr records s and fails it with err.
func (f *fakeActions) recordErr(s string, err error) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return ActionResult{Text: "progress line\n" + err.Error()}, err
}

// c fixes the failed required CI of your own PR. Its y/N names what only
// the daemon knows (the failed check, the failed jobs, the count of CI
// fixes), so c first asks the daemon's plan in a dry run, puts it as the
// question, and only y starts the CI fix, bound to what the plan asked
// about.
func TestCFixesCIOnYourOwnPRWithTheDaemonsPlan(t *testing.T) {
	m, _, act := newBoard(t, 170, 40, PRBoardOptions{SelfLogins: boardSelf})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(func(r *PRBoardRow) { r.Author = "zhuravel" })}})
	m, _ = boardAct(t, m, "c")
	if !slices.Equal(act.calls, []string{"fix --ci --dry-run talkable/talkable#5"}) {
		t.Fatalf("c called %v, want the plan alone", act.calls)
	}
	if want := ciFixAsk("talkable/talkable#5"); m.confirm == nil || m.confirm.question != want {
		t.Fatalf("c asked %+v, want %q", m.confirm, want)
	}
	if n, _ := send(t, m, keyMsg("n")); n.confirm != nil || len(act.calls) != 1 {
		t.Errorf("n acted or left the question up: %v", act.calls)
	}
	m, _ = boardAct(t, m, "y")
	if !slices.Equal(act.calls, []string{"fix --ci --dry-run talkable/talkable#5", ciFixCall("talkable/talkable#5", ciFixAsked)}) {
		t.Errorf("y called %v, want the CI fix bound to its plan", act.calls)
	}
}

// c refuses another's PR before it asks the daemon anything, and a PR
// merged, in a round or in a fix loop; what only the daemon knows (that no
// required check failed: talkable's dispatched run is outside the rollup
// the board reads) is its refusal, shown with no question.
func TestCRefusesAnothersPRAndAPRWithNothingFailed(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(r *PRBoardRow)
		want string
	}{
		{"another's", func(r *PRBoardRow) { r.Author = "alice" }, "talkable#5 is alice's, not yours: magnum fixes the CI of your own PRs alone"},
		{"merged", func(r *PRBoardRow) { r.Author, r.GHState = "zhuravel", "MERGED" }, "talkable#5 is merged: nothing to fix"},
		{"a round", func(r *PRBoardRow) { r.Author, r.State = "zhuravel", "reviewing" }, "talkable#5: round in progress (reviewing); K kills it"},
		{"a fix loop", func(r *PRBoardRow) { r.Author, r.FixLoop = "zhuravel", true }, "talkable#5: a fix loop works on it; K stops it"},
	} {
		m, _, act := newBoard(t, 170, 40, PRBoardOptions{SelfLogins: boardSelf})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(c.edit)}})
		m, _ = boardAct(t, m, "c")
		if len(act.calls) != 0 || m.confirm != nil {
			t.Errorf("%s: c called %v, asked %+v", c.name, act.calls, m.confirm)
		}
		mustContain(t, viewOf(m), c.want)
	}
	m, _, act := newBoard(t, 170, 40, PRBoardOptions{SelfLogins: boardSelf})
	act.ciRefuse = "talkable/talkable#5"
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(func(r *PRBoardRow) { r.Author = "zhuravel" })}})
	m, _ = boardAct(t, m, "c")
	if m.confirm != nil || !slices.Equal(act.calls, []string{"fix --ci --dry-run talkable/talkable#5"}) {
		t.Fatalf("c asked %+v after the daemon refused (calls %v)", m.confirm, act.calls)
	}
	mustContain(t, viewOf(m), "nothing to fix: no required check failed")
}

// The dashboard's c is the board's: the plan, then its question, then the
// CI fix; the dashboard does not know the author, so the daemon decides.
func TestDashboardCFixesCIAfterThePlan(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
	m, _, act := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	m, _ = dashAct(t, m, "j", "j", "c")
	if want := ciFixAsk("talkable#7"); m.confirm == nil || m.confirm.question != want {
		t.Fatalf("c asked %+v (calls %v), want %q", m.confirm, act.calls, want)
	}
	if _, _ = dashAct(t, m, "y"); !slices.Equal(act.calls, []string{"fix --ci --dry-run talkable#7", ciFixCall("talkable#7", ciFixAsked)}) {
		t.Errorf("calls %v, want the plan and the CI fix bound to it", act.calls)
	}
}

// The help of the board and of the dashboard names c, the key no other
// row action takes.
func TestTheHelpNamesCForACIFix(t *testing.T) {
	if !slices.Contains(BoardHelpKeys(), "c / E") || !slices.Contains(DashboardHelpKeys(), "c") {
		t.Errorf("the board's help %q or the dashboard's %q leaves c out", BoardHelpKeys(), DashboardHelpKeys())
	}
	if rowActDefs[actFixCI].key != "c" {
		t.Errorf("the CI fix's key is %q", rowActDefs[actFixCI].key)
	}
}
