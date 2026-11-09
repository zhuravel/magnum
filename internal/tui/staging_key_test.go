package tui

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// stagingAsk is what the fake StagingPlan asks of ref's envs.
func stagingAsk(ref string, envs []string) string {
	return "Deploy " + ref + "'s head abcdef1 to " + strings.Join(envs, " and ") + ": fixer-app[bot] merges it and pushes?"
}

func (f *fakeActions) StagingPlan(_ context.Context, ref string, envs []string) (ActionResult, error) {
	res, err := f.record("staging " + ref + " --deploy " + strings.Join(envs, ",") + " --dry-run")
	if err == nil {
		res.Ask = cmp.Or(f.ask, stagingAsk(ref, envs))
		res.Staging = &StagingAsked{Envs: envs, Head: "abcdef1234", Seen: []StagingSeen{{Env: envs[0], Migrations: []string{"db/migrate/1_a.rb"}}}}
	}
	return res, err
}

func (f *fakeActions) StagingDeploy(_ context.Context, ref string, a StagingAsked) (ActionResult, error) {
	return f.record("staging " + ref + " --deploy " + strings.Join(a.Envs, ",") + " at " + a.Head + " seen " + a.Seen[0].Migrations[0])
}

// stagedPR is actPR on a repository with void, bart and bastion.
func stagedPR(r *PRBoardRow) {
	r.Stagings = []StagingInfo{{Env: "void", State: "latest", InTip: "abcdef1234"}, {Env: "bart", State: "none"}, {Env: "bastion", State: "unknown",
		Why: "not read yet"}}
}

// E opens a chooser of the PR's staging envs, each with its state: 1, 2
// and 3 toggle one, a toggles them all, any other key but enter cancels.
// enter asks the daemon's plan of the chosen envs (a dry run) and puts it
// as one y/N; only y deploys, bound to the head and what the plan named.
func TestEChoosesTheStagingEnvsThenAsksWithTheDaemonsPlan(t *testing.T) {
	m, act := fixBoard(t, stagedPR)
	m, _ = boardAct(t, m, "E")
	if m.choose == nil || len(act.calls) != 0 {
		t.Fatalf("E opened no chooser, or called %v", act.calls)
	}
	v := viewOf(m)
	mustContain(t, v, "Deploy talkable#5 to", "1 [ ] void", "live", "2 [ ] bart", "not there", "3 [ ] bastion", "unknown", "a all", "enter asks")
	m, _ = boardAct(t, m, "enter")
	if m.choose == nil || len(act.calls) != 0 {
		t.Fatalf("enter with no env chosen closed the chooser or called %v", act.calls)
	}
	mustContain(t, viewOf(m), "choose an env first")
	m, _ = boardAct(t, m, "1", "3")
	mustContain(t, viewOf(m), "1 [x] void", "2 [ ] bart", "3 [x] bastion")
	m, _ = boardAct(t, m, "a")
	mustContain(t, viewOf(m), "1 [x] void", "2 [x] bart", "3 [x] bastion")
	m, _ = boardAct(t, m, "a", "2")
	mustContain(t, viewOf(m), "1 [ ] void", "2 [x] bart", "3 [ ] bastion")
	m, _ = boardAct(t, m, "1", "enter")
	if !slices.Equal(act.calls, []string{"staging talkable/talkable#5 --deploy void,bart --dry-run"}) {
		t.Fatalf("enter called %v, want the plan of void and bart alone", act.calls)
	}
	if want := stagingAsk("talkable/talkable#5", []string{"void", "bart"}); m.confirm == nil || m.confirm.question != want {
		t.Fatalf("asked %+v, want %q", m.confirm, want)
	}
	if n, _ := send(t, m, keyMsg("n")); n.confirm != nil || len(act.calls) != 1 {
		t.Errorf("n deployed or left the question up: %v", act.calls)
	}
	m, _ = boardAct(t, m, "y")
	if want := "staging talkable/talkable#5 --deploy void,bart at abcdef1234 seen db/migrate/1_a.rb"; len(act.calls) != 2 || act.calls[1] != want {
		t.Fatalf("y called %v, want %q", act.calls, want)
	}

	m, act = fixBoard(t, stagedPR)
	m, _ = boardAct(t, m, "E", "1", "x")
	if m.choose != nil || len(act.calls) != 0 {
		t.Fatalf("another key left the chooser up or called %v", act.calls)
	}
	mustContain(t, viewOf(m), "staging deploy cancelled")
}

// E refuses, before any chooser, a PR whose repository has no staging envs
// and a PR that is no longer open; the help names E.
func TestERefusesWhatHasNoStagingToDeployTo(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(r *PRBoardRow)
		want string
	}{
		{"no stagings", nil, "talkable#5's repository has no staging envs"},
		{"merged", func(r *PRBoardRow) { stagedPR(r); r.GHState = "MERGED" }, "talkable#5 is merged: nothing to deploy"},
	} {
		m, act := fixBoard(t, c.edit)
		m, _ = boardAct(t, m, "E")
		if m.choose != nil || len(act.calls) != 0 {
			t.Errorf("%s: E opened a chooser or called %v", c.name, act.calls)
		}
		mustContain(t, viewOf(m), c.want)
	}
	m, _ := fixBoard(t, stagedPR)
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "deploy to staging (choose, y/N)")
}

// The dashboard's E is the board's: the chooser of the row's envs, the
// plan, its question, then the deploy.
func TestDashboardEDeploysAfterTheChooserAndThePlan(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
	data.Queue[0].Stagings = []StagingInfo{{Env: "void", State: "none"}, {Env: "bart", State: "latest"}}
	m, _, act := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	m, _ = dashAct(t, m, "j", "j", "E")
	if m.choose == nil {
		t.Fatalf("E on the dashboard opened no chooser (calls %v)", act.calls)
	}
	mustContain(t, viewOf(m), "1 [ ] void", "2 [ ] bart")
	m, _ = dashAct(t, m, "1", "enter")
	if want := stagingAsk("talkable#7", []string{"void"}); m.confirm == nil || m.confirm.question != want {
		t.Fatalf("asked %+v (calls %v), want %q", m.confirm, act.calls, want)
	}
	if _, _ = dashAct(t, m, "y"); len(act.calls) != 2 || !strings.HasPrefix(act.calls[1], "staging talkable#7 --deploy void at abcdef1234") {
		t.Errorf("calls %v, want the plan and the deploy", act.calls)
	}
}

// planQuestion is a staging deploy's y/N as the daemon's plan words it
// (the cli's StagingQuestion): two envs, the migration Jenkins then runs
// on both, an env left out, and the warning of the default branch's CI,
// which comes last.
const planQuestion = "Deploy talkable#5's head 398a935 to void and bastion: fixer-app[bot] merges it into staging-void and " +
	"staging-bastion and pushes; Jenkins then runs db/migrate/20261105_add_coupons.rb on void's and bastion's shared databases; " +
	"bart is left out (already there: staging-bart holds PR #5's head); warning: master's CI is running at 1a2b3c4: a production " +
	"deploy may be building, and every staging build slows it?"

// mustShowTheWholeQuestion checks that view (w cells wide) shows all of
// planQuestion, wrapped over the lines above the footer's last, which ends
// in y/N: the migrations and the warning are on the screen when y is
// pressed.
func mustShowTheWholeQuestion(t *testing.T, view string, w int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("line %d is %d cells wide, past %d: %q", i, ansi.StringWidth(l), w, l)
		}
	}
	if len(lines) < 2 || !strings.Contains(lines[len(lines)-2], "y/N") {
		t.Fatalf("the footer's last line does not end the question:\n%s", view)
	}
	// The question's lines read back as one: the rule's glyphs and the
	// keys dropped, a line broken after a hyphen joined without a space.
	asked := ""
	for _, l := range lines[:len(lines)-1] {
		if asked == "" && !strings.Contains(l, "Deploy talkable#5's head") {
			continue
		}
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "─"))
		l, _, _ = strings.Cut(l, "  y confirms")
		if asked != "" && !strings.HasSuffix(asked, "-") {
			asked += " "
		}
		asked += l
	}
	if asked != planQuestion+" y/N" {
		t.Errorf("the screen asks\n%q\nwant\n%q\n%s", asked, planQuestion+" y/N", view)
	}
}

// E's y/N says all the daemon's plan names, at the widths the screens run
// at: on the board and the dashboard its words wrap over the footer's
// lines instead of being cut at the screen's edge, "y/N" ending the last.
func TestEsQuestionShowsTheMigrationsAndTheWarningWhole(t *testing.T) {
	for _, w := range []int{100, 120, 170} {
		m, _, act := newBoard(t, w, 40, PRBoardOptions{})
		act.ask = planQuestion
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(stagedPR)}})
		m, _ = boardAct(t, m, "E", "1", "enter")
		if m.confirm == nil {
			t.Fatalf("%d columns: no question (calls %v)", w, act.calls)
		}
		mustShowTheWholeQuestion(t, viewOf(m), w)
		if lines := strings.Split(viewOf(m), "\n"); len(lines) != 40 {
			t.Errorf("%d columns: the board is %d lines high, want 40", w, len(lines))
		}

		data := dashData()
		data.Queue[0].State, data.Queue[0].Next = "reviewed", "watching for new pushes"
		data.Queue[0].Stagings = []StagingInfo{{Env: "void", State: "none"}, {Env: "bart", State: "latest"}}
		d, _, dact := newDash(t, w, 40)
		dact.ask = planQuestion
		d, _ = send(t, d, dashDataMsg{data: data})
		d, _ = dashAct(t, d, "j", "j", "E", "1", "enter")
		if d.confirm == nil {
			t.Fatalf("%d columns: no question on the dashboard (calls %v)", w, dact.calls)
		}
		mustShowTheWholeQuestion(t, viewOf(d), w)
	}
}

// E's chooser keeps every env and its state on the footer, at a real
// label and the board's narrower widths: the key hint shortens, then goes,
// before an env is cut, and the line is measured in cells.
func TestEsChooserShowsEveryEnvBeforeItsKeys(t *testing.T) {
	c := &stagingChoice{label: "talkable#12212", envs: []StagingInfo{{Env: "void", State: "outdated"}, {Env: "bart", State: "pushed"},
		{Env: "bastion", State: "none"}}, on: make([]bool, 3)}
	for _, w := range []int{96, 116, 136, 166} {
		line := c.line(defaultStyles, w)
		if got := ansi.StringWidth(line); got > w {
			t.Errorf("%d cells: the chooser is %d wide", w, got)
		}
		plain := ansi.Strip(line)
		mustContain(t, plain, "1 [ ] void an older head", "2 [ ] bart pushed", "3 [ ] bastion not there")
		if w >= 116 && !strings.Contains(plain, "enter") {
			t.Errorf("%d cells: no key hint in %q", w, plain)
		}
	}
	if plain := ansi.Strip(c.line(defaultStyles, 166)); !strings.Contains(plain, "1-3 toggle, a all, enter asks, other keys cancel") {
		t.Errorf("a wide footer cuts the hint: %q", plain)
	}
	m, _, _ := newBoard(t, 120, 40, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(stagedPR)}})
	m, _ = boardAct(t, m, "E")
	mustContain(t, viewOf(m), "1 [ ] void live", "2 [ ] bart not there", "3 [ ] bastion unknown")
}
