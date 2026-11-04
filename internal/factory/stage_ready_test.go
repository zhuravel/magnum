package factory

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// fakeGitHub is GitHub as the publish meets it: a PR it opens is at the
// head the bare origin has for its branch, authored by zhuravel, ready for
// review; edit changes what ViewPR reads back (another head, base or
// author, a draft), createErr fails CreatePR once, and active is the login
// your gh config dir is logged in as ("" = zhuravel).
type fakeGitHub struct {
	mu        sync.Mutex
	r         *codingRig
	prs       []fakePR
	creates   []prCreate
	edit      func(*PRView)
	createErr error
	active    string
}

func (f *fakeGitHub) ActiveLogin(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cmp.Or(f.active, "zhuravel"), nil
}

type fakePR struct {
	branch string
	view   PRView
}

// prCreate is one CreatePR call: its arguments and the body file's text.
type prCreate struct{ repo, base, branch, title, body string }

func (f *fakeGitHub) CreatePR(_ context.Context, repo, base, branch, title, bodyFile string) (PRView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(bodyFile)
	if err != nil {
		return PRView{}, err
	}
	f.creates = append(f.creates, prCreate{repo: repo, base: base, branch: branch, title: title, body: string(b)})
	if err := f.createErr; err != nil {
		f.createErr = nil
		return PRView{}, err
	}
	n := 4501 + len(f.prs)
	v := PRView{Number: n, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n), Author: "zhuravel", Base: base,
		HeadSHA: f.r.gitOut(f.r.origin, "rev-parse", "refs/heads/"+branch)}
	f.prs = append(f.prs, fakePR{branch: branch, view: v})
	return v, nil
}

func (f *fakeGitHub) PRByBranch(_ context.Context, _ string, branch string) (PRView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.branch == branch {
			return f.edited(p.view), true, nil
		}
	}
	return PRView{}, false, nil
}

func (f *fakeGitHub) ViewPR(_ context.Context, _ string, number int) (PRView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.view.Number == number {
			return f.edited(p.view), nil
		}
	}
	return PRView{}, fmt.Errorf("no PR #%d", number)
}

func (f *fakeGitHub) edited(v PRView) PRView {
	if f.edit != nil {
		f.edit(&v)
	}
	return v
}

func (f *fakeGitHub) createCalls() []prCreate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.creates)
}

// addPR is a PR of branch that GitHub has already (somebody opened it).
func (f *fakeGitHub) addPR(branch string, v PRView) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, fakePR{branch: branch, view: v})
}

// readyRig is the review rig with the ready card and the publish wired:
// every local round clean unless answers say otherwise, your gh identity
// zhuravel, and the fake GitHub.
type readyRig struct {
	*reviewRig
	gh *fakeGitHub
}

func newReadyRig(t *testing.T, answers ...func(evalrun.LocalCase) evalrun.LocalResult) *readyRig {
	t.Helper()
	if len(answers) == 0 {
		answers = append(answers, result())
	}
	r := &readyRig{reviewRig: newReviewRig(t, answers...)}
	r.cfg.Identities = append(r.cfg.Identities, config.Identity{Name: "zhuravel", Kind: "gh", Login: "zhuravel"})
	r.gh = &fakeGitHub{r: r.codingRig}
	for _, d := range []*Deps{&r.deps, &r.e.d} {
		d.GitHub = r.gh
	}
	return r
}

const (
	acceptanceJSON = `{"checks": [{"id": "A1", "point": "a copied campaign keeps its points", "given": "a campaign with a rule",` +
		` "then": "the copy shows 10 points", "how": "spec/models/credit_points_spec.rb"}]}`
	prBodyDraft = "## What\n\nCredit#points counts the activities.\n\n| Point | Spec |\n|---|---|\n" +
		"| a copied campaign keeps its points | spec/models/credit_points_spec.rb |\n"
)

// toCard takes a bug task through its coding stage (the writer's fix, the
// factory's proof and precheck), its local review and the judge's PR body
// to its ready card: the task waits on you there, and its head is the fix.
func (r *readyRig) toCard(key string) (registry.Task, string) {
	r.t.Helper()
	return r.cardOf(r.codingTask(key, "bug"))
}

// cardOf is toCard for task, at its coding stage with its branch in the
// copy.
func (r *readyRig) cardOf(task registry.Task) (registry.Task, string) {
	r.t.Helper()
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", "spec/models/"))
	r.taskFile(task.ID, acceptanceFile, acceptanceJSON)
	r.startStage(task)
	head := r.fix(task)
	r.ended(task, "fx-writer")
	r.toBody(task)
	return r.reload(task.ID), head
}

// toBody runs task to its PR body stage, has the judge write it, and runs
// it to the card it draws next.
func (r *readyRig) toBody(task registry.Task) {
	r.t.Helper()
	judged := len(r.rt.promptsTo("fx-judge"))
	r.run(16, func() bool { return len(r.rt.promptsTo("fx-judge")) > judged })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rt.promptsTo("fx-judge")) == judged {
		r.t.Fatalf("task %s at %s waits %q: %q; want the judge's PR body turn", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	r.taskFile(task.ID, prBodyFile, prBodyDraft)
	r.ended(task, "fx-judge")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
}

// card is the task's ready card.
func (r *readyRig) card(id int64) ReadyCardFile {
	r.t.Helper()
	c, err := ReadReadyCard(r.layout, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// decideOn sends your choice on card c of task id as the screen does (a
// request) and runs the tick that answers it; the request as it ended.
func (r *readyRig) decideOn(id int64, c ReadyCardFile, choice, reason, note string) registry.Request {
	r.t.Helper()
	qid, err := r.reg.EnqueueRequest(r.ctx, ReqDecide, DecidePayload{TaskID: id, Choice: choice, Reason: reason, Note: note, HeadSHA: c.Head,
		ReportHash: c.ReportHash, DiffHash: c.DiffHash, Shown: c.Drawn, Surface: "screen"})
	if err != nil {
		r.t.Fatal(err)
	}
	r.tick()
	r.e.waitBackground()
	q, err := r.reg.RequestByID(r.ctx, qid)
	if err != nil {
		r.t.Fatal(err)
	}
	return q
}

// remoteHead is branch's head on the bare origin, "" when it has none.
func (r *readyRig) remoteHead(branch string) string {
	return r.gitOut(r.origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
}

// published runs task id until it is published or needs you.
func (r *readyRig) published(id int64) registry.Task {
	r.t.Helper()
	r.run(6, func() bool { s := r.reload(id).State; return s == registry.TaskPublished || s == registry.TaskNeedsYou })
	return r.reload(id)
}

// Before the card the judge drafts the PR body in its own turn: its prompt
// names the report, the proofs, the acceptance checks, the local review's
// end and the repository's PR template by their paths (never their text);
// the body keeps the judge's table from the ticket's points to their specs,
// and the factory appends the "Known" list of what the local review left
// open itself. The body is the pr_body artifact, and the card shows it.
func TestThePRBodyIsDraftedBeforeTheCard(t *testing.T) {
	r := newReadyRig(t, result(nitFinding))
	r.moveMaster(".github/pull_request_template.md", "## What\n\n## Checklist\n")
	r.git(r.main, "fetch", "--quiet", "origin")
	task, head := r.toCard("PR-12380")

	ps := r.rt.promptsTo("fx-judge")
	if len(ps) != 1 {
		t.Fatalf("the judge got %d prompts, want its PR body turn", len(ps))
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	for _, want := range []string{filepath.Join(dir, reportFile), filepath.Join(dir, acceptanceFile), filepath.Join(dir, proofFile),
		filepath.Join(dir, factoryProofFile), filepath.Join(dir, reviewSummaryFile), filepath.Join(dir, prBodyFile),
		filepath.Join(r.copyDir, ".github", "pull_request_template.md"), "table", "Known"} {
		if !strings.Contains(ps[0].Text, want) {
			t.Errorf("the PR body prompt lacks %q:\n%s", want, ps[0].Text)
		}
	}
	if strings.Contains(ps[0].Text, "## Checklist") || strings.Contains(ps[0].Text, "a copied campaign keeps its points") {
		t.Errorf("the PR body prompt carries a file's text:\n%s", ps[0].Text)
	}
	body := r.readTask(task.ID, prBodyFile)
	known := strings.Index(body, "## Known")
	if !strings.HasPrefix(body, prBodyDraft) || known < 0 || !strings.Contains(body[known:], nitFinding.Title) ||
		!strings.Contains(body[known:], "app/credit.rb:1") {
		t.Fatalf("pr-body.md:\n%s\nwant the judge's draft with its table, then a Known list naming the P3", body)
	}
	if as := r.artifacts(task.ID, "pr_body"); len(as) != 1 || as[0].Path != prBodyFile || as[0].SHA256 != sha256Hex([]byte(body)) {
		t.Fatalf("pr_body artifacts %+v", as)
	}
	c := r.card(task.ID)
	if task.State != registry.TaskNeedsYou || task.Stage != "ready" || !strings.HasPrefix(task.WaitsWhat, readyWait) ||
		c.Revision != 1 || c.Head != head || c.Title != "PR-12380 title" || c.Branch != task.Branch {
		t.Fatalf("task %s at %s waits %q; card %+v", task.State, task.Stage, task.WaitsWhat, c)
	}
	if c.ReportHash == "" || c.DiffHash == "" || len(c.Review.Leftovers) != 1 {
		t.Fatalf("card %+v, want the hashes it binds to and the review's leftover", c)
	}
}

// A clean exit whose round left a doubt lists it once: among the doubts,
// never among the leftovers too.
func TestACleanExitListsEachDoubtOnce(t *testing.T) {
	quiet := func(c evalrun.LocalCase) evalrun.LocalResult { // the finding posted, the counts and the verdict left it out
		res := result(refundFinding)(c)
		res.Counts, res.Verdict = [4]int{}, "clean"
		return res
	}
	r := newReviewRig(t, append(rounds(2, result(refundFinding)), quiet)...)
	r.cfg.Factory.LocalReview.Cycles = 4
	task, _ := r.reviewTask("PR-12381")
	for n := 1; n <= 2; n++ {
		r.run(6, func() bool {
			return fileIn(r.layout.FactoryTaskDir(task.ID), reviewFile(n)) && len(r.rt.promptsTo("fx-writer")) >= n
		})
		r.writerFixes(task, n, fixedCredit(n), nil)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage != "local_review" })
	var sum ReviewSummary
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reviewSummaryFile)), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Exit != registry.ExitClean || len(sum.Doubts) != 1 || slices.ContainsFunc(sum.Leftovers, func(i ReviewItem) bool { return i.Title == refundFinding.Title }) {
		t.Fatalf("summary %+v, want the finding among the doubts alone", sum)
	}
}

// Review Focus 1: no card is drawn, and nothing is pushed, unless the
// local review ended clean or on an oscillation at the task's head, and
// the factory's proof and precheck passed there: a summary of an
// unavailable or capped loop, of another head, or a head the proof or the
// precheck did not pass, needs you.
func TestNoCardOrPushWithoutACleanReviewOfTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(r *readyRig, id int64, head string)
		want string
	}{
		{"unavailable", func(r *readyRig, id int64, head string) { r.summary(id, registry.ExitUnavailable, head) }, "review unavailable"},
		{"cap", func(r *readyRig, id int64, head string) { r.summary(id, registry.ExitCap, head) }, "cap"},
		{"another head", func(r *readyRig, id int64, head string) { r.summary(id, registry.ExitClean, strings.Repeat("a", 40)) }, "another head"},
		{"no summary", func(r *readyRig, id int64, _ string) {
			_ = os.Remove(filepath.Join(r.layout.FactoryTaskDir(id), reviewSummaryFile))
		}, reviewSummaryFile},
		{"proof", func(r *readyRig, id int64, _ string) {
			r.taskFile(id, factoryProofFile, `{"passed": false, "head_sha": "x"}`)
		}, "proof"},
		{"precheck", func(r *readyRig, id int64, head string) {
			r.taskFile(id, precheckRecordFile, `{"head_sha": "`+head+`", "passed": false}`)
		}, "precheck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReadyRig(t)
			task, head := r.toCard("PR-12382")
			c := r.card(task.ID)
			// The card drawn, its review is then not what it was: no push.
			tc.edit(r, task.ID, head)
			if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestFailed || !strings.Contains(q.Result, tc.want) {
				t.Fatalf("push request %s: %q, want it refused naming %q", q.State, q.Result, tc.want)
			}
			r.run(4, nil)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "no ready card") || !strings.Contains(got.WaitsWhat, tc.want) {
				t.Fatalf("task %s waits %q, want no ready card naming %q", got.State, got.WaitsWhat, tc.want)
			}
			if r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
				t.Fatal("something was pushed or opened")
			}
		})
	}
}

// summary writes task id's review-summary.json with exit at head.
func (r *readyRig) summary(id int64, exit, head string) {
	r.t.Helper()
	b, _ := json.Marshal(ReviewSummary{Exit: exit, Rounds: 1, Head: head})
	r.taskFile(id, reviewSummaryFile, string(b))
}

// A conflict with current master at the card sends the task back through
// its stages, no second path: the writer gets a turn to merge the base,
// naming origin/master and the conflicting paths; the proof and the
// precheck run again at the merge, a fresh local round reviews it, the
// judge drafts the body again, and the card has a new revision at the new
// head. The factory's own git steps at the card change no work tree.
// toConflict takes a bug task to its card while master moves under it in
// app/credit.rb, so the card finds a conflict (before setup runs, after the
// judge's PR body turn began); then the ticks that follow the card.
func (r *readyRig) toConflict(key string, setup func(task registry.Task)) (registry.Task, string) {
	r.t.Helper()
	task := r.codingTask(key, "bug")
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", "spec/models/"))
	r.taskFile(task.ID, acceptanceFile, acceptanceJSON)
	r.startStage(task)
	head := r.fix(task)
	r.ended(task, "fx-writer")
	r.run(16, func() bool { return r.reload(task.ID).Stage == "pr_body" && len(r.rt.promptsTo("fx-judge")) > 0 })
	r.moveMaster("app/credit.rb", strings.Replace(creditRB, "    0\n", "    5\n", 1))
	if setup != nil {
		setup(task)
	}
	r.taskFile(task.ID, prBodyFile, prBodyDraft)
	r.ended(task, "fx-judge")
	r.run(4, func() bool { return r.reload(task.ID).Stage == "coding" || !isLive(r.reload(task.ID)) })
	return task, head
}

func TestAMasterConflictAtTheCardSendsTheWriterToMergeTheBase(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toConflict("PR-12384", nil)

	got := r.reload(task.ID)
	if c := r.card(task.ID); got.Stage != "coding" || !isLive(got) || c.Revision != 1 || c.Head != head ||
		!slices.Contains(c.Flags, "conflicts with the base: app/credit.rb") {
		t.Fatalf("task %s at %s waits %q; card %+v; want back at coding from card 1, which names the conflict", got.State, got.Stage,
			got.WaitsWhat, c)
	}
	if b := r.git(r.copyDir, "rev-parse", "HEAD"); b != head || r.git(r.copyDir, "status", "--porcelain", "--untracked-files=no") != "" {
		t.Fatalf("the copy at %s with changes after the card's merge check, want %s as it was", b, head)
	}
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.Contains(ps[1].Text, "origin/master") || !strings.Contains(ps[1].Text, "`app/credit.rb`") ||
		!strings.Contains(ps[1].Text, "git merge origin/master") {
		t.Fatalf("the writer's prompts %+v, want a turn to merge origin/master naming app/credit.rb", ps)
	}

	// The writer merges the base and resolves the conflict.
	r.clock.Add(time.Minute)
	r.gitOut(r.copyDir, "merge", "--quiet", "--no-edit", "origin/master")
	r.write(r.copyDir, "app/credit.rb", strings.Replace(creditRB, "    0\n", "    10\n", 1))
	r.git(r.copyDir, "add", "app/credit.rb")
	r.git(r.copyDir, "commit", "--quiet", "--no-edit")
	merged := r.git(r.copyDir, "rev-parse", "HEAD")
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.toBody(task)

	var proof Proof
	if err := json.Unmarshal([]byte(r.readTask(task.ID, factoryProofFile)), &proof); err != nil || !proof.Holds(merged) {
		t.Fatalf("the factory's proof %+v (%v), want it at the merge %s", proof, err, merged)
	}
	if !precheckPassedAt(r.layout.FactoryTaskDir(task.ID), merged) {
		t.Fatal("the precheck did not pass at the merge")
	}
	if calls := r.rv.calls(); len(calls) != 2 || calls[1].HeadSHA != merged || calls[1].Round != 1 {
		t.Fatalf("local rounds %+v, want a fresh loop's round 1 at the merge", calls)
	}
	c := r.card(task.ID)
	if got := r.reload(task.ID); c.Revision != 2 || c.Head != merged || got.State != registry.TaskNeedsYou || got.Stage != "ready" {
		t.Fatalf("card %+v; task %s at %s, want card 2 at the merge", c, got.State, got.Stage)
	}
}

// A merge turn passes only on a head that holds the base the card saw: a
// writer that ends its turn without merging gets one fix turn naming the
// base, within the stage's prompts, and after a second turn without the
// merge the task needs you, never another round of the card.
func TestAMergeTurnThatDoesNotMergeGetsAFixTurnThenNeedsYou(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toConflict("PR-12373", nil)
	var rw reworkRecord
	if err := json.Unmarshal([]byte(r.readTask(task.ID, reworkFile)), &rw); err != nil || rw.BaseSHA != r.git(r.work, "rev-parse", "HEAD") {
		t.Fatalf("rework.json %+v (%v), want origin/master's commit the card saw", rw, err)
	}
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })

	// The writer gives up on the merge (git merge --abort) and ends its turn.
	r.clock.Add(time.Minute)
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) > 2 || !isLive(r.reload(task.ID)) })
	ps := r.rt.promptsTo("fx-writer")
	if got := r.reload(task.ID); len(ps) != 3 || !strings.HasPrefix(ps[2].Text, fixLead) || !strings.Contains(ps[2].Text, "origin/master") ||
		!strings.Contains(ps[2].Text, short(rw.BaseSHA)) || got.Stage != "coding" {
		t.Fatalf("task %s at %s; the writer's prompts %+v; want a fix turn naming origin/master", got.State, got.Stage, ps)
	}

	r.clock.Add(time.Minute)
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWhat != "the fx-writer agent did not merge origin/master" || got.Stage != "coding" ||
		len(r.rt.promptsTo("fx-writer")) != 3 || got.HeadSHA != head {
		t.Fatalf("task %s at %s waits %q, head %s; %d writer prompts; want you, the base unmerged", got.State, got.Stage, got.WaitsWhat,
			short(got.HeadSHA), len(r.rt.promptsTo("fx-writer")))
	}
	if n := len(r.rv.calls()); n != 1 {
		t.Fatalf("%d local rounds, want none after the unmerged turn", n)
	}
}

// The same conflict at the same base a second time (a merge turn that the
// card already sent the task back for) needs you instead of another round.
func TestTheSameConflictAtTheSameBaseTwiceNeedsYou(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toConflict("PR-12372", func(task registry.Task) {
		b, _ := json.Marshal(reworkRecord{Kind: reworkMergeBase, Base: "origin/master", BaseSHA: r.git(r.work, "rev-parse", "HEAD"),
			Conflicts: []string{"app/credit.rb"}, Card: 1})
		r.taskFile(task.ID, "rework-1.json", string(b))
	})
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.Stage != "ready" || !strings.Contains(got.WaitsWhat, "the same conflict") ||
		!strings.Contains(got.WaitsWhat, "origin/master") || !strings.Contains(got.WaitsWhat, "app/credit.rb") {
		t.Fatalf("task %s at %s waits %q, want you, the same conflict named", got.State, got.Stage, got.WaitsWhat)
	}
	if len(r.rt.promptsTo("fx-writer")) != 1 {
		t.Fatalf("the writer got %d prompts, want no merge turn", len(r.rt.promptsTo("fx-writer")))
	}
}

// Review Focus 3: the branch moved after the card (a commit in the copy),
// and your push is refused, "the branch changed after you read it": nothing
// is pushed, no PR opened, and the task needs you naming the move.
func TestPushIsRefusedWhenTheBranchMovedAfterTheCard(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12385")
	c := r.card(task.ID)
	moved := r.commit("a commit after the card", map[string]string{"app/notes.rb": "class Note; end\n"})

	q := r.decideOn(task.ID, c, ChoicePush, "", "")
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "the branch changed after you read it") {
		t.Fatalf("push request %s: %q, want it refused", q.State, q.Result)
	}
	r.run(3, nil)
	got := r.reload(task.ID)
	if r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
		t.Fatal("the moved branch was pushed or its PR opened")
	}
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, short(moved)) || !strings.Contains(got.WaitsWhat, short(head)) {
		t.Fatalf("task %s waits %q, want you naming the move from %s to %s", got.State, got.WaitsWhat, short(head), short(moved))
	}
	if ds, err := r.reg.Decisions(r.ctx, task.ID); err != nil || len(ds) != 0 {
		t.Fatalf("decisions %+v (%v): a refused push records none", ds, err)
	}
}

// The y binds to the PR body as the card showed it: a body changed after
// the card is refused like a changed report, and the card is drawn again.
func TestPushIsRefusedWhenThePRBodyChangedAfterTheCard(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12386")
	c := r.card(task.ID)
	r.taskFile(task.ID, prBodyFile, r.readTask(task.ID, prBodyFile)+"\nOne more line.\n")
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestFailed ||
		!strings.Contains(q.Result, "the branch changed after you read it") {
		t.Fatalf("push request %s: %q, want it refused", q.State, q.Result)
	}
	r.run(3, nil)
	if again := r.card(task.ID); again.Revision != 2 || again.ReportHash == c.ReportHash || r.remoteHead(task.Branch) != "" {
		t.Fatalf("card %+v after the body changed, want revision 2 with the new hash and nothing pushed", again)
	}
}

// The y/N sentence names the branch it pushes, the copy it frees and the
// queued task that then starts; the card names the copy.
func TestTheConfirmationNamesTheCopyItFreesAndTheTaskThatStarts(t *testing.T) {
	task := registry.Task{JiraKey: "PR-12360", Branch: "zhuravel-PR-12360-activity-points-on-copy"}
	if got, want := ConfirmSentence(task, "talkable.factory3", "PR-12388"),
		"Push zhuravel-PR-12360-activity-points-on-copy and open the PR? This frees talkable.factory3, and PR-12388 starts. y/N"; got != want {
		t.Fatalf("ConfirmSentence = %q, want %q", got, want)
	}
	if got, want := ConfirmSentence(task, "talkable.factory3", ""),
		"Push zhuravel-PR-12360-activity-points-on-copy and open the PR? This frees talkable.factory3. y/N"; got != want {
		t.Fatalf("ConfirmSentence without a queued task = %q, want %q", got, want)
	}
	linked := registry.Task{JiraKey: "PR-12360", Branch: "zhuravel-activity-points-on-copy", RemoteBranch: "zhuravel-PR-12360-activity-points-on-copy"}
	if got, want := ConfirmSentence(linked, "", ""), "Push zhuravel-PR-12360-activity-points-on-copy and open the PR? y/N"; got != want {
		t.Fatalf("ConfirmSentence of a task pushed to its key's branch = %q, want %q", got, want)
	}
	tasks := []registry.Task{{ID: 1, State: registry.TaskNeedsYou, JiraKey: "PR-12360"},
		{ID: 2, State: registry.TaskQueued, JiraKey: "PR-12388", WaitsWho: waitCopy}, {ID: 3, State: registry.TaskQueued, JiraKey: "PR-12389", WaitsWho: waitCopy}}
	if got := NextQueued(tasks); got != "PR-12388" {
		t.Fatalf("NextQueued = %q, want the oldest queued task", got)
	}
	tasks[1].WaitsWho = waitMemory
	if got := NextQueued(tasks); got != "" {
		t.Fatalf("NextQueued = %q while the oldest waits on memory, want none: no copy starts it", got)
	}

	r := newReadyRig(t)
	card, _ := r.toCard("PR-12396")
	if c := r.card(card.ID); c.Copy != filepath.Base(r.copyDir) {
		t.Fatalf("the card frees %q, want %s", c.Copy, filepath.Base(r.copyDir))
	}
}

// Send back hands the writer your reason and your note: the note goes to a
// file of the task's directory, which the writer's next prompt names with
// the reason's key, never the note's words; the task is back at coding,
// and the decision and your note are recorded.
func TestSendBackReturnsTheTaskToCodingWithTheReasonAndTheNote(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12398")
	const note = "keep archived campaigns out of the copy MARKER-5c1d"
	if q := r.decideOn(task.ID, r.card(task.ID), ChoiceSendBack, "scope", note); q.State != registry.RequestDone {
		t.Fatalf("send back request %s: %q", q.State, q.Result)
	}
	got := r.reload(task.ID)
	if got.Stage != "coding" || got.State != registry.TaskActive {
		t.Fatalf("task %s at %s, want active at coding", got.State, got.Stage)
	}
	noteFile := filepath.Join(r.layout.FactoryTaskDir(task.ID), sendBackFile(1))
	if b, err := os.ReadFile(noteFile); err != nil || !strings.Contains(string(b), note) {
		t.Fatalf("%s: %q (%v), want your note", noteFile, b, err)
	}
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.Contains(ps[1].Text, noteFile) || !strings.Contains(ps[1].Text, "scope") || strings.Contains(ps[1].Text, "MARKER-5c1d") {
		t.Fatalf("the writer's prompts %+v, want a turn naming %s and the reason, never the note", ps, noteFile)
	}
	if b := r.git(r.copyDir, "rev-parse", "HEAD"); b != head {
		t.Fatalf("the copy at %s, want the branch kept at %s", b, head)
	}
	ds, err := r.reg.Decisions(r.ctx, task.ID)
	if err != nil || len(ds) != 1 || ds[0].Choice != ChoiceSendBack || ds[0].HeadSHA != head {
		t.Fatalf("decisions %+v (%v)", ds, err)
	}
	ins, err := r.reg.Inputs(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, in := range ins {
		kinds = append(kinds, in.Kind)
		if in.Kind == registry.InputSendBack && !strings.Contains(in.Text, "archived campaigns") {
			t.Errorf("the send back input %+v, want your note", in)
		}
	}
	if !slices.Contains(kinds, registry.InputDecision) || !slices.Contains(kinds, registry.InputSendBack) {
		t.Fatalf("inputs %q, want the decision and the send back", kinds)
	}

	// A send back without a reason key or a note, or with a reason that is
	// not a key, is refused.
	for _, bad := range []struct{ reason, note string }{{"", "a note"}, {"scope", ""}, {"two words", "a note"}} {
		err := r.e.decide(r.ctx, DecidePayload{TaskID: task.ID, Choice: ChoiceSendBack, Reason: bad.reason, Note: bad.note})
		if err == nil || (!strings.Contains(err.Error(), "reason") && !strings.Contains(err.Error(), "note")) {
			t.Errorf("a send back with reason %q and note %q: %v, want it refused for its reason or note", bad.reason, bad.note, err)
		}
	}
}

// After a send back the writer's new commit is reviewed afresh: a new head
// starts a fresh local loop, whose round runs at the new head before the
// judge drafts the body again and a new card is drawn.
func TestASentBackChangeGetsAFreshLocalReviewBeforeANewCard(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12399")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoiceSendBack, "scope", "count the archived ones too"); q.State != registry.RequestDone {
		t.Fatalf("send back request %s: %q", q.State, q.Result)
	}
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-writer")) > 1 })
	r.clock.Add(time.Minute)
	fix := r.commit("PR-12399 counts the archived activities", map[string]string{
		"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10\n", 1) + "# archived too\n"})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.toBody(task)

	if calls := r.rv.calls(); len(calls) != 2 || calls[0].HeadSHA != head || calls[1].HeadSHA != fix || calls[1].Round != 1 {
		t.Fatalf("local rounds %+v, want round 1 at %s, then a fresh round 1 at %s", calls, short(head), short(fix))
	}
	if c := r.card(task.ID); c.Revision != 2 || c.Head != fix {
		t.Fatalf("card %+v, want revision 2 at the new head", c)
	}
}

// Drop frees the copy and keeps the branch: the task stops, nothing is
// pushed, and the decision and your drop are recorded.
func TestDropKeepsTheBranchAndFreesTheCopy(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12379")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoiceDrop, "", ""); q.State != registry.RequestDone {
		t.Fatalf("drop request %s: %q", q.State, q.Result)
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskStopped || !slices.Contains(r.copies.releases(), task.ID) {
		t.Fatalf("task %s; releases %v, want stopped with its copy released", got.State, r.copies.releases())
	}
	if b := r.git(r.copyDir, "rev-parse", "refs/heads/"+task.Branch); b != head || r.remoteHead(task.Branch) != "" {
		t.Fatalf("the branch at %s, remote %q; want it kept at %s and not pushed", b, r.remoteHead(task.Branch), head)
	}
	ds, err := r.reg.Decisions(r.ctx, task.ID)
	if err != nil || len(ds) != 1 || ds[0].Choice != ChoiceDrop {
		t.Fatalf("decisions %+v (%v)", ds, err)
	}
	ins, _ := r.reg.Inputs(r.ctx, task.ID)
	if !slices.ContainsFunc(ins, func(in registry.Input) bool { return in.Kind == registry.InputDrop }) {
		t.Fatalf("inputs %+v, want your drop", ins)
	}
}

// A branch holds a Jira key only as a whole dash-delimited segment: key
// PR-1236 is not in a branch named for PR-12360, so a task of PR-1236 on
// such a branch pushes to a branch of its own key.
func TestABranchHoldsAKeyOnlyAsAWholeSegment(t *testing.T) {
	for _, tc := range []struct {
		branch, key string
		want        bool
	}{
		{"zhuravel-PR-1236-copy", "PR-1236", true},
		{"zhuravel-PR-1236", "PR-1236", true},
		{"PR-1236-copy", "PR-1236", true},
		{"zhuravel-PR-12360-copy", "PR-1236", false},
		{"zhuravel-XPR-1236-copy", "PR-1236", false},
		{"zhuravel-PR-12360", "PR-1236", false},
	} {
		if got := holdsKey(tc.branch, tc.key); got != tc.want {
			t.Errorf("holdsKey(%q, %q) = %v, want %v", tc.branch, tc.key, got, tc.want)
		}
	}
	h := newHarness(t)
	h.cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	h.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop"}}
	task, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: "PR-1236", Prompt: "copy the points", Kind: "bug", Repo: "example/shop",
		Title: "Copied points", State: registry.TaskActive, Stage: "ready", Branch: "zhuravel-PR-12360-copy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.nameRemoteBranch(h.ctx, &task); err != nil || task.RemoteBranch != "zhuravel-PR-1236-copied-points" {
		t.Fatalf("the PR's branch %q (%v), want zhuravel-PR-1236-copied-points", task.RemoteBranch, err)
	}
}

// The PR's title is the factory's: the Jira key and the task's title, or
// the title alone for a task of a prompt; a title that starts with its key
// keeps it once.
func TestThePRTitleIsTheKeyAndTheTitle(t *testing.T) {
	for _, tc := range []struct {
		task registry.Task
		want string
	}{
		{registry.Task{JiraKey: "PR-12360", Title: "Custom activities lose points on copy"}, "PR-12360 Custom activities lose points on copy"},
		{registry.Task{JiraKey: "PR-12360", Title: "PR-12360 Custom activities"}, "PR-12360 Custom activities"},
		{registry.Task{Title: " Count the archived activities "}, "Count the archived activities"},
	} {
		if got := prTitle(tc.task); got != tc.want {
			t.Errorf("prTitle(%+v) = %q, want %q", tc.task, got, tc.want)
		}
	}
}

// Your note on a send back is classified as it is recorded; the decision
// that came with it is your act, not words, and has no class.
func TestASendBacksNoteIsClassifiedWhenRecorded(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12397")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoiceSendBack, "scope", "wrong scope: keep archived campaigns out"); q.State != registry.RequestDone {
		t.Fatalf("send back request %s: %q", q.State, q.Result)
	}
	ins, err := r.reg.Inputs(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]string{}
	for _, in := range ins {
		classes[in.Kind] = in.Class
	}
	if classes[registry.InputSendBack] != registry.ClassCorrection {
		t.Errorf("the send back input has class %q, want %q; inputs %+v", classes[registry.InputSendBack], registry.ClassCorrection, ins)
	}
	if class, ok := classes[registry.InputDecision]; !ok || class != "" {
		t.Errorf("the decision input has class %q (recorded: %v), want none", class, ok)
	}
}
