package engine

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
)

// factoryNow is when the render tests draw the factory part: the day of
// factoryStatus's update.
var factoryNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// factoryStatus is a factory PR's status with something to say in every
// row, updated on factoryNow's day.
func factoryStatus() prstatus.Status {
	return prstatus.Status{V: prstatus.Version, UpdatedAt: time.Date(2026, 10, 5, 9, 41, 0, 0, time.UTC), TaskID: 3,
		Plan:        prstatus.Plan{QuestionsAsked: 3, QuestionsAnswered: 2},
		Code:        prstatus.Code{Commits: 6, Proof: prstatus.ProofRedToGreen, Specs: 3},
		LocalReview: prstatus.LocalReview{Rounds: 2, Verdict: prstatus.VerdictClean, NotesOpen: 2},
		PublishedAt: time.Date(2026, 10, 4, 16, 5, 0, 0, time.UTC),
		FixLoop:     prstatus.FixLoop{State: prstatus.FixRunning, Cycle: 2, Pushes: 3, ThreadsAnswered: 7},
		Staging:     prstatus.Staging{Env: "beta", State: prstatus.StagingDeployed},
		Now:         prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}}
}

// The factory part is a heading, the approved table of the factory's steps
// and one line of who the PR waits on, with the time the status last
// changed: short, in fixed words, a blank line around the table so GitHub
// draws it and does not read the Now line as a row.
func TestTheFactoryPartIsATableOfTheStepsAndANowLine(t *testing.T) {
	s := factoryStatus()
	s.Jira = prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraInCodeReview}
	want := "**Magnum factory** · Jira ABC-12: in code review\n" +
		"\n" +
		"| Step | State |\n" +
		"|---|---|\n" +
		"| Plan | 3 questions, 2 answered |\n" +
		"| Code | 6 commits, 3 specs: red on the base, green at the head |\n" +
		"| Local review | 2 rounds, clean, 2 notes left open |\n" +
		"| Published | 4 Oct, 16:05 UTC |\n" +
		"| Fix loop | running, cycle 2, 3 pushes, 7 threads answered |\n" +
		"| Staging | deployed to `beta` |\n" +
		"\n" +
		"Now: waits on the reviewers · updated 09:41 UTC"
	if got := factoryText(s, factoryNow); got != want {
		t.Fatalf("factory part:\n%s\nwant:\n%s", got, want)
	}
}

// Each row says its step in fixed words for every word of its enums and
// its counts, and a row with nothing to say reads "none".
func TestEachFactoryRowSaysItsStepInFixedWords(t *testing.T) {
	for _, c := range []struct {
		name, step, want string
		set              func(*prstatus.Status)
	}{
		{"no questions", "Plan", "no questions", func(s *prstatus.Status) { s.Plan = prstatus.Plan{} }},
		{"one question open", "Plan", "1 question, 0 answered", func(s *prstatus.Status) { s.Plan = prstatus.Plan{QuestionsAsked: 1} }},
		{"questions answered", "Plan", "3 questions, 3 answered",
			func(s *prstatus.Status) { s.Plan = prstatus.Plan{QuestionsAsked: 3, QuestionsAnswered: 3} }},

		{"no code to tell", "Code", "none", func(s *prstatus.Status) { s.Code = prstatus.Code{Proof: prstatus.ProofNone} }},
		{"commits without a proof", "Code", "6 commits: no proof",
			func(s *prstatus.Status) { s.Code = prstatus.Code{Commits: 6, Proof: prstatus.ProofNone} }},
		{"red", "Code", "1 commit, 1 spec: red on the base, not green at the head",
			func(s *prstatus.Status) { s.Code = prstatus.Code{Commits: 1, Proof: prstatus.ProofRed, Specs: 1} }},
		{"red to green, commits not counted", "Code", "3 specs: red on the base, green at the head",
			func(s *prstatus.Status) { s.Code = prstatus.Code{Proof: prstatus.ProofRedToGreen, Specs: 3} }},
		{"green", "Code", "6 commits, 3 specs: green at the head",
			func(s *prstatus.Status) { s.Code = prstatus.Code{Commits: 6, Proof: prstatus.ProofGreen, Specs: 3} }},
		{"green without specs", "Code", "green at the head", func(s *prstatus.Status) { s.Code = prstatus.Code{Proof: prstatus.ProofGreen} }},

		{"no local review", "Local review", "none",
			func(s *prstatus.Status) { s.LocalReview = prstatus.LocalReview{Verdict: prstatus.VerdictNone} }},
		{"clean", "Local review", "1 round, clean",
			func(s *prstatus.Status) {
				s.LocalReview = prstatus.LocalReview{Rounds: 1, Verdict: prstatus.VerdictClean}
			}},
		{"findings", "Local review", "3 rounds, findings left, 1 note left open",
			func(s *prstatus.Status) {
				s.LocalReview = prstatus.LocalReview{Rounds: 3, Verdict: prstatus.VerdictFindings, NotesOpen: 1}
			}},
		{"failed", "Local review", "1 round, did not finish",
			func(s *prstatus.Status) {
				s.LocalReview = prstatus.LocalReview{Rounds: 1, Verdict: prstatus.VerdictFailed}
			}},
		{"no verdict", "Local review", "2 rounds, no verdict",
			func(s *prstatus.Status) {
				s.LocalReview = prstatus.LocalReview{Rounds: 2, Verdict: prstatus.VerdictNone}
			}},

		{"published on another day", "Published", "4 Oct, 16:05 UTC", func(*prstatus.Status) {}},
		{"published today", "Published", "5 Oct, 08:15 UTC",
			func(s *prstatus.Status) { s.PublishedAt = time.Date(2026, 10, 5, 8, 15, 0, 0, time.UTC) }},

		{"no fix loop yet", "Fix loop", "none", func(s *prstatus.Status) { s.FixLoop = prstatus.FixLoop{State: prstatus.FixNone} }},
		{"left to the operator", "Fix loop", "left to the operator, cycle 1, 1 push, 1 thread answered",
			func(s *prstatus.Status) {
				s.FixLoop = prstatus.FixLoop{State: prstatus.FixNeedsYou, Cycle: 1, Pushes: 1, ThreadsAnswered: 1}
			}},
		{"done", "Fix loop", "done, cycle 3, 2 pushes",
			func(s *prstatus.Status) { s.FixLoop = prstatus.FixLoop{State: prstatus.FixDone, Cycle: 3, Pushes: 2} }},
		{"off", "Fix loop", "off", func(s *prstatus.Status) { s.FixLoop = prstatus.FixLoop{State: prstatus.FixOff} }},

		{"no staging", "Staging", "none", func(s *prstatus.Status) { s.Staging = prstatus.Staging{State: prstatus.StagingNone} }},
		{"nothing deployed to a named env", "Staging", "none",
			func(s *prstatus.Status) { s.Staging = prstatus.Staging{Env: "beta", State: prstatus.StagingNone} }},
		{"deploying", "Staging", "deploying to `beta`",
			func(s *prstatus.Status) { s.Staging = prstatus.Staging{Env: "beta", State: prstatus.StagingDeploying} }},
		{"deployed without a name", "Staging", "deployed",
			func(s *prstatus.Status) { s.Staging = prstatus.Staging{State: prstatus.StagingDeployed} }},
		{"failed", "Staging", "failed on `qa-2`",
			func(s *prstatus.Status) { s.Staging = prstatus.Staging{Env: "qa-2", State: prstatus.StagingFailed} }},
		{"failed without a name", "Staging", "failed",
			func(s *prstatus.Status) { s.Staging = prstatus.Staging{State: prstatus.StagingFailed} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := factoryStatus()
			c.set(&s)
			got := factoryText(s, factoryNow)
			if row := "\n| " + c.step + " | " + c.want + " |\n"; !strings.Contains(got, row) {
				t.Fatalf("factory part:\n%s\nwant the row %q", got, strings.TrimSpace(row))
			}
		})
	}
}

// The Now line says who the PR waits on in fixed words, the task's stage
// when it is not the published PR's, and when the status last changed, with
// its day once that is not the day of the edit.
func TestTheFactoryNowLineSaysWhoThePRWaitsOn(t *testing.T) {
	at := " · updated 09:41 UTC"
	for _, c := range []struct {
		name string
		now  prstatus.Now
		ago  time.Duration
		want string
	}{
		{"you", prstatus.Now{Who: prstatus.WhoYou, Stage: prstatus.StagePublished}, 0, "waits on the operator" + at},
		{"magnum", prstatus.Now{Who: prstatus.WhoMagnum, Stage: prstatus.StagePublished}, 0, "waits on Magnum" + at},
		{"reviewers", prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}, 0, "waits on the reviewers" + at},
		{"ci", prstatus.Now{Who: prstatus.WhoCI, Stage: prstatus.StagePublished}, 0, "waits on CI" + at},
		{"factory", prstatus.Now{Who: prstatus.WhoFactory, Stage: prstatus.StagePublished}, 0, "waits on the factory" + at},
		{"nobody", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StagePublished}, 0, "waits on nobody" + at},
		{"merged", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageDone}, 0, "merged" + at},
		{"stopped", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageStopped}, 0, "the task stopped" + at},
		{"failed", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageFailed}, 0, "the task failed" + at},
		{"planning", prstatus.Now{Who: prstatus.WhoYou, Stage: prstatus.StagePlan}, 0, "waits on the operator, planning" + at},
		{"coding", prstatus.Now{Who: prstatus.WhoFactory, Stage: prstatus.StageCode}, 0, "waits on the factory, coding" + at},
		{"in local review", prstatus.Now{Who: prstatus.WhoFactory, Stage: prstatus.StageReview}, 0, "waits on the factory, in local review" + at},
		{"ready", prstatus.Now{Who: prstatus.WhoYou, Stage: prstatus.StageReady}, 0, "waits on the operator, ready to publish" + at},
		{"a day old", prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}, 26 * time.Hour,
			"waits on the reviewers · updated 4 Oct, 07:41 UTC"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := factoryStatus()
			s.Now, s.UpdatedAt = c.now, s.UpdatedAt.Add(-c.ago)
			got := factoryText(s, factoryNow)
			if !strings.HasSuffix(got, "\n\nNow: "+c.want) {
				t.Fatalf("factory part:\n%s\nwant it to end with:\nNow: %s", got, c.want)
			}
		})
	}
}

// The heading names the Jira issue only when the status has one: its key
// and its status in fixed words.
func TestTheFactoryHeadingNamesTheJiraIssue(t *testing.T) {
	for _, c := range []struct {
		jira prstatus.Jira
		want string
	}{
		{prstatus.Jira{}, "**Magnum factory**"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraToDo}, "**Magnum factory** · Jira ABC-12: to do"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraInDevelopment}, "**Magnum factory** · Jira ABC-12: in development"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraInCodeReview}, "**Magnum factory** · Jira ABC-12: in code review"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraDone}, "**Magnum factory** · Jira ABC-12: done"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraOther}, "**Magnum factory** · Jira ABC-12: another status"},
		{prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraUnknown}, "**Magnum factory** · Jira ABC-12: status not known"},
	} {
		s := factoryStatus()
		s.Jira = c.jira
		if got, _, _ := strings.Cut(factoryText(s, factoryNow), "\n"); got != c.want {
			t.Errorf("%+v: heading %q, want %q", c.jira, got, c.want)
		}
	}
}

// Every enum word the contract has gets its own fixed words, so a new word
// in prstatus fails here until the comment can say it.
func TestEveryFactoryEnumWordHasItsWords(t *testing.T) {
	check := func(name string, n int, words []string) {
		t.Helper()
		if len(words) != n {
			t.Errorf("%s: %d words mapped, want %d", name, len(words), n)
		}
		seen := map[string]bool{}
		for _, w := range words {
			if w == "" || seen[w] {
				t.Errorf("%s: words %q are empty or not distinct", name, words)
			}
			seen[w] = true
		}
	}
	proofs := []prstatus.Proof{prstatus.ProofNone, prstatus.ProofRed, prstatus.ProofRedToGreen, prstatus.ProofGreen}
	verdicts := []prstatus.Verdict{prstatus.VerdictNone, prstatus.VerdictClean, prstatus.VerdictFindings, prstatus.VerdictFailed}
	fixes := []prstatus.FixState{prstatus.FixNone, prstatus.FixRunning, prstatus.FixNeedsYou, prstatus.FixDone, prstatus.FixOff}
	stagings := []prstatus.StagingState{prstatus.StagingNone, prstatus.StagingDeploying, prstatus.StagingDeployed, prstatus.StagingFailed}
	whos := []prstatus.Who{prstatus.WhoYou, prstatus.WhoMagnum, prstatus.WhoReviewers, prstatus.WhoCI, prstatus.WhoFactory, prstatus.WhoNone}
	jiras := []prstatus.JiraStatus{prstatus.JiraToDo, prstatus.JiraInDevelopment, prstatus.JiraInCodeReview, prstatus.JiraDone,
		prstatus.JiraOther, prstatus.JiraUnknown}
	stages := []prstatus.Stage{prstatus.StagePlan, prstatus.StageCode, prstatus.StageReview, prstatus.StageReady,
		prstatus.StageDone, prstatus.StageStopped, prstatus.StageFailed}
	check("proofs", len(proofs), mapWords(factoryProofWords, proofs))
	check("verdicts", len(verdicts), mapWords(factoryVerdictWords, verdicts))
	check("fix states", len(fixes), mapWords(factoryFixWords, fixes))
	check("staging states", len(stagings), mapWords(factoryStagingWords, stagings))
	check("who", len(whos), mapWords(factoryWhoWords, whos))
	check("jira statuses", len(jiras), mapWords(factoryJiraWords, jiras))
	check("stages", len(stages), mapWords(factoryStageWords, stages))
	for _, m := range []int{len(factoryProofWords) - len(proofs), len(factoryVerdictWords) - len(verdicts),
		len(factoryFixWords) - len(fixes), len(factoryStagingWords) - len(stagings), len(factoryWhoWords) - len(whos),
		len(factoryJiraWords) - len(jiras), len(factoryStageWords) - len(stages) - 1} {
		if m != 0 {
			t.Errorf("a word map holds %d words this test does not list", m)
		}
	}
	if w, ok := factoryStageWords[prstatus.StagePublished]; !ok || w != "" {
		t.Errorf("the published stage says %q (listed %v), want nothing", w, ok)
	}
}

// mapWords is the words m gives each of keys ("" for one it lacks).
func mapWords[K comparable](m map[K]string, keys []K) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// No string of the status reaches the comment but through a word map, the
// staging environment's slug and the Jira key, both checked again: a word
// outside its enum reads "not known", a key or an env that is not one is
// left out, so markup, a mention or a link in the file never posts.
func TestNoFactoryStatusStringReachesTheCommentUnmapped(t *testing.T) {
	hostile := []string{"@alice", "<b>bold</b>", "[link](https://example.com)", "| cell |", "**loud**", "x\ny"}
	for _, h := range hostile {
		s := factoryStatus()
		s.Jira = prstatus.Jira{Key: h, Status: prstatus.JiraStatus(h)}
		s.Code.Proof = prstatus.Proof(h)
		s.LocalReview.Verdict = prstatus.Verdict(h)
		s.FixLoop.State = prstatus.FixState(h)
		s.Staging = prstatus.Staging{Env: h, State: prstatus.StagingState(h)}
		s.Now = prstatus.Now{Who: prstatus.Who(h), Stage: prstatus.Stage(h)}
		got := factoryText(s, factoryNow)
		if got == "" {
			t.Fatalf("%q: no factory part", h)
		}
		for _, bad := range []string{h, "@", "<", "](", "**loud"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q: the factory part carries %q:\n%s", h, bad, got)
			}
		}
		if !strings.Contains(got, "\n| Code | 6 commits, 3 specs: not known |\n") || !strings.HasPrefix(got, "**Magnum factory**\n") {
			t.Errorf("%q: the unknown words do not read as not known:\n%s", h, got)
		}
	}
	s := factoryStatus()
	s.Jira = prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraDone}
	s.Staging.Env = "Beta Env"
	if got := factoryText(s, factoryNow); !strings.Contains(got, "\n| Staging | deployed |\n") || !strings.Contains(got, "Jira ABC-12: done") {
		t.Errorf("an env that is no slug, or a key that is one:\n%s", got)
	}
}

// widgetRepo is the placeholder repository of the factory part's engine
// tests, which example's watch (withExampleWatch) covers as zhuravel.
const widgetRepo = "example/widget"

// widgetHarness is a harness with the status comment on, a watch of
// example's repositories posting as zhuravel, and a push quiet period of two
// days: a PR of example/widget queued in it keeps its scheduled line, so
// only its factory part moves its comment.
func widgetHarness(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	quiet := func(h *harness) { h.cfg.Daemon.PushQuietPeriod = config.Duration{Duration: 48 * time.Hour} }
	return newHarness(t, append([]func(*harness){statusOn, withExampleWatch, quiet}, mods...)...)
}

// widgetQueued shows example/widget's #1 (a baseline at the first sync),
// then #12 at statusHead, queued, and returns #12.
func (h *harness) widgetQueued() store.PR {
	h.t.Helper()
	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()})
	h.startup()
	h.tick()
	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()}, prSpec{n: 12, head: statusHead, updated: h.clock.Now()})
	h.tick()
	pr := h.widgetPR(12)
	if pr.State != store.PRQueued {
		h.t.Fatalf("%s#12 state = %s, want queued", widgetRepo, pr.State)
	}
	return pr
}

// widgetPR is example/widget#n.
func (h *harness) widgetPR(n int) store.PR {
	h.t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, widgetRepo)
	if err != nil {
		h.t.Fatal(err)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, n)
	if err != nil {
		h.t.Fatal(err)
	}
	return pr
}

// widgetStatusPath is the status file of example/widget#n.
func (h *harness) widgetStatusPath(n int) string {
	h.t.Helper()
	path, err := prstatus.Path(h.layout, "example", "widget", n)
	if err != nil {
		h.t.Fatal(err)
	}
	return path
}

// writeWidgetStatus writes s, updated now, as example/widget#n's status
// file, as the factory daemon does, and returns it as Read gives it back.
func (h *harness) writeWidgetStatus(n int, s prstatus.Status) prstatus.Status {
	h.t.Helper()
	s.UpdatedAt = h.clock.Now()
	path := h.widgetStatusPath(n)
	if _, err := prstatus.Write(path, s); err != nil {
		h.t.Fatal(err)
	}
	got, ok, err := prstatus.Read(path)
	if err != nil || !ok {
		h.t.Fatalf("the status file reads back (%v, %v)", ok, err)
	}
	return got
}

// widgetEdits is how many times the comment on example/widget#12 was edited.
func (h *harness) widgetEdits() int {
	h.t.Helper()
	return h.gh.count("edit_issue_comment:" + widgetRepo + ":")
}

// factoryDigest is the digest kvPRFactoryStatus holds for the PR ("" =
// none).
func (h *harness) factoryDigest(prID int64) string {
	h.t.Helper()
	v, _ := h.e.getKV(h.ctx, kvPRFactoryStatus(prID))
	return v
}

// A PR a factory task published gets its factory part below magnum's
// lines, which it leaves as they are, and the header's time with them; a
// PR without a status file gets none, and nothing is kept of a part.
func TestAFactoryPRsStatusCommentCarriesTheFactoryPart(t *testing.T) {
	h := widgetHarness(t)
	s := h.writeWidgetStatus(12, factoryStatus())
	start := h.clock.Now()
	pr := h.widgetQueued()

	c := h.statusComment(12)
	magnums, part, ok := strings.Cut(strings.TrimPrefix(c.body, statusHeader(start)), "\n\n**Magnum factory**")
	if !ok || !strings.HasPrefix(magnums, "- [x] Review queued for `a1b2c3d`: not before ") ||
		!strings.HasSuffix(magnums, "\n- [ ] In progress\n- [ ] Posted\n\n"+statusSelf+"\n"+statusMerge) {
		t.Fatalf("status comment:\n%s", c.body)
	}
	if want := factoryText(s, h.clock.Now()); "**Magnum factory**"+part != want {
		t.Fatalf("factory part:\n%s\nwant:\n%s", "**Magnum factory**"+part, want)
	}
	if got := h.factoryDigest(pr.ID); got != s.Digest {
		t.Fatalf("%s = %q, want the file's digest %q", kvPRFactoryStatus(pr.ID), got, s.Digest)
	}

	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: start}, prSpec{n: 12, head: statusHead, updated: start},
		prSpec{n: 13, head: statusPush, updated: h.clock.Now()})
	h.tick()
	other := h.statusComment(13)
	if strings.Contains(other.body, "Magnum factory") || !strings.HasSuffix(other.body, "\n"+statusMerge) {
		t.Fatalf("a PR without a status file:\n%s", other.body)
	}
	if got := h.factoryDigest(h.widgetPR(13).ID); got != "" {
		t.Fatalf("a PR without a status file keeps the digest %q", got)
	}
}

// A status file never opens a comment where magnum would post none: on a
// draft nobody asked to review, and on a watch with the comment off.
func TestAFactoryStatusFileNeverOpensAComment(t *testing.T) {
	t.Run("a draft", func(t *testing.T) {
		h := widgetHarness(t)
		h.writeWidgetStatus(12, factoryStatus())
		h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()})
		h.startup()
		h.tick()
		h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()}, prSpec{n: 12, head: statusHead, draft: true, updated: h.clock.Now()})
		h.tick()
		h.advance(3 * time.Minute)
		h.tick()
		if cs := h.gh.commentsOn(12); len(cs) != 0 {
			t.Fatalf("a draft (state %s) got %+v", h.widgetPR(12).State, cs)
		}
	})
	t.Run("the comment off", func(t *testing.T) {
		h := widgetHarness(t, func(h *harness) { h.cfg.Watches[len(h.cfg.Watches)-1].StatusComment = new(false) })
		h.writeWidgetStatus(12, factoryStatus())
		h.widgetQueued()
		h.advance(3 * time.Minute)
		h.tick()
		if cs := h.gh.commentsOn(12); len(cs) != 0 {
			t.Fatalf("a watch with the comment off got %+v", cs)
		}
	})
}

// An unchanged status file costs no edit, whatever the factory rewrites
// and however long it stays: the next day's tick leaves the "updated" time
// as it was written.
func TestAnUnchangedFactoryStatusMakesNoEdit(t *testing.T) {
	h := widgetHarness(t)
	s := h.writeWidgetStatus(12, factoryStatus())
	h.widgetQueued()
	posted := h.statusComment(12)
	for range 4 {
		h.advance(3 * time.Minute)
		h.writeWidgetStatus(12, factoryStatus()) // the same content: Write writes nothing
		h.tick()
	}
	h.advance(24 * time.Hour)
	h.tick()
	if n := h.widgetEdits(); n != 0 {
		t.Fatalf("an unchanged status file edited the comment %d times:\n%s", n, h.statusComment(12).body)
	}
	if c := h.statusComment(12); c.body != posted.body || !strings.Contains(c.body, "Now: waits on the reviewers · updated "+utcClock(s.UpdatedAt)) {
		t.Fatalf("status comment:\n%s", c.body)
	}
}

// A changed status file reaches the comment though magnum's lines did not
// change: once, at the first tick the comment's spacing allows, and the
// header keeps the time magnum's status last changed.
func TestAChangedFactoryStatusEditsOnceUnderTheRateLimit(t *testing.T) {
	h := widgetHarness(t)
	start := h.clock.Now()
	pr := h.widgetQueued()
	magnums := strings.TrimPrefix(h.statusComment(12).body, statusHeader(start))

	h.advance(30 * time.Second)
	s := h.writeWidgetStatus(12, factoryStatus())
	h.tick()
	if n := h.widgetEdits(); n != 0 {
		t.Fatalf("an edit %d times within 2 minutes of the post", n)
	}
	h.advance(90 * time.Second)
	h.tick()
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, h.clock.Now()))
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("edits = %d, want 1", n)
	}
	h.advance(30 * time.Second)
	h.tick()
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("the same status edited again: %d edits", n)
	}

	next := factoryStatus()
	next.FixLoop = prstatus.FixLoop{State: prstatus.FixDone, Cycle: 2, Pushes: 4, ThreadsAnswered: 8}
	next.Now.Who = prstatus.WhoYou
	s = h.writeWidgetStatus(12, next)
	h.advance(30 * time.Second)
	h.tick()
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("a change within 2 minutes of the last edit edited at once: %d edits", n)
	}
	h.advance(time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, h.clock.Now()))
	if n := h.widgetEdits(); n != 2 {
		t.Fatalf("edits = %d, want 2", n)
	}
	if got := h.factoryDigest(pr.ID); got != s.Digest {
		t.Fatalf("%s = %q, want %q", kvPRFactoryStatus(pr.ID), got, s.Digest)
	}
	var stages []string
	for _, ev := range h.events("pr.status_comment") {
		stages = append(stages, ev.Message)
	}
	if want := "status comment posted: scheduled (review of a1b2c3d)|status comment updated: scheduled (review of a1b2c3d)|" +
		"status comment updated: scheduled (review of a1b2c3d)"; strings.Join(stages, "|") != want {
		t.Fatalf("pr.status_comment events = %q", stages)
	}
}

// A status file that goes away drops the factory part at the next edit,
// and the PR keeps no digest.
func TestAFactoryStatusFileThatGoesAwayDropsThePart(t *testing.T) {
	h := widgetHarness(t)
	start := h.clock.Now()
	h.writeWidgetStatus(12, factoryStatus())
	pr := h.widgetQueued()
	magnums, _, _ := strings.Cut(strings.TrimPrefix(h.statusComment(12).body, statusHeader(start)), "\n\n**Magnum factory**")

	if err := os.Remove(h.widgetStatusPath(12)); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	h.tick()
	h.wantStatusSince(h.statusComment(12), start, magnums)
	if got := h.factoryDigest(pr.ID); got != "" {
		t.Fatalf("%s = %q after the file went", kvPRFactoryStatus(pr.ID), got)
	}
	h.advance(2 * time.Minute)
	h.tick()
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("edits = %d, want 1", n)
	}
}

// A file Read refuses is no factory part (one shown goes at the next edit)
// and one warning while it stays refused the same way, not one a tick; a
// new refusal is logged once more, a file that reads again brings the part
// back, and the same refusal after it is new again.
func TestARefusedFactoryStatusFileIsNoPartAndOneLog(t *testing.T) {
	logs := &syncBuffer{}
	h := widgetHarness(t, func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
	start := h.clock.Now()
	h.writeWidgetStatus(12, factoryStatus())
	pr := h.widgetQueued()
	magnums, _, _ := strings.Cut(strings.TrimPrefix(h.statusComment(12).body, statusHeader(start)), "\n\n**Magnum factory**")
	refused := "a factory PR's status file is refused"

	path := h.widgetStatusPath(12)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), `"questions_asked": 3`, `"questions_asked": 30`, 1)
	if tampered == string(b) {
		t.Fatalf("the file has no questions_asked to change:\n%s", b)
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		h.advance(time.Minute)
		h.tick()
	}
	h.wantStatusSince(h.statusComment(12), start, magnums)
	if n := logLines(logs, "WARN", refused); n != 1 {
		t.Fatalf("a refused file logged %d warnings over 4 ticks, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "digest does not match") || strings.Contains(logs.String(), `"questions_asked"`) {
		t.Fatalf("the warning says why in fixed words only:\n%s", logs.String())
	}
	if got := h.factoryDigest(pr.ID); got != "" {
		t.Fatalf("%s = %q for a refused file", kvPRFactoryStatus(pr.ID), got)
	}

	if err := os.WriteFile(path, []byte(`{"v": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := logLines(logs, "WARN", refused); n != 2 {
		t.Fatalf("a new refusal: %d warnings in all, want 2", n)
	}

	s := h.writeWidgetStatus(12, factoryStatus())
	h.advance(2 * time.Minute)
	h.tick()
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, h.clock.Now()))
	if err := os.WriteFile(path, []byte(`{"v": 2}`), 0o600); err != nil { // the refusal logged last
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	h.tick()
	if n := logLines(logs, "WARN", refused); n != 3 {
		t.Fatalf("the same refusal after a good read: %d warnings in all, want 3", n)
	}
	h.wantStatusSince(h.statusComment(12), start, magnums)
}

// A PR closed before its review gets its last edit with the factory part
// as the file says it then, and nothing after: a later change of the file
// edits nothing, as a later change of magnum's own lines would not.
func TestAClosedFactoryPRsCommentEndsWithItsFactoryPart(t *testing.T) {
	h := widgetHarness(t)
	h.writeWidgetStatus(12, factoryStatus())
	h.widgetQueued()
	h.advance(time.Minute)
	next := factoryStatus()
	next.Now.Who = prstatus.WhoMagnum
	s := h.writeWidgetStatus(12, next)
	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()})
	h.gh.closed[12] = "CLOSED"
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	if pr := h.widgetPR(12); pr.State != store.PRClosed {
		t.Fatalf("state = %s, want closed", pr.State)
	}
	ended := h.statusComment(12)
	if !strings.HasSuffix(ended.body, ")\nClosed before the review of `a1b2c3d` posted.\n\n"+factoryText(s, h.clock.Now())) {
		t.Fatalf("the last edit:\n%s", ended.body)
	}

	last := factoryStatus()
	last.Now = prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageStopped}
	h.writeWidgetStatus(12, last)
	for range 3 {
		h.advance(5 * time.Minute)
		h.tick()
	}
	if c := h.statusComment(12); c.edits != ended.edits || c.body != ended.body {
		t.Fatalf("the ended comment was edited again (%d edits, %d before):\n%s", c.edits, ended.edits, c.body)
	}
}
