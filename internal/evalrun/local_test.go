package evalrun

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// localRun is what the local round's engine got: its scratch layout, its
// agents' tag and the round.
type localRun struct {
	layout paths.Layout
	tag    string
	round  engine.LocalRound
}

// localFixture is the deps of a local round whose engine is a fake: it
// records each round and answers result (nil: a dry run whose judge wrote
// result as its result file, "" for none). The watch's identity is an App;
// a gh identity is configured beside it.
func localFixture(t *testing.T, result string) (Deps, *[]localRun) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Identities = []config.Identity{
		{Name: "talkable-app", Kind: "app", Login: "talkable[bot]"},
		{Name: "zhuravel", Kind: "gh", Login: "zhuravel"},
	}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "talkable-app", PollIdentity: "talkable-app"}}
	var mu sync.Mutex
	var runs []localRun
	d := Deps{Layout: paths.Layout{Home: t.TempDir()}, Config: cfg}
	d.localRound = func(_ context.Context, _ *config.Config, l paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
		mu.Lock()
		runs = append(runs, localRun{layout: l, tag: tag, round: lr})
		mu.Unlock()
		dir := l.ReviewDir(lr.Owner, lr.Repo, lr.Number, lr.Head)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return pipeline.RoundResult{}, err
		}
		if result != "" {
			if err := os.WriteFile(filepath.Join(dir, "codex-judge.json"), []byte(result), 0o600); err != nil {
				return pipeline.RoundResult{}, err
			}
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, ReportDir: dir}, nil
	}
	return d, &runs
}

// localCase is round n of task 7's change in a copy.
func localCase(t *testing.T, n int) LocalCase {
	t.Helper()
	return LocalCase{Checkout: t.TempDir(), BaseSHA: strings.Repeat("cd", 20), HeadSHA: strings.Repeat("ab", 20),
		Branch: "zhuravel-PR-12361-credit-points", Title: "PR-12361 Credit points count the activities", Body: "## Plan\n\nCount them.",
		TicketMD: "# PR-12361\n\nCount the activities.", Watch: config.Watch{Owner: "example", Identity: "talkable-app"},
		Roles: []string{"claude-review", "codex-review", "codex-judge"}, Round: n, TaskID: 7, Repo: "example/widgets", Base: "master",
		Env: map[string]string{"WT_BRANCH": "factory1", "REDIS_DB_OFFSET": "16"}}
}

const cleanResult = `{"status":"dry_run","verdict":"clean","findings":{"P0":0,"P1":0,"P2":0,"P3":0},"planned_review":{"event":"APPROVE","body":"No problems found.","comments":[]}}`

// Each local round is its own: a fresh scratch directory under the task's,
// a new agent tag, and the synthetic PR number 900000 + task id × 10 +
// round, so two rounds never share a registry, an agent, a herdr
// workspace label or an eval case lock.
// A judge's result whose planned review this code does not read (a shape
// eval.ParseResult refuses) still hands the writer the findings the
// provenance names, with their severity, title, path and line (no body):
// one synthetic "review's body" finding the writer cannot act on would
// spend the loop's rounds to its cap.
func TestALocalResultWhosePlannedReviewDoesNotParseKeepsItsFindings(t *testing.T) {
	data := []byte(`{"status":"dry_run","verdict":"blocking","event":"REQUEST_CHANGES",
 "findings":{"P0":0,"P1":1,"P2":0,"P3":0},
 "provenance":[{"id":"F1","title":"Points skip the refunded activities","severity":"P1","path":"app/credit.rb","line":42,"verdict":"posted"}],
 "planned_review":{"event":"REQUEST_CHANGES","body":"Blocking: 1 problem.","comments":{"path":"app/credit.rb"}}}`)
	r := ParseLocalResult(data)
	if r.Verdict != "blocking" || len(r.Findings) != 1 {
		t.Fatalf("result %+v, want the blocking verdict and the provenance's one finding", r)
	}
	if f := r.Findings[0]; f.Severity != "P1" || f.Title != "Points skip the refunded activities" || f.Path != "app/credit.rb" || f.Line != 42 {
		t.Fatalf("finding %+v, want the provenance's P1 on app/credit.rb:42", f)
	}
}

func TestRunLocalSeedsAFreshScratchPerRound(t *testing.T) {
	d, runs := localFixture(t, cleanResult)
	for _, n := range []int{1, 2} {
		res, err := RunLocal(t.Context(), d, localCase(t, n))
		if err != nil || res.Outcome != pipeline.OutcomeDryRun || res.Verdict != "clean" || res.Round != n {
			t.Fatalf("round %d: %+v, %v", n, res, err)
		}
	}
	if len(*runs) != 2 {
		t.Fatalf("rounds: %d", len(*runs))
	}
	a, b := (*runs)[0], (*runs)[1]
	if a.layout.Scratch == "" || a.layout.Scratch == b.layout.Scratch {
		t.Fatalf("scratch dirs %q and %q, want two of their own", a.layout.Scratch, b.layout.Scratch)
	}
	for _, r := range *runs {
		if !strings.HasPrefix(r.layout.Scratch, d.Layout.FactoryTaskDir(7)+string(filepath.Separator)) {
			t.Errorf("scratch %s is not under the task's directory", r.layout.Scratch)
		}
		if r.layout.State() != d.Layout.State() {
			t.Errorf("the round's locks and logs are at %s, want the install's %s", r.layout.State(), d.Layout.State())
		}
	}
	if a.tag == "" || a.tag == b.tag {
		t.Fatalf("agent tags %q and %q, want two of their own", a.tag, b.tag)
	}
	if a.round.Number != 900071 || b.round.Number != 900072 || LocalNumber(7, 2) != 900072 {
		t.Fatalf("numbers %d and %d, want 900071 and 900072", a.round.Number, b.round.Number)
	}
	if !strings.Contains(a.round.URL, "/pull/900071") || a.round.Owner != "example" || a.round.Repo != "widgets" {
		t.Fatalf("round 1's case %+v", a.round.EvalCase)
	}
}

// The judge reads the change's title and body and its ticket as files the
// round writes in its scratch directory (description.md, ticket.md); the
// round runs as a gh identity (the watch's is an App, whose token magnum
// would mint from GitHub), on the branch as its head ref, at the change's
// head and base in the task's copy, with this round's roles. A change
// without a ticket gets no ticket file.
func TestRunLocalWritesTheTitleBodyAndTicketAsFiles(t *testing.T) {
	d, runs := localFixture(t, cleanResult)
	c := localCase(t, 1)
	if _, err := RunLocal(t.Context(), d, c); err != nil {
		t.Fatal(err)
	}
	lr := (*runs)[0].round
	desc, err := os.ReadFile(lr.DescriptionFile)
	if err != nil || string(desc) != "# "+c.Title+"\n\n"+c.Body+"\n" {
		t.Fatalf("description.md = %q, %v", desc, err)
	}
	ticket, err := os.ReadFile(lr.TicketFile)
	if err != nil || string(ticket) != c.TicketMD+"\n" {
		t.Fatalf("ticket.md = %q, %v", ticket, err)
	}
	for _, f := range []string{lr.DescriptionFile, lr.TicketFile} {
		if filepath.Dir(f) != (*runs)[0].layout.Scratch {
			t.Errorf("%s is not in the round's scratch directory %s", f, (*runs)[0].layout.Scratch)
		}
	}
	if !maps.Equal(lr.Env, c.Env) {
		t.Fatalf("the round's env %v, want the copy's %v", lr.Env, c.Env)
	}
	if lr.Identity != "zhuravel" || lr.HeadRef != c.Branch || lr.Title != c.Title || lr.Head != c.HeadSHA || lr.BaseSHA != c.BaseSHA ||
		lr.Checkout != c.Checkout || lr.BaseRef != "master" || !slices.Equal(lr.Watch.Roles, c.Roles) {
		t.Fatalf("round %+v", lr)
	}

	c = localCase(t, 2)
	c.TicketMD = ""
	if _, err := RunLocal(t.Context(), d, c); err != nil {
		t.Fatal(err)
	}
	if lr := (*runs)[1].round; lr.TicketFile != "" {
		t.Fatalf("a change without a ticket got %s", lr.TicketFile)
	}
}

// A round reads its judge's result into the factory's terms: the verdict
// and the counts (store.ParseReviewResult), each posted finding with its
// severity, title, path and line (the planned review's comments and the
// provenance), a simplification, and a question the judge marked one. A
// round whose judge wrote nothing readable has no verdict.
func TestRunLocalReadsTheJudgesResult(t *testing.T) {
	const result = `{"status":"dry_run","verdict":"blocking","findings":{"P0":0,"P1":1,"P2":0,"P3":1},
"provenance":[
 {"id":"F1","title":"Total skips tax","severity":"P1","path":"app/models/order.rb","line":42,"sources":["claude-review"],"verdict":"posted"},
 {"id":"F2","title":"Should refunds count?","severity":"P3","path":"app/models/credit.rb","line":7,"sources":["judge"],"verdict":"posted","question":true},
 {"id":"F3","title":"Dropped","severity":"P2","path":"lib/x.rb","line":3,"sources":["codex-review"],"verdict":"rejected","reason_code":"pre_existing"}],
"planned_review":{"event":"APPROVE","body":"Blocking: 1 problem must be fixed before merging.","comments":[
 {"path":"app/models/order.rb","line":42,"body":"**[P1] Total skips tax**\n\nThe total leaves tax out."},
 {"path":"app/models/credit.rb","line":7,"body":"**[P3] Should refunds count? (optional, no reply needed)**\n\nThe ticket is silent."},
 {"path":"app/models/credit.rb","line":12,"body":"**Simplification** (keeps behavior: apply it, or reply with the risk)\n\nOne branch less."}]}}`
	d, _ := localFixture(t, result)
	res, err := RunLocal(t.Context(), d, localCase(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != "blocking" || res.Counts != [4]int{0, 1, 0, 1} || res.ReportDir == "" {
		t.Fatalf("result %+v", res)
	}
	if len(res.Findings) != 3 {
		t.Fatalf("findings %+v, want the two posted and the simplification", res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != "P1" || f.Title != "Total skips tax" || f.Path != "app/models/order.rb" || f.Line != 42 || !strings.Contains(f.Body, "leaves tax out") ||
		f.Question || f.Simplification {
		t.Errorf("finding 1 %+v", f)
	}
	if q := res.Findings[1]; !q.Question || q.Severity != "P3" || q.Title != "Should refunds count?" {
		t.Errorf("finding 2 %+v, want the question", q)
	}
	if s := res.Findings[2]; !s.Simplification || s.Line != 12 {
		t.Errorf("finding 3 %+v, want the simplification", s)
	}

	d, _ = localFixture(t, "not json")
	if res, err := RunLocal(t.Context(), d, localCase(t, 1)); err != nil || res.Verdict != "" {
		t.Fatalf("an unreadable result: %+v, %v; want no verdict", res, err)
	}
}

// A case the round cannot run never starts: no copy, no base or head, no
// task, no repository, no round.
func TestRunLocalRefusesACaseItCannotRun(t *testing.T) {
	d, runs := localFixture(t, cleanResult)
	for name, bad := range map[string]func(*LocalCase){
		"no checkout": func(c *LocalCase) { c.Checkout = "" }, "no base": func(c *LocalCase) { c.BaseSHA = "" },
		"no head": func(c *LocalCase) { c.HeadSHA = "master" }, "no task": func(c *LocalCase) { c.TaskID = 0 },
		"no repo": func(c *LocalCase) { c.Repo = "widgets" }, "no round": func(c *LocalCase) { c.Round = 0 },
	} {
		c := localCase(t, 1)
		bad(&c)
		if _, err := RunLocal(t.Context(), d, c); err == nil {
			t.Errorf("%s: the round ran", name)
		}
	}
	if len(*runs) != 0 {
		t.Fatalf("rounds ran: %d", len(*runs))
	}
	d.Config.Identities = d.Config.Identities[:1] // the App's only
	_, err := RunLocal(t.Context(), d, localCase(t, 1))
	var ce *ConfigError
	if !errors.As(err, &ce) || !strings.Contains(ce.Key, "[[identity]]") || len(*runs) != 0 {
		t.Fatalf("no gh identity: %v (%d rounds); want a configuration error naming [[identity]]", err, len(*runs))
	}
}
