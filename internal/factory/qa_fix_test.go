package factory

import (
	"encoding/json"
	"errors"
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

// cleanProbes are the probes of a walk whose page shows the dates right.
var cleanProbes = map[string]string{"P1": "clean-tooltip.json"}

// The walker's own finds on the security settings: the tooltip's markup
// shown as text (the factory's checks find it too), naming the partial that
// renders it, and a flow of the check that does not do what it says, which
// only the walker can see.
var (
	escapedFind = QADefect{ID: "D1", Class: QAEscapedMarkup, Severity: "P2", Page: "P1", Title: "the dates show HTML",
		Paths: []string{"app/views/settings/_keys.html.erb"}}
	flowFind = QADefect{ID: "D2", Class: QAFlow, Severity: "P2", Page: "P1", Title: "the date opens no tooltip"}
)

// walkWith is a walk of one page, the security settings, which the walker
// spells path, with the walker's finds ds.
func walkWith(path string, ds ...QADefect) QAVerdict {
	return QAVerdict{Pages: []QAPage{qaPage(1, "P1", path, "A1")}, Defects: ds}
}

// escapedWalk is a walk of the security settings at path with the walker's
// own find of the tooltip's markup.
func escapedWalk(path string) QAVerdict { return walkWith(path, escapedFind) }

// acceptQA sends your retry of task id with --accept-qa, as the CLI does,
// and runs the tick that answers it; the request as that tick left it.
func (r *qaRig) acceptQA(id int64) registry.Request {
	r.t.Helper()
	qid, err := r.reg.EnqueueRequest(r.ctx, ReqRetry, RetryPayload{TaskID: id, Surface: "cli", AcceptQA: true})
	if err != nil {
		r.t.Fatal(err)
	}
	r.tick()
	q, err := r.reg.RequestByID(r.ctx, qid)
	if err != nil {
		r.t.Fatal(err)
	}
	return q
}

// toQAAt runs the ticks until the task is back at its qa stage after k
// review rounds, and returns its head.
func (r *qaRig) toQAAt(task registry.Task, k int) string {
	r.t.Helper()
	r.run(12, func() bool { return len(r.rv.calls()) == k && r.reload(task.ID).Stage == "qa" })
	got := r.reload(task.ID)
	if got.Stage != "qa" || len(r.rv.calls()) != k {
		r.t.Fatalf("after %d review rounds: task %s at %s waits %q: %q", len(r.rv.calls()), got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	return got.HeadSHA
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
// the task needs you naming the defects, the writer's reply and what each
// retry does, and no review round runs. Your retry without --accept-qa
// refuses the decline: QA walks again at the head, and the writer gets the
// defect once more, marked as declined and not accepted, never as come back
// after its fix.
func TestADeclinedQADefectNeedsYouAndNeverBecomesKnown(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12443")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	got := r.toYou(task)
	reply := filepath.Join(r.runDir(task.ID, 1, head), "reply-1.json")
	for _, want := range []string{"declined", "escaped_markup on /settings/security", reply, fmt.Sprintf("magnum factory retry %d --accept-qa", task.ID),
		"accepted by you", fmt.Sprintf("`magnum factory retry %d` walks QA again and hands it to the writer once more (1 of the task's 2 fix turns left)", task.ID)} {
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
	if rf := r.qaDefects(task.ID, 2, head); len(rf.Findings) != 1 || !strings.Contains(rf.Findings[0].Body, "the operator did not accept the decline") ||
		strings.Contains(rf.Findings[0].Body, "came back") {
		t.Fatalf("defects-2.json %+v, want the defect marked as declined in run 1 and not accepted", rf)
	}
}

// I1: the writer fixes one defect and declines the other with its risk,
// beside a commit. The fix goes through the review and QA, and once QA
// finds the declined defect again the task needs you, naming the reply of
// the decline: the writer gets no turn of it again (no defects-2.json),
// and it never came back after a fix. Your --accept-qa accepts it at that
// head: the next run passes with it accepted by you, never a known note.
func TestAPartialDeclineReachesYouAndYourAcceptPassesItAtItsHead(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12447")
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", escapedFind, flowFind), escapedProbes)
	r.toWriter(task, 1)
	rf := r.qaDefects(task.ID, 1, head)
	if len(rf.Findings) != 2 || !strings.HasPrefix(rf.Findings[1].Title, "flow: ") {
		t.Fatalf("defects-1.json %+v, want the markup and the flow", rf.Findings)
	}
	r.writerFixesQA(task, 1, head, fixedCredit(1), map[string]string{"Q2": "the tooltip's script is shared with the mailer"})
	fix := r.toQAAt(task, 1)
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", flowFind), cleanProbes)
	got := r.toYou(task)
	reply := filepath.Join(r.runDir(task.ID, 1, head), "reply-1.json")
	for _, want := range []string{"the writer declined 1 defect that blocks (flow on /settings/security) with its risk", reply,
		fmt.Sprintf("magnum factory retry %d --accept-qa", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	if strings.Contains(got.WaitsWhat, "came back") || strings.Contains(got.WaitsWhat, "shared with the mailer") {
		t.Errorf("the task waits %q: a decline is no return after a fix, and the wait quotes no risk", got.WaitsWhat)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	if fsx.Exists(filepath.Join(dir, qaDefectsFile(2, fix))) || len(r.rt.promptsTo("fx-writer")) != 1 {
		t.Fatalf("the writer got the declined defect again (%d prompts)", len(r.rt.promptsTo("fx-writer")))
	}

	if q := r.acceptQA(task.ID); q.State != registry.RequestDone || !strings.Contains(q.Result, "accepted by you") {
		t.Fatalf("the retry with --accept-qa %+v", q)
	}
	var acc qaAccepted
	if err := json.Unmarshal([]byte(r.readTask(task.ID, qaAcceptedFile)), &acc); err != nil || acc.Head != fix || len(acc.Fingerprints) != 1 ||
		acc.Fingerprints[0] != rf.Findings[1].Fingerprint {
		t.Fatalf("qa-accepted.json %+v (%v), want the flow's fingerprint at %s", acc, err, fix)
	}
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", flowFind), cleanProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q: %q, want pr_body", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	sum, ok := readQASummary(dir)
	if !ok || sum.Outcome != QAOutcomeClean || sum.Head != fix || len(sum.Accepted) != 1 || sum.Accepted[0].Class != QAFlow ||
		slices.ContainsFunc(sum.Known, func(d QADefect) bool { return d.Class == QAFlow }) {
		t.Fatalf("qa-summary.json %+v (%v), want clean at %s with the flow accepted by you, not known", sum, ok, fix)
	}
	if len(sum.Fixed) != 1 || sum.Fixed[0].Class != QAEscapedMarkup || sum.Fixed[0].Run != 1 {
		t.Errorf("the summary's fixed %+v, want the markup alone: the flow you accepted QA still finds", sum.Fixed)
	}
}

// What you accept is exactly the declined defects at their head: another
// defect at that head still blocks and goes to the writer, and a new head
// blocks the accepted one again.
func TestAnAcceptedQADefectLetsAnotherBlockAndANewHeadBlocksItAgain(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12448")
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", flowFind), cleanProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the tooltip's script is shared with the mailer"})
	r.toYou(task)
	if q := r.acceptQA(task.ID); q.State != registry.RequestDone {
		t.Fatalf("the retry with --accept-qa %+v", q)
	}
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", escapedFind, flowFind), escapedProbes)
	r.toWriter(task, 2)
	rf := r.qaDefects(task.ID, 2, head)
	if len(rf.Findings) != 1 || !strings.HasPrefix(rf.Findings[0].Title, "escaped_markup: ") {
		t.Fatalf("defects-2.json %+v, want the markup alone: the flow is accepted at this head", rf.Findings)
	}
	if res := r.qaResult(task.ID, 2, head); len(res.Accepted) != 1 || res.Accepted[0].Class != QAFlow || res.Outcome != QAOutcomeDefects {
		t.Fatalf("run 2's result %+v, want the markup blocking and the flow accepted", res)
	}
	r.writerFixesQA(task, 2, head, fixedCredit(2), nil)
	fix := r.toQAAt(task, 1)
	r.toWalker(task)
	r.walk(task, walkWith("/settings/security", flowFind), cleanProbes)
	got := r.toYou(task)
	if res := r.qaResult(task.ID, 3, fix); res.Outcome != QAOutcomeDefects || len(res.Accepted) != 0 || len(res.Blocking) != 1 ||
		res.Blocking[0].Class != QAFlow {
		t.Fatalf("run 3's result %+v, want the flow blocking again at the new head %s", res, fix)
	}
	if !strings.Contains(got.WaitsWhat, "after the task's 2 fix turns") || !strings.Contains(got.WaitsWhat, "--accept-qa` accepts the 1 the writer declined") {
		t.Fatalf("the task waits %q, want the cap naming the declined flow", got.WaitsWhat)
	}
}

// --accept-qa accepts only what the writer declined: a task whose QA
// stopped for you with no decline is refused, and stays with you.
func TestAcceptQARefusesATaskWithNoDeclinedDefect(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(bool, int) (int, error) { return 0, errRefused })
	task, _ := r.qaTask("PR-12449")
	r.toYou(task)
	q := r.acceptQA(task.ID)
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "--accept-qa") {
		t.Fatalf("the retry with --accept-qa %+v, want it refused", q)
	}
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou {
		t.Fatalf("task %s, want it still with you", got.State)
	}
	if fsx.Exists(filepath.Join(r.layout.FactoryTaskDir(task.ID), qaAcceptedFile)) {
		t.Fatal("qa-accepted.json was written")
	}
}

// M2: a run that looked at another head than the task's (a stop there, or
// defects with no file) starts no QA run at the task's head before the
// local review passed there.
func TestAQARunAtAnotherHeadThanTheTasksGoesToTheReviewFirst(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12450")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	r.toYou(task)
	moved := r.commit(task.JiraKey+" escapes the tooltip by hand", fixedCredit(1))
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("head_sha", moved)
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.run(2, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "local_review" {
		t.Fatalf("task %s at %s waits %q: %q, want the local review at %s", got.State, got.Stage, got.WaitsWho, got.WaitsWhat, moved)
	}
	if n := newestQARun(r.layout.FactoryTaskDir(task.ID)); n != 1 {
		t.Fatalf("QA run %d started before the review of %s", n, moved)
	}
}

// M3: a stop of the fix loop whose move to you did not land (the daemon
// stopped first) still reaches you on the next pass, even after another
// alert of the task: only its own alert lets it go.
func TestADeclineStopReachesYouEvenAfterAnotherAlert(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12451")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.e.beforeStop = func() error { return errors.New("the daemon stops before the move") }
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	dir := r.layout.FactoryTaskDir(task.ID)
	r.run(4, func() bool { return fsx.Exists(filepath.Join(r.runDir(task.ID, 1, head), qaFixStopFile)) })
	if got := r.reload(task.ID); got.State == registry.TaskNeedsYou || !fsx.Exists(filepath.Join(r.runDir(task.ID, 1, head), qaFixStopFile)) {
		t.Fatalf("task %s waits %q: want the stop written and no move yet", got.State, got.WaitsWhat)
	}
	r.e.beforeStop = nil
	r.e.alert(r.ctx, task.ID, TaskKey(task)+": you: another cause")
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "the writer declined") || newestQARun(dir) != 1 {
		t.Fatalf("task waits %q after run %d, want the decline", got.WaitsWhat, newestQARun(dir))
	}
}

// M4: a page whose route lives in its URL's hash is followed by that route,
// never by "/" alone; any other path is cleaned, its query and anchor left
// out.
func TestQAProbePathFollowsAHashRoute(t *testing.T) {
	for raw, want := range map[string]string{
		"http://app.example.localhost:3001/settings/security/":       "/settings/security",
		"http://app.example.localhost:3001/settings?tab=keys#top":    "/settings",
		"http://app.example.localhost:3001/#/settings/security":      "/#/settings/security",
		"http://app.example.localhost:3001/app#!/orders":             "/app#!/orders",
		"http://app.example.localhost:3001":                          "/",
		"http://app.example.localhost:3001/settings#/not-a-route-of": "/settings#/not-a-route-of",
	} {
		if got := qaProbePath(raw); got != want {
			t.Errorf("qaProbePath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// M7: a wait names at most three classes on their pages, then how many
// more.
func TestQAClassesAndPathsNamesAtMostThree(t *testing.T) {
	var pages []QAPage
	var ds []QADefect
	for i := range 5 {
		id := fmt.Sprintf("P%d", i+1)
		pages = append(pages, QAPage{ID: id, Path: fmt.Sprintf("/page/%d", i+1)})
		ds = append(ds, QADefect{Class: QAErrorPage, Page: id})
	}
	if got, want := qaClassesAndPaths(ds, pages), "error_page on /page/1, error_page on /page/2, error_page on /page/3, and 2 more"; got != want {
		t.Fatalf("qaClassesAndPaths = %q, want %q", got, want)
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
	for _, want := range []string{"QA still finds 1 defect that blocks after the task's 2 fix turns",
		"came back after its fix: escaped_markup on settings/security",
		"the writer's replies: " + filepath.Join(r.runDir(task.ID, 1, heads[0]), "reply-1.json") + ", " + filepath.Join(r.runDir(task.ID, 2, heads[1]), "reply-2.json"),
		"its defects with their shots: " + filepath.Join(r.runDir(task.ID, 2, heads[1]), "defects-2.json"),
		"see " + filepath.Join(r.runDir(task.ID, 3, heads[2]), qaResultFile),
		fmt.Sprintf("`magnum factory retry %d` walks QA once more and stops here again unless the branch changed", task.ID),
		"a commit of yours on " + task.Branch + " goes back through coding",
		"a higher `fixes` in [factory.repo.qa], after `magnum factory install`, gives the writer another turn",
		"no defect here was declined with its risk, so `--accept-qa` accepts none"} {
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

// The summary of the run that passed names what QA found in an earlier run
// and the writer fixed: its class, severity, page and state, and the run
// that found it, for the PR body.
func TestTheQASummaryNamesWhatTheWriterFixed(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12457")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	fix := r.toQAAt(task, 1)
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), cleanProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID))
	want := []QAFixed{{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: "/settings/security", State: "signed_in"}}
	if !ok || sum.Run != 2 || sum.Head != fix || !slices.Equal(sum.Fixed, want) {
		t.Fatalf("qa-summary.json %+v (%v), want run 2 at %s naming %+v", sum, ok, fix, want)
	}
	if len(sum.Walk) != 1 || sum.Walk[0].Path != "/settings/security" || sum.Walk[0].State != "signed_in" {
		t.Errorf("the summary's pages %+v", sum.Walk)
	}
}

// shortcutsProbes are the probes of a walk of the help's shortcuts page
// alone, which shows nothing that blocks.
var shortcutsProbes = map[string]string{"P1": "angle-placeholder.json"}

// After a fix turn, the next run checks the writer's fix only on a page it
// walks: the walker's prompt names the earlier defects' pages and states to
// walk again (recheck.json), and a run that walks other pages only is no
// pass for them. It needs you as a state not reached would, naming the
// defect's class, page and state and the run that found it, and nothing is
// "fixed"; your retry walks again, and a walk of the page in its state that
// finds it no more passes with the defect fixed.
func TestAQARunAfterAFixMustWalkTheDefectsPageAgain(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12458")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	fix := r.toQAAt(task, 1)
	r.toWalker(task)
	recheck := filepath.Join(r.runDir(task.ID, 2, fix), qaRecheckFile)
	if text := r.rt.promptsTo("fx-qa")[1].Text; !strings.Contains(text, recheck) {
		t.Errorf("run 2's walker prompt does not name %s:\n%s", recheck, text)
	}
	var again []qaRecheck
	if err := json.Unmarshal([]byte(r.readTask(task.ID, filepath.Join(qaRunDir(2, fix), qaRecheckFile))), &again); err != nil ||
		len(again) != 1 || again[0].Path != "/settings/security" || again[0].State != "signed_in" || again[0].Class != QAEscapedMarkup {
		t.Fatalf("recheck.json %+v (%v), want the security settings signed in, escaped_markup", again, err)
	}
	r.walk(task, QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/help/shortcuts", "A1")}}, shortcutsProbes)
	got := r.toYou(task)
	for _, want := range []string{"escaped_markup on /settings/security in the state signed_in, which QA run 1 found",
		"not walked again after the writer's fix", fmt.Sprintf("magnum factory retry %d", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	if _, ok := readQASummary(dir); ok {
		t.Fatal("a run that did not walk the fixed defect's page passed QA")
	}
	if res := r.qaResult(task.ID, 2, fix); res.Outcome != QAOutcomeUnavailable || len(res.Fixed) != 0 || len(res.Blocked) != 1 {
		t.Fatalf("run 2's result.json %+v, want unavailable, nothing fixed, the page not walked again", res)
	}
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), cleanProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	sum, ok := readQASummary(dir)
	want := []QAFixed{{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: "/settings/security", State: "signed_in"}}
	if !ok || sum.Run != 3 || !slices.Equal(sum.Fixed, want) || len(sum.Blocked) != 0 {
		t.Fatalf("qa-summary.json %+v (%v), want run 3 clean with the defect fixed", sum, ok)
	}
}

// A fix the user-facing check filters out entirely still gets its walk: the
// earlier defect's page is checked again, never passed as not needed.
func TestAQARunAfterAFixWalksEvenWithNoUserFacingFile(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12459")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	r.userFacing = func([]byte) (string, int) { return "", 0 }
	r.toQAAt(task, 1)
	r.toWalker(task)
	if sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatalf("qa-summary.json %+v: the fix passed QA as not needed", sum)
	}
}

// fixAndRecheck is run 1's walk of the security settings, labelled label,
// that finds the tooltip's markup, the writer's fix of it, and the ticks
// until the next run's walker has its prompt; the fix's head.
func (r *qaRig) fixAndRecheck(task registry.Task, head, label string) string {
	r.t.Helper()
	r.toWalker(task)
	r.walk(task, escapedWalk(label), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), nil)
	fix := r.toQAAt(task, 1)
	r.toWalker(task)
	return fix
}

// A page counts as the fixed defect's page walked again by the path its
// probe read alone, never by the walker's label: a walk of the help's
// shortcuts that the walker calls /settings/security, in the same state, is
// no walk of the security settings. It needs you as a skipped walk would,
// and nothing is "fixed".
func TestAMislabelledPageIsNoWalkOfAFixedDefectsPage(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12460")
	fix := r.fixAndRecheck(task, head, "/settings/security")
	r.walk(task, oneSecurityPage(), shortcutsProbes)
	got := r.toYou(task)
	if want := "not walked again after the writer's fix"; !strings.Contains(got.WaitsWhat, want) {
		t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
	}
	if sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatalf("qa-summary.json %+v: a mislabelled page passed the fix", sum)
	}
	if res := r.qaResult(task.ID, 2, fix); len(res.Fixed) != 0 || len(res.Blocked) != 1 || res.Blocked[0].Why != qaNotWalkedWhy {
		t.Fatalf("run 2's result.json %+v, want nothing fixed and the page not walked again", res)
	}
}

// The page a defect was found on is the one its probe read, whatever the
// walker called it then: a run that found the tooltip's markup on a page
// it labelled /account hands the writer /settings/security to walk again,
// and a walk of that page in its state that finds the defect no more passes
// with it fixed there.
func TestAFixedDefectsPageIsTheOneItsProbeRead(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12461")
	fix := r.fixAndRecheck(task, head, "/account")
	var again []qaRecheck
	if err := json.Unmarshal([]byte(r.readTask(task.ID, filepath.Join(qaRunDir(2, fix), qaRecheckFile))), &again); err != nil ||
		len(again) != 1 || again[0].Path != "/settings/security" {
		t.Errorf("recheck.json %+v (%v), want the page its probe read", again, err)
	}
	r.walk(task, oneSecurityPage(), cleanProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID))
	want := []QAFixed{{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: "/settings/security", State: "signed_in"}}
	if !ok || sum.Run != 2 || !slices.Equal(sum.Fixed, want) || len(sum.Blocked) != 0 {
		t.Fatalf("qa-summary.json %+v (%v), want run 2 clean with %+v fixed", sum, ok, want)
	}
	if len(sum.Walk) != 1 || sum.Walk[0].ProbePath != "/settings/security" {
		t.Errorf("the summary's pages %+v, want the path the probe read kept", sum.Walk)
	}
}

// A walk after a fix that skips the fixed defect's page is no state the
// walker cannot reach (run 1 reached it), so it never goes on flagged: the
// second such walk at the head needs you again, with no summary written,
// and the wait names the ways out, a walk of the page or --accept-qa.
func TestTwoWalksThatSkipAFixedDefectsPageNeverGoOnFlagged(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12462")
	r.fixAndRecheck(task, head, "/settings/security")
	shortcuts := QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/help/shortcuts", "A1")}}
	r.walk(task, shortcuts, shortcutsProbes)
	r.toYou(task)
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, shortcuts, shortcutsProbes)
	got := r.toYou(task)
	dir := r.layout.FactoryTaskDir(task.ID)
	if sum, ok := readQASummary(dir); ok {
		t.Fatalf("qa-summary.json %+v: two walks that skipped the fixed page passed QA flagged", sum)
	}
	for _, want := range []string{"not walked again after the writer's fix", fmt.Sprintf("magnum factory retry %d --accept-qa", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	if strings.Contains(got.WaitsWhat, "flagged") {
		t.Errorf("the task waits %q: a skipped walk of a fixed page does not go on flagged", got.WaitsWhat)
	}
}

// Your --accept-qa lets a fix that QA did not walk again go on unchecked,
// at the head: the screen's A offers it, the next walk there passes with
// the fix named as accepted by you and not walked again (never as fixed),
// and the PR body says so.
func TestYourAcceptLetsAFixQADidNotWalkAgainGoOn(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12463")
	fix := r.fixAndRecheck(task, head, "/settings/security")
	shortcuts := QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/help/shortcuts", "A1")}}
	r.walk(task, shortcuts, shortcutsProbes)
	waiting := r.toYou(task)
	dir := r.layout.FactoryTaskDir(task.ID)
	if run, a, err := qaAcceptable(dir, waiting); err != nil {
		t.Fatalf("A is not offered for a fix QA did not walk again: %v", err)
	} else if what, want := qaAcceptWhat(dir, run, a), "1 fix QA did not walk again at "+fix[:7]+
		" (escaped_markup on /settings/security in the state signed_in)"; what != want {
		t.Errorf("A accepts %q, want %q", what, want)
	}
	if q := r.acceptQA(task.ID); q.State != registry.RequestDone || !strings.Contains(q.Result, "1 fix it did not walk again") {
		t.Fatalf("your accept: %+v", q)
	}
	r.toWalker(task)
	r.walk(task, shortcuts, shortcutsProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	sum, ok := readQASummary(dir)
	want := []QAFixed{{Run: 1, Class: QAEscapedMarkup, Severity: "P2", Path: "/settings/security", State: "signed_in"}}
	unchecked := slices.Clone(sum.Unchecked)
	for i := range unchecked {
		unchecked[i].Fingerprint = ""
	}
	if !ok || sum.Head != fix || !slices.Equal(unchecked, want) || len(sum.Fixed) != 0 || len(sum.Blocked) != 0 {
		t.Fatalf("qa-summary.json %+v (%v), want a pass with %+v unchecked, accepted by you", sum, ok, want)
	}
	if !strings.Contains(sum.Why, "1 fix not walked again, accepted by you") {
		t.Errorf("the pass's why %q leaves your accept out", sum.Why)
	}
	if line := qaCardLine(sum); !strings.HasPrefix(line, "passed with 1 fix not walked again, accepted by you") {
		t.Errorf("the card's QA line %q", line)
	}
	section := qaSection(sum)
	if want := "- P2 escaped_markup on `/settings/security` in the state `signed_in`: HTML markup shown as text, found by QA run 1"; !strings.Contains(section, "The writer's fixes QA did not walk again, accepted by you:\n\n"+want) {
		t.Errorf("the PR body's section lacks the unchecked fix:\n%s", section)
	}
}

// skipWalk is a walk of the help's shortcuts alone, at /help/shortcuts as
// its probe reads it, which shows nothing that blocks.
func skipWalk() QAVerdict {
	return QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/help/shortcuts", "A1")}}
}

// An accept of a fix that QA did not walk again holds at its head alone: the
// run that passed with it is no pass of that fix. A run at a new head (a
// send-back, a base merge) walks the page again, and one there that skips it
// waits on you; a run at the accept's head keeps the fix as accepted by you.
func TestAnUncheckedFixAcceptedAtAHeadIsWalkedAgainAtTheNext(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12470")
	fix := r.fixAndRecheck(task, head, "/settings/security")
	r.walk(task, skipWalk(), shortcutsProbes)
	r.toYou(task)
	if q := r.acceptQA(task.ID); q.State != registry.RequestDone {
		t.Fatalf("your accept: %+v", q)
	}
	r.toWalker(task)
	r.walk(task, skipWalk(), shortcutsProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	dir := r.layout.FactoryTaskDir(task.ID)
	sum, ok := readQASummary(dir)
	if !ok || sum.Head != fix || len(sum.Unchecked) != 1 {
		t.Fatalf("qa-summary.json %+v (%v), want the pass at %s with the fix accepted unchecked", sum, ok, fix)
	}
	next := sum.Run + 1
	if again := qaRechecks(dir, next); len(again) != 1 || again[0].Path != "/settings/security" {
		t.Fatalf("the next run walks again %+v, want the unchecked fix's page", again)
	}
	at := func(h string) QAResult {
		res := QAResult{Walk: []QAPage{{ID: "P1", Path: "/help/shortcuts", State: "signed_in", ProbePath: "/help/shortcuts"}}}
		qaCheckFixes(dir, qaRun{N: next, Head: h}, &res)
		return res
	}
	if res := at(strings.Repeat("e", 40)); len(res.Unchecked) != 0 || len(res.Blocked) != 1 || res.Blocked[0].Fingerprint == "" ||
		res.Blocked[0].Why != qaNotWalkedWhy {
		t.Errorf("a run at a new head that skips the page: %+v, want it waiting on you for the fix", res)
	}
	if res := at(fix); len(res.Unchecked) != 1 || len(res.Blocked) != 0 {
		t.Errorf("a run at the accept's head: %+v, want the fix still accepted by you", res)
	}
}

// A defect the writer declined and you accepted stays accepted by you, not
// fixed: a walk at the accept's head that skips its page passes with it
// under "Accepted by you, not fixed", never waits on you again, never offers
// it as a fix QA did not walk again and never says the writer fixed it. At
// a new head the accept no longer holds: a walk that skips the page waits on
// you for the declined defect, never for a fix.
func TestAnAcceptedDeclineAWalkSkipsStaysAcceptedNotFixed(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12471")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	r.toYou(task)
	if q := r.acceptQA(task.ID); q.State != registry.RequestDone {
		t.Fatalf("your accept of the decline: %+v", q)
	}
	r.toWalker(task)
	r.walk(task, skipWalk(), shortcutsProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	dir := r.layout.FactoryTaskDir(task.ID)
	sum, ok := readQASummary(dir)
	if !ok {
		t.Fatalf("no qa-summary.json: the task waits %q", r.reload(task.ID).WaitsWhat)
	}
	if len(sum.Accepted) != 1 || sum.Accepted[0].Class != QAEscapedMarkup || len(sum.Fixed) != 0 || len(sum.Unchecked) != 0 ||
		len(sum.Blocked) != 0 {
		t.Fatalf("qa-summary.json %+v, want the declined defect accepted by you, nothing fixed", sum)
	}
	s := qaSection(sum)
	if strings.Contains(s, "Fixed by the writer") || strings.Contains(s, "fixed before this PR") {
		t.Errorf("the PR body says the writer fixed a defect it declined:\n%s", s)
	}
	if want := "Accepted by you, not fixed:\n\n- P2 escaped_markup on `/settings/security` in the state `signed_in`: HTML markup shown as text"; !strings.Contains(s, want) {
		t.Errorf("the PR body lacks %q:\n%s", want, s)
	}
	res := QAResult{Walk: []QAPage{{ID: "P1", Path: "/help/shortcuts", State: "signed_in", ProbePath: "/help/shortcuts"}}}
	qaCheckFixes(dir, qaRun{N: sum.Run + 1, Head: strings.Repeat("e", 40)}, &res)
	if len(res.Accepted) != 0 || len(res.Unchecked) != 0 || len(res.Fixed) != 0 || len(res.Blocked) != 1 ||
		res.Blocked[0].Why != qaDeclinedNotWalkedWhy {
		t.Errorf("a run at a new head that skips the page: %+v, want it waiting on you for the declined defect", res)
	}
}

// A decline stays a decline whatever commits sit beside it: one that names
// no risk, made with a commit, whose page the next run skips, waits on you as
// a decline (no fix was made), A offers it as one, and the pass after your
// accept lists it as accepted by you, not fixed, never "Fixed by the
// writer". Found again, the writer reads that it declined it with no risk,
// never that it came back after its fix.
func TestADeclineWithoutARiskBesideACommitIsNeverAFix(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12480")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), map[string]string{"Q1": ""})
	r.toQAAt(task, 1)
	r.toWalker(task)
	r.walk(task, skipWalk(), shortcutsProbes)
	got := r.toYou(task)
	if want := qaDeclinedNoRiskNotWalkedWhy; !strings.Contains(got.WaitsWhat, want) || strings.Contains(got.WaitsWhat, "after the writer's fix") {
		t.Errorf("the task waits %q, want %q and no fix", got.WaitsWhat, want)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	reply := filepath.Join(dir, filepath.FromSlash(qaReplyFile(1, head)))
	if want := "the writer's reply (no risk named): " + reply; !strings.Contains(got.WaitsWhat, want) || strings.Contains(got.WaitsWhat, "with its risk") {
		t.Errorf("the task waits %q, want %q and no risk", got.WaitsWhat, want)
	}
	run, a, err := qaAcceptable(dir, got)
	if what := qaAcceptWhat(dir, run, a); err != nil || !strings.HasPrefix(what, "1 QA defect the writer declined at ") || strings.Contains(what, "fix") {
		t.Fatalf("A accepts %q (%v), want the decline, never a fix", what, err)
	}
	if q := r.acceptQA(task.ID); q.State != registry.RequestDone {
		t.Fatalf("your accept: %+v", q)
	}
	r.toWalker(task)
	r.walk(task, skipWalk(), shortcutsProbes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	sum, ok := readQASummary(dir)
	if !ok || len(sum.Accepted) != 1 || len(sum.Unchecked) != 0 || len(sum.Fixed) != 0 {
		t.Fatalf("qa-summary.json %+v (%v), want the decline accepted by you, no fix", sum, ok)
	}
	if s := qaSection(sum); strings.Contains(s, "Fixed by the writer") || strings.Contains(s, "fixed before this PR") ||
		!strings.Contains(s, "Accepted by you, not fixed") {
		t.Errorf("the PR body:\n%s", s)
	}
	it := qaFixItem(0, QADefect{ID: "D1", Class: QAEscapedMarkup, Severity: "P2", Page: "P1"}, QAPage{Path: "/settings/security"}, "",
		qaHanded{run: 1, declinedNoRisk: true})
	if !strings.Contains(it.Body, "you declined it with no risk in run 1") || strings.Contains(it.Body, "came back after its fix") {
		t.Errorf("the writer's item %q", it.Body)
	}
}

// At the head the writer declined a defect at, a walk that does not find it
// is no proof it is gone (the factory's own check found it on that page at
// that code): unless you accepted the decline, the task waits on you, naming
// the writer's reply with its risk, and no summary passes it.
func TestADeclineYouDidNotAcceptThatAWalkAtItsHeadMissesNeedsYou(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12481")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	r.toYou(task)
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), cleanProbes)
	got := r.toYou(task)
	dir := r.layout.FactoryTaskDir(task.ID)
	if sum, ok := readQASummary(dir); ok {
		t.Fatalf("qa-summary.json %+v: a decline you did not accept passed at its head", sum)
	}
	reply := filepath.Join(dir, filepath.FromSlash(qaReplyFile(1, head)))
	for _, want := range []string{"QA did not find again 1 defect the writer declined at this head", qaDeclinedNotFoundWhy,
		"the writer's reply, with its risk: " + reply, fmt.Sprintf("magnum factory retry %d --accept-qa", task.ID), "stops here again"} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	if strings.Contains(got.WaitsWhat, "could not reach") {
		t.Errorf("the task waits %q: the walk reached the page", got.WaitsWhat)
	}
}

// The fix turn's event counts a decline as declined whether or not it names
// a risk: a commit beside a decline with no risk fixed nothing of it.
func TestTheFixEventCountsADeclineWithoutARiskAsDeclined(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12492")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, fixedCredit(1), map[string]string{"Q1": ""})
	fix := r.toQAAt(task, 1)
	if got, want := r.eventsWith(task.ID, "qa", "run 1's fix at "), []string{"run 1's fix at " + fix[:7] + ": 0 fixed, 1 declined"}; !slices.Equal(got, want) {
		t.Errorf("qa events %q, want %q", got, want)
	}
}

// Your accept's own record says who accepted: the qa event names the
// liaison when it ran the retry for you, and qa-accepted.json keeps that
// surface.
func TestAnAcceptSaysWhoAcceptedIt(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12483")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	r.toYou(task)
	qid, err := r.reg.EnqueueRequest(r.ctx, ReqRetry, RetryPayload{TaskID: task.ID, Surface: "liaison", AcceptQA: true})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	if q, err := r.reg.RequestByID(r.ctx, qid); err != nil || q.State != registry.RequestDone {
		t.Fatalf("the liaison's accept: %+v (%v)", q, err)
	}
	if got := r.eventsWith(task.ID, "qa", "through the liaison"); len(got) != 1 {
		t.Errorf("qa events %q, want the accept naming the liaison", got)
	}
	if acc, ok := readQAAccepted(r.layout.FactoryTaskDir(task.ID)); !ok || acc.By != "liaison" {
		t.Errorf("qa-accepted.json %+v (%v), want it by the liaison", acc, ok)
	}
}

// A decline that a walk at its head did not find again is counted as such,
// never as a state the walker could not reach: the run's qa event and its
// report name it apart.
func TestADeclineNotFoundAgainIsNoStateNotReachedInTheEventOrTheReport(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12486")
	r.toWalker(task)
	r.walk(task, escapedWalk("/settings/security"), escapedProbes)
	r.toWriter(task, 1)
	r.writerFixesQA(task, 1, head, nil, map[string]string{"Q1": "the key is shared with the mailer"})
	r.toYou(task)
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), cleanProbes)
	r.toYou(task)
	if got := r.eventsWith(task.ID, "qa", "run 2: the factory's checks"); len(got) != 1 ||
		!strings.Contains(got[0], "0 states not reached, 1 defect the writer declined not found again") {
		t.Errorf("run 2's qa event %q, want the decline counted apart", got)
	}
	report := r.readTask(task.ID, filepath.Join(qaRunDir(2, head), qaReportFile))
	if !strings.Contains(report, "## Declined by the writer, not found again at this head (1)") || strings.Contains(report, "## Not reached") {
		t.Errorf("run 2's report:\n%s", report)
	}
}

// A run that leaves you a fix it did not walk again beside states the
// walker could not reach says that your accept takes the fix alone: the
// states wait on you again, for a second accept, once a walk is left with
// them alone.
func TestAMixedRunsWaitSaysTheStatesNeedASecondAccept(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12487")
	r.fixAndRecheck(task, head, "/settings/security")
	v := skipWalk()
	v.Blocked = []QABlocked{{What: "the passkey autofill list", Why: "headless Chrome draws no autofill", Check: "A1"}}
	r.walk(task, v, shortcutsProbes)
	got := r.toYou(task)
	if want := "the 1 state the walker could not reach is not part of that accept"; !strings.Contains(got.WaitsWhat, want) ||
		!strings.Contains(got.WaitsWhat, "a second `magnum factory retry") {
		t.Errorf("the task waits %q, want %q and the second accept", got.WaitsWhat, want)
	}
}
