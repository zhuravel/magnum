package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// stagingAsk is what the fake StagingPlan asks of ref's envs.
func stagingAsk(ref string, envs []string) string {
	return "Deploy " + ref + "'s head abcdef1 to " + strings.Join(envs, " and ") + ": fixer-app[bot] merges it and pushes?"
}

func (f *fakeActions) StagingPlan(_ context.Context, ref string, envs []string) (ActionResult, error) {
	res, err := f.record("staging " + ref + " --deploy " + strings.Join(envs, ",") + " --dry-run")
	if err == nil {
		res.Ask = stagingAsk(ref, envs)
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
