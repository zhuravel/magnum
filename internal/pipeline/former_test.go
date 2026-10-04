package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// Former logins (RoundInput.FormerLogins): after the engine moves a PR from
// one posting identity to another, the reviews and threads of the identity it
// left are the round's history, while everything the round verifies and
// dismisses is still the round's own login's. The round here posts as
// zhuravel-app (zhuravel[bot]); the identity it replaced is talkable-app
// (talkable[bot], the fake GitHub's graph login "talkable").

// newFormerEnv is newEnv with the round posting as the App zhuravel-app.
func newFormerEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.cfg.Identities = append(e.cfg.Identities, config.Identity{Name: "zhuravel-app", Kind: "app", Login: "zhuravel[bot]",
		NoFindingsEvent: "COMMENT", BlockingEvent: "REQUEST_CHANGES"})
	e.r.Identity = fakeIdentity{name: "zhuravel-app", login: "zhuravel[bot]", kind: "app",
		env: map[string]string{"GH_CONFIG_DIR": "/state/gh/zhuravel-app", "GH_TOKEN": ""}}
	return e
}

// formerPosts is the judge posting a review as zhuravel-app.
func formerPosts(e *env, id int64, state, event string) judgePost {
	p := e.judgePosts(id, state, event)
	p.graphLogin, p.graphType, p.restLogin, p.restType = "zhuravel", "Bot", "zhuravel[bot]", "Bot"
	return p
}

// formerAgents records the JudgeData of every judge prompt.
type formerAgents struct {
	*fakeAgents

	mu    sync.Mutex
	judge []agents.JudgeData
}

func (f *formerAgents) RolePrompt(role config.Role, kind string, data any) (string, error) {
	if jd, ok := data.(agents.JudgeData); ok {
		f.mu.Lock()
		f.judge = append(f.judge, jd)
		f.mu.Unlock()
	}
	return f.fakeAgents.RolePrompt(role, kind, data)
}

// captureJudge makes the runner's agents record the judge data.
func captureJudge(e *env) *formerAgents {
	fa := &formerAgents{fakeAgents: e.ag}
	e.r.Agents = fa
	return fa
}

func (f *formerAgents) data(t *testing.T) agents.JudgeData {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.judge) == 0 {
		t.Fatal("no judge prompt was rendered")
	}
	return f.judge[0]
}

// ---- 7. threads of a former login ----

func formerComment(id int64, login, typ, body string) github.ThreadComment {
	return github.ThreadComment{ID: id, AuthorLogin: login, AuthorType: typ, Body: body, CreatedAt: t0.Add(-time.Hour)}
}

// A thread the reviewer login or a former login started is the judge's own,
// and so is a reply by one of them; a user account that shares a former
// login's name is not that login, nor is anybody else.
func TestOwnThreadsIncludeAFormerLoginsThreads(t *testing.T) {
	threads := []github.Thread{
		{ID: "T_former", Path: "app/a.rb", Line: 4, Comments: []github.ThreadComment{
			formerComment(1, "talkable", "Bot", "**[P1] Former finding**\n\nmore"),
			formerComment(2, "octocat", "User", "Fixed in 1a2b3c4"),
			formerComment(3, "talkable", "Bot", "The former login's rebuttal"),
			formerComment(4, "zhuravel", "Bot", "The new identity's own reply"),
			formerComment(5, "talkable", "User", "a user named like the former App"),
		}},
		{ID: "T_current", Path: "app/b.rb", Line: 8, Comments: []github.ThreadComment{
			formerComment(6, "zhuravel", "Bot", "**[P2] Current finding**"),
			formerComment(7, "talkable", "Bot", "a former login replying"),
		}},
		{ID: "T_user_named_like_former", Path: "app/c.rb", Line: 1, Comments: []github.ThreadComment{
			formerComment(8, "talkable", "User", "**[P2] Not the App**")}},
		{ID: "T_other", Path: "app/d.rb", Line: 2, Comments: []github.ThreadComment{
			formerComment(9, "octocat", "User", "a question"),
			formerComment(10, "talkable", "Bot", "an answer")}},
		{ID: "T_other_bot", Path: "app/e.rb", Line: 3, Comments: []github.ThreadComment{
			formerComment(11, "bob", "Bot", "**[P2] A third bot**")}},
		{ID: "T_rest_form", Path: "app/f.rb", Line: 5, Comments: []github.ThreadComment{
			formerComment(12, "talkable[bot]", "", "**[P3] Login with the suffix**")}},
		{ID: "T_empty"},
	}
	ids := func(ts []agents.ReviewThread) []string {
		var out []string
		for _, th := range ts {
			out = append(out, th.ID)
		}
		return out
	}
	round := func(former ...string) *round {
		return &round{in: RoundInput{FormerLogins: former}, login: "zhuravel[bot]", expectBot: true}
	}

	t.Run("with the former login", func(t *testing.T) {
		got := round("talkable[bot]").ownThreads(threads)
		if want := []string{"T_former", "T_current", "T_rest_form"}; !slices.Equal(ids(got), want) {
			t.Fatalf("threads = %q, want %q", ids(got), want)
		}
		first := got[0]
		if first.CommentID != 1 || first.Finding != "**[P1] Former finding**" || first.Location != "app/a.rb:4" || len(first.Replies) != 4 {
			t.Fatalf("former thread = %+v", first)
		}
		r := first.Replies
		if r[0].Own || r[0].Class != ReplyFixed || r[0].Author != "octocat" {
			t.Errorf("author's reply = %+v, want not own, fixed", r[0])
		}
		if !r[1].Own || r[1].Class != "" || r[1].Author != "talkable" {
			t.Errorf("the former login's reply = %+v, want own without a class", r[1])
		}
		if !r[2].Own || r[2].Class != "" {
			t.Errorf("the round's own reply = %+v, want own without a class", r[2])
		}
		if r[3].Own || r[3].Class != ReplyOther {
			t.Errorf("a user's reply under the former login's name = %+v, want not own, other", r[3])
		}
		if cur := got[1]; len(cur.Replies) != 1 || !cur.Replies[0].Own {
			t.Errorf("the round's own thread = %+v, want the former login's reply marked own", cur)
		}
	})
	t.Run("without it only the round's own thread is the judge's", func(t *testing.T) {
		got := round().ownThreads(threads)
		if want := []string{"T_current"}; !slices.Equal(ids(got), want) {
			t.Fatalf("threads = %q, want %q", ids(got), want)
		}
		if rep := got[0].Replies[0]; rep.Own || rep.Class != ReplyOther {
			t.Errorf("a former login's reply without former logins = %+v, want not own", rep)
		}
	})
	t.Run("a former login that is a user account", func(t *testing.T) {
		got := round("talkable").ownThreads(threads)
		if want := []string{"T_current", "T_user_named_like_former"}; !slices.Equal(ids(got), want) {
			t.Fatalf("threads = %q, want %q (a user's login is not a bot's; the bot's threads are not the user's)", ids(got), want)
		}
	})
}

// ---- 8. recovery reads the threads of a previous review ----

// formerRun runs a round of kind posting as zhuravel-app over the PR's
// threads (prThreads: started by the App "talkable" and others) and returns
// everything a test looks at.
func formerRun(t *testing.T, e *env, kind string, prev *PreviousReview, former []string) (*threadsGitHub, *formerAgents, RoundResult) {
	t.Helper()
	gh := &threadsGitHub{fakeGitHub: e.gh, threads: prThreads()}
	e.r.GitHub = gh
	fa := captureJudge(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{formerPosts(e, 601, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(kind)
	in.Round, in.Previous, in.FormerLogins = 2, prev, former
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	return gh, fa, res
}

// A recovery round (the identity migration starts fresh sessions, so it is
// one) with a previous review reads the threads of the reviewer and its
// former logins as a file and names it to the judge; one without a previous
// review has no history to read.
func TestRecoveryReadsThePreviousReviewsThreads(t *testing.T) {
	prev := &PreviousReview{ID: 901, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}

	t.Run("a former login's threads", func(t *testing.T) {
		e := newFormerEnv(t)
		gh, fa, res := formerRun(t, e, KindRecovery, prev, []string{"talkable[bot]"})
		if gh.calls != 1 {
			t.Fatalf("thread reads = %d, want 1", gh.calls)
		}
		path := filepath.Join(res.ReportDir, ThreadsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("threads file: %v", err)
		}
		var got []agents.ReviewThread
		if err := json.Unmarshal(b, &got); err != nil || len(got) != 2 || got[0].ID != "PRRT_1" || got[1].ID != "PRRT_2" {
			t.Fatalf("threads file = %s (%v)", b, err)
		}
		jd := fa.data(t)
		if jd.ThreadsFile != path || len(jd.Threads) != 2 || jd.ThreadSummary != "2 threads (1 resolved, 1 outdated); replies: 1 fixed, 2 not a bug" {
			t.Fatalf("judge data: file %q, %d threads, summary %q", jd.ThreadsFile, len(jd.Threads), jd.ThreadSummary)
		}
		evs := eventsOfKind(e.events(), "round.threads")
		if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "by zhuravel[bot]") {
			t.Fatalf("round.threads events: %+v", evs)
		}
		mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "mode: recovery")
	})
	t.Run("without former logins the threads are another login's", func(t *testing.T) {
		e := newFormerEnv(t)
		_, fa, res := formerRun(t, e, KindRecovery, prev, nil)
		b, err := os.ReadFile(filepath.Join(res.ReportDir, ThreadsFile))
		if err != nil || strings.TrimSpace(string(b)) != "[]" {
			t.Fatalf("threads file = %q (%v), want an empty list", b, err)
		}
		if jd := fa.data(t); len(jd.Threads) != 0 || jd.ThreadSummary != "no threads" {
			t.Fatalf("judge data: %d threads, summary %q", len(jd.Threads), jd.ThreadSummary)
		}
	})
	t.Run("the round's own login, no former logins", func(t *testing.T) {
		e := newEnv(t) // posts as talkable-app
		gh := &threadsGitHub{fakeGitHub: e.gh, threads: prThreads()}
		e.r.GitHub = gh
		fa := captureJudge(e)
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindRecovery)
		in.Round, in.Previous = 2, prev
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted {
			t.Fatalf("RunRound = %+v, %v", res, err)
		}
		if jd := fa.data(t); gh.calls != 1 || jd.ThreadsFile == "" || len(jd.Threads) != 2 {
			t.Fatalf("thread reads %d, judge data: file %q, %d threads", gh.calls, jd.ThreadsFile, len(jd.Threads))
		}
	})
	for name, noPrev := range map[string]*PreviousReview{"no previous review": nil, "a previous review without an id": {Event: "COMMENTED"}} {
		t.Run(name, func(t *testing.T) {
			e := newFormerEnv(t)
			gh, fa, res := formerRun(t, e, KindRecovery, noPrev, []string{"talkable[bot]"})
			if gh.calls != 0 {
				t.Fatalf("thread reads = %d, want none", gh.calls)
			}
			if _, err := os.Stat(filepath.Join(res.ReportDir, ThreadsFile)); !os.IsNotExist(err) {
				t.Fatalf("threads file: %v, want none", err)
			}
			if jd := fa.data(t); jd.ThreadsFile != "" || len(jd.Threads) != 0 || jd.ThreadSummary != "" {
				t.Fatalf("judge data names threads: file %q, %d threads, summary %q", jd.ThreadsFile, len(jd.Threads), jd.ThreadSummary)
			}
		})
	}
}

// ---- 9. dismissing the previous review ----

// The round dismisses its own login's stale CHANGES_REQUESTED once its
// COMMENT review is up, and only that: a Previous posted by a former login
// is left to the engine, which dismisses it with the former identity's own
// credentials. Rereview and recovery (the migration round) behave alike.
func TestDismissStaleLeavesAFormerLoginsReview(t *testing.T) {
	cases := []struct {
		name   string
		login  string // PreviousReview.Login
		former bool   // PreviousReview.Former, which the engine decides
		want   bool
	}{
		{"a former login", "talkable[bot]", true, false},
		{"a former user named like the round's App", "zhuravel", true, false},
		{"the round's login (bare)", "zhuravel", false, true},
		{"the round's login (REST form)", "zhuravel[bot]", false, true},
		{"an unknown login", "", false, true},
	}
	for _, kind := range []string{KindRereview, KindRecovery} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				e := newFormerEnv(t)
				e.ag.behaviors[agents.RoleJudge] = []behavior{formerPosts(e, 506, "COMMENTED", "COMMENT").behavior(t)}
				in := e.input(kind)
				in.Round = 2
				in.Previous = &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour),
					Login: tc.login, Former: tc.former}
				in.FormerLogins = []string{"talkable[bot]"}
				res, err := e.r.RunRound(e.ctx, in)
				if err != nil || res.Outcome != OutcomePosted {
					t.Fatalf("RunRound = %+v, %v", res, err)
				}
				if tc.want {
					if len(e.gh.dismissed) != 1 || e.gh.dismissed[0].ID != 900 || res.DismissedReviewID != 900 {
						t.Fatalf("dismissed %+v (result %d), want review 900", e.gh.dismissed, res.DismissedReviewID)
					}
					return
				}
				if len(e.gh.dismissed) != 0 || res.DismissedReviewID != 0 {
					t.Fatalf("dismissed %+v (result %d), want nothing: the review is the former login's", e.gh.dismissed, res.DismissedReviewID)
				}
				if evs := eventsOfKind(e.events(), "round.dismiss"); len(evs) != 0 {
					t.Fatalf("round.dismiss events: %+v", evs)
				}
			})
		}
	}
}

// ---- 10. verification accepts the round's login only ----

// formerReview is a COMMENTED review of the former login talkable[bot] on the
// target, carrying marker (none when empty), posted now.
func formerReview(e *env, f *fakeAgents, id int64, marker string) {
	body := "**Verdict** earlier findings\n"
	if marker != "" {
		body += fmt.Sprintf("<!-- magnum:run=%s head=%s -->", marker, target[:7])
	}
	url := fmt.Sprintf("https://github.com/talkable/talkable/pull/11920#pullrequestreview-%d", id)
	e.gh.add(github.Review{DatabaseID: id, State: "COMMENTED", Body: body, URL: url, SubmittedAt: f.st.Clock(), CommitOid: target,
		AuthorLogin: "talkable", AuthorType: "Bot"},
		github.RESTReview{ID: id, UserLogin: "talkable[bot]", UserType: "Bot", State: "COMMENTED", SubmittedAt: f.st.Clock(), CommitID: target, HTMLURL: url})
}

// formerBefore and formerAfter run a former login's review next to a behavior.
func formerBefore(e *env, id int64, marker string, next behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		formerReview(e, f, id, marker)
		return next(f, run, text)
	}
}

func formerAfter(e *env, id int64, marker string, next behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		err := next(f, run, text)
		formerReview(e, f, id, marker)
		return err
	}
}

// A review of a former login on the target, posted after the prompt and
// carrying an older run's marker (or none), is neither this round's review
// nor an identity leak: only zhuravel[bot] is the round's reviewer.
func TestVerificationIgnoresAFormerLoginsReview(t *testing.T) {
	former := []string{"talkable[bot]"}
	run := func(t *testing.T, e *env, b behavior) RoundResult {
		t.Helper()
		e.ag.behaviors[agents.RoleJudge] = []behavior{b}
		in := e.input(KindRecovery)
		in.Round, in.FormerLogins = 2, former
		in.Previous = &PreviousReview{ID: 800, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil {
			t.Fatalf("RunRound: %v", err)
		}
		return res
	}
	blocked := judgePost{status: "blocked", extra: map[string]any{"blocker": "HEAD mismatch"}}

	for _, marker := range []string{"r-old-1", ""} {
		name := "an old marker"
		if marker == "" {
			name = "no marker"
		}
		t.Run("no review of the round, "+name, func(t *testing.T) {
			e := newFormerEnv(t)
			res := run(t, e, formerBefore(e, 801, marker, blocked.behavior(t)))
			if res.Outcome != OutcomeBlocked || res.ReviewID != 0 {
				t.Fatalf("result = %+v, want blocked without a review (not posted, not a leak)", res)
			}
		})
		t.Run("the result file names it, "+name, func(t *testing.T) {
			e := newFormerEnv(t)
			p := judgePost{status: "posted", reviewID: 801, event: "COMMENT", findings: map[string]int{"P2": 1}}
			res := run(t, e, formerBefore(e, 801, marker, p.behavior(t)))
			if res.Outcome != OutcomeNeedsAttention || res.ReviewID != 0 || !strings.Contains(res.Error, "zhuravel[bot]") {
				t.Fatalf("result = %+v, want needs-attention naming zhuravel[bot]: the former login's review is not the round's", res)
			}
		})
	}

	t.Run("the round's own review is verified next to it", func(t *testing.T) {
		for name, wrap := range map[string]func(e *env, b behavior) behavior{
			"former review first": func(e *env, b behavior) behavior { return formerBefore(e, 801, "r-old-1", b) },
			"former review after": func(e *env, b behavior) behavior { return formerAfter(e, 801, "r-old-1", b) },
			"marker-less former review first": func(e *env, b behavior) behavior {
				return formerBefore(e, 801, "", b)
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := newFormerEnv(t)
				res := run(t, e, wrap(e, formerPosts(e, 802, "COMMENTED", "COMMENT").behavior(t)))
				if res.Outcome != OutcomePosted || res.ReviewID != 802 || res.Event != "COMMENTED" {
					t.Fatalf("result = %+v, want the round's own review 802 posted", res)
				}
				jr := e.runOf(agents.RoleJudge, store.RunRecovery)
				if jr.State != store.RunVerified || store.Deref(jr.ReviewID) != 802 || jr.ReviewerLogin != "zhuravel[bot]" {
					t.Fatalf("judge run = %s review %v login %q", jr.State, jr.ReviewID, jr.ReviewerLogin)
				}
			})
		}
	})

	t.Run("a former login posting the round's own marker is a leak", func(t *testing.T) {
		e := newFormerEnv(t)
		res := run(t, e, e.judgePosts(803, "COMMENTED", "COMMENT").behavior(t)) // posted as talkable[bot]
		if res.Outcome != OutcomeIdentityLeak || res.ReviewID != 803 || !strings.Contains(res.Error, "talkable") {
			t.Fatalf("result = %+v, want an identity leak naming talkable", res)
		}
	})
}

// ---- 11. the judge is told the former logins ----

func TestJudgeDataCarriesTheFormerLogins(t *testing.T) {
	for name, former := range map[string][]string{
		"none":     nil,
		"one":      {"talkable[bot]"},
		"several":  {"talkable[bot]", "bob[bot]", "alice"},
		"explicit": {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFormerEnv(t)
			fa := captureJudge(e)
			e.ag.behaviors[agents.RoleJudge] = []behavior{formerPosts(e, 601, "COMMENTED", "COMMENT").behavior(t)}
			in := e.rereviewInput()
			in.FormerLogins = former
			if _, err := e.r.RunRound(e.ctx, in); err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			jd := fa.data(t)
			if !slices.Equal(jd.FormerLogins, former) {
				t.Fatalf("JudgeData.FormerLogins = %q, want %q", jd.FormerLogins, former)
			}
			if jd.ReviewerLogin != "zhuravel[bot]" {
				t.Fatalf("JudgeData.ReviewerLogin = %q", jd.ReviewerLogin)
			}
			// The judge data owns its copy of the list.
			if len(former) > 0 {
				jd.FormerLogins[0] = "changed"
				if in.FormerLogins[0] == "changed" {
					t.Fatal("JudgeData.FormerLogins shares the round input's backing array")
				}
			}
		})
	}
}

// The judge prompts name the former logins once their .next templates are
// live (or the live ones render them): an empty list adds nothing.
func TestJudgePromptsNameTheFormerLogins(t *testing.T) {
	for _, tc := range []struct {
		kind, file string
		mk         func(e *env) RoundInput
	}{
		{KindRereview, "judge-rereview.md", func(e *env) RoundInput { return e.rereviewInput() }},
		{KindRecovery, "judge-recovery.md", func(e *env) RoundInput {
			in := e.input(KindRecovery)
			in.Round = 2
			in.Previous = &PreviousReview{ID: 901, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour)}
			return in
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			text, err := os.ReadFile(filepath.Join("..", "..", "prompts", tc.file+".next"))
			if os.IsNotExist(err) {
				text, err = os.ReadFile(filepath.Join("..", "..", "prompts", tc.file))
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(text), "FormerLogins") {
				t.Skipf("%s does not render FormerLogins yet", tc.file)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.file), text, 0o600); err != nil {
				t.Fatal(err)
			}
			for name, former := range map[string][]string{"former logins": {"talkable[bot]", "bob[bot]"}, "none": nil} {
				e := newFormerEnv(t)
				e.cfg.Pipeline.PromptsDir = dir
				e.r.GitHub = &threadsGitHub{fakeGitHub: e.gh, threads: prThreads()}
				e.ag.behaviors[agents.RoleJudge] = []behavior{formerPosts(e, 601, "COMMENTED", "COMMENT").behavior(t)}
				in := tc.mk(e)
				in.FormerLogins = former
				if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
					t.Fatalf("%s: RunRound = %+v, %v", name, res, err)
				}
				prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
				mustContain(t, name+" prompt", prompt, "reviewer_login: zhuravel[bot]")
				if len(former) > 0 {
					mustContain(t, name+" prompt", prompt, "former_logins: talkable[bot], bob[bot]", "`talkable[bot]`", "`bob[bot]`")
				} else if strings.Contains(prompt, "before magnum moved") {
					t.Errorf("%s prompt tells about a migration:\n%s", name, prompt)
				}
			}
		})
	}
}
