package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
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
	if got := factoryText(s, "", factoryNow); got != want {
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
			got := factoryText(s, "", factoryNow)
			if row := "\n| " + c.step + " | " + c.want + " |\n"; !strings.Contains(got, row) {
				t.Fatalf("factory part:\n%s\nwant the row %q", got, strings.TrimSpace(row))
			}
		})
	}
}

// A repository with several staging environments gets one Staging row per
// env the status gives (Stagings), in its order, each named by the env's
// slug and saying that env's deploy in the fixed words; an env that is no
// slug is a row without its name. A status without Stagings (a writer from
// before them, a repository without staging envs) keeps the one Staging
// row of its Staging field.
func TestEachStagingEnvGetsItsOwnRowInTheStatussOrder(t *testing.T) {
	s := factoryStatus()
	s.Stagings = []prstatus.Staging{
		{Env: "void", State: prstatus.StagingNone},
		{Env: "bart", State: prstatus.StagingDeployed},
		{Env: "bastion", State: prstatus.StagingDeploying},
		{Env: "qa-2", State: prstatus.StagingFailed},
		{State: prstatus.StagingDeployed},
	}
	s.Staging = s.Stagings[0]
	want := "| Fix loop | running, cycle 2, 3 pushes, 7 threads answered |\n" +
		"| Staging · void | none |\n" +
		"| Staging · bart | deployed |\n" +
		"| Staging · bastion | deploying |\n" +
		"| Staging · qa-2 | failed |\n" +
		"| Staging | deployed |\n" +
		"\n" +
		"Now: "
	if got := factoryText(s, "", factoryNow); !strings.Contains(got, want) {
		t.Fatalf("factory part:\n%s\nwant the rows:\n%s", got, want)
	}

	s.Stagings = []prstatus.Staging{}
	if got := factoryText(s, "", factoryNow); !strings.Contains(got, "\n| Staging | none |\n\nNow: ") {
		t.Fatalf("an empty Stagings, factory part:\n%s\nwant the one row of Staging", got)
	}
}

// A change of one staging env's row alone is a change of the factory part:
// it edits the comment once, at the first tick the spacing allows, and the
// PR keeps the new part's sum while magnum's body's (statusRecord.Sum)
// stays. On a comment a person deleted it posts nothing: the record waits
// for magnum's next change (statusRecord.Repost), as for any change of the
// part alone.
func TestAStagingRowsChangeAloneEditsTheCommentOnce(t *testing.T) {
	h := widgetHarness(t)
	s := factoryStatus()
	s.Stagings = []prstatus.Staging{{Env: "void", State: prstatus.StagingDeployed}, {Env: "bart", State: prstatus.StagingDeploying}}
	s.Staging = s.Stagings[0]
	s = h.writeWidgetStatus(12, s)
	pr := h.widgetQueued()
	posted := h.e.statusRecordOf(h.ctx, pr.ID)
	shown := h.factoryShown(pr.ID)
	if c := h.statusComment(12); !strings.Contains(c.body, "\n| Staging · void | deployed |\n| Staging · bart | deploying |\n") {
		t.Fatalf("status comment:\n%s\nwant a row per staging env", c.body)
	}

	// write keeps the file's updated time: only the staging row changes.
	write := func(s prstatus.Status) {
		t.Helper()
		if wrote, err := prstatus.Write(h.widgetStatusPath(12), s); err != nil || !wrote {
			t.Fatalf("write the status file: %v, %v", wrote, err)
		}
	}
	s.Stagings[1].State = prstatus.StagingDeployed
	write(s)
	h.advance(2 * time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("edits = %d, want 1 for the staging row", n)
	}
	c := h.statusComment(12)
	if !strings.HasSuffix(c.body, "\n\n"+factoryText(s, "", h.clock.Now())) || !strings.Contains(c.body, "\n| Staging · bart | deployed |\n") {
		t.Fatalf("status comment:\n%s\nwant bart's row deployed", c.body)
	}
	if rec := h.e.statusRecordOf(h.ctx, pr.ID); rec.Sum != posted.Sum {
		t.Errorf("magnum's body sum = %q, want %q: its lines did not change", rec.Sum, posted.Sum)
	}
	if got := h.factoryShown(pr.ID); got == shown || got != statusSum(factoryText(s, "", h.clock.Now())) {
		t.Errorf("%s = %q, want the new part's sum (was %q)", kvPRFactoryStatus(pr.ID), got, shown)
	}

	h.gh.deleteComment(c.id)
	s.Stagings[0].State = prstatus.StagingFailed
	write(s)
	h.advance(2 * time.Minute)
	h.tick()
	h.advance(2 * time.Minute)
	h.tick()
	if cs := h.gh.commentsOn(12); len(cs) != 0 || h.gh.count("issue_comment:"+widgetRepo) != 1 {
		t.Fatalf("a staging row's change on a deleted comment: %d posts, comments %+v", h.gh.count("issue_comment:"+widgetRepo), cs)
	}
	if rec := h.e.statusRecordOf(h.ctx, pr.ID); rec.ID != 0 || !rec.Repost || rec.Sum != posted.Sum {
		t.Errorf("record = %+v, want it waiting for magnum's next change with magnum's sum", rec)
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
			got := factoryText(s, "", factoryNow)
			if !strings.HasSuffix(got, "\n\nNow: "+c.want) {
				t.Fatalf("factory part:\n%s\nwant it to end with:\nNow: %s", got, c.want)
			}
		})
	}
}

// On the comment's last edit the Now line says the end in magnum's words
// while the file still says the PR is open (the factory writes its own end
// after magnum's last edit), without the file's time, which is from before
// the end (the header has the end's); a file that says the task's own end
// keeps its words. The rows stay as the file says them.
func TestAnEndedFactoryPartsNowLineSaysTheEnd(t *testing.T) {
	at := " · updated 09:41 UTC"
	for _, c := range []struct {
		name, end string
		now       prstatus.Now
		want      string
	}{
		{"merged, the file still open", "merged", prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}, "merged"},
		{"closed, the file waits on magnum", "closed", prstatus.Now{Who: prstatus.WhoMagnum, Stage: prstatus.StagePublished}, "closed"},
		{"a draft", "a draft", prstatus.Now{Who: prstatus.WhoYou, Stage: prstatus.StagePublished}, "a draft"},
		{"merged, the file says so", "merged", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageDone}, "merged" + at},
		{"closed, the task stopped", "closed", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageStopped}, "the task stopped" + at},
		{"closed, the task failed", "closed", prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageFailed}, "the task failed" + at},
		{"goes on", "", prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}, "waits on the reviewers" + at},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := factoryStatus()
			s.Now = c.now
			got := factoryText(s, c.end, factoryNow)
			if !strings.HasSuffix(got, "\n\nNow: "+c.want) {
				t.Fatalf("factory part:\n%s\nwant it to end with:\nNow: %s", got, c.want)
			}
			open, _, _ := strings.Cut(factoryText(s, "", factoryNow), "\n\nNow: ")
			if table, _, _ := strings.Cut(got, "\n\nNow: "); table != open {
				t.Fatalf("the ended part's rows:\n%s\nwant:\n%s", table, open)
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
		if got, _, _ := strings.Cut(factoryText(s, "", factoryNow), "\n"); got != c.want {
			t.Errorf("%+v: heading %q, want %q", c.jira, got, c.want)
		}
	}
}

// Every enum word the contract declares gets its own fixed words, and no
// map holds a word the contract lacks: the words come from internal/
// prstatus's source (contractWords), so a word the contract gains fails
// here until the comment can say it, instead of posting "not known".
func TestEveryFactoryEnumWordHasItsWords(t *testing.T) {
	words := contractWords(t)
	mapped := map[string]bool{}
	check := func(typ string, m map[string]string, valid func(string) bool) {
		t.Helper()
		mapped[typ] = true
		want := words[typ]
		if len(want) == 0 {
			t.Errorf("the contract declares no word of %s", typ)
		}
		for _, w := range want {
			if !valid(w) {
				t.Errorf("%s %q, read from the contract's source, is not Valid", typ, w)
			}
		}
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Errorf("%s: the word map has %q, the contract %q", typ, got, want)
		}
		seen := map[string]bool{}
		for w, says := range m {
			if (says == "" && !(typ == "Stage" && w == string(prstatus.StagePublished))) || seen[says] {
				t.Errorf("%s %q says %q: empty or not distinct", typ, w, says)
			}
			seen[says] = true
		}
	}
	check("Proof", stringKeys(factoryProofWords), func(w string) bool { return prstatus.Proof(w).Valid() })
	check("Verdict", stringKeys(factoryVerdictWords), func(w string) bool { return prstatus.Verdict(w).Valid() })
	check("FixState", stringKeys(factoryFixWords), func(w string) bool { return prstatus.FixState(w).Valid() })
	check("StagingState", stringKeys(factoryStagingWords), func(w string) bool { return prstatus.StagingState(w).Valid() })
	check("Who", stringKeys(factoryWhoWords), func(w string) bool { return prstatus.Who(w).Valid() })
	check("JiraStatus", stringKeys(factoryJiraWords), func(w string) bool { return prstatus.JiraStatus(w).Valid() })
	check("Stage", stringKeys(factoryStageWords), func(w string) bool { return prstatus.Stage(w).Valid() })
	for typ := range words {
		if !mapped[typ] {
			t.Errorf("the contract's %s has no word map", typ)
		}
	}
	if w, ok := factoryStageWords[prstatus.StagePublished]; !ok || w != "" {
		t.Errorf("the published stage says %q (listed %v), want nothing", w, ok)
	}
}

// stringKeys is m with its keys as plain strings.
func stringKeys[K ~string](m map[K]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

// contractWords is every enum word internal/prstatus declares, by its
// type's name, read from the package's source files: the string constants
// of each const block, a constant without a type of its own counted as the
// block's type (Valid would take it as one).
func contractWords(t *testing.T) map[string][]string {
	t.Helper()
	dir := filepath.Join("..", "prstatus")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, ent := range ents {
		if name := ent.Name(); !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			typ := ""
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); ok {
					typ = id.Name
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING || typ == "" {
						continue
					}
					w, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[typ] = append(out[typ], w)
				}
			}
		}
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
		s.Stagings = []prstatus.Staging{s.Staging, {Env: "bart", State: prstatus.StagingState(h)}}
		s.Now = prstatus.Now{Who: prstatus.Who(h), Stage: prstatus.Stage(h)}
		got := factoryText(s, "", factoryNow)
		if got == "" {
			t.Fatalf("%q: no factory part", h)
		}
		for _, bad := range []string{h, "@", "<", "](", "**loud"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q: the factory part carries %q:\n%s", h, bad, got)
			}
		}
		if !strings.Contains(got, "\n| Code | 6 commits, 3 specs: not known |\n") || !strings.HasPrefix(got, "**Magnum factory**\n") ||
			!strings.Contains(got, "\n| Staging | not known |\n| Staging · bart | not known |\n") {
			t.Errorf("%q: the unknown words do not read as not known:\n%s", h, got)
		}
	}
	s := factoryStatus()
	s.Jira = prstatus.Jira{Key: "ABC-12", Status: prstatus.JiraDone}
	s.Staging.Env = "Beta Env"
	if got := factoryText(s, "", factoryNow); !strings.Contains(got, "\n| Staging | deployed |\n") || !strings.Contains(got, "Jira ABC-12: done") {
		t.Errorf("an env that is no slug, or a key that is one:\n%s", got)
	}
	s.Stagings = []prstatus.Staging{s.Staging}
	if got := factoryText(s, "", factoryNow); !strings.Contains(got, "\n| Staging | deployed |\n") {
		t.Errorf("a staging env that is no slug:\n%s", got)
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

// factoryShown is what kvPRFactoryStatus holds for the PR: the sum of the
// factory part its comment carries ("" = none).
func (h *harness) factoryShown(prID int64) string {
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
	if want := factoryText(s, "", h.clock.Now()); "**Magnum factory**"+part != want {
		t.Fatalf("factory part:\n%s\nwant:\n%s", "**Magnum factory**"+part, want)
	}
	if got, want := h.factoryShown(pr.ID), statusSum(factoryText(s, "", h.clock.Now())); got != want {
		t.Fatalf("%s = %q, want the part's sum %q", kvPRFactoryStatus(pr.ID), got, want)
	}

	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: start}, prSpec{n: 12, head: statusHead, updated: start},
		prSpec{n: 13, head: statusPush, updated: h.clock.Now()})
	h.tick()
	other := h.statusComment(13)
	if strings.Contains(other.body, "Magnum factory") || !strings.HasSuffix(other.body, "\n"+statusMerge) {
		t.Fatalf("a PR without a status file:\n%s", other.body)
	}
	if got := h.factoryShown(h.widgetPR(13).ID); got != "" {
		t.Fatalf("a PR without a status file keeps the part sum %q", got)
	}
}

// A status file never opens a comment where magnum would post none: on a
// draft nobody asked to review, whether the watch skips drafts or queues
// them, on a watch with the comment off, and on a PR reviewed before the
// watch switched the comment on (its posted line only edits a comment).
func TestAFactoryStatusFileNeverOpensAComment(t *testing.T) {
	storetest.Parallel(t)
	t.Run("a draft the watch queues", func(t *testing.T) {
		h := widgetHarness(t, func(h *harness) { h.cfg.Watches[len(h.cfg.Watches)-1].IncludeDrafts = new(true) })
		h.writeWidgetStatus(12, factoryStatus())
		h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()})
		h.startup()
		h.tick()
		h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()}, prSpec{n: 12, head: statusHead, draft: true, updated: h.clock.Now()})
		h.tick()
		if pr := h.widgetPR(12); pr.State != store.PRQueued {
			t.Fatalf("state = %s, want queued", pr.State)
		}
		h.advance(3 * time.Minute)
		h.tick()
		if cs := h.gh.commentsOn(12); len(cs) != 0 {
			t.Fatalf("a draft nobody asked about got %+v", cs)
		}
	})
	t.Run("reviewed before the comment was on", func(t *testing.T) {
		h := newHarness(t, statusOn, withExampleWatch, func(h *harness) { h.cfg.Watches[len(h.cfg.Watches)-1].StatusComment = new(false) })
		h.widgetQueued()
		h.advance(5 * time.Minute)
		h.tick() // the round runs and posts
		if pr := h.widgetPR(12); pr.State != store.PRReviewed {
			t.Fatalf("state = %s, want reviewed", pr.State)
		}
		h.writeWidgetStatus(12, factoryStatus())
		h.cfg.Watches[len(h.cfg.Watches)-1].StatusComment = new(true)
		for range 2 {
			h.advance(3 * time.Minute)
			h.tick()
		}
		if cs := h.gh.commentsOn(12); len(cs) != 0 {
			t.Fatalf("a PR reviewed before the comment was on got %+v", cs)
		}
	})
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

// An unchanged status file costs no edit, whatever the factory rewrites,
// but the one that dates its time: the first tick of the next UTC day
// that the comment's spacing allows gives the "updated" time its day, as
// magnum's own times get theirs, once, and later ticks edit nothing.
func TestAnUnchangedFactoryStatusEditsOnlyToDateItsTime(t *testing.T) {
	h := widgetHarness(t)
	s := h.writeWidgetStatus(12, factoryStatus())
	pr := h.widgetQueued()
	posted := h.statusComment(12)
	for range 4 {
		h.advance(3 * time.Minute)
		h.writeWidgetStatus(12, factoryStatus()) // the same content: Write writes nothing
		h.tick()
	}
	if n := h.widgetEdits(); n != 0 {
		t.Fatalf("an unchanged status file edited the comment %d times:\n%s", n, h.statusComment(12).body)
	}
	if c := h.statusComment(12); c.body != posted.body || !strings.Contains(c.body, "\n\nNow: waits on the reviewers · updated "+utcClock(s.UpdatedAt)) {
		t.Fatalf("status comment:\n%s", c.body)
	}

	h.advance(24 * time.Hour)
	h.tick()
	for range 3 {
		h.advance(3 * time.Minute)
		h.tick()
	}
	if n := h.widgetEdits(); n != 1 {
		t.Fatalf("the next day: %d edits, want 1:\n%s", n, h.statusComment(12).body)
	}
	magnums, _, _ := strings.Cut(posted.body, "\n\n**Magnum factory**")
	dated := factoryText(s, "", h.clock.Now())
	if c := h.statusComment(12); c.body != magnums+"\n\n"+dated || !strings.HasSuffix(c.body, "\nNow: waits on the reviewers · updated "+statusDay(s.UpdatedAt)) {
		t.Fatalf("the next day's comment:\n%s", c.body)
	}
	if got := h.factoryShown(pr.ID); got != statusSum(dated) {
		t.Fatalf("%s = %q, want the dated part's sum", kvPRFactoryStatus(pr.ID), got)
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
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, "", h.clock.Now()))
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
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, "", h.clock.Now()))
	if n := h.widgetEdits(); n != 2 {
		t.Fatalf("edits = %d, want 2", n)
	}
	if got, want := h.factoryShown(pr.ID), statusSum(factoryText(s, "", h.clock.Now())); got != want {
		t.Fatalf("%s = %q, want %q", kvPRFactoryStatus(pr.ID), got, want)
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
// and the PR keeps no part sum.
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
	if got := h.factoryShown(pr.ID); got != "" {
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
	if got := h.factoryShown(pr.ID); got != "" {
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
	h.wantStatusSince(h.statusComment(12), start, magnums+"\n\n"+factoryText(s, "", h.clock.Now()))
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

// widgetEnds closes example/widget#12 on GitHub (state CLOSED or MERGED)
// and ticks until magnum has it closed.
func (h *harness) widgetEnds(state string) {
	h.t.Helper()
	h.gh.set(widgetRepo, prSpec{n: 1, head: "w1", updated: h.clock.Now()})
	h.gh.closed[12] = state
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	if pr := h.widgetPR(12); pr.State != store.PRClosed {
		h.t.Fatalf("state = %s, want closed", pr.State)
	}
}

// A factory PR's comment ends on a true Now line: the last edit, at once,
// says the end in magnum's words while the file still says the PR is open,
// even where magnum's own lines stand as they were (merged after its
// review), and nothing after it: the factory's own end, written a tick
// later, edits nothing, as a later change of magnum's lines would not.
func TestAnEndedFactoryPRsCommentEndsOnATrueNowLine(t *testing.T) {
	storetest.Parallel(t)
	wantEnded := func(t *testing.T, h *harness, ended fakeComment) {
		t.Helper()
		last := factoryStatus()
		last.Now = prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageDone}
		h.writeWidgetStatus(12, last)
		for range 3 {
			h.advance(5 * time.Minute)
			h.tick()
		}
		if c := h.statusComment(12); c.edits != ended.edits || c.body != ended.body {
			t.Fatalf("the ended comment was edited again (%d edits, %d before):\n%s", c.edits, ended.edits, c.body)
		}
	}

	t.Run("closed before the review", func(t *testing.T) {
		h := widgetHarness(t)
		h.writeWidgetStatus(12, factoryStatus())
		h.widgetQueued()
		h.advance(time.Minute)
		next := factoryStatus()
		next.Now.Who = prstatus.WhoMagnum
		s := h.writeWidgetStatus(12, next)
		h.widgetEnds("CLOSED")
		ended := h.statusComment(12)
		if !strings.HasSuffix(ended.body, ")\nClosed before the review of `a1b2c3d` posted.\n\n"+factoryText(s, "closed", h.clock.Now())) ||
			!strings.HasSuffix(ended.body, "\n\nNow: closed") {
			t.Fatalf("the last edit:\n%s", ended.body)
		}
		wantEnded(t, h, ended)
	})

	t.Run("merged after the review", func(t *testing.T) {
		h := newHarness(t, statusOn, withExampleWatch)
		s := h.writeWidgetStatus(12, factoryStatus())
		h.widgetQueued()
		h.advance(5 * time.Minute)
		h.tick() // the round runs and posts
		h.advance(2 * time.Minute)
		h.tick()
		pr := h.widgetPR(12)
		if pr.State != store.PRReviewed {
			t.Fatalf("state = %s, want reviewed", pr.State)
		}
		posted := h.statusComment(12)
		line := "\nReview of `a1b2c3d` posted " + utcClock(*pr.ReviewedAt)
		if !strings.Contains(posted.body, line) || !strings.HasSuffix(posted.body, "\n\n"+factoryText(s, "", h.clock.Now())) {
			t.Fatalf("after the review:\n%s", posted.body)
		}

		h.widgetEnds("MERGED")
		ended := h.statusComment(12)
		magnums, _, _ := strings.Cut(posted.body, "\n\n**Magnum factory**")
		if ended.edits != posted.edits+1 || ended.body != magnums+"\n\n"+factoryText(s, "merged", h.clock.Now()) ||
			!strings.HasSuffix(ended.body, "\n\nNow: merged") {
			t.Fatalf("the last edit (%d edits, %d before):\n%s", ended.edits, posted.edits, ended.body)
		}
		wantEnded(t, h, ended)
	})

	t.Run("the task's own end", func(t *testing.T) {
		h := widgetHarness(t)
		h.writeWidgetStatus(12, factoryStatus())
		h.widgetQueued()
		h.advance(time.Minute)
		stopped := factoryStatus()
		stopped.Now = prstatus.Now{Who: prstatus.WhoNone, Stage: prstatus.StageStopped}
		s := h.writeWidgetStatus(12, stopped)
		h.widgetEnds("CLOSED")
		if c := h.statusComment(12); !strings.HasSuffix(c.body, "\n\nNow: the task stopped · updated "+utcClock(s.UpdatedAt)) {
			t.Fatalf("the last edit:\n%s", c.body)
		}
	})
}

// A comment a person deleted is no comment the factory part alone posts
// again: an edit only the part asked for finds it gone and posts nothing,
// whatever the factory writes after; magnum's next change of its own lines
// (here a mute's line, which opens no comment) posts it once more, part
// included, as its edit would have without the file, and a close then
// posts nothing, as a last edit of a deleted comment does not.
func TestADeletedCommentIsPostedOnceMoreOnlyForMagnumsChange(t *testing.T) {
	storetest.Parallel(t)
	// deleted posts #12's comment with its factory part, has a person
	// delete it and the factory write three changes, and returns the last.
	deleted := func(t *testing.T, h *harness) prstatus.Status {
		t.Helper()
		h.writeWidgetStatus(12, factoryStatus())
		h.widgetQueued()
		h.gh.deleteComment(h.statusComment(12).id)
		var s prstatus.Status
		for _, who := range []prstatus.Who{prstatus.WhoYou, prstatus.WhoCI, prstatus.WhoMagnum} {
			next := factoryStatus()
			next.Now.Who = who
			s = h.writeWidgetStatus(12, next)
			h.advance(2 * time.Minute)
			h.tick()
		}
		if cs := h.gh.commentsOn(12); len(cs) != 0 || h.gh.count("issue_comment:"+widgetRepo) != 1 || h.widgetEdits() != 1 {
			t.Fatalf("the factory part alone: %d posts, %d edits, comments %+v", h.gh.count("issue_comment:"+widgetRepo), h.widgetEdits(), cs)
		}
		return s
	}

	t.Run("magnum's change", func(t *testing.T) {
		h := widgetHarness(t)
		s := deleted(t, h)
		id := h.enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: widgetRepo + "#12"}})
		h.tick()
		if r := h.request(id); r.State != store.RequestDone {
			t.Fatalf("mute: %s %q", r.State, deref(r.Result))
		}
		h.wantStatus(h.statusComment(12), "Magnum will not review this head. The operator was told.\n\n"+factoryText(s, "", h.clock.Now()))
	})

	t.Run("a close", func(t *testing.T) {
		h := widgetHarness(t)
		deleted(t, h)
		h.widgetEnds("CLOSED")
		for range 3 {
			h.advance(5 * time.Minute)
			h.tick()
		}
		if cs := h.gh.commentsOn(12); len(cs) != 0 || h.gh.count("issue_comment:"+widgetRepo) != 1 {
			t.Fatalf("a close after the deletion: %d posts, comments %+v", h.gh.count("issue_comment:"+widgetRepo), cs)
		}
		if rec := h.e.statusRecordOf(h.ctx, h.widgetPR(12).ID); rec.End == "" || rec.Repost {
			t.Fatalf("the record does not end: %+v", rec)
		}
	})
}

// A factory part's change that GitHub refused is written at the next try:
// the comment keeps nothing of it, and pr.<id>.factory_status keeps the
// part the comment carries, until GitHub takes the edit.
func TestARefusedFactoryEditIsWrittenAtTheNextTry(t *testing.T) {
	h := widgetHarness(t)
	h.writeWidgetStatus(12, factoryStatus())
	pr := h.widgetQueued()
	shown := h.factoryShown(pr.ID)
	posted := h.statusComment(12)

	h.advance(2 * time.Minute)
	next := factoryStatus()
	next.FixLoop = prstatus.FixLoop{State: prstatus.FixDone, Cycle: 2, Pushes: 4, ThreadsAnswered: 8}
	s := h.writeWidgetStatus(12, next)
	h.gh.commentErr = &github.APIError{Op: "comment", Status: 502, Message: "Bad Gateway"}
	h.tick()
	if h.widgetEdits() != 1 || h.statusComment(12).body != posted.body || h.factoryShown(pr.ID) != shown {
		t.Fatalf("a refused edit: %d attempts, %s = %q (was %q):\n%s", h.widgetEdits(), kvPRFactoryStatus(pr.ID),
			h.factoryShown(pr.ID), shown, h.statusComment(12).body)
	}

	h.gh.commentErr = nil
	h.advance(2 * time.Minute)
	h.tick()
	magnums, _, _ := strings.Cut(posted.body, "\n\n**Magnum factory**")
	if c := h.statusComment(12); c.body != magnums+"\n\n"+factoryText(s, "", h.clock.Now()) {
		t.Fatalf("the next try:\n%s", c.body)
	}
	if got, want := h.factoryShown(pr.ID), statusSum(factoryText(s, "", h.clock.Now())); got != want {
		t.Fatalf("%s = %q, want %q", kvPRFactoryStatus(pr.ID), got, want)
	}
}
