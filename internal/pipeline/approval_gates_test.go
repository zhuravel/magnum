package pipeline

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// promptField is the value of the <magnum> field name in a judge prompt.
func promptField(t *testing.T, text, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `: (.*)$`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no %s in the judge prompt:\n%s", name, text)
	}
	return m[1]
}

// cleanJudge is a judge that finds nothing and posts as its prompt says, as
// the skill does: the event its no_findings_event names, which its
// post_review line must pass as --no-findings-event too (post-review then
// refuses an APPROVE where it is COMMENT). It records each prompt's event.
func (e *env) cleanJudge(t *testing.T, id int64, seen *[]string, mu *sync.Mutex) behavior {
	t.Helper()
	return func(f *fakeAgents, run store.Run, text string) error {
		ev := promptField(t, text, "no_findings_event")
		if line := promptField(t, text, "post_review"); !strings.Contains(line, " --no-findings-event "+ev+" ") {
			t.Errorf("post_review %q does not pass the prompt's no_findings_event %s", line, ev)
		}
		mu.Lock()
		*seen = append(*seen, ev)
		mu.Unlock()
		p := e.judgePosts(id, map[string]string{"APPROVE": "APPROVED", "COMMENT": "COMMENTED"}[ev], ev)
		if e.r.Identity.Kind() == "gh" {
			p.graphLogin, p.graphType, p.restLogin, p.restType = "zhuravel", "User", "zhuravel", "User"
		}
		p.body, p.findings = "No problems found. LGTM :shipit:", map[string]int{}
		return p.behavior(t)(f, run, text)
	}
}

// rollupOf is the rollup of pr's stored checks ("" = none).
func rollupOf(pr store.PR) string {
	if pr.CI == nil {
		return ""
	}
	return pr.CI.State
}

// approvingEnv is a round in a repository whose judge approves a clean
// review: as the App with the [[repo]] override, or as a gh identity, whose
// default is APPROVE.
func approvingEnv(t *testing.T, kind string) *env {
	t.Helper()
	e := newEnv(t)
	e.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", NoFindingsEvent: "APPROVE"}}
	if kind == "gh" {
		e.cfg.Repos = nil
		e.r.Identity = fakeIdentity{name: "zhuravel", login: "zhuravel", kind: "gh"}
	}
	return e
}

// A PR the approval's gates refuse at dispatch (spec 09: here one that
// changes its own workflows), reviewed clean, in a repository whose
// no-findings event is APPROVE, as the App and as a gh identity alike: the
// judge's prompt says COMMENT in no_findings_event and in its post_review
// line, with the reason in fixed words, the posted event is COMMENT, and
// the round's judge event says why. The same round without a refusal
// approves.
func TestTheApprovalsGatesTurnAJudgesCleanReviewIntoAComment(t *testing.T) {
	const why = "the PR changes its own workflows or actions"
	for _, kind := range []string{"app", "gh"} {
		for _, refused := range []bool{true, false} {
			e := approvingEnv(t, kind)
			var seen []string
			var mu sync.Mutex
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.cleanJudge(t, 931, &seen, &mu)}
			in := e.input(KindInitial)
			if refused {
				in.NoApproval = why
			}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("%s: RunRound = %+v, %v", kind, res, err)
			}
			prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
			judged := eventsOfKind(e.events(), "round.judge")
			if want := map[bool]string{true: why}[refused]; res.NoApproval != want {
				t.Fatalf("%s: the round's result says no approval %q, want %q (the needs-you list reads it)", kind, res.NoApproval, want)
			}
			if !refused {
				if len(seen) != 1 || seen[0] != "APPROVE" || res.Event != "APPROVED" || strings.Contains(prompt, "This round cannot approve") {
					t.Fatalf("%s without a refusal: events %v, posted %q: want APPROVE\n%s", kind, seen, res.Event, prompt)
				}
				continue
			}
			mustContain(t, kind+" judge prompt", prompt, "\nno_findings_event: COMMENT\n", "blocking_event: REQUEST_CHANGES",
				"This round cannot approve: "+why+", so a clean verdict posts as a comment", " --no-findings-event COMMENT ")
			if len(seen) != 1 || seen[0] != "COMMENT" || res.Event != "COMMENTED" {
				t.Fatalf("%s: the judge saw %v and posted %q: want COMMENT", kind, seen, res.Event)
			}
			if len(judged) != 1 || !strings.Contains(judged[0].Message, "; no APPROVE this round: "+why) || !strings.Contains(string(judged[0].Data), `"no_approval":"`+why+`"`) {
				t.Fatalf("%s: round.judge events %+v: want the reason", kind, judged)
			}
		}
	}
}

// The approval's CI gate decides when the judge's prompt is written, from
// the registry's read of the PR the failing checks come from (here stored
// after the round's input was taken, so the input's PR has no checks), of
// the round's target: red CI it refuses gives COMMENT, with its words in
// the prompt; checks that still run let the APPROVE through. The round's
// judge event records the CI state the gate decided by either way.
func TestTheCIGateDecidesWhenTheJudgesPromptIsWritten(t *testing.T) {
	for _, tc := range []struct {
		rollup, state, want string
	}{
		{"FAILURE", CIRed, "COMMENT"},
		{"PENDING", CIRunning, "APPROVE"},
	} {
		e := approvingEnv(t, "app")
		var seen []string
		var mu sync.Mutex
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.cleanJudge(t, 932, &seen, &mu)}
		in := e.input(KindInitial)
		var asked []string
		in.CIGate = func(_ context.Context, pr store.PR, target string) CIGateResult {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, rollupOf(pr)+"@"+target)
			if rollupOf(pr) == "FAILURE" {
				return CIGateResult{Refusal: "the head's checks fail", State: CIRed}
			}
			return CIGateResult{State: CIRunning}
		}
		e.setCI(store.CIStatus{SHA: target, State: tc.rollup, Total: 1, Complete: true,
			Checks: []store.CheckResult{{Name: "RSpec", Workflow: "CI", State: map[string]string{"FAILURE": store.CheckFailed, "PENDING": store.CheckPending}[tc.rollup]}}})
		if _, err := e.r.RunRound(e.ctx, in); err != nil {
			t.Fatalf("%s: RunRound: %v", tc.rollup, err)
		}
		if len(asked) != 1 || asked[0] != tc.rollup+"@"+target {
			t.Fatalf("%s: the gate was asked %v: want once, of the registry's checks of %s", tc.rollup, asked, target)
		}
		prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
		if len(seen) != 1 || seen[0] != tc.want || strings.Contains(prompt, "This round cannot approve") != (tc.want == "COMMENT") {
			t.Fatalf("%s: the judge was asked for %v, want %s\n%s", tc.rollup, seen, tc.want, prompt)
		}
		judged := eventsOfKind(e.events(), "round.judge")
		if len(judged) != 1 || !strings.Contains(string(judged[0].Data), `"ci_gate":"`+tc.state+`"`) {
			t.Fatalf("%s: round.judge events %+v: want ci_gate %s", tc.rollup, judged, tc.state)
		}
	}
}

// A paused round that continues decides the CI gate again when its
// prompt is written: the paused round's prompt approved on running checks,
// the head's CI turned red before the continue, and the continue's prompt
// asks for COMMENT in no_findings_event and in its post_review line alike
// (one decision renders both), and the judge posts a COMMENT.
func TestAContinueWhoseCIChangedDecidesTheCIGateAgain(t *testing.T) {
	e := approvingEnv(t, "app")
	var seen []string
	var mu sync.Mutex
	gate := func(_ context.Context, pr store.PR, _ string) CIGateResult {
		if rollupOf(pr) == "FAILURE" {
			return CIGateResult{Refusal: "the head's checks fail", State: CIRed}
		}
		return CIGateResult{State: CIRunning}
	}
	running := []store.CheckResult{{Name: "RSpec", Workflow: "CI", State: store.CheckPending}}
	e.setCI(store.CIStatus{SHA: target, State: "PENDING", Total: 1, Complete: true, Checks: running})
	paused := judgePost{status: "error", event: "APPROVE"}
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		mu.Lock()
		seen = append(seen, promptField(t, text, "no_findings_event"))
		mu.Unlock()
		return paused.behavior(t)(f, run, text)
	}, e.cleanJudge(t, 933, &seen, &mu)}
	first := e.input(KindInitial)
	first.CIGate = gate
	res, _ := e.r.RunRound(e.ctx, first)

	failed := []store.CheckResult{{Name: "RSpec", Workflow: "CI", State: store.CheckFailed, At: t0}}
	e.setCI(store.CIStatus{SHA: target, State: "FAILURE", Total: 1, Complete: true, Checks: failed})
	in := e.input(KindContinue)
	in.ContinueRunID, in.CIGate = res.JudgeRunID, gate
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.Event != "COMMENTED" || res.NoApproval != "the head's checks fail" {
		t.Fatalf("continued round = %+v, %v: want its COMMENT posted, the CI gate's refusal in its result", res, err)
	}
	if len(seen) != 2 || seen[0] != "APPROVE" || seen[1] != "COMMENT" {
		t.Fatalf("the prompts asked for %v: want APPROVE on running checks, then COMMENT on red ones", seen)
	}
	prompts := e.ag.submitsFor(agents.RoleJudge)
	cont := prompts[len(prompts)-1].Text
	mustContain(t, "continue prompt", cont, "mode: continue", "\nno_findings_event: COMMENT\n", " --no-findings-event COMMENT ",
		"This round cannot approve: the head's checks fail")
}

// A post-merge round and a self-authored one comment both ways whatever
// the gates say: their prompts name COMMENT for both events, say nothing
// of the gates, and never ask the CI gate.
func TestPostMergeAndSelfAuthoredRoundsKeepTheirCommentAndAskNoGate(t *testing.T) {
	for _, what := range []string{"post-merge", "self-authored"} {
		e := approvingEnv(t, "app")
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(934, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindInitial)
		in.NoApproval = "the PR changes its own workflows or actions"
		asked := false
		in.CIGate = func(context.Context, store.PR, string) CIGateResult {
			asked = true
			return CIGateResult{Refusal: "the head's checks fail", State: CIRed}
		}
		if what == "post-merge" {
			in.PostMerge = true
		} else {
			in.PR.AuthorLogin = new("zhuravel")
		}
		if _, err := e.r.RunRound(e.ctx, in); err != nil {
			t.Fatalf("%s: RunRound: %v", what, err)
		}
		prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
		mustContain(t, what+" prompt", prompt, "\nno_findings_event: COMMENT\n", "\nblocking_event: COMMENT\n", " --no-findings-event COMMENT ")
		if asked || strings.Contains(prompt, "This round cannot approve") {
			t.Fatalf("%s: gate asked %v, prompt:\n%s", what, asked, prompt)
		}
	}
}
