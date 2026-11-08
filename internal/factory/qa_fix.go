package factory

// QA's fix loop (R55 slice 1, step 6). A QA run whose defects block hands
// them to the writer: defects-<k>.json in the run's folder, in the shape of
// the local review's findings (a ReviewFile), for one fix turn of the
// coding stage's roles in the qa stage with QAFixPrompt (runTurn, with the
// fix's end fact of the local review and the run's reply file,
// reviewTurn.reply), until an attempt after the file's passed. The proof
// and the precheck run at the new head through the proof end fact; then
// the task goes back to the local review, whose loop starts afresh (its
// last cycle reviewed another head), and QA runs after it, at most
// [factory.repo.qa] fixes turns a task. A defect QA finds that blocks
// never becomes a known note (roadmap R56), and only you can accept it:
// one that comes back after its fix still blocks, named so, never a doubt;
// one the writer declines with its risk stops the task for you, at once
// when it committed nothing, else once QA finds it again; past the cap the
// task needs you, never the PR body. After a stop, `magnum factory retry
// --accept-qa` accepts the defects the writer declined, at the head (the
// next run there lets exactly those pass, and its summary names them as
// accepted by you), and a plain retry walks QA again: a decline you saw and
// did not accept goes to the writer once more, within the cap.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// QAFixPrompt is the prompt of the writer's fix turn of QA's defects: the
// coding stage's roles get it in place of their own prompt.
const QAFixPrompt = "factory-qa-fix.md"

// qaFixStopFile is fix-stop.json in a run's folder: why its fix loop
// stopped for you (the fixes cap, the writer's decline) and when, written
// before the move to you (qaFixStopForYou), so a daemon that stops before
// the move finds the stop with no word to you since and makes the move
// then, never a fresh run.
const qaFixStopFile = "fix-stop.json"

// qaFixStop is fix-stop.json: why, when, and the defects that block which
// the writer declined with its risk (what --accept-qa accepts).
type qaFixStop struct {
	Why      string       `json:"why"`
	At       time.Time    `json:"at"`
	Declined []qaDeclined `json:"declined,omitempty"`
}

// qaDeclined is a defect the writer declined with its risk: its
// fingerprint, and the run whose reply declined it.
type qaDeclined struct {
	Fingerprint string `json:"fingerprint"`
	Run         int    `json:"run"`
}

// qaAcceptedFile is qa-accepted.json in the task's directory: the QA
// defects you accepted (`magnum factory retry --accept-qa`), by
// fingerprint, at the head QA found them at.
const qaAcceptedFile = "qa-accepted.json"

// qaAccepted is qa-accepted.json: at Head, the defects you let pass
// (Fingerprints: the writer declined them), the defects whose fix you let go
// without QA's walk of their pages (Unchecked), when you last accepted, and
// where (By: the retry's surface, cli, screen or liaison).
type qaAccepted struct {
	Head         string    `json:"head"`
	Fingerprints []string  `json:"fingerprints"`
	Unchecked    []string  `json:"unchecked,omitempty"`
	At           time.Time `json:"at"`
	By           string    `json:"by,omitempty"`
}

// qaAccept is what --accept-qa accepts at a run's head, by fingerprint: the
// defects the writer declined (declined: at its fix loop's stop, or whose
// pages the run did not walk again), and the fixes of the writer's whose
// pages the run did not walk again (unchecked).
type qaAccept struct {
	declined  []string
	unchecked []string
}

// qaDefectsFile is run k's defects for the writer at head, relative to the
// task's directory: qa/<k>-<sha7>/defects-<k>.json, a ReviewFile.
func qaDefectsFile(k int, head string) string {
	return fmt.Sprintf("%s/defects-%d.json", qaRunDir(k, head), k)
}

// qaReplyFile is the writer's reply to run k's defects at head:
// qa/<k>-<sha7>/reply-<k>.json, a ReviewReply.
func qaReplyFile(k int, head string) string {
	return fmt.Sprintf("%s/reply-%d.json", qaRunDir(k, head), k)
}

// isFixTurn reports whether st is a fix turn of the writer: the local
// review's or QA's.
func isFixTurn(st config.FactoryStage) bool {
	return st.Prompt == ReviewFixPrompt || st.Prompt == QAFixPrompt
}

// qaDefectsRe is a run's defects-<k>.json.
var qaDefectsRe = regexp.MustCompile(`^defects-([1-9][0-9]*)\.json$`)

// qaDefectsRef is a run with a defects file: its number and its folder's
// head (sha7).
type qaDefectsRef struct {
	k    int
	head string
}

// qaDefectsFiles are the runs with a defects file in the task directory
// dir, ascending: a file whose number is not its folder's is none.
func qaDefectsFiles(dir string) []qaDefectsRef {
	paths, _ := filepath.Glob(filepath.Join(dir, "qa", "*", "defects-*.json"))
	var out []qaDefectsRef
	for _, p := range paths {
		run := filepath.Base(filepath.Dir(p))
		m, d := qaRunDirRe.FindStringSubmatch(run), qaDefectsRe.FindStringSubmatch(filepath.Base(p))
		if m == nil || d == nil || m[1] != d[1] {
			continue
		}
		if k, err := strconv.Atoi(d[1]); err == nil {
			out = append(out, qaDefectsRef{k: k, head: run[len(m[1])+1:]})
		}
	}
	slices.SortFunc(out, func(a, b qaDefectsRef) int { return a.k - b.k })
	return out
}

// newestQADefects is the newest run with a defects file in the task
// directory dir: its number and its folder's head (sha7); 0 and "" for
// none.
func newestQADefects(dir string) (k int, head string) {
	if refs := qaDefectsFiles(dir); len(refs) > 0 {
		return refs[len(refs)-1].k, refs[len(refs)-1].head
	}
	return 0, ""
}

// qaFixTurns is how many fix turns QA's defects got in task t: the defects
// files of its runs.
func (e *Engine) qaFixTurns(t registry.Task) int {
	return len(qaDefectsFiles(e.d.Layout.FactoryTaskDir(t.ID)))
}

// qaFixes is task t's cap of QA fix turns: its repository's
// [factory.repo.qa] fixes.
func (e *Engine) qaFixes(t registry.Task) int {
	if repo, ok := e.cfg.FactoryRepo(t.Repo); ok && repo.QA != nil {
		return cmp.Or(repo.QA.Fixes, config.DefaultQAFixes)
	}
	return config.DefaultQAFixes
}

// qaFixPhase goes on with the fix loop of run's defects that block (its
// result res) for task t at qa stage st, and reports whether the loop holds
// the stage (false: a fresh run is QA's next step). A run at another head
// than the task's with no fix of its own to close (a stop there, or no
// defects file) holds it only to send the task to the review first, when
// the review has not passed at the task's head (qaReviewFirst). A stop for
// you holds it until you were told; then your retry walks again. At the
// head QA looked at, the first pass writes the defects file (qaStartFix;
// a decline you were not told of, or the cap, stops for you instead), then
// the writer's fix turn runs until it passed, and its close follows
// (closeQAFix). A task with no head yet counts as at the run's.
func (e *Engine) qaFixPhase(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	head := cmp.Or(t.HeadSHA, run.Head)
	stop, stopped := readQAFixStop(dir, run)
	rf, handed := readQADefects(dir, run)
	if run.Head != head && (stopped || !handed) {
		return e.qaReviewFirst(ctx, t, run)
	}
	if stopped {
		told, err := e.qaToldOfFixStop(ctx, t, stop)
		return err != nil || !told, err
	}
	if !handed {
		var err error
		if rf, handed, err = e.qaStartFix(ctx, t, st, run, res); err != nil || !handed {
			return true, err
		}
	}
	done, err := e.fixDone(ctx, t, st, rf)
	if err != nil {
		return true, err
	}
	if !done {
		roles := e.writerRoles(t)
		if len(roles) == 0 {
			return true, e.stageFailed(ctx, t, needsYou{"no stage of the " + t.Kind + " kind writes the change: QA's defects have no writer to fix them"})
		}
		live := make([]string, 0, len(rf.Findings))
		for _, i := range rf.Findings {
			live = append(live, i.ID)
		}
		turn := config.FactoryStage{Name: st.Name, Type: config.FactoryStageAgents, Roles: roles, End: config.FactoryEndProof, Prompt: QAFixPrompt}
		rt := &reviewTurn{n: run.N, reviewed: run.Head, live: live, reply: qaReplyFile(run.N, run.Head), by: "QA walked"}
		if done, err = e.runTurn(ctx, t, turn, agentsTurn{review: rt}); err != nil || !done {
			return true, err
		}
	}
	return e.closeQAFix(ctx, t, run, res, rf)
}

// qaReviewFirst lets go of run, a QA run at another head than task t's, and
// reports whether the stage is held: the task goes back to the review stage
// before qa (backToReview) when the review has not passed at the task's head
// (qaFixReviewed), so no QA run starts at a head the review never saw.
func (e *Engine) qaReviewFirst(ctx context.Context, t registry.Task, run qaRun) (bool, error) {
	reviewed, err := e.qaFixReviewed(ctx, t)
	if err != nil || reviewed {
		return err != nil, err
	}
	return true, e.backToReview(ctx, t, fmt.Sprintf("QA run %d looked at %s, another head than %s: the local review and QA run at the task's head",
		run.N, textx.ShortSHA(run.Head), textx.ShortSHA(t.HeadSHA)))
}

// qaStartFix writes run's defects file for the writer from its defects
// that block (res) and reports whether it did. It stops for you instead
// (qaFixStopForYou) when the writer declined one of them with its risk in
// an earlier run's reply and no stop told you of that decline yet: only you
// can accept a defect QA finds, and the writer gets no turn of it before
// you saw the decline; then once task t had its fixes cap of fix turns. Each
// stop records the defects that the writer declined, which --accept-qa
// accepts. Each defect is a ReviewItem: Q<i>, its fingerprint and severity,
// its class before its title, the first code path it names, and a body
// with its page's path, state and shot (an absolute path) and its quote,
// and what an earlier run made of it (qaFixItem).
func (e *Engine) qaStartFix(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (ReviewFile, bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	pages := qaVerdictPages(dir, run)
	hist := qaHistory(dir, run.N)
	seen := qaSeenDeclines(dir, run.N)
	var back, declined []QADefect
	var stopDeclined []qaDeclined
	unseen := false
	for _, d := range res.Blocking {
		h, ok := hist[d.Fingerprint]
		switch {
		case !ok:
		case h.declined:
			dc := qaDeclined{Fingerprint: d.Fingerprint, Run: h.run}
			declined, stopDeclined = append(declined, d), append(stopDeclined, dc)
			unseen = unseen || !seen[dc]
		case h.declinedNoRisk: // open: it goes to the writer again, as no fix of it came back
		default:
			back = append(back, d)
		}
	}
	fixes, turns := e.qaFixes(t), e.qaFixTurns(t)
	switch {
	case unseen:
		why := e.qaDeclinedWhat(t, run, declined, slices.Collect(maps.Values(pages)), qaReplies(dir, declined, hist), fixes-turns, fixes)
		return ReviewFile{}, false, e.qaFixStopForYou(ctx, t, run, qaFixStop{Why: why, Declined: stopDeclined}, "the writer declined QA's defects")
	case turns >= fixes:
		why := e.qaCapWhat(t, dir, run, res, slices.Collect(maps.Values(pages)), back, declined, turns)
		return ReviewFile{}, false, e.qaFixStopForYou(ctx, t, run, qaFixStop{Why: why, Declined: stopDeclined},
			"QA's defects outlasted the writer's fix turns")
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return ReviewFile{}, false, err
	}
	runPath := filepath.Join(dir, filepath.FromSlash(run.Dir))
	rf := ReviewFile{Round: run.N, Head: run.Head, AttemptsBefore: n}
	for i, d := range res.Blocking {
		rf.Findings = append(rf.Findings, qaFixItem(i, d, pages[d.Page], runPath, hist[d.Fingerprint]))
	}
	name := qaDefectsFile(run.N, run.Head)
	if err := writeJSON(filepath.Join(dir, filepath.FromSlash(name)), rf); err != nil {
		return ReviewFile{}, false, err
	}
	what := fmt.Sprintf("run %d at %s: %s that block%s for the writer (%s)", run.N, textx.ShortSHA(run.Head),
		plural(len(rf.Findings), "defect"), blocksS(len(rf.Findings)), name)
	if len(back) > 0 {
		what += fmt.Sprintf(", %d came back after its fix", len(back))
	}
	if len(declined) > 0 {
		what += fmt.Sprintf(", %d declined before and not accepted by you", len(declined))
	}
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, what)
	return rf, true, nil
}

// qaDeclinedWhat is the wait on you of run's stop for defects ds that block,
// which the writer declined with its risk (its replies), with left of the
// task's fixes fix turns left: what --accept-qa and a plain retry do.
func (e *Engine) qaDeclinedWhat(t registry.Task, run qaRun, ds []QADefect, pages []QAPage, replies []string, left, fixes int) string {
	n := len(ds)
	it := "it"
	if n > 1 {
		it = "them"
	}
	s := fmt.Sprintf("the writer declined %s that block%s (%s) with its risk, and QA passes no visible defect as a known note: see %s; "+
		"`magnum factory retry %d --accept-qa` accepts %s at %s (the PR body names %s as accepted by you)", plural(n, "defect"), blocksS(n),
		qaClassesAndPaths(ds, pages), strings.Join(replies, ", "), t.ID, it, textx.ShortSHA(run.Head), it)
	if left > 0 {
		return s + fmt.Sprintf(", and `magnum factory retry %d` walks QA again and hands %s to the writer once more (%d of the task's %s left)",
			t.ID, it, left, plural(fixes, "fix turn"))
	}
	return s + fmt.Sprintf(", and `magnum factory retry %d` walks QA again and stops at the cap of the task's %s", t.ID, plural(fixes, "fix turn"))
}

// qaCapWhat is the wait on you of run's stop at the cap, after the task's
// turns fix turns: its defects that block (res) by class and page, those
// that came back after their fix (back), the writer's replies and the newest
// defects file (the bodies with the shots), the run's result, and what each
// way out does: a retry walks once more and stops here again, a commit of
// yours goes back through coding, a higher fixes gives the writer a turn,
// and --accept-qa accepts the defects the writer declined (declined).
func (e *Engine) qaCapWhat(t registry.Task, dir string, run qaRun, res QAResult, pages []QAPage, back, declined []QADefect, turns int) string {
	n := len(res.Blocking)
	s := fmt.Sprintf("QA still finds %s that block%s after the task's %s (%s)", plural(n, "defect"), blocksS(n), plural(turns, "fix turn"), res.Why)
	if len(back) > 0 {
		s += "; came back after its fix: " + qaClassesAndPaths(back, pages)
	}
	refs := qaDefectsFiles(dir)
	var replies []string
	for _, ref := range refs {
		if p := filepath.Join(dir, filepath.FromSlash(qaReplyFile(ref.k, ref.head))); fsx.Exists(p) {
			replies = append(replies, p)
		}
	}
	if len(replies) > 0 {
		s += "; the writer's replies: " + strings.Join(replies, ", ")
	}
	if len(refs) > 0 {
		last := refs[len(refs)-1]
		s += "; its defects with their shots: " + filepath.Join(dir, filepath.FromSlash(qaDefectsFile(last.k, last.head)))
	}
	s += "; see " + filepath.Join(dir, filepath.FromSlash(run.Dir), qaResultFile)
	s += fmt.Sprintf("; `magnum factory retry %d` walks QA once more and stops here again unless the branch changed; a commit of yours on %s "+
		"goes back through coding, the local review and QA; a higher `fixes` in [factory.repo.qa], after `magnum factory install`, gives "+
		"the writer another turn", t.ID, t.Branch)
	if len(declined) == 0 {
		return s + "; no defect here was declined with its risk, so `--accept-qa` accepts none"
	}
	return s + fmt.Sprintf("; `magnum factory retry %d --accept-qa` accepts the %d the writer declined with its risk (%s) at %s", t.ID,
		len(declined), qaClassesAndPaths(declined, pages), textx.ShortSHA(run.Head))
}

// qaFixItem is QA's defect d, the i-th that blocks, as the writer gets it
// (qaStartFix), on page pg in the run's folder runPath; h is what an
// earlier run made of it (the zero value for none): a defect it handed the
// writer to fix came back after its fix; one the writer declined, which you
// saw and did not accept, is to be fixed now.
func qaFixItem(i int, d QADefect, pg QAPage, runPath string, h qaHanded) ReviewItem {
	it := ReviewItem{ID: fmt.Sprintf("Q%d", i+1), Fingerprint: d.Fingerprint, Severity: d.Severity, Title: d.Class + ": " + d.Title}
	if len(d.Paths) > 0 {
		it.Path = qaClip(d.Paths[0])
	}
	body := "page " + cmp.Or(qaClip(pg.Path), "id "+qaClip(d.Page))
	if pg.State != "" {
		body += ", state " + qaClip(pg.State)
	}
	if pg.Shot != "" {
		body += ", shot " + filepath.Join(runPath, filepath.FromSlash(pg.Shot))
	}
	if d.Quote != "" {
		body += "; the page shows: " + d.Quote
	}
	switch {
	case h.run > 0 && h.declined:
		body += fmt.Sprintf("; you declined it with its risk in run %d, and the operator did not accept the decline: fix it, or decline it "+
			"again with its risk, which stops the task for the operator", h.run)
	case h.run > 0 && h.declinedNoRisk:
		body += fmt.Sprintf("; you declined it with no risk in run %d, which counts as open: fix it, or decline it with its risk, which "+
			"stops the task for the operator", h.run)
	case h.run > 0:
		body += fmt.Sprintf("; came back after its fix (run %d)", h.run)
	}
	it.Body = body
	return it
}

// closeQAFix closes the fix turn of run's defects rf once it passed, and
// reports whether the loop still holds the stage. The task's head is the
// one the factory's checks passed at (passStage put it there). At run's
// head the writer committed nothing, so it declined every defect with its
// risk (commitProblem passes no other turn without a commit): the task
// needs you at once, as only you can accept a defect QA finds. At a new head
// the local review and QA run again (backToReview), unless the review
// passed at that head since (the move landed: QA's fresh run there is
// next); a defect the writer declined beside its commit reaches you once QA
// finds it again (qaStartFix).
func (e *Engine) closeQAFix(ctx context.Context, t registry.Task, run qaRun, res QAResult, rf ReviewFile) (bool, error) {
	cur, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil {
		return true, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if cur.HeadSHA == "" || cur.HeadSHA == run.Head {
		var declined []qaDeclined
		for _, i := range rf.Findings {
			declined = append(declined, qaDeclined{Fingerprint: i.Fingerprint, Run: run.N})
		}
		reply := filepath.Join(dir, filepath.FromSlash(qaReplyFile(run.N, run.Head)))
		why := e.qaDeclinedWhat(cur, run, res.Blocking, slices.Collect(maps.Values(qaVerdictPages(dir, run))), []string{reply},
			e.qaFixes(cur)-e.qaFixTurns(cur), e.qaFixes(cur))
		return true, e.qaFixStopForYou(ctx, cur, run, qaFixStop{Why: why, Declined: declined}, "the writer declined QA's defects")
	}
	reviewed, err := e.qaFixReviewed(ctx, cur)
	if err != nil || reviewed {
		return err != nil, err
	}
	declined := len(readQAReply(dir, run).declinedWithRisk())
	e.event(ctx, t.ID, "qa", registry.PhaseOK, fmt.Sprintf("run %d's fix at %s: %d fixed, %d declined", run.N, textx.ShortSHA(cur.HeadSHA),
		len(rf.Findings)-declined, declined))
	return true, e.backToReview(ctx, cur, fmt.Sprintf("QA's fix at %s: the local review and QA run again", textx.ShortSHA(cur.HeadSHA)))
}

// reviewStageBefore is the review stage of task t's kind before its stage
// (the qa stage), the nearest one; "" for none.
func (e *Engine) reviewStageBefore(t registry.Task) string {
	kind := e.cfg.Factory.Kinds[t.Kind]
	name := ""
	for _, st := range kind.Stages[:max(stageIndex(kind, t.Stage), 0)] {
		if st.Type == config.FactoryStageReview {
			name = st.Name
		}
	}
	return name
}

// qaFixReviewed reports whether task t's head needs no review before QA
// walks it: its kind has no review stage before its qa stage, or the local
// review's newest cycle ended clean or on an oscillation at that head (the
// review stage passed there, as runReview passes it).
func (e *Engine) qaFixReviewed(ctx context.Context, t registry.Task) (bool, error) {
	if e.reviewStageBefore(t) == "" {
		return true, nil
	}
	cs, err := e.reg.Cycles(ctx, t.ID)
	if err != nil || len(cs) == 0 {
		return false, err
	}
	last := cs[len(cs)-1]
	return !last.Ended.IsZero() && (last.Exit == registry.ExitClean || last.Exit == registry.ExitOscillation) && last.HeadBefore == t.HeadSHA, nil
}

// backToReview sends task t from its qa stage back to the review stage of
// its kind before it (reviewStageBefore), with its attempt count reset and
// its wait cleared, because: the review starts a fresh loop (its last cycle
// reviewed another head), and QA runs after it. A kind with no review stage
// there leaves the task where it is.
func (e *Engine) backToReview(ctx context.Context, t registry.Task, because string) error {
	name := e.reviewStageBefore(t)
	if name == "" {
		return nil
	}
	if err := e.reg.TransitionTask(ctx, t.ID, liveStates, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", name)
		u.Set("attempt", 0)
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because(because)
	}); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, "back to "+name+": "+because)
	return nil
}

// qaFixStopForYou stops run's fix loop for you with stop (its time set
// here): fix-stop.json first, a qa event, then task t moves to you with its
// why (a daemon that stops before the move makes it on the next pass,
// qaToldOfFixStop).
func (e *Engine) qaFixStopForYou(ctx context.Context, t registry.Task, run qaRun, stop qaFixStop, because string) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	stop.At = e.now()
	if err := writeJSON(filepath.Join(dir, filepath.FromSlash(run.Dir), qaFixStopFile), stop); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseFail, fmt.Sprintf("run %d at %s: its fix loop stopped for you", run.N, textx.ShortSHA(run.Head)))
	if e.beforeStop != nil {
		if err := e.beforeStop(); err != nil {
			return err
		}
	}
	e.toOperator(ctx, t, stop.Why, because)
	return nil
}

// qaToldOfFixStop reports whether you were told of the fix loop's stop: its
// own alert since it, the one whose text holds its why (another alert of
// the task, of another cause, tells you nothing of it). A live task told so
// is one you let go on, and QA walks again. One not told is moved to you
// now, with the stop's reason.
func (e *Engine) qaToldOfFixStop(ctx context.Context, t registry.Task, stop qaFixStop) (bool, error) {
	evs, err := e.reg.Events(ctx, t.ID, qaEventsRead)
	if err != nil {
		return false, err
	}
	why := execx.Redact(stop.Why)
	if slices.ContainsFunc(evs, func(ev registry.Event) bool {
		return ev.Kind == "alert" && !ev.At.Before(stop.At) && strings.Contains(ev.Text, why)
	}) {
		return true, nil
	}
	e.toOperator(ctx, t, stop.Why, "QA's fix loop stopped for you")
	return false, nil
}

// acceptQA records your acceptance, through surface (the retry's: cli,
// screen or liaison), of what task t's newest QA run left you at its head
// (qaAcceptable): the defects the writer declined, at its fix loop's stop
// (fix-stop.json) or whose pages the run did not walk again, as declines,
// and the writer's fixes the run did not walk again, unchecked. It writes
// them to qa-accepted.json with those accepted at that head before (an
// accept holds at its head alone) and the surface, writes a qa event that
// names the surface, and returns what the retry's answer adds. A task whose
// newest run left none of them at its head is refused: --accept-qa accepts
// nothing else. undo puts the file back as it was (the retry's move
// failed).
func (e *Engine) acceptQA(ctx context.Context, t registry.Task, surface string) (what string, undo func(), err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	run, a, err := qaAcceptable(dir, t)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, qaAcceptedFile)
	before, readErr := os.ReadFile(path)
	acc, _ := readQAAccepted(dir)
	if acc.Head != run.Head {
		acc = qaAccepted{Head: run.Head}
	}
	for _, fp := range a.declined {
		if !slices.Contains(acc.Fingerprints, fp) {
			acc.Fingerprints = append(acc.Fingerprints, fp)
		}
	}
	for _, fp := range a.unchecked {
		if !slices.Contains(acc.Unchecked, fp) {
			acc.Unchecked = append(acc.Unchecked, fp)
		}
	}
	acc.At, acc.By = e.now(), surface
	if err := writeJSON(path, acc); err != nil {
		return "", nil, err
	}
	undo = func() {
		if readErr != nil {
			_ = os.Remove(path)
			return
		}
		_ = fsx.WriteFileAtomic(path, before, 0o600)
	}
	short := textx.ShortSHA(run.Head)
	var events, lets []string
	if n := len(a.declined); n > 0 {
		events = append(events, fmt.Sprintf("accepted %s the writer declined", plural(n, "QA defect")))
		lets = append(lets, fmt.Sprintf("%s the writer declined", plural(n, "defect")))
	}
	if n := len(a.unchecked); n > 0 {
		events = append(events, fmt.Sprintf("let %s QA did not walk again go unchecked", qaFixes(n)))
		lets = append(lets, qaFixes(n)+" it did not walk again")
	}
	n := len(a.declined) + len(a.unchecked)
	e.event(ctx, t.ID, "qa", registry.PhaseOK, fmt.Sprintf("you %s at %s (run %d), %s", strings.Join(events, " and "), short, run.N,
		qaAcceptedThrough(surface)))
	return fmt.Sprintf("; QA lets %s pass at %s, and the PR body names %s as accepted by you", strings.Join(lets, " and "), short,
		map[bool]string{true: "it", false: "them"}[n == 1]), undo, nil
}

// qaAcceptable is what --accept-qa accepts of task t (its task directory
// dir): the newest run and its fix loop's stop at the task's head, which
// names the defects the writer declined, or else the newest run at the
// head that did not walk again the pages of defects the writer fixed or
// declined, or did not find a decline again where it was made
// (qaNotWalkedAcceptable); an error that says why it accepts nothing (no
// such stop at the head, one that names no decline, or one whose declines
// you accepted at the head already: qa-accepted.json holds them, and the
// task waits on you for another cause). The screen's A is offered exactly
// when it holds.
func qaAcceptable(dir string, t registry.Task) (qaRun, qaAccept, error) {
	run, ok := readQARun(dir)
	stop, stopped := readQAFixStop(dir, run)
	if ok && !stopped && run.Head == t.HeadSHA {
		if a, holds, err := qaNotWalkedAcceptable(dir, t, run); holds {
			return run, a, err
		}
	}
	switch {
	case !ok || !stopped || run.Head != t.HeadSHA:
		return qaRun{}, qaAccept{}, fmt.Errorf("%s does not wait on QA's fix loop at its head: --accept-qa accepts only the QA defects "+
			"the writer declined with its risk, or the writer's fixes a QA run did not walk again, and a plain `magnum factory retry %d` "+
			"lets it go on", TaskKey(t), t.ID)
	case len(stop.Declined) == 0:
		return qaRun{}, qaAccept{}, fmt.Errorf("%s: the writer declined none of the defects QA run %d still finds, so --accept-qa has "+
			"none to accept; a plain `magnum factory retry %d` walks QA again", TaskKey(t), run.N, t.ID)
	}
	if acc, ok := readQAAccepted(dir); ok && acc.Head == run.Head && !slices.ContainsFunc(stop.Declined, func(d qaDeclined) bool {
		return !slices.Contains(acc.Fingerprints, d.Fingerprint)
	}) {
		return qaRun{}, qaAccept{}, fmt.Errorf("%s: you accepted the defects the writer declined at %s already, and QA run %d has not "+
			"walked since; a plain `magnum factory retry %d` lets it go on", TaskKey(t), textx.ShortSHA(run.Head), run.N, t.ID)
	}
	var a qaAccept
	for _, d := range stop.Declined {
		if !slices.Contains(a.declined, d.Fingerprint) {
			a.declined = append(a.declined, d.Fingerprint)
		}
	}
	return run, a, nil
}

// qaNotWalkedAcceptable is what --accept-qa accepts of task t at run, the
// newest at its head when no fix loop's stop holds there: the defects whose
// pages the run did not walk again (its result's blocked states with a
// fingerprint), each a decline of the writer's (qaHistory) or a fix of its,
// and an error once you accepted all of them at the head already; holds is
// false for a run with none of them.
func qaNotWalkedAcceptable(dir string, t registry.Task, run qaRun) (a qaAccept, holds bool, err error) {
	res, ok := readQAResult(dir, run)
	if !ok || res.Outcome != QAOutcomeUnavailable {
		return qaAccept{}, false, nil
	}
	hist := qaHistory(dir, run.N)
	for _, b := range res.Blocked {
		switch {
		case b.Fingerprint == "":
		case hist[b.Fingerprint].declined, hist[b.Fingerprint].declinedNoRisk:
			a.declined = append(a.declined, b.Fingerprint)
		default:
			a.unchecked = append(a.unchecked, b.Fingerprint)
		}
	}
	if len(a.declined)+len(a.unchecked) == 0 {
		return qaAccept{}, false, nil
	}
	if acc, ok := readQAAccepted(dir); ok && acc.Head == run.Head && qaHolds(acc.Fingerprints, a.declined) && qaHolds(acc.Unchecked, a.unchecked) {
		return qaAccept{}, true, fmt.Errorf("%s: you accepted what QA run %d did not walk again at %s already, and it has not walked "+
			"since; a plain `magnum factory retry %d` lets it go on", TaskKey(t), run.N, textx.ShortSHA(run.Head), t.ID)
	}
	return a, true, nil
}

// qaHolds reports whether every one of fps is in have.
func qaHolds(have, fps []string) bool {
	return !slices.ContainsFunc(fps, func(fp string) bool { return !slices.Contains(have, fp) })
}

// qaAcceptedThrough is where an accept came from, as its event says it:
// "through the liaison" (it ran the retry for you, on your word in its
// chat), "on the screen", "at the CLI".
func qaAcceptedThrough(surface string) string {
	switch surface {
	case runtime.SurfaceLiaison:
		return "through the liaison"
	case "", "cli":
		return "at the CLI"
	}
	return "on the " + surface
}

// qaFixes is "1 fix" or "N fixes".
func qaFixes(n int) string {
	if n == 1 {
		return "1 fix"
	}
	return fmt.Sprintf("%d fixes", n)
}

// qaAcceptWhat is what --accept-qa accepts at run's head, as the task
// card's A names it: "1 QA defect the writer declined at abc1234 (flow on
// /settings/security)", the classes on their pages from the run's result,
// or as an earlier run found them for a page the run did not walk again;
// "1 fix QA did not walk again at abc1234 (escaped_markup on
// /settings/security in the state signed_in)"; both, joined by "and".
func qaAcceptWhat(dir string, run qaRun, a qaAccept) string {
	res, _ := readQAResult(dir, run)
	handed := map[string]qaHandedDefect{}
	for _, h := range qaHandedBefore(dir, run.N) {
		handed[h.fingerprint] = h
	}
	on := func(fps []string) string {
		var ds []QADefect
		var out []string
		for _, fp := range fps {
			if i := slices.IndexFunc(res.Blocking, func(d QADefect) bool { return d.Fingerprint == fp }); i >= 0 {
				ds = append(ds, res.Blocking[i])
			} else if h, ok := handed[fp]; ok {
				out = append(out, qaRecheckOn(h))
			}
		}
		if len(ds) > 0 {
			out = append([]string{qaClassesAndPaths(ds, slices.Collect(maps.Values(qaVerdictPages(dir, run))))}, out...)
		}
		if len(out) == 0 {
			return ""
		}
		return " (" + strings.Join(out[:min(len(out), qaListMost)], ", ") + ")"
	}
	short := textx.ShortSHA(run.Head)
	var parts []string
	if n := len(a.declined); n > 0 {
		parts = append(parts, fmt.Sprintf("%s the writer declined at %s%s", plural(n, "QA defect"), short, on(a.declined)))
	}
	if n := len(a.unchecked); n > 0 {
		parts = append(parts, fmt.Sprintf("%s QA did not walk again at %s%s", qaFixes(n), short, on(a.unchecked)))
	}
	return strings.Join(parts, " and ")
}

// qaApplyAccepted moves the defects of res that block and that you accepted
// at run's head (qa-accepted.json in the task directory dir) to Accepted:
// exactly those fingerprints, at that head alone (a new head blocks them
// again). A run left with none that blocks is clean.
func qaApplyAccepted(dir string, run qaRun, res *QAResult) {
	acc, ok := readQAAccepted(dir)
	if !ok || acc.Head != run.Head || res.Outcome != QAOutcomeDefects {
		return
	}
	var blocking []QADefect
	for _, d := range res.Blocking {
		if slices.Contains(acc.Fingerprints, d.Fingerprint) {
			res.Accepted = append(res.Accepted, d)
			continue
		}
		blocking = append(blocking, d)
	}
	res.Blocking = blocking
	if len(blocking) == 0 {
		res.Outcome = QAOutcomeClean
	}
}

// readQAAccepted reads qa-accepted.json in the task directory dir; false for
// none.
func readQAAccepted(dir string) (qaAccepted, bool) {
	var acc qaAccepted
	b, err := os.ReadFile(filepath.Join(dir, qaAcceptedFile))
	if err != nil || json.Unmarshal(b, &acc) != nil || acc.Head == "" {
		return qaAccepted{}, false
	}
	return acc, true
}

// readQAFixStop reads run's fix-stop.json in the task directory dir; false
// for none.
func readQAFixStop(dir string, run qaRun) (qaFixStop, bool) {
	var s qaFixStop
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(run.Dir), qaFixStopFile))
	if err != nil || json.Unmarshal(b, &s) != nil || s.Why == "" {
		return qaFixStop{}, false
	}
	return s, true
}

// readQADefects reads run's defects file in the task directory dir; false
// for none (or one that does not read: it is written again).
func readQADefects(dir string, run qaRun) (ReviewFile, bool) {
	var rf ReviewFile
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(qaDefectsFile(run.N, run.Head))))
	if err != nil || json.Unmarshal(b, &rf) != nil || rf.Round != run.N {
		return ReviewFile{}, false
	}
	return rf, true
}

// readQAReply reads the writer's reply to run's defects in the task
// directory dir; an empty one for none (the end fact read it).
func readQAReply(dir string, run qaRun) ReviewReply {
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(qaReplyFile(run.N, run.Head)))); err == nil {
		_ = json.Unmarshal(b, &rep)
	}
	return rep
}

// qaHanded is what a run made of a defect it handed the writer: the run,
// and whether the writer's reply declined it with its risk (declined) or
// with none (declinedNoRisk: no fix of it was made either, and it counts as
// open).
type qaHanded struct {
	run            int
	declined       bool
	declinedNoRisk bool
}

// qaHistory is what the runs before run k in the task directory dir handed
// the writer, by fingerprint, the newest run's for each: declined when its
// reply declined it with its risk, declinedNoRisk when it declined it with
// none (open, and no fix either), else it was to be fixed (fixed, no
// reply).
func qaHistory(dir string, k int) map[string]qaHanded {
	out := map[string]qaHanded{}
	for _, ref := range qaDefectsFiles(dir) {
		if ref.k >= k {
			continue
		}
		run := qaRun{N: ref.k, Head: ref.head, Dir: qaRunDir(ref.k, ref.head)}
		rf, ok := readQADefects(dir, run)
		if !ok {
			continue
		}
		rep := readQAReply(dir, run)
		risks := rep.declinedWithRisk()
		declined := map[string]bool{}
		for _, r := range rep.Replies {
			declined[r.ID] = declined[r.ID] || r.Action == replyDeclined
		}
		for _, i := range rf.Findings {
			out[i.Fingerprint] = qaHanded{run: ref.k, declined: risks[i.ID] != "", declinedNoRisk: declined[i.ID] && risks[i.ID] == ""}
		}
	}
	return out
}

// qaSeenDeclines are the declines the stops of the runs before run k in the
// task directory dir put to you: a stop holds the loop until you were told
// of it (qaToldOfFixStop), so a run after it means you saw them, and a plain
// retry did not accept them.
func qaSeenDeclines(dir string, k int) map[qaDeclined]bool {
	out := map[qaDeclined]bool{}
	paths, _ := filepath.Glob(filepath.Join(dir, "qa", "*", qaFixStopFile))
	for _, p := range paths {
		m := qaRunDirRe.FindStringSubmatch(filepath.Base(filepath.Dir(p)))
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err != nil || n >= k {
			continue
		}
		var s qaFixStop
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &s) == nil {
			for _, d := range s.Declined {
				out[d] = true
			}
		}
	}
	return out
}

// qaReplies are the writer's replies (absolute paths, once each) that
// declined defects ds, by what the earlier runs made of them (hist).
func qaReplies(dir string, ds []QADefect, hist map[string]qaHanded) []string {
	runs := map[int]string{}
	for _, ref := range qaDefectsFiles(dir) {
		runs[ref.k] = ref.head
	}
	var out []string
	for _, d := range ds {
		h := hist[d.Fingerprint]
		if p := filepath.Join(dir, filepath.FromSlash(qaReplyFile(h.run, runs[h.run]))); !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// qaVerdictPages are the pages of run's verdict.json in the task directory
// dir, by id: the walker's paths, states and shots, which the end fact and
// evaluateQA checked; none for a verdict that no longer reads.
func qaVerdictPages(dir string, run qaRun) map[string]QAPage {
	out := map[string]QAPage{}
	var v QAVerdict
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(run.Dir), qaVerdictFile))
	if err != nil || json.Unmarshal(b, &v) != nil {
		return out
	}
	for _, pg := range v.Pages {
		out[pg.ID] = pg
	}
	return out
}
