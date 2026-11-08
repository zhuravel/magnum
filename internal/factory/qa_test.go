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
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
)

// errRefused is a readiness probe nothing answers.
var errRefused = errors.New("dial tcp 127.0.0.1:3001: connect: connection refused")

// qaRig is the review rig with visual QA for example/shop: its qa block
// (bin/user-facing, the copy's server bin/dev at
// http://app.example.localhost:{port}, ready at /up, a 5-minute boot), the
// bug kind with its qa stage after its local review, a readiness probe that
// answers from probe (up: the copy's server started and did not stop since;
// n counts the probes since it came up, from 1), a probe timer that fires at
// once (no other timer fires) and a clock that moves a minute at each probe.
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
	if kind := r.cfg.Factory.Kinds["bug"]; stageIndex(kind, "qa") < 0 {
		i := stageIndex(kind, "local_review")
		kind.Stages = slices.Insert(slices.Clone(kind.Stages), i+1,
			config.FactoryStage{Name: "qa", Type: config.FactoryStageQA, Roles: []string{"fx-qa"}, End: config.FactoryEndQA})
		r.cfg.Factory.Kinds["bug"] = kind
	}
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

// seams puts the rig's probe and timer on the engine: the probe's
// interval passes at once, and no other bound (a precheck's cap) runs out:
// its timer is a nil channel, which never fires, on purpose. A new timer on
// the seam that a QA test needs to fire must be named here, or its test
// waits for go test's timeout.
func (r *qaRig) seams() {
	r.e.qaProbe = r.probeQA
	r.e.after = func(d time.Duration) <-chan time.Time {
		if d != qaProbeEvery {
			return nil
		}
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

// qaAcceptance is the judge's acceptance check of the rig's tasks: A1, on
// the security settings.
const qaAcceptance = `{"checks": [{"id": "A1", "point": "the passkeys list says when each passkey was added", ` +
	`"given": "a user with a passkey", "then": "the date shows as a date", "how": "QA: /settings/security"}]}`

// qaTask is a bug task at its qa stage: its writer's fix committed and
// proven, the head its local review passed, and its acceptance check.
func (r *qaRig) qaTask(key string) (registry.Task, string) {
	r.t.Helper()
	task, head := r.reviewTask(key)
	r.taskFile(task.ID, acceptanceFile, qaAcceptance)
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

// crashDaemon is a daemon that dies in the middle of the run: no teardown
// runs (the kernel frees its QA lock with the process), and the next
// daemon's startup reconciles what it left (Copies.Reconcile, cutQA).
func (r *qaRig) crashDaemon() {
	r.t.Helper()
	r.e.qaMu.Lock()
	sessions := r.e.qaSessions
	r.e.qaSessions = map[int64]*qaSession{}
	r.e.qaMu.Unlock()
	for _, s := range sessions {
		s.unlock()
	}
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

// A restart in the middle of the run cuts it: the daemon that stops records
// it interrupted by its drain and fails the stage's attempt the run began,
// and the next daemon starts a fresh run at the same head.
func TestARestartDuringQARunsAFreshRun(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12413")
	r.toWalk(task)
	walker, err := r.reg.BeginAttempt(r.ctx, task.ID, "qa")
	if err != nil {
		t.Fatal(err)
	}
	r.restartDaemon()
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutDrain ||
		res.Why != "the daemon stopped during the run (a drain or a signal): its server and browser were stopped" {
		t.Fatalf("run 1's result.json %+v, want interrupted by the drain", res)
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

// panickyCopies are the rig's copies with a stop of the browser that
// panics, and a start of the browser or of the server that panics once.
type panickyCopies struct {
	*fakeCopies
	mu                                       sync.Mutex
	stopBrowser, startBrowserOnce, startOnce bool
}

func (p *panickyCopies) StopBrowser(ctx context.Context, c Copy) error {
	if p.stopBrowser {
		panic("lsof: unexpected output")
	}
	return p.fakeCopies.StopBrowser(ctx, c)
}

func (p *panickyCopies) StartBrowser(ctx context.Context, c Copy, dir string) (string, error) {
	p.mu.Lock()
	once := p.startBrowserOnce
	p.startBrowserOnce = false
	p.mu.Unlock()
	if once {
		panic("ps: unexpected output")
	}
	return p.fakeCopies.StartBrowser(ctx, c, dir)
}

func (p *panickyCopies) StartServer(ctx context.Context, c Copy) error {
	p.mu.Lock()
	once := p.startOnce
	p.startOnce = false
	p.mu.Unlock()
	if once {
		panic("ps: unexpected output")
	}
	return p.fakeCopies.StartServer(ctx, c)
}

// Two installs during one head's QA are routine, no crash: each records its
// run interrupted by its drain, which no cap counts, and a fresh run starts
// each time.
func TestTwoDrainsDuringOneHeadsQADoNotNeedYou(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12418")
	r.toWalk(task)
	r.restartDaemon()
	r.toWalk(task)
	r.restartDaemon()
	r.toWalk(task)
	if got := r.reload(task.ID); got.State != registry.TaskActive {
		t.Fatalf("task %s waits %q: %q, want at work after two drains", got.State, got.WaitsWho, got.WaitsWhat)
	}
	for k := 1; k <= 2; k++ {
		if res := r.qaResult(task.ID, k, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutDrain {
			t.Fatalf("run %d's result.json %+v, want interrupted by a drain", k, res)
		}
	}
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 3 {
		t.Fatalf("qa-run.json %+v, want run 3", run)
	}
}

// Only a crash counts towards the cap of interrupted runs: two crashes in a
// row at a head make QA unavailable, and a drain between them neither counts
// nor breaks the row.
func TestOnlyCrashesCountTowardsTheCapOfInterruptedRuns(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12419")
	r.toWalk(task)
	r.crashDaemon()
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutCrash ||
		res.Why != "the daemon stopped during the run: its server and browser were stopped" {
		t.Fatalf("run 1's result.json %+v, want interrupted by a crash", res)
	}
	r.toWalk(task)
	r.restartDaemon()
	r.toWalk(task)
	r.crashDaemon()
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "the daemon stopped during 2 QA runs in a row at this head") ||
		!strings.Contains(got.WaitsWhat, "magnum factory retry") {
		t.Fatalf("the task waits %q, want the two crashes named", got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.started) != 3 {
		t.Fatalf("servers started %v, want three runs", l.started)
	}
}

// A QA run is work a restart would cut: it is in the work record from its
// server's start until its session ends, and a session the factory ended
// (the task went to you) is no crash at the next start.
func TestAQARunIsInTheWorkRecordUntilItEnds(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12420")
	r.toWalk(task)
	rec, ok, err := ReadWork(r.layout)
	if err != nil || !ok || len(rec.Work) != 1 || rec.Work[0].Kind != WorkQA || rec.Work[0].Task != task.ID ||
		rec.Work[0].What != "QA run 1 of PR-12420" {
		t.Fatalf("work.json %+v (%v, %v), want the QA run", rec, ok, err)
	}
	r.e.toOperator(r.ctx, r.reload(task.ID), "a test sends it to you", "test")
	if rec, _, _ := ReadWork(r.layout); len(rec.Work) != 0 {
		t.Fatalf("work.json %+v, want nothing once the session ended", rec)
	}
	r.restart()
	if err := r.e.startup(r.ctx); err != nil {
		t.Fatal(err)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutTask {
		t.Fatalf("run 1's result.json %+v, want interrupted as its task left it, no crash", res)
	}
}

// A drain ends a QA session that has not begun its walk at once, so
// install --drain never waits on a session that waits on the drain; a
// session whose walker is at work stays in the record until it ends. No
// fresh run starts while the drain holds.
func TestADrainEndsAQASessionThatHasNotBegunItsWalk(t *testing.T) {
	r := newQARig(t)
	r.e.drainerAlive = func(Drain) bool { return true }
	task, head := r.qaTask("PR-12421")
	r.toWalk(task)
	walker, err := r.reg.BeginAttempt(r.ctx, task.ID, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionAttempt(r.ctx, walker.ID, []string{registry.AttemptLaunching}, registry.AttemptRunning, nil); err != nil {
		t.Fatal(err)
	}
	value := Drain{Since: r.clock.Now(), PID: os.Getpid()}.Value()
	if err := WriteDrain(r.layout, value); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if rec, _, _ := ReadWork(r.layout); rec.Drain != value || len(rec.Work) != 1 || rec.Work[0].Kind != WorkQA {
		t.Fatalf("work.json %+v, want the drain held and the walking run in it", rec)
	}
	if l := r.copies.log(); len(l.stopped) != 0 {
		t.Fatalf("servers stopped %v under a walker at work", l.stopped)
	}
	if err := r.reg.TransitionAttempt(r.ctx, walker.ID, []string{registry.AttemptRunning}, registry.AttemptFailed, nil); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if rec, _, _ := ReadWork(r.layout); rec.Drain != value || len(rec.Work) != 0 {
		t.Fatalf("work.json %+v, want the drain held and nothing in flight", rec)
	}
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 || len(l.started) != 1 {
		t.Fatalf("servers started %v, stopped %v, browsers stopped %v; want the one run stopped, none started",
			l.started, l.stopped, l.stoppedBrowsers)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutDrain {
		t.Fatalf("run 1's result.json %+v, want interrupted by the drain", res)
	}
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 1 {
		t.Fatalf("qa-run.json %+v: a fresh run began while the drain holds", run)
	}
	r.qaFree(task.ID)
}

// A stop of the browser that panics in the teardown still stops the server
// and frees the Mac's QA lock and the heavy slot.
func TestATeardownWhoseBrowserStopPanicsStillFreesTheMac(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12422")
	r.toWalk(task)
	r.e.d.Copies = &panickyCopies{fakeCopies: r.copies, stopBrowser: true}
	r.e.toOperator(r.ctx, r.reload(task.ID), "a test sends it to you", "test")
	if l := r.copies.log(); len(l.stopped) != 1 {
		t.Fatalf("servers stopped %v, want the one", l.stopped)
	}
	if !slices.ContainsFunc(r.events(task.ID, "qa"), func(ev registry.Event) bool {
		return ev.Phase == registry.PhaseFail && strings.Contains(ev.Text, "stop its browser panicked")
	}) {
		t.Fatalf("qa events %+v, want the browser's stop named as failed", r.events(task.ID, "qa"))
	}
	r.qaFree(task.ID)
}

// A start of the browser that panics leaves no session ready for the walk
// without a browser: the next pass starts it again.
func TestAPanicInStartBrowserStartsItAgain(t *testing.T) {
	r := newQARig(t)
	r.e.d.Copies = &panickyCopies{fakeCopies: r.copies, startBrowserOnce: true}
	task, _ := r.qaTask("PR-12423")
	r.toWalk(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.CDP != "http://127.0.0.1:9333" {
		t.Fatalf("qa-run.json %+v, want the browser's endpoint", run)
	}
	if l := r.copies.log(); len(l.browsers) != 1 {
		t.Fatalf("browsers started %v, want the one after the panic", l.browsers)
	}
}

// A start of the server that panics ends its session at once, without the
// wait for a boot that never began, and the next pass starts it again.
func TestAPanicInStartServerDoesNotStallTheLoop(t *testing.T) {
	r := newQARig(t)
	r.e.d.Copies = &panickyCopies{fakeCopies: r.copies, startOnce: true}
	task, _ := r.qaTask("PR-12424")
	var worst time.Duration
	for range 8 {
		if r.reload(task.ID).WaitsWhat == qaReadyWhat {
			break
		}
		start := time.Now()
		r.tick()
		r.e.waitBackground()
		worst = max(worst, time.Since(start))
	}
	if worst >= qaBootWait/3 {
		t.Fatalf("a tick took %s, want no wait for a boot that never began", worst)
	}
	r.toWalk(task)
	if l := r.copies.log(); len(l.started) != 1 || len(l.stopped) != 1 {
		t.Fatalf("servers started %v, stopped %v; want the panicked start stopped and one start after it", l.started, l.stopped)
	}
}

// The daemon's stop, whatever ends its loop, ends the QA sessions
// (RunLocked's deferred stopQASessions).
func TestRunLockedEndsTheQASessionsAsItStops(t *testing.T) {
	r := newQARig(t)
	task, _ := r.qaTask("PR-12425")
	r.toWalk(task)
	r.captureSignals()
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	_ = r.e.RunLocked(ctx) // an ended context: its startup fails, its defers run
	l := r.copies.log()
	if len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 || slices.ContainsFunc(l.stopCtxErr, func(err error) bool { return err != nil }) {
		t.Fatalf("browsers stopped %v, servers stopped %v, their contexts %v; want each once, on a context of its own",
			l.stoppedBrowsers, l.stopped, l.stopCtxErr)
	}
	r.qaFree(task.ID)
}

// The user-facing check runs at most 30 seconds (it runs on the loop), with
// the changed files on its stdin.
func TestTheUserFacingCheckRunsAtMostThirtySeconds(t *testing.T) {
	r := newQARig(t)
	var seen []execx.Cmd
	r.seenMise = func(c execx.Cmd) { seen = append(seen, c) }
	task, _ := r.qaTask("PR-12426")
	r.toWalk(task)
	i := slices.IndexFunc(seen, func(c execx.Cmd) bool { return c.Args[len(c.Args)-1] == "bin/user-facing" })
	if i < 0 || seen[i].Timeout != 30*time.Second || len(seen[i].Stdin) == 0 {
		t.Fatalf("the user-facing check ran as %+v, want 30s and the changed files on stdin", seen)
	}
}

// qaPage is the walker's n-th page, id at path, showing checks: its shot,
// probe and console log named as factory-qa.md names them.
func qaPage(n int, id, path string, checks ...string) QAPage {
	base := fmt.Sprintf("pages/%02d-%s-signed_in-after", n, strings.ToLower(id))
	return QAPage{ID: id, Path: path, State: "signed_in", Side: "after", Shot: base + ".png", Probe: base + ".json",
		Console: fmt.Sprintf("console/%02d.log", n), Status: 200, Checks: checks}
}

// toWalker runs the ticks until the task's session is ready for the walk,
// then until the walker got the first prompt of its turn.
func (r *qaRig) toWalker(task registry.Task) {
	r.t.Helper()
	before := len(r.rt.promptsTo("fx-qa"))
	r.toWalk(task)
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-qa")) > before })
	if len(r.rt.promptsTo("fx-qa")) == before {
		got := r.reload(task.ID)
		r.t.Fatalf("the walker got no prompt; task %s at %s waits %q: %q", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
}

// runFile writes text to rel in the run's folder runPath.
func (r *qaRig) runFile(runPath, rel, text string) {
	r.t.Helper()
	p := filepath.Join(runPath, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// savePages writes what the walker saves of each page of v in the task's
// newest run: a PNG shot, the probe probes[page id] names (a page of
// testdata/qa, the run's nonce in place of the fixture's) and an empty
// console log. It returns the run.
func (r *qaRig) savePages(task registry.Task, v QAVerdict, probes map[string]string) qaRun {
	r.t.Helper()
	run, ok := readQARun(r.layout.FactoryTaskDir(task.ID))
	if !ok || run.Nonce == "" {
		r.t.Fatalf("qa-run.json %+v (%v), want a run with its nonce", run, ok)
	}
	runPath := r.runDir(task.ID, run.N, run.Head)
	for _, pg := range v.Pages {
		b, err := os.ReadFile(filepath.Join("testdata", "qa", probes[pg.ID]))
		if err != nil {
			r.t.Fatal(err)
		}
		r.runFile(runPath, pg.Shot, pngMagic+"the page")
		r.runFile(runPath, pg.Probe, strings.Replace(string(b), `"n0nce"`, `"`+run.Nonce+`"`, 1))
		r.runFile(runPath, pg.Console, "")
	}
	return run
}

// writeVerdict writes v as the run's verdict.json, version 1 at the run's
// head unless v says otherwise, a second after the clock.
func (r *qaRig) writeVerdict(task registry.Task, run qaRun, v QAVerdict) {
	r.t.Helper()
	if v.Version == 0 {
		v.Version = QAVerdictVersion
	}
	if v.Head == "" {
		v.Head = run.Head
	}
	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	r.taskFile(task.ID, filepath.Join(run.Dir, qaVerdictFile), string(b))
}

// walk is the walker's turn: its pages saved (savePages), v as the run's
// verdict.json, then its turn's end.
func (r *qaRig) walk(task registry.Task, v QAVerdict, probes map[string]string) {
	r.t.Helper()
	run := r.savePages(task, v, probes)
	r.writeVerdict(task, run, v)
	r.ended(task, "fx-qa")
}

// oneSecurityPage is a verdict of one page, the security settings, which
// shows A1.
func oneSecurityPage() QAVerdict {
	return QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/settings/security", "A1")}}
}

// qaAttempts are the states of the task's attempts of its qa stage, oldest
// first.
func (r *qaRig) qaAttempts(id int64) []string {
	r.t.Helper()
	var out []string
	for _, a := range r.attempts(id) {
		if a.Stage == "qa" {
			out = append(out, a.State)
		}
	}
	return out
}

// stageAtStop are the rig's copies that note the task's stage at each stop
// of its server.
type stageAtStop struct {
	*fakeCopies
	reg    *registry.Registry
	id     int64
	mu     sync.Mutex
	stages []string
}

func (s *stageAtStop) StopServer(ctx context.Context, c Copy) error {
	if t, err := s.reg.TaskByID(context.WithoutCancel(ctx), s.id); err == nil {
		s.mu.Lock()
		s.stages = append(s.stages, t.Stage)
		s.mu.Unlock()
	}
	return s.fakeCopies.StopServer(ctx, c)
}

// The walker's turn: the qa stage launches fx-qa alone, in the QA session's
// copy (no tab in your browser, and its browser tool attached to the
// factory's Chrome by its env), and its prompt names the session's CDP
// endpoint with the flags that attach to it, the run's verdict, the probe
// and the acceptance checks.
func TestTheWalkerGetsTheBrowserTheRunAndTheAcceptanceChecks(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12430")
	r.toWalker(task)
	if got := r.rt.launchedRoles(); !slices.EqualFunc(got, [][]string{{"fx-qa"}}, slices.Equal) {
		t.Fatalf("launches %v, want fx-qa alone", got)
	}
	session := fmt.Sprintf("magnum-qa-%d-1", task.ID)
	cs := r.rt.launchedCopies()
	if len(cs) != 1 {
		t.Fatalf("launches in %d copies, want 1", len(cs))
	}
	if env := cs[0].Env; cs[0].Path != r.copyDir || env["BROWSER"] != "true" || !strings.HasPrefix(env["PATH"], r.layout.FactoryShims()+":") ||
		env["AGENT_BROWSER_CDP"] != "http://127.0.0.1:9333" || env["AGENT_BROWSER_SESSION"] != session || env["PORT"] != "3001" {
		t.Fatalf("the walker launched in %s with env %v, want the QA session's copy", cs[0].Path, env)
	}
	if ps := r.rt.allPrompts(); slices.ContainsFunc(ps, func(p sentPrompt) bool { return p.Role != "fx-qa" }) {
		t.Fatalf("prompts %+v, want the walker's alone", ps)
	}
	text := r.rt.promptsTo("fx-qa")[0].Text
	dir := r.layout.FactoryTaskDir(task.ID)
	runDir := filepath.Join(dir, "qa", "1-"+head[:7])
	for _, want := range []string{"http://127.0.0.1:9333", "--session " + session + " --cdp http://127.0.0.1:9333",
		filepath.Join(runDir, "verdict.json"), filepath.Join(runDir, "probe.js"), filepath.Join(dir, "acceptance.json"),
		"http://app.example.localhost:3001", "> " + runDir + "/pages/"} {
		if !strings.Contains(text, want) {
			t.Errorf("the walker's prompt lacks %q:\n%s", want, text)
		}
	}
}

// A walk whose pages the factory's checks find clean passes QA: the
// browser and the server stop before the stage moves, the summary names
// the head, and the qa check passes there. A questions file the walker
// wrote anyway suspends nothing and reaches no one.
func TestACleanWalkPassesQAAndStopsEverything(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12431")
	watch := &stageAtStop{fakeCopies: r.copies, reg: r.reg, id: task.ID}
	r.e.d.Copies = watch
	r.toWalker(task)
	r.taskFile(task.ID, roleQuestionsFile("fx-qa"), `[{"text": "Which user should I sign in as?", "context": "the sign-in", `+
		`"example": "qa@example.com", "options": [{"label": "qa", "means": "the qa user"}, {"label": "admin", "means": "an admin"}]}]`)
	r.walk(task, oneSecurityPage(), map[string]string{"P1": "clean-tooltip.json"})
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	got := r.reload(task.ID)
	if got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q: %q, want pr_body", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	watch.mu.Lock()
	stages := slices.Clone(watch.stages)
	watch.mu.Unlock()
	if !slices.Equal(stages, []string{"qa"}) {
		t.Fatalf("the server stopped at stages %v, want once, before the stage moved", stages)
	}
	sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID))
	if !ok || sum.Outcome != QAOutcomeClean || sum.Head != head || sum.Pages != 1 || sum.Run != 1 {
		t.Fatalf("qa-summary.json %+v (%v), want clean at %s with 1 page", sum, ok, head)
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"pass:@" + head}) {
		t.Fatalf("qa checks %q, want one passed at the head", got)
	}
	if got := r.rt.roleQuitList(); len(got) != 0 {
		t.Fatalf("agents quit %v: a walker that ended its turn is left as it is", got)
	}
	if qs, err := r.reg.Questions(r.ctx, task.ID); err != nil || len(qs) != 0 {
		t.Fatalf("questions %+v (%v), want none asked", qs, err)
	}
	files, err := r.e.questionFiles(got, r.layout.FactoryTaskDir(task.ID))
	if err != nil || slices.ContainsFunc(files, func(q questionsOf) bool { return q.raisedBy == "fx-qa" }) {
		t.Fatalf("question files %+v (%v): the walker's would reach you", files, err)
	}
	r.qaFree(task.ID)
}

// The #785 page: the walker says nothing is wrong, and the factory's own
// check of the saved page finds the tooltip's markup shown as text. QA
// blocks: the defect is the factory's, no summary passes the stage, the
// qa check fails as the change's, and the writer gets the defect.
func TestTheEscapedTooltipBlocksQAWhateverTheWalkerSays(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12432")
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), map[string]string{"P1": "escaped-tooltip.json"})
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	res := r.qaResult(task.ID, 1, head)
	if res.Outcome != QAOutcomeDefects || len(res.Blocking) != 1 || res.Blocking[0].Class != QAEscapedMarkup ||
		res.Blocking[0].Source != qaSourceFactory || res.Pages != 1 {
		t.Fatalf("result.json %+v, want the factory's escaped_markup blocking", res)
	}
	if got := r.reload(task.ID); got.Stage == "pr_body" {
		t.Fatal("the stage passed with a defect that blocks")
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: the stage passed")
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"fail:code@" + head}) {
		t.Fatalf("qa checks %q, want one failed as the change's", got)
	}
	r.qaFree(task.ID)
}

// A visible defect the local review left as a P3 still blocks QA: QA's
// decision reads no severity of the review's, and leaves its summary alone.
func TestQABlocksAVisibleDefectTheReviewLeftAsP3(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12433")
	b, err := json.Marshal(ReviewSummary{Exit: registry.ExitClean, Rounds: 1, Head: head,
		Leftovers: []ReviewItem{{ID: "F1", Severity: "P3", Title: "dates show HTML", Path: "app/views/settings/_keys.html.erb"}}})
	if err != nil {
		t.Fatal(err)
	}
	r.taskFile(task.ID, reviewSummaryFile, string(b))
	r.toWalker(task)
	r.walk(task, oneSecurityPage(), map[string]string{"P1": "escaped-tooltip.json"})
	r.run(4, func() bool { return len(r.rt.promptsTo("fx-writer")) > 0 })
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeDefects {
		t.Fatalf("result.json %+v, want defects", res)
	}
	if got := r.readTask(task.ID, reviewSummaryFile); got != string(b) {
		t.Fatalf("review-summary.json changed:\n%s\nwant\n%s", got, b)
	}
}

// A verdict whose page is off the copy's own server gets the walker one
// nudge naming verdict.json and why; a second such verdict needs you, and
// the run's server is stopped.
func TestAnInvalidVerdictGetsOneNudgeThenNeedsYou(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12435")
	r.toWalker(task)
	v := oneSecurityPage()
	run := r.savePages(task, v, map[string]string{"P1": "clean-tooltip.json"})
	runPath := r.runDir(task.ID, run.N, run.Head)
	b, err := os.ReadFile(filepath.Join(runPath, v.Pages[0].Probe))
	if err != nil {
		t.Fatal(err)
	}
	r.runFile(runPath, v.Pages[0].Probe, strings.Replace(string(b), "http://app.example.localhost:3001", "http://staging.example.com", 1))
	r.writeVerdict(task, run, v)
	r.ended(task, "fx-qa")
	r.run(3, func() bool { return len(r.rt.promptsTo("fx-qa")) > 1 })
	ps := r.rt.promptsTo("fx-qa")
	verdict := filepath.Join("qa", "1-"+head[:7], "verdict.json")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, nudgeLead) || !strings.Contains(ps[1].Text, verdict) ||
		!strings.Contains(ps[1].Text, "staging.example.com") {
		t.Fatalf("the walker's prompts %+v, want its nudge naming %s and the host", ps, verdict)
	}
	r.ended(task, "fx-qa")
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "the fx-qa agent wrote no valid "+verdict) {
		t.Fatalf("the task waits %q, want the walker's verdict named", got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.stopped) != 1 || len(l.stoppedBrowsers) != 1 {
		t.Fatalf("servers stopped %v, browsers stopped %v; want each once", l.stopped, l.stoppedBrowsers)
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("an invalid verdict passed the stage")
	}
	r.qaFree(task.ID)
}

// A walk that reached no page cannot tell: QA is unavailable, and the task
// needs you with the walker's reason.
func TestAWalkThatReachesNoPageNeedsYou(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12436")
	r.toWalker(task)
	r.walk(task, QAVerdict{Blocked: []QABlocked{{What: "the security tab", Why: "sign-in failed", Check: "A1"}}}, nil)
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "QA reached no page: sign-in failed") || !strings.Contains(got.WaitsWhat, "magnum factory retry") {
		t.Fatalf("the task waits %q, want the walker's reason and the retry", got.WaitsWhat)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeUnavailable || len(res.Blocked) != 1 {
		t.Fatalf("result.json %+v, want unavailable with the blocked state", res)
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"fail:env@" + head}) {
		t.Fatalf("qa checks %q, want one failed for the environment", got)
	}
	r.qaFree(task.ID)
}

// A state the walker could not reach is a page no check judged, so a run
// with one is not clean however clean its other pages are. It needs you,
// naming the state and why: a writer's turn cannot make a sign-in work or
// headless Chrome draw an autofill list, so it is no defect for the fix
// loop.
func TestAWalkWithABlockedStateNeedsYouNotClean(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12437")
	r.toWalker(task)
	v := oneSecurityPage()
	v.Blocked = []QABlocked{{What: "the passkey autofill list", Why: "headless Chrome draws no autofill", Check: "A1"}}
	r.walk(task, v, map[string]string{"P1": "clean-tooltip.json"})
	got := r.toYou(task)
	for _, want := range []string{"QA could not reach 1 state", "the passkey autofill list (headless Chrome draws no autofill)",
		fmt.Sprintf("`magnum factory retry %d` walks once more", task.ID), "flagged on the card and in the PR body"} {
		if !strings.Contains(got.WaitsWhat, want) {
			t.Errorf("the task waits %q, want %q in it", got.WaitsWhat, want)
		}
	}
	res := r.qaResult(task.ID, 1, head)
	if res.Outcome == QAOutcomeClean || res.Pages != 1 || len(res.Blocked) != 1 || len(res.Blocking) != 0 {
		t.Fatalf("result.json %+v, want a run that is not clean, with its page and its blocked state", res)
	}
	if _, ok := readQASummary(r.layout.FactoryTaskDir(task.ID)); ok {
		t.Fatal("a qa-summary.json was written: the stage passed")
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"fail:env@" + head}) {
		t.Fatalf("qa checks %q, want one failed for the environment", got)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts for a state QA could not reach", len(ps))
	}
}

// A run past its cap ends whatever holds the loop's stages: the sweep of
// every tick stops its browser and its server and needs you, here with
// herdr down, when no stage runs.
func TestQAOverItsCapNeedsYou(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12438")
	r.toWalker(task)
	herdrDown := errors.New("herdr: connection refused")
	r.e.d.HerdrUp = func(context.Context) error { return herdrDown }
	r.clock.Add(51 * time.Minute)
	r.tick()
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "over its cap of 50m0s") {
		t.Fatalf("task %s waits %q, want NEEDS YOU over the cap", got.State, got.WaitsWhat)
	}
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeUnavailable {
		t.Fatalf("result.json %+v, want unavailable", res)
	}
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptFailed}) {
		t.Fatalf("qa attempts %v, want the walker's failed", got)
	}
	if got := r.rt.roleQuitList(); !slices.Equal(got, []string{"PR-12438/fx-qa"}) {
		t.Fatalf("agents quit %v, want the walker", got)
	}
	r.qaFree(task.ID)
}

// Critical memory pressure stops the walk: its server and browser stop, the
// run ends as pressure, the walker's attempt fails and its agent quits, so
// nothing of the run holds memory. Once the hold lifts a fresh run walks; a
// second pressure in a row needs you.
func TestCriticalPressureStopsTheWalkAndRetriesOnceAfterIt(t *testing.T) {
	r := newQARig(t)
	var critical atomic.Bool
	r.e.d.Memory = func(context.Context) engine.MemorySample {
		if critical.Load() {
			return engine.MemorySample{Level: engine.MemoryCritical}
		}
		return engine.MemorySample{Level: engine.MemoryNormal}
	}
	task, head := r.qaTask("PR-12439")
	r.toWalker(task)
	critical.Store(true)
	r.run(2, func() bool { return r.e.qaSessionOf(task.ID) == nil })
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomePressure {
		t.Fatalf("run 1's result.json %+v, want pressure", res)
	}
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptFailed}) {
		t.Fatalf("qa attempts %v, want the walker's failed", got)
	}
	if got := r.rt.roleQuitList(); !slices.Equal(got, []string{"PR-12439/fx-qa"}) {
		t.Fatalf("agents quit %v, want the walker", got)
	}
	r.tick()
	if got := r.reload(task.ID); got.WaitsWho != HoldMemory {
		t.Fatalf("the task waits %q: %q, want the memory hold", got.WaitsWho, got.WaitsWhat)
	}
	critical.Store(false)
	r.toWalker(task)
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); run.N != 2 {
		t.Fatalf("qa-run.json %+v, want run 2 after the hold lifted", run)
	}
	critical.Store(true)
	r.run(2, func() bool { return r.e.qaSessionOf(task.ID) == nil })
	critical.Store(false)
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "the Mac's memory stopped QA 2 times in a row at this head") {
		t.Fatalf("the task waits %q, want the two pressures named", got.WaitsWhat)
	}
	r.qaFree(task.ID)
}

// A restart in the middle of the walk cuts the run and fails the walker's
// attempt; the next daemon's fresh run walks in an attempt of its own.
func TestARestartMidWalkFailsTheWalkersAttempt(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12440")
	r.toWalker(task)
	r.restartDaemon()
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted {
		t.Fatalf("run 1's result.json %+v, want interrupted", res)
	}
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptFailed}) {
		t.Fatalf("qa attempts %v, want the walker's failed", got)
	}
	r.toWalker(task)
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptFailed, registry.AttemptRunning}) {
		t.Fatalf("qa attempts %v, want run 2's walker in an attempt of its own", got)
	}
	ps := r.rt.promptsTo("fx-qa")
	if len(ps) != 2 || ps[0].Attempt == ps[1].Attempt || !strings.Contains(ps[1].Text, "qa/2-"+head[:7]) {
		t.Fatalf("the walker's prompts %+v, want run 2's in a new attempt", ps)
	}
}

// An agent that needs you in the middle of the walk cuts the run: its
// server and browser stop, the run is interrupted by its task leaving it,
// and the walker's attempt fails, so your retry walks a fresh run.
func TestNeedsYouDuringTheWalkCutsTheRunAndFailsTheWalkersAttempt(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12441")
	r.toWalker(task)
	r.observeEvents(runtime.Event{TaskID: task.ID, Role: "fx-qa", RunID: r.lastRun("fx-qa"), Kind: runtime.EventStuck, Reason: "no output for 20m"})
	got := r.toYou(task)
	if !strings.Contains(got.WaitsWhat, "the fx-qa agent is stuck") {
		t.Fatalf("the task waits %q, want the stuck walker", got.WaitsWhat)
	}
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutTask {
		t.Fatalf("run 1's result.json %+v, want interrupted by its task leaving it", res)
	}
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptFailed}) {
		t.Fatalf("qa attempts %v, want the walker's failed", got)
	}
	if l := r.copies.log(); len(l.stoppedBrowsers) != 1 || len(l.stopped) != 1 {
		t.Fatalf("browsers stopped %v, servers stopped %v; want each once", l.stoppedBrowsers, l.stopped)
	}
	if got := r.rt.roleQuitList(); !slices.Equal(got, []string{"PR-12441/fx-qa"}) {
		t.Fatalf("agents quit %v, want the walker", got)
	}
	r.qaFree(task.ID)
}

// A walker whose launch waits (the memory's soft hold) has not begun its
// walk: a drain ends its session at once, as one whose walk never started,
// so install --drain never waits on a launch the drain holds.
func TestADrainEndsAQASessionWhoseWalkerNeverLaunched(t *testing.T) {
	r := newQARig(t)
	r.e.drainerAlive = func(Drain) bool { return true }
	var full atomic.Bool
	r.e.d.Memory = func(context.Context) engine.MemorySample {
		if full.Load() {
			return engine.MemorySample{Level: engine.MemoryNormal, Swap: &engine.MemorySwap{UsedBytes: 95, TotalBytes: 100}}
		}
		return engine.MemorySample{Level: engine.MemoryNormal}
	}
	task, head := r.qaTask("PR-12442")
	r.toWalk(task)
	full.Store(true)
	r.tick()
	if got := r.qaAttempts(task.ID); !slices.Equal(got, []string{registry.AttemptLaunching}) || len(r.rt.launchedRoles()) != 0 {
		t.Fatalf("qa attempts %v, launches %v; want the walker's launch held", got, r.rt.launchedRoles())
	}
	if err := WriteDrain(r.layout, Drain{Since: r.clock.Now(), PID: os.Getpid()}.Value()); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if res := r.qaResult(task.ID, 1, head); res.Outcome != QAOutcomeInterrupted || res.Cut != qaCutDrain {
		t.Fatalf("run 1's result.json %+v, want interrupted by the drain", res)
	}
	if rec, _, _ := ReadWork(r.layout); len(rec.Work) != 0 {
		t.Fatalf("work.json %+v, want nothing in flight", rec)
	}
	r.qaFree(task.ID)
}

// Your retry after a walk that reached its pages but not every state walks
// once more; a second such walk at the head goes on with those states
// flagged: qa-summary.json clean with them, each run's result and failed
// check kept as they are, and no passing check. A walk that reached no page
// is no such walk: your retry walks again, as often as you retry.
func TestASecondBlockedOnlyWalkGoesOnWithTheStatesFlagged(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12445")
	v := oneSecurityPage()
	v.Blocked = []QABlocked{{What: "the passkey autofill list", Why: "headless Chrome draws no autofill", Check: "A1"}}
	r.toWalker(task)
	r.walk(task, v, map[string]string{"P1": "clean-tooltip.json"})
	r.toYou(task)
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, v, map[string]string{"P1": "clean-tooltip.json"})
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q: %q, want pr_body", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	sum, ok := readQASummary(r.layout.FactoryTaskDir(task.ID))
	if !ok || sum.Outcome != QAOutcomeClean || sum.Head != head || sum.Run != 2 || sum.Pages != 1 || len(sum.Blocked) != 1 ||
		sum.Blocked[0].What != "the passkey autofill list" {
		t.Fatalf("qa-summary.json %+v (%v), want run 2 clean at the head with its blocked state", sum, ok)
	}
	for k := 1; k <= 2; k++ {
		if res := r.qaResult(task.ID, k, head); res.Outcome != QAOutcomeUnavailable || len(res.Blocked) != 1 {
			t.Fatalf("run %d's result.json %+v, want it as the walk ended", k, res)
		}
	}
	if got := r.qaChecks(task.ID); !slices.Equal(got, []string{"fail:env@" + head, "fail:env@" + head}) {
		t.Fatalf("qa checks %q, want each run's failed check and no pass", got)
	}

	n := newQARig(t)
	other, _ := n.qaTask("PR-12446")
	none := QAVerdict{Blocked: []QABlocked{{What: "the security tab", Why: "sign-in failed", Check: "A1"}}}
	for range 2 {
		n.toWalker(other)
		n.walk(other, none, nil)
		if got := n.toYou(other); !strings.Contains(got.WaitsWhat, "QA reached no page") {
			t.Fatalf("the task waits %q, want the walk that reached no page", got.WaitsWhat)
		}
		n.retryAnswer(other.ID)
	}
	if _, ok := readQASummary(n.layout.FactoryTaskDir(other.ID)); ok {
		t.Fatal("a walk that reached no page passed the stage")
	}
}

// The cap of a run counts from its server's first start: a boot more after
// a failed one starts no new cap.
func TestTheCapCountsFromTheRunsFirstBoot(t *testing.T) {
	r := newQARig(t)
	r.setProbe(func(up bool, _ int) (int, error) {
		if !up || len(r.copies.log().started) < 2 {
			return 0, errRefused
		}
		return 200, nil
	})
	task, head := r.qaTask("PR-12447")
	r.toWalker(task)
	run, _ := readQARun(r.layout.FactoryTaskDir(task.ID))
	if l := r.copies.log(); len(l.started) != 2 || run.Began.IsZero() || run.Started.Sub(run.Began) < 4*time.Minute {
		t.Fatalf("servers started %v, qa-run.json %+v; want two boots, the run begun at the first", l.started, run)
	}
	r.clock.Add(47*time.Minute - r.clock.Now().Sub(run.Started)) // within 50m of the second start, past them since the first
	r.tick()
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "over its cap of 50m0s") {
		t.Fatalf("task %s waits %q, want NEEDS YOU over the cap", got.State, got.WaitsWhat)
	}
	if res := r.qaResult(task.ID, 1, head); !res.Started.Equal(run.Began) {
		t.Fatalf("result.json started %s, want the run's first start %s", res.Started, run.Began)
	}
}

// A walk that passed but whose end did not land (its result was not
// written) ends on the next pass, with the time the run took from its
// server's first start: the teardown that forgot the server's start keeps
// when the run began.
func TestAWalkWhoseEndDidNotLandRecordsItsTime(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12448")
	r.toWalker(task)
	v := oneSecurityPage()
	run := r.savePages(task, v, map[string]string{"P1": "clean-tooltip.json"})
	r.writeVerdict(task, run, v)
	as := r.attempts(task.ID)
	if err := r.reg.TransitionAttempt(r.ctx, as[len(as)-1].ID, []string{registry.AttemptRunning}, registry.AttemptVerified, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.e.endQA(r.ctx, r.reload(task.ID), "its walk ended", ""); err != nil { // the end's teardown, then nothing
		t.Fatal(err)
	}
	if run, _ := readQARun(r.layout.FactoryTaskDir(task.ID)); !run.Started.IsZero() {
		t.Fatalf("qa-run.json %+v, want its server's start forgotten", run)
	}
	r.clock.Add(3 * time.Minute)
	r.run(3, func() bool { return r.reload(task.ID).Stage != "qa" })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task at %s waits %q, want pr_body", got.Stage, got.WaitsWhat)
	}
	cs, err := r.reg.Checks(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cs, func(c registry.Check) bool { return c.Kind == registry.CheckQA })
	if i < 0 || cs[i].Result != registry.CheckPass || cs[i].HeadSHA != head || cs[i].Took < 3*time.Minute {
		t.Fatalf("qa checks %+v, want a pass that took the run's time", cs)
	}
}

// Each QA result writes the run's report, summary.md in its folder, which
// the ready card's e opens: the run, its head and the copy's URL, each page
// with its path, its state and its shot's absolute path, the notes, and the
// states not reached. A walk that passes with its states flagged writes it
// again, as passed.
func TestAQARunsReportNamesItsPagesShotsAndStates(t *testing.T) {
	r := newQARig(t)
	task, head := r.qaTask("PR-12456")
	v := QAVerdict{Pages: []QAPage{qaPage(1, "P1", "/settings/security", "A1"), qaPage(2, "P2", "/orders")},
		Defects: []QADefect{{ID: "D1", Class: QABrokenLayout, Severity: "P3", Page: "P2", Title: "the table scrolls sideways"}},
		Blocked: []QABlocked{{What: "the passkey autofill list", Why: "headless Chrome draws no autofill", Check: "A1"}}}
	probes := map[string]string{"P1": "clean-tooltip.json", "P2": "clean-tooltip.json"}
	r.toWalker(task)
	r.walk(task, v, probes)
	r.toYou(task)
	report := func(k int) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(r.runDir(task.ID, k, head), qaReportFile))
		if err != nil {
			t.Fatalf("run %d's report: %v", k, err)
		}
		return string(b)
	}
	first := report(1)
	shot := filepath.Join(r.runDir(task.ID, 1, head), "pages", "01-p1-signed_in-after.png")
	for _, want := range []string{"run 1", head, "http://app.example.localhost:3001", "/settings/security", "signed_in", shot,
		"/orders", "P3 broken_layout on /orders: the table scrolls sideways", "the passkey autofill list", "headless Chrome draws no autofill",
		"unavailable"} {
		if !strings.Contains(first, want) {
			t.Errorf("run 1's report lacks %q:\n%s", want, first)
		}
	}
	r.retryAnswer(task.ID)
	r.toWalker(task)
	r.walk(task, v, probes)
	r.run(4, func() bool { return r.reload(task.ID).Stage != "qa" })
	second := report(2)
	if !strings.Contains(second, "passed with 1 state not reached: 2 pages walked at "+head[:7]+", 1 note") ||
		!strings.Contains(second, filepath.Join(r.runDir(task.ID, 2, head), "pages", "02-p2-signed_in-after.png")) {
		t.Errorf("run 2's report, which passed with its state flagged:\n%s", second)
	}
	if fi, err := os.Stat(filepath.Join(r.runDir(task.ID, 2, head), qaReportFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the report's mode %v (%v), want 0600", fi.Mode(), err)
	}
}
