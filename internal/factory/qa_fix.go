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
// never becomes a known note (roadmap R56): one that comes back after its
// fix still blocks, named so, never a doubt; one the writer declines with
// its risk stops the task for you; past the cap the task needs you, never
// the PR body. Your retry after either walks QA again at the head.

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
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
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

// qaFixStop is fix-stop.json.
type qaFixStop struct {
	Why string    `json:"why"`
	At  time.Time `json:"at"`
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

// qaFixPhase goes on with the fix loop of run's defects that block (its
// result res) for task t at qa stage st, and reports whether the loop holds
// the stage (false: a fresh run is QA's next step). A stop for you holds it
// until you were told; then your retry walks again. At the head QA looked
// at, the first pass writes the defects file (qaStartFix; past the cap, the
// stop instead), then the writer's fix turn runs until it passed, and its
// close follows (closeQAFix). A run at another head with neither file is
// none of the loop's: QA runs at the task's head.
func (e *Engine) qaFixPhase(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if stop, ok := readQAFixStop(dir, run); ok {
		if run.Head != t.HeadSHA {
			return false, nil
		}
		told, err := e.qaToldOfFixStop(ctx, t, stop)
		return err != nil || !told, err
	}
	rf, ok := readQADefects(dir, run)
	if !ok {
		if run.Head != t.HeadSHA {
			return false, nil
		}
		var err error
		if rf, ok, err = e.qaStartFix(ctx, t, st, run, res); err != nil || !ok {
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

// qaStartFix writes run's defects file for the writer from its defects
// that block (res) and reports whether it did: none once task t had its
// fixes cap of fix turns, where the loop stops for you instead
// (qaFixStopForYou), naming the defects and those that came back after
// their fix. Each defect is a ReviewItem: Q<i>, its fingerprint and
// severity, its class before its title, the first code path it names, and
// a body with its page's path, state and shot (an absolute path) and its
// quote, and "came back after its fix" when an earlier run's defects file
// has its fingerprint.
func (e *Engine) qaStartFix(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (ReviewFile, bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	pages := qaVerdictPages(dir, run)
	earlier := qaEarlierDefects(dir, run.N)
	var back []QADefect
	for _, d := range res.Blocking {
		if earlier[d.Fingerprint] > 0 {
			back = append(back, d)
		}
	}
	fixes := config.DefaultQAFixes
	if repo, ok := e.cfg.FactoryRepo(t.Repo); ok && repo.QA != nil {
		fixes = cmp.Or(repo.QA.Fixes, fixes)
	}
	if turns := e.qaFixTurns(t); turns >= fixes {
		n := len(res.Blocking)
		why := fmt.Sprintf("QA still finds %s that block%s after %s (%s)", plural(n, "defect"), blocksS(n), plural(turns, "fix turn"), res.Why)
		if len(back) > 0 {
			why += "; came back after its fix: " + qaClassesAndPaths(back, slices.Collect(maps.Values(pages)))
		}
		why += "; see " + filepath.Join(dir, filepath.FromSlash(run.Dir), qaResultFile) + retryHint(t)
		return ReviewFile{}, false, e.qaFixStopForYou(ctx, t, run, why, "QA's defects outlasted the writer's fix turns")
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return ReviewFile{}, false, err
	}
	runPath := filepath.Join(dir, filepath.FromSlash(run.Dir))
	rf := ReviewFile{Round: run.N, Head: run.Head, AttemptsBefore: n}
	for i, d := range res.Blocking {
		rf.Findings = append(rf.Findings, qaFixItem(i, d, pages[d.Page], runPath, earlier[d.Fingerprint]))
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
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, what)
	return rf, true, nil
}

// qaFixItem is QA's defect d, the i-th that blocks, as the writer gets it
// (qaStartFix), on page pg in the run's folder runPath; cameBack is the
// earlier run whose defects file had it (0 for none).
func qaFixItem(i int, d QADefect, pg QAPage, runPath string, cameBack int) ReviewItem {
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
	if cameBack > 0 {
		body += fmt.Sprintf("; came back after its fix (run %d)", cameBack)
	}
	it.Body = body
	return it
}

// closeQAFix closes the fix turn of run's defects rf once it passed, and
// reports whether the loop still holds the stage. The task's head is the
// one the factory's checks passed at (passStage put it there). At run's
// head the writer committed nothing, so it declined every defect with its
// risk (commitProblem passes no other turn without a commit): the task
// needs you, as a decline never makes a defect QA finds a known note. At a
// new head the local review and QA run again (backToReview), unless the
// review passed at that head since (the move landed: QA's fresh run there
// is next).
func (e *Engine) closeQAFix(ctx context.Context, t registry.Task, run qaRun, res QAResult, rf ReviewFile) (bool, error) {
	cur, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil {
		return true, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if cur.HeadSHA == "" || cur.HeadSHA == run.Head {
		n := len(res.Blocking)
		why := fmt.Sprintf("the writer declined QA's %s that block%s, each with its risk (%s), and QA passes no visible defect as a "+
			"known note: see %s%s, and QA walks again", plural(n, "defect"), blocksS(n), res.Why,
			filepath.Join(dir, filepath.FromSlash(qaReplyFile(run.N, run.Head))), retryHint(cur))
		return true, e.qaFixStopForYou(ctx, cur, run, why, "the writer declined QA's defects")
	}
	reviewed, err := e.qaFixReviewed(ctx, cur)
	if err != nil || reviewed {
		return err != nil, err
	}
	var rep ReviewReply
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(qaReplyFile(run.N, run.Head)))); err == nil {
		_ = json.Unmarshal(b, &rep) // the end fact read it
	}
	declined := len(rep.declinedWithRisk())
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

// qaFixStopForYou stops run's fix loop for you, because why: fix-stop.json
// first, a qa event, then task t moves to you (a daemon that stops before
// the move makes it on the next pass, qaToldOfFixStop).
func (e *Engine) qaFixStopForYou(ctx context.Context, t registry.Task, run qaRun, why, because string) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := writeJSON(filepath.Join(dir, filepath.FromSlash(run.Dir), qaFixStopFile), qaFixStop{Why: why, At: e.now()}); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseFail, fmt.Sprintf("run %d at %s: its fix loop stopped for you", run.N, textx.ShortSHA(run.Head)))
	if e.beforeStop != nil {
		if err := e.beforeStop(); err != nil {
			return err
		}
	}
	e.toOperator(ctx, t, why, because)
	return nil
}

// qaToldOfFixStop reports whether you were told of the fix loop's stop (an
// alert since it): a live task told so is one you let go on, and QA walks
// again. One not told is moved to you now, with the stop's reason.
func (e *Engine) qaToldOfFixStop(ctx context.Context, t registry.Task, stop qaFixStop) (bool, error) {
	evs, err := e.reg.Events(ctx, t.ID, qaEventsRead)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(evs, func(ev registry.Event) bool { return ev.Kind == "alert" && !ev.At.Before(stop.At) }) {
		return true, nil
	}
	e.toOperator(ctx, t, stop.Why, "QA's fix loop stopped for you")
	return false, nil
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

// qaEarlierDefects are the fingerprints of the defects files of the runs
// before run k in the task directory dir, each with the newest such run.
func qaEarlierDefects(dir string, k int) map[string]int {
	out := map[string]int{}
	for _, ref := range qaDefectsFiles(dir) {
		if ref.k >= k {
			continue
		}
		if rf, ok := readQADefects(dir, qaRun{N: ref.k, Head: ref.head, Dir: qaRunDir(ref.k, ref.head)}); ok {
			for _, i := range rf.Findings {
				out[i.Fingerprint] = ref.k
			}
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
