package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/prstatus"
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
