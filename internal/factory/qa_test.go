package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
)

// errRefused is a readiness probe nothing answers.
var errRefused = errors.New("dial tcp 127.0.0.1:3001: connect: connection refused")

// qaRig is the review rig with visual QA for example/shop: its qa block
// (bin/user-facing, the copy's server bin/dev at
// http://app.example.localhost:{port}, ready at /up, a 5-minute boot), a bug
// kind with a qa stage after its local review, a readiness probe that
// answers from probe (up: the copy's server started and did not stop since;
// n counts the probes since it came up, from 1), a probe timer that fires at
// once and a clock that moves a minute at each probe.
type qaRig struct {
	*reviewRig
	mu      sync.Mutex
	probe   func(up bool, n int) (int, error)
	probed  []string // "<url> <host>" of each probe
	sinceUp int
	stdin   [][]byte // what bin/user-facing read, each time
}

func newQARig(t *testing.T, answers ...func(evalrun.LocalCase) evalrun.LocalResult) *qaRig {
	t.Helper()
	if len(answers) == 0 {
		answers = append(answers, result())
	}
	r := &qaRig{reviewRig: newReviewRig(t, answers...)}
	repo := &r.cfg.Factory.Repos[0]
	repo.Server = "bin/dev"
	repo.QA = &config.FactoryQA{When: "bin/user-facing", URL: "http://app.example.localhost:{port}", Ready: "/up",
		Boot: config.Duration{Duration: 5 * time.Minute}, Cap: config.Duration{Duration: 50 * time.Minute}, MaxPages: 12, Fixes: 2}
	kind := r.cfg.Factory.Kinds["bug"]
	i := stageIndex(kind, "local_review")
	kind.Stages = slices.Insert(slices.Clone(kind.Stages), i+1,
		config.FactoryStage{Name: "qa", Type: config.FactoryStageQA, Roles: []string{"fx-qa"}, End: config.FactoryEndQA})
	r.cfg.Factory.Kinds["bug"] = kind
	r.userFacing = func(stdin []byte) (string, int) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stdin = append(r.stdin, stdin)
		return "app/credit.rb\n", 0
	}
	r.probe = func(up bool, _ int) (int, error) {
		if !up {
			return 0, errRefused
		}
		return 200, nil
	}
	r.seams()
	return r
}

// seams puts the rig's probe and timer on the engine.
func (r *qaRig) seams() {
	r.e.qaProbe = r.probeQA
	r.e.after = func(time.Duration) <-chan time.Time {
		c := make(chan time.Time)
		close(c)
		return c
	}
}

func (r *qaRig) probeQA(_ context.Context, url, host string) (int, error) {
	up := r.copies.serverUp("factory1")
	r.mu.Lock()
	r.probed = append(r.probed, url+" "+host)
	if up {
		r.sinceUp++
	} else {
		r.sinceUp = 0
	}
	n, fn := r.sinceUp, r.probe
	r.mu.Unlock()
	r.clock.Add(time.Minute)
	return fn(up, n)
}

func (r *qaRig) setProbe(fn func(up bool, n int) (int, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probe = fn
}

// qaTask is a bug task at its qa stage: its writer's fix committed and
// proven, the head its local review passed.
func (r *qaRig) qaTask(key string) (registry.Task, string) {
	r.t.Helper()
	task, head := r.reviewTask(key)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "qa")
	}); err != nil {
		r.t.Fatal(err)
	}
	return r.reload(task.ID), head
}

// toWalk runs the ticks until the task's QA session is ready for the walk.
func (r *qaRig) toWalk(task registry.Task) {
	r.t.Helper()
	r.run(8, func() bool { return r.reload(task.ID).WaitsWhat == qaReadyWhat })
	if got := r.reload(task.ID); got.WaitsWho != waitQA || got.WaitsWhat != qaReadyWhat {
		r.t.Fatalf("task %s at %s waits %q: %q, want %s: %s", got.State, got.Stage, got.WaitsWho, got.WaitsWhat, waitQA, qaReadyWhat)
	}
}

// toYou runs the ticks until the task needs you.
func (r *qaRig) toYou(task registry.Task) registry.Task {
	r.t.Helper()
	r.run(10, func() bool { return r.reload(task.ID).State == registry.TaskNeedsYou })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou {
		r.t.Fatalf("task %s at %s waits %q: %q, want NEEDS YOU", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	return got
}

// restartDaemon is a daemon's stop in the middle of the run (its
// RunLocked ends its QA sessions) and the next daemon's startup.
func (r *qaRig) restartDaemon() {
	r.t.Helper()
	r.e.stopQASessions(r.ctx)
	r.restart()
	r.seams()
	if err := r.e.startup(r.ctx); err != nil {
		r.t.Fatal(err)
	}
}

// runDir is the folder of run k at head in the task's directory.
func (r *qaRig) runDir(id int64, k int, head string) string {
	return filepath.Join(r.layout.FactoryTaskDir(id), qaRunDir(k, head))
}

// result is run k's result.json at head.
func (r *qaRig) qaResult(id int64, k int, head string) QAResult {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.runDir(id, k, head), qaResultFile))
	if err != nil {
		r.t.Fatal(err)
	}
	var res QAResult
	if err := json.Unmarshal(b, &res); err != nil {
		r.t.Fatal(err)
	}
	return res
}

// qaChecks are the task's qa checks, "<result>:<cause>@<head>".
func (r *qaRig) qaChecks(id int64) []string {
	r.t.Helper()
	cs, err := r.reg.Checks(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		if c.Kind == registry.CheckQA {
			out = append(out, c.Result+":"+c.Cause+"@"+c.HeadSHA)
		}
	}
	return out
}

// qaFree checks that nothing of a QA run is left held: the machine's QA
// lock is free, and the task holds no heavy slot.
func (r *qaRig) qaFree(id int64) {
	r.t.Helper()
	unlock, held, err := engine.AcquireLock(r.layout.FactoryQALock())
	if err != nil {
		r.t.Fatal(err)
	}
	if held {
		r.t.Fatal("the machine's QA lock is still held")
	}
	unlock()
	r.e.heavyMu.Lock()
	_, heavy := r.e.heavyHeld[id]
	r.e.heavyMu.Unlock()
	if heavy {
		r.t.Fatal("the task still holds a heavy slot")
	}
	if r.e.qaSessionOf(id) != nil {
		r.t.Fatal("the task still has a QA session")
	}
}

// A repository without [factory.repo.qa] has no QA: after a clean local
// review the task moves past the kind's qa stage, and says why.
func TestARepoWithoutAQABlockSkipsTheQAStage(t *testing.T) {
	r := newQARig(t, result())
	r.cfg.Factory.Repos[0].QA = nil
	task, _ := r.reviewTask("PR-12401")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task at %s, want pr_body past the qa stage", got.Stage)
	}
	if !slices.ContainsFunc(r.events(task.ID, "stage"), func(ev registry.Event) bool {
		return ev.Text == "qa skipped: example/shop has no [factory.repo.qa]"
	}) {
		t.Fatalf("stage events %+v, want the skip and why", r.events(task.ID, "stage"))
	}
	if l := r.copies.log(); len(l.started) != 0 {
		t.Fatalf("servers started %v, want none", l.started)
	}
}

// A change whose user-facing check prints no file needs no QA: the stage
// passes at the head with a not_needed summary and a skipped check, and no
// server starts. The check reads the changed files on its stdin.
func TestAChangeWithNoUserFacingFileNeedsNoQA(t *testing.T) {
	r := newQARig(t)
	var seen []byte
	r.userFacing = func(stdin []byte) (string, int) {
		seen = stdin
		return "", 0
	}
	task, head := r.qaTask("PR-12402")
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || got.WaitsWho != "" {
		t.Fatalf("task at %s waits %q: %q, want pr_body", got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID))
	if !ok || sum.Outcome != QAOutcomeNotNeeded || sum.Head != head {
		t.Fatalf("qa-summary.json %+v (%v), want not_needed at %s", sum, ok, head)
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"skipped:@" + head}) {
		t.Fatalf("qa checks %q, want one skipped at the head", got)
	}
	if l := r.copies.log(); len(l.started) != 0 {
		t.Fatalf("servers started %v, want none", l.started)
	}
	if !strings.Contains(string(seen), "app/credit.rb\n") {
		t.Fatalf("the user-facing check read %q, want the changed files one per line", seen)
	}
}

// A user-facing check that fails cannot tell: the task needs you with the
// reason and the retry command, never a pass as nothing user-facing.
func TestAFailingWhenCommandNeedsYouNotNotNeeded(t *testing.T) {
	r := newQARig(t)
	r.userFacing = func([]byte) (string, int) { return "bin/user-facing: cannot read config/routes.rb\nmore\n", 2 }
	task, _ := r.qaTask("PR-12403")
	got := r.toYou(task)
	for _, want := range []string{"QA could not run", "`bin/user-facing` exited 2: bin/user-facing: cannot read config/routes.rb",
		fmt.Sprintf("magnum factory retry %d", task.ID)} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	if strings.Contains(got.WaitsWhat, "more") {
		t.Errorf("the task waits %q, want the first line of stderr only", got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.started) != 0 {
		t.Fatalf("servers started %v, want none", l.started)
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: the stage passed")
	}
	if got := r.qaChecks(task.ID); len(got) != 1 || !strings.HasPrefix(got[0], "fail:env@") {
		t.Fatalf("qa checks %q, want one failed for the environment", got)
	}
}

// QA starts the copy's server, waits until it answers its ready path with a
// 2xx or 3xx (a 503 is not ready), then starts the factory's Chrome in the
// run's folder and records its endpoint, with the run's nonce in the probe
// and the user-facing files in changed.txt.
func TestQAStartsTheServerThenWaitsForItToAnswer(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(up bool, n int) (int, error) {
		switch {
		case !up:
			return 0, errRefused
		case n == 1:
			return 503, nil
		}
		return 200, nil
	})
	task, head := r.qaTask("PR-12404")
	r.toWalk(task)
	dir := r.layout.FactoryTaskDir(task.ID)
	l := r.copies.log()
	if len(l.started) != 1 || len(l.browsers) != 1 || len(l.stopped) != 0 {
		t.Fatalf("servers started %v, stopped %v, browsers %v; want one server and one browser", l.started, l.stopped, l.browsers)
	}
	if want := filepath.Join(dir, "qa", "1-"+head[:7]); l.browserDirs[0] != want {
		t.Fatalf("the browser's folder is %s, want %s", l.browserDirs[0], want)
	}
	run, ok := readQARun(dir)
	if !ok || run.N != 1 || run.Dir != "qa/1-"+head[:7] || run.Head != head || run.CDP != "http://127.0.0.1:9333" || len(run.Nonce) != 32 ||
		run.Port != 3001 || run.URL != "http://app.example.localhost:3001" || run.Ready != "/up" || run.Started.IsZero() {
		t.Fatalf("qa-run.json %+v (%v)", run, ok)
	}
	if probe := r.readTask(task.ID, filepath.Join(run.Dir, qaProbeFile)); !strings.Contains(probe, run.Nonce) {
		t.Fatalf("probe.js lacks the run's nonce %s", run.Nonce)
	}
	if got := r.readTask(task.ID, filepath.Join(run.Dir, qaChangedFile)); got != "app/credit.rb\n" {
		t.Fatalf("changed.txt is %q, want the user-facing files", got)
	}
	r.mu.Lock()
	probed := slices.Clone(r.probed)
	r.mu.Unlock()
	want := "http://app.example.localhost:3001/up app.example.localhost:3001"
	if len(probed) != 3 || slices.ContainsFunc(probed, func(p string) bool { return p != want }) {
		t.Fatalf("probes %q, want the port's check, the 503 and the 200 of %s", probed, want)
	}
}

// The copy's port answering before its server started is another server
// (yours, another app's): QA would walk the wrong app, so it starts nothing
// and needs you.
func TestQARefusesAPortThatAnswersBeforeTheServerStarts(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(bool, int) (int, error) { return 200, nil })
	task, _ := r.qaTask("PR-12405")
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "port 3001 already answers") {
		t.Fatalf("the task waits %q, want the port named", got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.started) != 0 {
		t.Fatalf("servers started %v, want none", l.started)
	}
	r.qaFree(task.ID)
}

// A server that never answers gets one more boot, each stopped with its
// proof; then QA is unavailable and the task needs you with the retry
// command, the lock and the heavy slot free.
func TestAServerThatNeverAnswersNeedsYouWithTheRetryCommand(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(bool, int) (int, error) { return 0, errRefused })
	task, head := r.qaTask("PR-12406")
	got := r.toYou(task)
	l := r.copies.log()
	if len(l.started) != 2 || len(l.stopped) != 2 || len(l.browsers) != 0 {
		t.Fatalf("servers started %v, stopped %v, browsers %v; want two boots, each stopped, and no browser", l.started, l.stopped, l.browsers)
	}
	if slices.ContainsFunc(l.stopCtxErr, func(err error) bool { return err != nil }) {
		t.Fatalf("stops ran on ended contexts: %v", l.stopCtxErr)
	}
	res := r.qaResult(task.ID, 1, head)
	if res.Outcome != QAOutcomeUnavailable || !strings.Contains(res.Why, "did not answer /up within 5m0s") {
		t.Fatalf("result.json %+v, want unavailable for the boot", res)
	}
	if !strings.Contains(got.WaitsWhat, "QA could not run") || !strings.Contains(got.WaitsWhat, "magnum factory retry") {
		t.Fatalf("the task waits %q, want the reason and the retry command", got.WaitsWhat)
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"fail:env@" + head}) {
		t.Fatalf("qa checks %q, want one failed for the environment", got)
	}
	r.qaFree(task.ID)
}

// A server that fails to start gets one more start; the second failure is
// QA unavailable, naming the start's error.
func TestAServerThatFailsToStartGetsOneRetry(t *testing.T) {
	r := newQARig(t)
	r.copies.startErrs = []error{
		errors.New("the server of factory1 (pid 7) ended at once; its log is /logs/factory-server-factory1.log"),
		errors.New("the server of factory1 (pid 9) ended at once; its log is /logs/factory-server-factory1.log"),
	}
	task, head := r.qaTask("PR-12407")
	got := r.toYou(task)
	l := r.copies.log()
	if len(l.started) != 2 || len(l.browsers) != 0 {
		t.Fatalf("servers started %v, browsers %v; want two starts and no browser", l.started, l.browsers)
	}
	res := r.qaResult(task.ID, 1, head)
	if res.Outcome != QAOutcomeUnavailable || !strings.Contains(res.Why, "did not start") || !strings.Contains(res.Why, "(pid 9) ended at once") {
		t.Fatalf("result.json %+v, want unavailable naming the start's error", res)
	}
	if !strings.Contains(got.WaitsWhat, "magnum factory retry") {
		t.Fatalf("the task waits %q, want the retry command", got.WaitsWhat)
	}
	r.qaFree(task.ID)
}

// One QA runs on the Mac at a time: while another holds the machine's QA
// lock, the task waits, holding no heavy slot, and starts once it is free.
func TestOneQAAtATimeOnTheMac(t *testing.T) {
	r := newQARig(t)
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryQALock()), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(r.layout.FactoryQALock())
	if err != nil || held {
		t.Fatalf("take the lock: %v, held %v", err, held)
	}
	task, _ := r.qaTask("PR-12408")
	r.run(4, func() bool { return r.reload(task.ID).WaitsWhat == "another QA runs on this Mac" })
	if got := r.reload(task.ID); got.WaitsWho != waitQA || got.WaitsWhat != "another QA runs on this Mac" {
		t.Fatalf("the task waits %q: %q, want the other QA", got.WaitsWho, got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.started) != 0 {
		t.Fatalf("servers started %v, want none while another QA runs", l.started)
	}
	r.e.heavyMu.Lock()
	_, heavy := r.e.heavyHeld[task.ID]
	r.e.heavyMu.Unlock()
	if heavy {
		t.Fatal("a task that waits for the lock holds a heavy slot")
	}
	unlock()
	r.toWalk(task)
	if l := r.copies.log(); len(l.started) != 1 {
		t.Fatalf("servers started %v, want one once the lock is free", l.started)
	}
}

// A panic in the boot probe ends the session: its server stops, the lock
// and the heavy slot go, and the task needs you with the panic named.
func TestQATeardownRunsWhenTheBootProbePanics(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(up bool, _ int) (int, error) {
		if !up {
			return 0, errRefused
		}
		panic("the probe broke")
	})
	task, head := r.qaTask("PR-12409")
	got := r.toYou(task)
	if l := r.copies.log(); len(l.started) != 1 || len(l.stopped) != 1 {
		t.Fatalf("servers started %v, stopped %v; want the one stopped", l.started, l.stopped)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeUnavailable || !strings.Contains(res.Why, "panic") {
		t.Fatalf("result.json %+v, want unavailable naming the panic", res)
	}
	if !strings.Contains(got.WaitsWhat, "panic") {
		t.Fatalf("the task waits %q, want the panic named", got.WaitsWhat)
	}
	r.qaFree(task.ID)
}

// The daemon's stop ends every QA session, on a context of its own: its
// browser and its server stop even when the daemon's context has ended.
func TestQATeardownRunsWhenTheDaemonStops(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12410")
	r.toWalk(task)
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	r.e.stopQASessions(ctx)
	l := r.copies.log()
	if len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	if len(l.stopCtxErr) != 2 || slices.ContainsFunc(l.stopCtxErr, func(err error) bool { return err != nil }) {
		t.Fatalf("the stops' contexts ended: %v", l.stopCtxErr)
	}
	r.qaFree(task.ID)
}

// A task sent to you during its QA has its browser and its server stopped.
func TestQATeardownRunsWhenTheTaskGoesToYou(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12411")
	r.toWalk(task)
	r.e.toOperator(r.ctx, r.reload(task.ID), "a test sends it to you", "test")
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	r.qaFree(task.ID)
}

// A task that leaves its qa stage during the run (a send back) has its
// session ended by the next tick's sweep.
func TestQATeardownRunsWhenTheTaskLeavesItsStage(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12412")
	r.toWalk(task)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "coding")
	}); err != nil {
		t.Fatal(err)
	}
	r.e.sweepQA(r.ctx, []registry.Task{r.reload(task.ID)})
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	r.qaFree(task.ID)
}

// A restart in the middle of the run cuts it: the next daemon records it
// interrupted, fails the stage's attempt the run began, and starts a fresh
// run at the same head.
func TestARestartDuringQARunsAFreshRun(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12413")
	r.toWalk(task)
	walker, err := r.reg.BeginAttempt(r.ctx, task.ID, "qa")
	if err != nil {
		t.Fatal(err)
	}
	r.restartDaemon()
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted ||
		res.Why != "the daemon stopped during the run: its server and browser were stopped" {
		t.Fatalf("run 1's result.json %+v, want interrupted", res)
	}
	if as := r.attempts(task.ID); !slices.ContainsFunc(as, func(a registry.Attempt) bool {
		return a.ID == walker.ID && a.State == registry.AttemptFailed
	}) {
		t.Fatalf("attempts %+v, want the run's attempt failed", as)
	}
	r.toWalk(task)
	l := r.copies.log()
	if len(l.started) != 2 || len(l.browsers) != 2 || l.browserDirs[1] != r.runDir(task.ID, 2, head) {
		t.Fatalf("servers started %v, browsers %v in %v; want run 2 started", l.started, l.browsers, l.browserDirs)
	}
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 2 || run.Dir != "qa/2-"+head[:7] {
		t.Fatalf("qa-run.json %+v, want run 2", run)
	}
}

// A daemon that stops while the task only waits for the machine's QA lock
// cut nothing: no server started, so the next daemon goes on with the run.
func TestARestartBeforeTheServerStartsKeepsTheRun(t *testing.T) {
	r := newQARig(t)
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryQALock()), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, _, err := engine.AcquireLock(r.layout.FactoryQALock())
	if err != nil {
		t.Fatal(err)
	}
	task, head := r.qaTask("PR-12414")
	r.run(4, func() bool { return r.reload(task.ID).WaitsWhat == "another QA runs on this Mac" })
	r.restartDaemon()
	if _, err := os.Stat(filepath.Join(r.runDir(task.ID, 1, head), qaResultFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run 1 has a result after a restart that cut nothing: %v", err)
	}
	unlock()
	r.toWalk(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 1 {
		t.Fatalf("qa-run.json %+v, want run 1 going on", run)
	}
}

// Your retry after QA was unavailable starts a fresh run. A daemon that
// stopped between the result and the move to you (no alert since the
// result) moves the task to you on its next pass instead, never a fresh run.
func TestARetryAfterUnavailableStartsAFreshRun(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(bool, int) (int, error) { return 0, errRefused })
	task, head := r.qaTask("PR-12415")
	r.toYou(task)
	r.setProbe(func(up bool, _ int) (int, error) {
		if !up {
			return 0, errRefused
		}
		return 200, nil
	})
	if _, err := r.reg.EnqueueRequest(r.ctx, ReqRetry, RetryPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	r.toWalk(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 2 || run.Dir != "qa/2-"+head[:7] {
		t.Fatalf("qa-run.json %+v, want run 2 after the retry", run)
	}

	c := newQARig(t)
	c.setProbe(func(bool, int) (int, error) { return 0, errRefused })
	c.e.beforeStop = func() error { return errors.New("the daemon stops before the move") }
	other, otherHead := c.qaTask("PR-12416")
	c.run(10, func() bool { return fsx.Exists(filepath.Join(c.runDir(other.ID, 1, otherHead), qaResultFile)) })
	if got := c.reload(other.ID); got.State == registry.TaskNeedsYou {
		t.Fatal("the task moved to you although the daemon stopped before the move")
	}
	c.e.beforeStop = nil
	got := c.toYou(other)
	if !strings.Contains(got.WaitsWhat, "QA could not run") || !strings.Contains(got.WaitsWhat, "did not answer /up") {
		t.Fatalf("the task waits %q, want the run's reason", got.WaitsWhat)
	}
	if l := c.copies.log(); len(l.started) != 2 {
		t.Fatalf("servers started %v, want no fresh run before you were told", l.started)
	}
}

// The QA walker's copy gives its agent no way to open a tab in your browser
// either: BROWSER=true and the factory's `open` first on its PATH.
func TestTheQAWalkersCopyOpensNoBrowserTab(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12417")
	r.toWalk(task)
	sess := r.e.qaSessionOf(task.ID)
	if sess == nil {
		t.Fatal("no QA session")
	}
	if env := sess.copy.Env; env["BROWSER"] != "true" || !strings.HasPrefix(env["PATH"], r.layout.FactoryShims()+":") || env["PORT"] != "3001" {
		t.Fatalf("the walker's env %v, want BROWSER=true, the shims first on its PATH and the copy's own", env)
	}
	fi, err := os.Stat(filepath.Join(r.layout.FactoryShims(), "open"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the open shim: %v, %v", fi, err)
	}
}

// The readiness probe asks this Mac whatever host the url names (a name
// under .localhost need not resolve), with that host as its Host, and takes
// a redirect as the answer, never following it off the copy's server.
func TestTheReadinessProbeAsksTheMacAndFollowsNoRedirect(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		hosts = append(hosts, req.Host)
		mu.Unlock()
		http.Redirect(w, req, "https://staging.example.com/sign_in", http.StatusFound)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host := "app.example.localhost:" + port
	status, err := httpQAProbe(t.Context(), "http://"+host+"/up", host)
	if err != nil || status != http.StatusFound {
		t.Fatalf("the probe answered %d, %v; want the redirect's 302", status, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(hosts, []string{host}) {
		t.Fatalf("the server saw hosts %q, want one ask with %s", hosts, host)
	}

	srv.Close()
	if _, err := httpQAProbe(t.Context(), "http://"+host+"/up", host); err == nil {
		t.Fatal("a port nothing listens on answered")
	}
}
