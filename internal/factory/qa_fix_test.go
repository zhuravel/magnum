package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
)

// escapedProbes are the probes of an escaped walk: the security settings
// with the tooltip's markup shown as text, at the copy's own
// /settings/security whatever the walker calls the page.
var escapedProbes = map[string]string{"P1": "escaped-tooltip.json"}

// escapedWalk is a walk of one page, the security settings, which the
// walker spells path, and the walker's own find of the tooltip's markup
// there, naming the partial that renders it.
func escapedWalk(path string) QAVerdict {
	return QAVerdict{Pages: []QAPage{qaPage(1, "P1", path, "A1")},
		Defects: []QADefect{{ID: "D1", Class: QAEscapedMarkup, Severity: "P2", Page: "P1", Title: "the dates show HTML",
			Paths: []string{"app/views/settings/_keys.html.erb"}}}}
}

// qaDefects is defects-<k>.json of run k at head.
func (r *qaRig) qaDefects(id int64, k int, head string) ReviewFile {
	r.t.Helper()
	var rf ReviewFile
	if err := json.Unmarshal([]byte(r.readTask(id, qaDefectsFile(k, head))), &rf); err != nil {
		r.t.Fatal(err)
	}
	return rf
}

// toWriter runs the ticks until the writer got its n-th prompt.
func (r *qaRig) toWriter(task registry.Task, n int) {
	r.t.Helper()
	r.run(6, func() bool { return len(r.rt.promptsTo("fx-writer")) >= n })
	if got := len(r.rt.promptsTo("fx-writer")); got < n {
		cur := r.reload(task.ID)
		r.t.Fatalf("the writer got %d prompts, want %d; task %s at %s waits %q: %q", got, n, cur.State, cur.Stage, cur.WaitsWho, cur.WaitsWhat)
	}
}

// writerFixesQA is the writer's fix turn of QA run k at head: a commit
// (none when files is nil), proof.json, its reply to defects-<k>.json (each
// defect fixed, but those declined with the risk given, "" for none), then
// its turn's end.
func (r *qaRig) writerFixesQA(task registry.Task, k int, head string, files, declined map[string]string) {
	r.t.Helper()
	r.clock.Add(time.Minute)
	if files != nil {
		r.commit(task.JiraKey+" fixes what QA found", files)
	}
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	var replies []map[string]string
	for _, f := range r.qaDefects(task.ID, k, head).Findings {
		reply := map[string]string{"id": f.ID, "action": "fixed"}
		if risk, ok := declined[f.ID]; ok {
			reply["action"], reply["risk"] = "declined", risk
		}
		replies = append(replies, reply)
	}
	b, err := json.Marshal(map[string]any{"replies": replies})
	if err != nil {
		r.t.Fatal(err)
	}
	r.taskFile(task.ID, qaReplyFile(k, head), string(b))
	r.ended(task, "fx-writer")
}

// eventsWith are the texts of the task's events of kind that hold text.
func (r *qaRig) eventsWith(id int64, kind, text string) []string {
	r.t.Helper()
	var out []string
	for _, ev := range r.events(id, kind) {
		if strings.Contains(ev.Text, text) {
			out = append(out, ev.Text)
		}
	}
	return out
}

// QA's defects that block go to the writer as one fix turn of the qa
// stage, never to you: defects-<k>.json in the run's folder in the shape of
// a review's findings (Q1, its class in its title, the code path the walker
// named, the page, its state, its shot and its quote in its body), and the
// writer's prompt names it and the file of its reply.
func TestQADefectsGoToTheWriterAsAFixTurn(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12440")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	got := r.reload(task.ID)
	if got.Stage != "qa" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q: %q, want the qa stage at work", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	rf := r.qaDefects(task.ID, 1, head)
	if rf.Round != 1 || rf.Head != head || len(rf.Findings) != 1 {
		t.Fatalf("defects-1.json %+v, want one defect of run 1 at %s", rf, head)
	}
	f := rf.Findings[0]
	if f.ID != "Q1" || f.Severity != "P2" || !strings.HasPrefix(f.Title, "escaped_markup: ") || f.Path != "app/views/settings/_keys.html.erb" ||
		f.Fingerprint == "" {
		t.Fatalf("defect %+v, want Q1, P2, escaped_markup, at the partial the walker named", f)
	}
	runDir := r.runDir(task.ID, 1, head)
	for _, want := range []string{"/settings/security", "signed_in", filepath.Join(runDir, "pages", "01-p1-signed_in-after.png"), "<span"} {
		if !strings.Contains(f.Body, want) {
			t.Errorf("the defect's body %q lacks %q", f.Body, want)
		}
	}
	if strings.Contains(f.Body, "came back") {
		t.Errorf("the first run's defect says it came back: %q", f.Body)
	}
	if roles := r.rt.launchedRoles(); !slices.EqualFunc(roles, [][]string{{"fx-qa"}, {"fx-writer"}}, slices.Equal) {
		t.Fatalf("launches %v, want the walker, then the writer", roles)
	}
	text := r.rt.promptsTo("fx-writer")[0].Text
	for _, want := range []string{filepath.Join(runDir, "defects-1.json"), filepath.Join(runDir, "reply-1.json")} {
		if !strings.Contains(text, want) {
			t.Errorf("the writer's prompt lacks %q:\n%s", want, text)
		}
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: the stage passed")
	}
	r.qaFree(task.ID)
}

// The writer's fix goes back through the factory's checks at its head (the
// proof and the precheck), then the local review afresh (its last cycle,
// if any, reviewed another head), then QA, whose clean walk at the fix's
// head passes the stage.
func TestAQAFixGoesBackThroughTheReviewThenQA(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12441")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "qa" })
	fix := r.git(r.copyDir, "rev-parse", "HEAD")
	got := r.reload(task.ID)
	if fix == head || got.Stage != "local_review" || got.HeadSHA != fix || !isLive(got) {
		t.Fatalf("task %s at %s with head %s waits %q: %q, want the local review at the fix %s", got.State, got.Stage, got.HeadSHA,
			got.WaitsWho, got.WaitsWhat, fix)
	}
	for _, word := range []string{"bin/lint", "bin/specs"} {
		if runs := r.ran(word); len(runs) == 0 || runs[len(runs)-1].Head != fix {
			t.Errorf("%s runs %+v, want the last at the fix %s", word, runs, fix)
		}
	}
	if evs := r.eventsWith(task.ID, "qa", "back to local_review"); len(evs) != 1 || !strings.Contains(evs[0], "QA's fix at "+fix[:7]) {
		t.Fatalf("qa events %q, want one move back to local_review naming the fix", evs)
	}
	r.run(10, func() bool { return r.reload(task.ID).Stage == "qa" })
	if calls := r.rv.calls(); len(calls) != 1 || calls[0].HeadSHA != fix {
		t.Fatalf("review rounds %+v, want one at the fix %s", calls, fix)
	}
	r.toWalker(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 2 || run.Dir != "qa/2-"+fix[:7] {
		t.Fatalf("qa-run.json %+v, want run 2 at the fix", run)
	}
	r.walk(task, oneSecurityPage(), map[string]string{"P1": "clean-tooltip.json"})
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q: %q, want pr_body", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	if sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); !ok || sum.Outcome != QAOutcomeClean || sum.Head != fix || sum.Run != 2 {
		t.Fatalf("qa-summary.json %+v (%v), want run 2 clean at the fix %s", sum, ok, fix)
	}
	r.qaFree(task.ID)
}

// The writer's work on QA's defects moves the branch and leaves changes in
// the copy while its turn runs: none of it is a move of the branch that
// sends the task back to coding, nor a copy that keeps QA from starting.
func TestAWriterAtWorkOnQAsDefectsIsNoMoveOfTheBranch(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12442")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.commit(task.JiraKey+" escapes the tooltip once", fixedCredit(1))
	r.write(r.copyDir, "app/credit.rb", "class Credit\nend\n")
	r.run(3, nil)
	if got := r.reload(task.ID); got.Stage != "qa" || !isLive(got) || got.HeadSHA != head || len(r.rt.promptsTo("fx-writer")) != 1 {
		t.Fatalf("task %s at %s with head %s waits %q: %q, want the writer still at work at the qa stage", got.State, got.Stage,
			got.HeadSHA, got.WaitsWho, got.WaitsWhat)
	}
}

// Review Focus: the writer declines every QA defect with its risk and
// commits nothing. The stage ends without a loop, and a defect QA finds
// never becomes a known note (roadmap R56): no summary passes the stage,
// the task needs you naming the defects and the writer's reply, and no
// review round runs. Your retry walks QA again at the head, and the writer
// gets the defect once more, marked as come back after its fix.
func TestADeclinedQADefectNeedsYouAndNeverBecomesKnown(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12443")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	got := r.toYou(task)
	reply := filepath.Join(r.runDir(task.ID, 1, head), "reply-1.json")
	for _, want := range []string{"declined", "escaped_markup on /settings/security", reply, fmt.Sprintf("magnum factory retry %d", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	for _, bad := range []string{"<span", "the key is shared"} {
		if strings.Contains(got.WaitsWhat, bad) {
			t.Errorf("the task waits %q: it quotes %q", got.WaitsWhat, bad)
		}
	}
	if got.Stage != "qa" || got.HeadSHA != head {
		t.Fatalf("task at %s with head %s, want the qa stage at %s", got.Stage, got.HeadSHA, head)
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: the declined defect passed the stage")
	}
	r.run(3, nil)
	dir := r.layout.FactoryTaskDir(task.ID)
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 || len(r.rv.calls()) != 0 || newestQARun(dir) != 1 {
		t.Fatalf("%d writer prompts, %d review rounds, newest QA run %d: want one fix turn and no loop", n, len(r.rv.calls()), newestQARun(dir))
	}

	if q := r.retry(task.ID); q.State != registry.RequestDone {
		t.Fatalf("the retry %+v", q)
	}
	r.toWalker(task)
	if run, _ := readQARun(dir); run.N != 2 || run.Head != head {
		t.Fatalf("qa-run.json %+v, want run 2 at the head after your retry", run)
	}
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 2)
	if rf := r.qaDefects(task.ID, 2, head); len(rf.Findings) != 1 || !strings.Contains(rf.Findings[0].Body, "came back after its fix") {
		t.Fatalf("defects-2.json %+v, want the defect marked as come back after its fix", rf)
	}
}

// A decline that names no risk is no decline: with no commit either, the
// writer's next prompt asks for the commit or the risk.
func TestAQADeclineWithoutARiskNeedsACommit(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12444")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": ""})
	r.toWriter(task, 2)
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "commit your fixes, or decline each finding with its risk") {
		t.Fatalf("the writer's prompts %+v, want a fix turn asking for its commit", ps)
	}
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou || got.Stage != "qa" {
		t.Fatalf("task %s at %s waits %q", got.State, got.Stage, got.WaitsWhat)
	}
}

// At most fixes (2) writer turns: a defect that comes back after each fix,
// whatever the walker calls its page (the factory follows the page by the
// path its probe read), is the same defect, never a doubt and never a known
// note: after the second fix turn the third run's defect needs you, with
// no third defects file and nothing passed.
func TestQAFixTurnsStopAtTheCap(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12445")
	heads := []string{head}
	spellings := []string{"/settings/security", "/settings/security/", "settings/security"}
	for k := 1; k <= 2; k++ {
		r.toWalker(task)
		r.walk(task, escapedWalk(spellings[k-1]), escapedProbes)
		r.toWriter(task, k)
		r.writerFixesQA(task, k, heads[k-1], fixedCredit(k), nil)
		r.run(12, func() bool { return len(r.rv.calls()) == k && r.reload(task.ID).Stage == "qa" })
		got := r.reload(task.ID)
		if got.Stage != "qa" || got.HeadSHA == heads[k-1] {
			t.Fatalf("after fix %d: task %s at %s with head %s waits %q: %q", k, got.State, got.Stage, got.HeadSHA, got.WaitsWho, got.WaitsWhat)
		}
		heads = append(heads, got.HeadSHA)
	}
	rf1, rf2 := r.qaDefects(task.ID, 1, heads[0]), r.qaDefects(task.ID, 2, heads[1])
	if len(rf2.Findings) != 1 || rf2.Findings[0].Fingerprint != rf1.Findings[0].Fingerprint ||
		!strings.Contains(rf2.Findings[0].Body, "came back after its fix") {
		t.Fatalf("run 2's defects %+v, run 1's %+v: want the same defect, come back after its fix", rf2.Findings, rf1.Findings)
	}
	r.toWalker(task)
	r.walk(task, escapedWalk(spellings[2]), escapedProbes)
	got := r.toYou(task)
	for _, want := range []string{"QA still finds 1 defect that blocks after 2 fix turns", "came back after its fix: escaped_markup on settings/security",
		"see " + filepath.Join(r.runDir(task.ID, 3, heads[2]), qaResultFile), fmt.Sprintf("magnum factory retry %d", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	if fsx.Exists(filepath.Join(r.layout.FactoryTaskDir(task.ID), qaDefectsFile(3, heads[2]))) {
		t.Fatal("defects-3.json was written past the cap")
	}
	if n := len(r.rt.promptsTo("fx-writer")); got.Stage != "qa" || n != 2 {
		t.Fatalf("task at %s after %d writer prompts, want the qa stage after 2", got.Stage, n)
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: a defect that came back passed the stage")
	}
	r.qaFree(task.ID)
}

// A QA fix whose move back to the local review did not land (the daemon
// stopped between the fix's checks and the move) still goes to the review:
// no QA run starts at a head the review has not reviewed.
func TestAQAFixGoesToTheReviewEvenWhenItsMoveThereWasLost(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12446")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	r.run(8, func() bool { return r.reload(task.ID).Stage != "qa" })
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "qa")
		u.Set("attempt", 0)
	}); err != nil {
		t.Fatal(err)
	}
	r.run(2, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q: %q, want the local review", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	if n := newestQARun(r.layout.FactoryTaskDir(task.ID)); n != 1 {
		t.Fatalf("QA run %d started before the review of the fix", n)
	}
}

// The reply file of a fix turn: the local review's keeps its name, a QA
// fix turn's is the run's.
func TestAReviewFixTurnStillRepliesInItsOwnFile(t *testing.T) {
	if got := (&reviewTurn{n: 2}).replyFile(); got != "review-reply-2.json" {
		t.Fatalf("the review's reply file %q, want review-reply-2.json", got)
	}
	if got := (&reviewTurn{n: 1, reply: qaReplyFile(1, "abc1234def5678")}).replyFile(); got != "qa/1-abc1234/reply-1.json" {
		t.Fatalf("QA's reply file %q, want qa/1-abc1234/reply-1.json", got)
	}
	if got := qaDefectsFile(3, "def5678abc1234"); got != "qa/3-def5678/defects-3.json" {
		t.Fatalf("QA's defects file %q", got)
	}
}

// newestQADefects finds the newest run's defects file, whatever its head,
// and none in a task without one.
func TestNewestQADefectsIsTheNewestRunsFile(t *testing.T) {
	dir := t.TempDir()
	if k, head := newestQADefects(dir); k != 0 || head != "" {
		t.Fatalf("newestQADefects of an empty task = %d, %q", k, head)
	}
	for _, name := range []string{qaDefectsFile(1, "abc1234"), qaDefectsFile(2, "def5678"), "qa/3-def5678/result.json", "qa/4-abc1234/defects-1.json"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if k, head := newestQADefects(dir); k != 2 || head != "def5678" {
		t.Fatalf("newestQADefects = %d, %q, want run 2 at def5678", k, head)
	}
}
