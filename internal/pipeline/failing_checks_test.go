package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// setCI stores ci as the PR's checks, as a Details fetch does, after the
// round's input was taken (e.pr has none).
func (e *env) setCI(ci store.CIStatus) {
	e.t.Helper()
	ci.Tally()
	if _, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.repo.ID, NodeID: "PR_11920", Number: 11920,
		URL: e.pr.URL, HeadSHA: target, CI: &ci}); err != nil {
		e.t.Fatalf("UpsertPRFromGitHub: %v", err)
	}
}

// A PR got LGTM and the operator's auto-approval while its RSpec check had
// failed on that head 25 minutes earlier, and the review's Checks did not
// mention it. The judge's prompt names failing-checks.json as
// `failing_checks` when the registry's checks are the head's and one
// failed: the failed ones only, read when the prompt goes out (not when the
// round started). Checks of another commit, a head whose checks all passed
// and a blind replay get none. No event carries a check's name: the PR's
// workflows name the checks.
func TestTheJudgeGetsTheHeadsFailingChecks(t *testing.T) {
	at := t0.Add(-25 * time.Minute)
	failed := store.CheckResult{Name: "RSpec", State: store.CheckFailed, Workflow: "CI", At: at}
	checks := []store.CheckResult{
		failed,
		{Name: "Lint", State: store.CheckPassed, Workflow: "CI", At: at},
		{Name: "deploy/preview", State: store.CheckPending},
	}

	e := newEnv(t)
	e.setCI(store.CIStatus{SHA: target, State: "FAILURE", Total: 3, Complete: true, Checks: checks})
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(821, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, FailingChecksFile)
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nfailing_checks: "+path+"\n")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got FailingChecks
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	want := FailingChecks{PR: "talkable/talkable#11920", HeadSHA: target, Complete: true, Checks: []store.CheckResult{failed}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %+v\nwant %+v", FailingChecksFile, got, want)
	}
	if evs := e.eventsOf("round.failing_checks"); len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "1 failing check") {
		t.Errorf("round.failing_checks events = %+v", evs)
	}
	for _, ev := range e.events() {
		if strings.Contains(ev.Message+string(ev.Data), "RSpec") {
			t.Errorf("event %s carries a check's name: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}

	none := func(name string, ci store.CIStatus, blind bool) {
		t.Helper()
		e := newEnv(t)
		e.setCI(ci)
		in := e.input(KindInitial)
		if blind {
			p := e.judgePosts(0, "", "COMMENT")
			p.gh, p.status = nil, "dry_run"
			e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
			in.Blind, in.DryRun = true, true
		} else {
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(822, "COMMENTED", "COMMENT").behavior(t)}
		}
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || (res.Outcome != OutcomePosted && res.Outcome != OutcomeDryRun) {
			t.Fatalf("%s: RunRound = %+v, %v", name, res, err)
		}
		if p := e.ag.submitsFor(agents.RoleJudge)[0].Text; strings.Contains(p, "failing_checks") {
			t.Errorf("%s: the judge prompt names failing checks:\n%s", name, p)
		}
		if _, err := os.Stat(filepath.Join(res.ReportDir, FailingChecksFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %s: %v, want none", name, FailingChecksFile, err)
		}
	}
	none("another commit's checks", store.CIStatus{SHA: head2, State: "FAILURE", Total: 3, Complete: true, Checks: checks}, false)
	none("the head's checks all passed", store.CIStatus{SHA: target, State: "SUCCESS", Total: 1, Complete: true, Checks: checks[1:2]}, false)
	none("a blind replay", store.CIStatus{SHA: target, State: "FAILURE", Total: 3, Complete: true, Checks: checks}, true)
}
