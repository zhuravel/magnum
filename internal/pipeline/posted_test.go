package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// DeletePendingReview makes fakeGitHub a ReviewDeleter: it removes a
// pending review and refuses any other (as GitHub does).
func (g *fakeGitHub) DeletePendingReview(ctx context.Context, owner, repo string, number int, reviewID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleted = append(g.deleted, reviewID)
	i := slices.IndexFunc(g.reviews, func(r github.Review) bool { return r.DatabaseID == reviewID })
	if i < 0 || !isPending(g.reviews[i].State) {
		return fmt.Errorf("review %d: 422 can only delete pending reviews", reviewID)
	}
	g.reviews = slices.Delete(g.reviews, i, i+1)
	return nil
}

// ReviewComments makes fakeGitHub a ReviewCommentLister.
func (g *fakeGitHub) ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]github.ReviewComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.comments[reviewID], nil
}

var (
	_ ReviewDeleter       = (*fakeGitHub)(nil)
	_ ReviewCommentLister = (*fakeGitHub)(nil)
	_ ReviewDeleter       = (*github.Client)(nil)
	_ ReviewCommentLister = (*github.Client)(nil)
)

// plainGitHub hides fakeGitHub's optional methods.
type plainGitHub struct{ GitHub }

// postTwice scripts a judge that posts review first, then a second
// submitted review and a pending draft, all carrying the run's marker.
func (e *env) postTwice(t *testing.T, first, second, pending int64) behavior {
	p := e.judgePosts(first, "CHANGES_REQUESTED", "REQUEST_CHANGES")
	p.keepWorking = true
	post := p.behavior(t)
	return func(f *fakeAgents, run store.Run, text string) error {
		if err := post(f, run, text); err != nil {
			return err
		}
		body := fmt.Sprintf("**Verdict** findings\n<!-- magnum:run=%s head=%s -->", markerRunID(t, text), target[:7])
		e.gh.add(github.Review{DatabaseID: second, State: "CHANGES_REQUESTED", Body: body, SubmittedAt: f.st.Clock(), CommitOid: target,
			AuthorLogin: "talkable", AuthorType: "Bot"},
			github.RESTReview{ID: second, UserLogin: "talkable[bot]", UserType: "Bot", State: "CHANGES_REQUESTED", SubmittedAt: f.st.Clock(), CommitID: target})
		if pending > 0 {
			e.gh.add(github.Review{DatabaseID: pending, State: "PENDING", Body: body, CommitOid: target, AuthorLogin: "talkable", AuthorType: "Bot"},
				github.RESTReview{ID: pending, UserLogin: "talkable[bot]", UserType: "Bot", State: "PENDING", CommitID: target})
		}
		return f.end(run.ID)
	}
}

func eventsOfKind(evs []store.Event, kind string) []store.Event {
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// One run, one review: of two reviews carrying the marker the first is the
// round's, the second is reported, a pending draft is deleted.
func TestDuplicateReviewKeepsTheFirstAndDeletesThePendingDraft(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.postTwice(t, 601, 602, 603)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if res.ReviewID != 601 {
		t.Fatalf("review = %d, want the first, 601", res.ReviewID)
	}
	if !slices.Equal(e.gh.deleted, []int64{603}) {
		t.Fatalf("deleted = %v, want only the pending draft 603", e.gh.deleted)
	}
	evs := eventsOfKind(e.events(), "round.duplicate_review")
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("duplicate events = %+v", evs)
	}
	var data struct {
		Kept       int64   `json:"kept"`
		Duplicates []int64 `json:"duplicates"`
		Deleted    []int64 `json:"deleted"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data.Kept != 601 || !slices.Equal(data.Duplicates, []int64{602}) ||
		!slices.Equal(data.Deleted, []int64{603}) {
		t.Fatalf("event data = %s (%v)", evs[0].Data, err)
	}
	mustContain(t, "message", evs[0].Message, "602", "cannot delete a submitted review", "deleted the pending duplicate 603")
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "602") }) {
		t.Errorf("warnings = %q", res.Warnings)
	}
	if run := e.runOf(agents.RoleJudge, KindInitial); store.Deref(run.Outcome) != OutcomePosted {
		t.Errorf("judge run outcome = %q", store.Deref(run.Outcome))
	}
}

func TestDuplicateReviewWithoutADeleterOnlyWarns(t *testing.T) {
	e := newEnv(t)
	e.r.GitHub = plainGitHub{e.gh}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.postTwice(t, 601, 602, 603)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 601 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.deleted) != 0 {
		t.Fatalf("deleted %v without a deleter", e.gh.deleted)
	}
	evs := eventsOfKind(e.events(), "round.duplicate_review")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "could not delete the pending duplicate 603") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestASingleReviewIsNoDuplicate(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	for _, kind := range []string{"round.duplicate_review", "round.local_paths", "round.environment"} {
		if evs := eventsOfKind(e.events(), kind); len(evs) != 0 {
			t.Errorf("%s events = %+v", kind, evs)
		}
	}
}

// Paths of the review machine in a posted review are reported; the review
// stays posted.
func TestLocalPathsInAPostedReviewAreAWarning(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(601, "COMMENTED", "COMMENT")
	p.body = "**[P2] Bad**\n\nRepro: `ruby /tmp/repro.rb` in " + slotPath + "/app/x.rb, notes at " + e.layout.State() + "/notes/x.md." +
		" See https://example.com/Users/x and app/models/home/x.rb."
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	e.gh.comments = map[int64][]github.ReviewComment{601: {
		{ID: 1, Path: "app/models/order.rb", Line: 12, Body: "**[P2] x**\n\n```\n$ bin/rspec /Users/me/scratch/order_spec.rb\n```"},
		{ID: 2, Path: "app/models/order.rb", Line: 30, Body: "**[P3] y**, see spec/models/order_spec.rb"},
	}}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	evs := eventsOfKind(e.events(), "round.local_paths")
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("events = %+v", evs)
	}
	mustContain(t, "message", evs[0].Message, "/tmp/repro.rb", slotPath+"/app/x.rb", e.layout.State()+"/notes/x.md",
		"app/models/order.rb:12: /Users/me/scratch/order_spec.rb")
	if strings.Contains(evs[0].Message, "order.rb:30") {
		t.Errorf("a comment without a local path is reported: %s", evs[0].Message)
	}
	for _, not := range []string{"example.com", "app/models/home"} {
		if strings.Contains(evs[0].Message, not) {
			t.Errorf("message flags %q: %s", not, evs[0].Message)
		}
	}
}

func TestLocalPaths(t *testing.T) {
	for _, c := range []struct {
		text    string
		literal []string
		want    []string
	}{
		{"run `/tmp/x.sh`.", nil, []string{"/tmp/x.sh"}},
		{"see (/private/var/folders/ab/T/out.log) and /Users/me/p/a.rb:12:", nil, []string{"/private/var/folders/ab/T/out.log", "/Users/me/p/a.rb:12"}},
		{"/home/ci/x and /home/ci/x again", nil, []string{"/home/ci/x"}},
		{"https://github.com/Users/x, ./tmp/x, a/tmp/b, ~/tmp/x", nil, nil},
		{"in /Volumes/dev/pool/slot3/app.rb", []string{"/Volumes/dev/pool/slot3"}, []string{"/Volumes/dev/pool/slot3"}},
		{"in /Users/x/slot/app.rb", []string{"/Users/x/slot"}, []string{"/Users/x/slot/app.rb"}},
		{"plain text", []string{"", "/"}, nil},
	} {
		if got := localPaths(c.text, c.literal); !slices.Equal(got, c.want) {
			t.Errorf("localPaths(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// Failures of the review machine stay out of the review; the round reports
// them to the operator.
func TestEnvironmentFailuresAreRecorded(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(601, "COMMENTED", "COMMENT")
	p.extra = map[string]any{"environment_failures": []any{
		map[string]any{"cmd": "bin/rspec spec/a_spec.rb", "error": "Table 'app_test.snapshots' doesn't exist"},
		"Deadlock found when trying to get lock",
		42,
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	evs := eventsOfKind(e.events(), "round.environment")
	if len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("events = %+v", evs)
	}
	mustContain(t, "message", evs[0].Message, "2 failures", "bin/rspec spec/a_spec.rb: Table 'app_test.snapshots' doesn't exist", "; Deadlock found")
}

func TestParseEnvFailures(t *testing.T) {
	r, ok := parseResult([]byte(`{"status":"posted","environment_failures":[{"cmd":"x","result":"no ruby"},{"cmd":""},"  "]}`))
	if !ok || len(r.EnvironmentFailures) != 1 || r.EnvironmentFailures[0] != (envFailure{Cmd: "x", Error: "no ruby"}) {
		t.Fatalf("parsed = %+v", r.EnvironmentFailures)
	}
	if r, _ := parseResult([]byte(`{"status":"posted","environment_failures":"nope"}`)); r.EnvironmentFailures != nil {
		t.Fatalf("malformed = %+v", r.EnvironmentFailures)
	}
}

// Without a merge base (unknown) codex review falls back to the base ref.
func TestCodexReviewFallsBackToTheBaseRef(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.BaseSHA = ""
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(e.ag.codexCalls) != 1 || !strings.Contains(e.ag.codexCalls[0].Script, "command codex review -c model_reasoning_effort=high -c features.apps=false -c skills.include_instructions=false --base origin/master; } |") {
		t.Fatalf("codex calls = %+v", e.ag.codexCalls)
	}
}

// codex-review's line carries its effort for the round: rereview_effort in
// a re-review, as a session role's prompt does.
func TestCodexReviewRunsAtItsRereviewEffortInAReReview(t *testing.T) {
	e := newEnv(t)
	for i, r := range e.cfg.Roles {
		if r.Name == string(agents.RoleCodexReview) {
			e.cfg.Roles[i].RereviewEffort = "medium"
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindRereview)
	in.Previous = &PreviousReview{ID: 901, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour)}
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(e.ag.codexCalls) != 1 || !strings.Contains(e.ag.codexCalls[0].Script, "command codex review -c model_reasoning_effort=medium -c features.apps=false -c skills.include_instructions=false --base ") {
		t.Fatalf("codex calls = %+v", e.ag.codexCalls)
	}
}
