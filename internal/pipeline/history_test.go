package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

func readHistory(t *testing.T, path string) (FilesHistory, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var h FilesHistory
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return h, string(b)
}

// noHistory fails when the round wrote history.json in dir or a prompt
// names one.
func (e *env) noHistory(dir string) {
	e.t.Helper()
	if _, err := os.Stat(filepath.Join(dir, HistoryFile)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("%s: %v, want none", HistoryFile, err)
	}
	for _, role := range []agents.Role{agents.RoleJudge, agents.RoleClaude} {
		for _, p := range e.ag.submitsFor(role) {
			if strings.Contains(p.Text, HistoryFile) {
				e.t.Errorf("a %s prompt names the history:\n%s", role, p.Text)
			}
		}
	}
}

// In 4 of the 5 known misses the changed file's own log named the fix the PR
// undid. Before the reviewers, a round writes history.json: each file the PR
// changes that exists on its base, related_ignore's paths aside, with its
// last 8 commits on origin/<base> and the PR number a squash merge's subject
// ends with. Both judge prompts of a two-phase round name it as `history`,
// claude-review's prompt in one sentence; no event carries a subject.
func TestARoundWritesTheChangedFilesHistory(t *testing.T) {
	e := newEnv(t)
	e.git.modified = []string{"Gemfile.lock", "app/models/order.rb", "spec/support/database_utils.rb"}
	e.git.logs = map[string][]gitx.Commit{
		"Gemfile.lock": {{SHA: "aaa1111", Date: "2026-10-01", Subject: "Bump rack (#12000)"}},
		"app/models/order.rb": {
			{SHA: "bbb2222", Date: "2026-09-30", Subject: "Skip irrelevant discounts in PriceRuleManager#lookup (#10889)"},
			{SHA: "ccc3333", Date: "2026-09-01", Subject: `Revert "Cache orders (#10001)" for now`},
		},
		"spec/support/database_utils.rb": {
			{SHA: "ddd4444", Date: "2026-08-15", Subject: "Fix flaky mock generation (#11391)"},
			{SHA: "eee5555", Date: "2026-08-01", Subject: "Stop CI hang from contended OPTIMIZE TABLE in retriever_spec (#11630)"},
			{SHA: "fff6666", Date: "2026-07-01", Subject: ""},
		},
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(811, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	in := e.ownInput(KindInitial)
	in.Related = defaultRelated

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, HistoryFile)
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 2 {
		t.Fatalf("judge prompts = %d, want the own pass and the candidates", len(judge))
	}
	for i, p := range judge {
		mustContain(t, fmt.Sprintf("judge prompt %d", i), p.Text, "\nhistory: "+path+"\n")
	}
	mustContain(t, "claude-review prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text,
		"The last commits on the base branch that touched each changed file are in "+path+": ")

	got, _ := readHistory(t, path)
	want := FilesHistory{PR: "talkable/talkable#11920", HeadSHA: target, Base: "origin/master", Files: []FileHistory{
		{Path: "app/models/order.rb", Commits: []HistoryCommit{
			{SHA: "bbb2222", Date: "2026-09-30", Subject: "Skip irrelevant discounts in PriceRuleManager#lookup (#10889)", PR: 10889},
			{SHA: "ccc3333", Date: "2026-09-01", Subject: `Revert "Cache orders (#10001)" for now`},
		}},
		{Path: "spec/support/database_utils.rb", Commits: []HistoryCommit{
			{SHA: "ddd4444", Date: "2026-08-15", Subject: "Fix flaky mock generation (#11391)", PR: 11391},
			{SHA: "eee5555", Date: "2026-08-01", Subject: "Stop CI hang from contended OPTIMIZE TABLE in retriever_spec (#11630)", PR: 11630},
			{SHA: "fff6666", Date: "2026-07-01"},
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history.json = %+v\nwant %+v", got, want)
	}
	diffs, logged := e.git.gitCalls()
	if want := []string{in.BaseSHA + "..." + target}; !slices.Equal(diffs, want) {
		t.Errorf("ModifiedPaths calls = %q, want %q", diffs, want)
	}
	if want := []string{"origin/master 8 app/models/order.rb", "origin/master 8 spec/support/database_utils.rb"}; !slices.Equal(logged, want) {
		t.Errorf("FileLog calls = %q, want %q (no lockfile)", logged, want)
	}
	evs := e.eventsOf("round.history")
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("round.history events = %+v", evs)
	}
	mustContain(t, "round.history", evs[0].Message, HistoryFile, "2 changed file(s)", "origin/master")
	for _, ev := range e.events() {
		if strings.Contains(ev.Message+string(ev.Data), "flaky") {
			t.Errorf("event %s carries a commit subject: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}
}

// history.json holds at most 40 files, in git's order, and counts the rest;
// each file has at most its last 8 commits, none as an empty list.
func TestTheHistoryCapsFilesAndCommits(t *testing.T) {
	e := newEnv(t)
	for i := range 45 {
		e.git.modified = append(e.git.modified, fmt.Sprintf("lib/f%02d.rb", i))
	}
	var many []gitx.Commit
	for i := range 10 {
		many = append(many, gitx.Commit{SHA: fmt.Sprintf("c%06d", i), Date: "2026-09-01", Subject: fmt.Sprintf("Change %d (#%d)", i, 100+i)})
	}
	e.git.logs = map[string][]gitx.Commit{"lib/f00.rb": many}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(812, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got, raw := readHistory(t, filepath.Join(res.ReportDir, HistoryFile))
	if len(got.Files) != maxHistoryFiles || got.More != 5 || got.Files[39].Path != "lib/f39.rb" {
		t.Fatalf("files = %d (last %q), more = %d; want 40, lib/f39.rb, 5", len(got.Files), got.Files[len(got.Files)-1].Path, got.More)
	}
	if c := got.Files[0].Commits; len(c) != maxHistoryCommits || c[0].PR != 100 || c[7].SHA != "c000007" {
		t.Fatalf("lib/f00.rb commits = %+v", c)
	}
	if !strings.Contains(raw, `"path": "lib/f01.rb",`+"\n"+`      "commits": []`) {
		t.Errorf("a file without commits is not an empty list:\n%s", raw)
	}
	_, logged := e.git.gitCalls()
	if len(logged) != maxHistoryFiles || !slices.ContainsFunc(logged, func(s string) bool { return s == "origin/master 8 lib/f39.rb" }) ||
		slices.ContainsFunc(logged, func(s string) bool { return strings.HasSuffix(s, "lib/f40.rb") }) {
		t.Errorf("FileLog calls = %q", logged)
	}
}

// A squash merge's subject ends with its PR number; a number elsewhere in
// the subject is not the commit's PR.
func TestHistoryPRNumber(t *testing.T) {
	for subject, want := range map[string]int{
		"Fix flaky mock generation (#11391)":   11391,
		"Fix x(#7)":                            7,
		"Fix x (#7) ":                          7,
		`Revert "Cache orders (#10001)" (#12)`: 12,
		`Revert "Cache orders (#10001)"`:       0,
		"Fixes #12 in the cache":               0,
		"Bump (#0)":                            0,
		"(#)":                                  0,
		"":                                     0,
	} {
		if got := historyPR(subject); got != want {
			t.Errorf("historyPR(%q) = %d, want %d", subject, got, want)
		}
	}
}

// A blind replay measures a review of a pinned head, so its history is read
// at the merge base (base_sha), never at origin/<base>, which may hold the
// PR's own merge and the fixes that followed it; without a merge base it
// gets none.
func TestABlindReplaysHistoryStopsAtTheMergeBase(t *testing.T) {
	dryRun := func(e *env) behavior {
		p := e.judgePosts(0, "", "COMMENT")
		p.gh, p.status = nil, "dry_run"
		return p.behavior(t)
	}
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.logs = map[string][]gitx.Commit{"app/models/order.rb": {{SHA: "bbb2222", Date: "2026-09-30", Subject: "Fix totals (#10889)"}}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{dryRun(e)}
	in := e.input(KindInitial)
	in.Blind, in.DryRun = true, true

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got, _ := readHistory(t, filepath.Join(res.ReportDir, HistoryFile))
	if got.Base != in.BaseSHA || len(got.Files) != 1 || got.Files[0].Commits[0].PR != 10889 {
		t.Fatalf("history.json = %+v", got)
	}
	diffs, logged := e.git.gitCalls()
	if !slices.Equal(diffs, []string{in.BaseSHA + "..." + target}) || !slices.Equal(logged, []string{in.BaseSHA + " 8 app/models/order.rb"}) {
		t.Errorf("calls = %q, %q; want the merge base only", diffs, logged)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nhistory: ")

	e = newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.ag.behaviors[agents.RoleJudge] = []behavior{dryRun(e)}
	in = e.input(KindInitial)
	in.Blind, in.DryRun, in.BaseSHA = true, true, ""
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound without a merge base = %+v, %v", res, err)
	}
	e.noHistory(e.reportDir())
	if diffs, logged := e.git.gitCalls(); len(diffs)+len(logged) != 0 {
		t.Errorf("calls without a merge base = %q, %q", diffs, logged)
	}
}

// The history is a hint: a log git cannot read leaves every prompt without
// it, with a warning, and the review goes on; a PR whose every file is new
// or ignored gets none, without a word.
func TestAHistoryGitCannotReadNeverStopsTheRound(t *testing.T) {
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.logErr = errors.New("fatal: bad revision 'origin/master'")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(813, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noHistory(e.reportDir())
	if evs := e.eventsOf("round.history"); len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "bad revision") {
		t.Fatalf("round.history events = %+v", evs)
	}

	e = newEnv(t)
	e.git.modified = []string{"Gemfile.lock", "yarn.lock"}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(814, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Related = defaultRelated
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noHistory(e.reportDir())
	if _, logged := e.git.gitCalls(); len(logged) != 0 || len(e.eventsOf("round.history")) != 0 {
		t.Errorf("ignored files only: FileLog calls %q, events %+v", logged, e.eventsOf("round.history"))
	}
}

// The reviewers wait for the history, so a git log that walks a big
// repository's whole history for long is cut at HistoryTimeout, and the
// round goes on without it.
func TestASlowHistoryIsCutShort(t *testing.T) {
	defer func(d time.Duration) { HistoryTimeout = d }(HistoryTimeout)
	HistoryTimeout = 50 * time.Millisecond
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb", "lib/old.rb"}
	e.git.logHangs = true
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(816, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	e.noHistory(e.reportDir())
	if evs := e.eventsOf("round.history"); len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "deadline exceeded") {
		t.Fatalf("round.history events = %+v", evs)
	}
}

// A restart on a newer head writes the history again in that head's report
// directory, and the restarted reviewer and the judge name that one.
func TestARestartWritesTheHistoryOfTheNewHead(t *testing.T) {
	e := newEnv(t)
	e.git.modified = []string{"app/models/order.rb"}
	e.git.logs = map[string][]gitx.Commit{"app/models/order.rb": {{SHA: "bbb2222", Date: "2026-09-30", Subject: "Fix totals (#10889)"}}}
	in := e.input(KindInitial)
	e.withRestarts(&in, 2)
	pushAfterCodex := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunVerified)
		e.push(head2)
		return nil
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushAfterCodex, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(815, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.Restarts != 1 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	old, newer := filepath.Join(e.dirOf(target), HistoryFile), filepath.Join(e.dirOf(head2), HistoryFile)
	if h, _ := readHistory(t, newer); h.HeadSHA != head2 {
		t.Fatalf("the new head's history.json = %+v", h)
	}
	claude := e.ag.submitsFor(agents.RoleClaude)
	if len(claude) != 2 {
		t.Fatalf("claude prompts = %d, want 2", len(claude))
	}
	mustContain(t, "claude's first prompt", claude[0].Text, old)
	mustContain(t, "claude's restart prompt", claude[1].Text, newer)
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nhistory: "+newer+"\n")
	if diffs, _ := e.git.gitCalls(); !slices.Equal(diffs, []string{in.BaseSHA + "..." + target, "base-" + head2[:7] + "..." + head2}) {
		t.Errorf("ModifiedPaths calls = %q", diffs)
	}
}
