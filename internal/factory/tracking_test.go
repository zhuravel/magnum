package factory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// trackJira is the factory's Jira in the tracking tests: StatusSince
// answers each key's preset status (To Do, new, for a key with none) and
// records the keys, in order. While down is set every read fails as curl's
// exit code down does, a key refused fails with its HTTP status, and a key
// partial answers its status with a failed changelog page, each through a
// real jira.Client over a fake curl, so the errors are the jira package's
// own. While block is set a read waits for it to close, or for its
// context, after it says so on started. Ticket fails as down or ticketCode
// says (an intake try).
type trackJira struct {
	mu         sync.Mutex
	statuses   map[string]jira.IssueStatus
	refused    map[string]int
	partial    map[string]bool
	noAccount  map[string]bool
	down       int
	ticketCode int
	block      chan struct{}
	started    chan string
	cut        []error // the context errors of the reads a block's wait ended by
	asked      []string
	priors     []jira.IssueStatus
	tickets    []string
	client     *jira.Client
}

func newTrackJira(t *testing.T) *trackJira {
	t.Helper()
	f := &trackJira{statuses: map[string]jira.IssueStatus{}, refused: map[string]int{}, partial: map[string]bool{}, noAccount: map[string]bool{}}
	token := filepath.Join(t.TempDir(), "jira.token")
	if err := os.WriteFile(token, []byte("ATATT3xFfGF0-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.client = &jira.Client{Runner: &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Fn: f.curl}}},
		URL: jiraTestSite, User: "alice@example.com", TokenFile: token}
	return f
}

// curl fails a read as the fake says: curl's exit code while down, else
// the key's HTTP status with a body that names the ticket.
func (f *trackJira) curl(c execx.Cmd) (execx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != 0 {
		return execx.Result{Code: f.down}, &execx.ExitError{Cmd: c, Code: f.down}
	}
	code := f.ticketCode
	for key, n := range f.refused {
		if strings.Contains(string(c.Stdin), "/issue/"+key+"?") {
			code = n
		}
	}
	if code == 0 {
		code = 404
	}
	return execx.Result{Stdout: fmt.Appendf(nil, `{"errorMessages":["Payouts secret: no such issue"]}`+"\n%d", code)}, nil
}

func (f *trackJira) Ticket(ctx context.Context, key string) (jira.Ticket, error) {
	f.mu.Lock()
	f.tickets = append(f.tickets, key)
	f.mu.Unlock()
	return f.client.Ticket(ctx, key)
}

func (f *trackJira) Search(context.Context, string, int) ([]jira.Ticket, error) {
	return nil, errors.New("no search in the tracking tests")
}

func (f *trackJira) StatusSince(ctx context.Context, key string, prior jira.IssueStatus) (jira.IssueStatus, error) {
	f.mu.Lock()
	f.asked = append(f.asked, key)
	f.priors = append(f.priors, prior)
	_, refused := f.refused[key]
	fails, partial, noAccount := f.down != 0 || refused, f.partial[key], f.noAccount[key]
	s, ok := f.statuses[key]
	block, started := f.block, f.started
	f.mu.Unlock()
	if block != nil {
		if started != nil {
			started <- key
		}
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.cut = append(f.cut, ctx.Err())
			f.mu.Unlock()
			return jira.IssueStatus{}, ctx.Err()
		}
	}
	if fails {
		return f.client.Status(ctx, key)
	}
	if !ok {
		s = jira.IssueStatus{Key: key, Name: "To Do", Category: "new", StatusID: "1", Since: t0.Add(-72 * time.Hour), Searched: true}
	}
	if partial {
		_, err := f.client.Status(ctx, "PR-1") // a 404, as a changelog page's
		s.Since, s.Searched = time.Time{}, false
		return s, &jira.PartError{Part: jira.PartChangelog, Err: err}
	}
	if noAccount && s.Assigned {
		_, err := f.client.Status(ctx, "PR-1") // a 404, as myself's
		s.AssigneeYou = false
		return s, &jira.PartError{Part: jira.PartAccount, Err: err}
	}
	return s, nil
}

// set makes key's status s.
func (f *trackJira) set(key string, s jira.IssueStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[key] = s
}

// edit runs fn on the fake under its lock.
func (f *trackJira) edit(fn func(f *trackJira)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// reads are the keys StatusSince was asked for, in order, and forget
// forgets them.
func (f *trackJira) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

func (f *trackJira) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = nil
}

// trackRig is a harness whose Jira is the fake, whose stages do nothing
// and whose log the test reads.
type trackRig struct {
	*harness
	jira *trackJira
	log  *syncBuffer
}

func newTrackRig(t *testing.T, edit ...func(d *Deps)) *trackRig {
	t.Helper()
	f, log := newTrackJira(t), &syncBuffer{}
	h := newHarness(t, append([]func(d *Deps){func(d *Deps) {
		d.Jira, d.Log = f, slog.New(slog.NewTextHandler(log, nil))
	}}, edit...)...)
	for _, typ := range []string{config.FactoryStageAgents, config.FactoryStageGate, config.FactoryStageReview, config.FactoryStageDecision,
		config.FactoryStageEffect, config.FactoryStageQA} {
		h.stage(typ, idleStage)
	}
	return &trackRig{harness: h, jira: f, log: log}
}

// pass is a tick whose tracking batch has ended and been taken, as the
// next pass of the loop takes it: what the tick read is in the registry.
func (h *harness) pass() {
	h.t.Helper()
	h.tick()
	h.e.waitBackground()
	h.e.takeTracking(h.ctx)
}

// tracking is task id's tracking row; ok is false before its first try.
func (h *harness) tracking(id int64) (registry.Tracking, bool) {
	h.t.Helper()
	tr, err := h.reg.Tracking(h.ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return registry.Tracking{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return tr, true
}

// inReview is an issue in code review since a day before t0, assigned to
// you.
func inReview(key string) jira.IssueStatus {
	return jira.IssueStatus{Key: key, Name: "In Review", Category: "indeterminate", StatusID: "10002", Since: t0.Add(-24 * time.Hour),
		Searched: true, Assigned: true, AssigneeYou: true}
}

// A task's issue is read on the first pass, then again only once 10
// minutes passed since the last read, and each read replaces the row's
// facts: the status's name, id and category, since when and whether it was
// searched, the assignee. The next read hands Jira the last one, so the
// same status needs no changelog.
func TestATasksJiraStatusIsReadAgainAfterTenMinutesAndNotBefore(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.pass()
	got, ok := r.tracking(task.ID)
	want := registry.Tracking{TaskID: task.ID, JiraKey: "PR-12360", Status: "In Review", StatusID: "10002", Category: "indeterminate",
		Since: t0.Add(-24 * time.Hour), Searched: true, Assigned: true, AssigneeYou: true, ReadAt: t0, TriedAt: t0}
	if !ok || got != want {
		t.Fatalf("after the first pass: %+v (row %v)\nwant %+v", got, ok, want)
	}
	r.jira.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Done", Category: "done", StatusID: "4", Since: t0.Add(5 * time.Minute), Searched: true})
	r.clock.Add(trackEvery - time.Second)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("reads %q within 10 minutes, want the first alone", reads)
	}
	r.clock.Add(time.Second)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Fatalf("reads %q at 10 minutes, want a second", reads)
	}
	got, _ = r.tracking(task.ID)
	if got.Status != "Done" || got.StatusID != "4" || got.Category != "done" || got.Assigned || got.AssigneeYou || !got.ReadAt.Equal(r.clock.Now()) ||
		!got.Since.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("after the second read: %+v", got)
	}
	r.jira.edit(func(f *trackJira) {
		if p := f.priors; p[0] != (jira.IssueStatus{}) || p[1].StatusID != "10002" || !p[1].Searched || !p[1].Since.Equal(t0.Add(-24*time.Hour)) {
			t.Errorf("the reads' priors %+v, want none, then the first read", p)
		}
	})
}

// The reads run off the loop: a tick hands one batch the due reads and
// returns, the batch's reads reach the registry only on the next pass, and
// a tick while a batch runs starts no other.
func TestTrackingReadsAreAppliedOnTheNextPass(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.tick()
	r.e.waitBackground()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("reads %q, want the batch's one", reads)
	}
	if _, ok := r.tracking(task.ID); ok {
		t.Fatal("the batch wrote the registry: only the loop's next pass does")
	}
	r.clock.Add(45 * time.Second)
	r.tick()
	if got, ok := r.tracking(task.ID); !ok || got.Status != "To Do" || !got.ReadAt.Equal(t0) {
		t.Fatalf("the next pass: %+v (row %v), want the read's facts", got, ok)
	}
}

// A Jira that hangs never holds a tick: the tick that hands the batch its
// reads returns while the read waits, and so do the ticks after it, which
// start no second batch; once the read answers, the next pass takes it.
func TestAHungJiraNeverDelaysATick(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	block, started := make(chan struct{}), make(chan string, 4)
	r.jira.edit(func(f *trackJira) { f.block, f.started = block, started })
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		for range 3 {
			_ = r.e.Tick(r.ctx)
			r.clock.Add(45 * time.Second)
		}
	}()
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("the ticks wait for a Jira that hangs")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no read started")
	}
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Errorf("reads %q while one hangs, want that one: no second batch", reads)
	}
	close(block)
	r.e.waitBackground()
	r.tick()
	if _, ok := r.tracking(task.ID); !ok {
		t.Error("the read that answered late was not taken")
	}
}

// A batch whose reads Jira does not answer (curl's timeout, or a read that
// hangs past its bound) holds the next batch back 1, 2, 4, 8 and at most
// 15 minutes; a batch Jira answers ends the hold. The site's failure is
// logged once while its cause stays.
func TestABatchJiraDoesNotAnswerHoldsTheNextOneBack(t *testing.T) {
	r := newTrackRig(t)
	for i := range 30 {
		r.task(fmt.Sprintf("PR-%d", 12400+i), registry.TaskNeedsYou, "investigate")
	}
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	var batchesAt []time.Duration
	for range 70 { // 52 minutes and a half
		before := len(r.jira.reads())
		r.pass()
		if len(r.jira.reads()) > before {
			batchesAt = append(batchesAt, r.clock.Now().Sub(t0))
		}
		r.clock.Add(45 * time.Second)
	}
	want := []time.Duration{0, 90 * time.Second, 225 * time.Second, 495 * time.Second, 990 * time.Second, 1890 * time.Second, 2790 * time.Second}
	if !slices.Equal(batchesAt, want) {
		t.Errorf("batches at %v, want held 1, 2, 4, 8, 15, 15 minutes after each (to the next tick): %v", batchesAt, want)
	}
	if n := strings.Count(r.log.String(), "Jira did not answer within 20s"); n != 1 {
		t.Errorf("Jira down logged %d times, want once:\n%s", n, r.log.String())
	}
	r.jira.edit(func(f *trackJira) { f.down = 0 })
	for len(batchesAt) < 9 {
		before := len(r.jira.reads())
		r.pass()
		if len(r.jira.reads()) > before {
			batchesAt = append(batchesAt, r.clock.Now().Sub(t0))
		}
		r.clock.Add(45 * time.Second)
	}
	if gap := batchesAt[8] - batchesAt[7]; gap != 45*time.Second {
		t.Errorf("after a batch Jira answered the next came %v later, want the next tick", gap)
	}
	r.jira.edit(func(f *trackJira) { f.down = 28 }) // down again: the hold starts over at 1 minute
	for len(batchesAt) < 11 {
		before := len(r.jira.reads())
		r.pass()
		if len(r.jira.reads()) > before {
			batchesAt = append(batchesAt, r.clock.Now().Sub(t0))
		}
		r.clock.Add(45 * time.Second)
	}
	if gap := batchesAt[10] - batchesAt[9]; gap != 90*time.Second {
		t.Errorf("down again after a batch Jira answered: the next batch came %v later, want the 1-minute hold (to the next tick)", gap)
	}

	hung := newTrackRig(t)
	hung.e.trackReadBound, hung.e.trackBatchBound = 20*time.Millisecond, 200*time.Millisecond
	hung.task("PR-12360", registry.TaskNeedsYou, "investigate")
	hung.task("PR-12361", registry.TaskNeedsYou, "investigate")
	hung.jira.edit(func(f *trackJira) { f.block = make(chan struct{}) })
	hung.pass()
	if reads := hung.jira.reads(); len(reads) != 1 {
		t.Errorf("a read that hung past its bound: reads %q, want it alone", reads)
	}
	if got, _ := hung.tracking(1); got.LastError != "Jira did not answer within 20ms" || !got.ReadAt.IsZero() {
		t.Errorf("a read that hung past its bound: %+v", got)
	}
	hung.clock.Add(45 * time.Second)
	hung.pass()
	if reads := hung.jira.reads(); len(reads) != 1 {
		t.Errorf("the batch after a hung one came within its hold: reads %q", reads)
	}
}

// The drain cuts a batch in flight: the daemon's stop cancels its read and
// waits for it within the drain's wait, and the cut read records nothing.
func TestTheDrainCutsATrackingBatch(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	started := make(chan string, 1)
	r.jira.edit(func(f *trackJira) { f.block, f.started = make(chan struct{}), started })
	r.tick()
	<-started
	stopped := make(chan struct{})
	go func() {
		r.e.stopBackground()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain waited on the batch past its cut")
	}
	r.jira.edit(func(f *trackJira) {
		if len(f.cut) != 1 || !errors.Is(f.cut[0], context.Canceled) {
			t.Errorf("the read's context ended with %v, want the drain's cancel", f.cut)
		}
	})
	r.e.takeTracking(r.ctx)
	if _, ok := r.tracking(task.ID); ok {
		t.Error("the cut read was recorded")
	}
}

// A task of no Jira issue (your words), one whose key is another tracker's
// (a Linear key is never read from Jira) and one that waits for Jira at its
// start (its intake tries Jira) are never read; with [jira] off nothing is.
func TestOnlyATasksJiraIssueIsTracked(t *testing.T) {
	r := newTrackRig(t)
	r.cfg.Board.Trackers = []string{"https://linear.app/example/issue/EX-{num}"}
	words, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "fix the digest", Kind: "bug", Repo: "example/shop", Title: "fix", State: registry.TaskNeedsYou})
	if err != nil {
		t.Fatal(err)
	}
	r.task("EX-12", registry.TaskNeedsYou, "investigate")
	waiting, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12370", Kind: "bug", Repo: "example/shop", Title: "PR-12370",
		State: registry.TaskWaiting, WaitsWho: waitJira, WaitsWhat: jiraWaitWhat})
	if err != nil {
		t.Fatal(err)
	}
	r.e.jiraTries[waiting.ID] = &jiraTry{next: t0.Add(time.Hour)} // its next intake try is not due
	for range 3 {
		r.pass()
		r.clock.Add(trackEvery)
	}
	if reads := r.jira.reads(); len(reads) != 0 {
		t.Errorf("reads %q, want none", reads)
	}
	if _, ok := r.tracking(words.ID); ok {
		t.Error("a task of your words has a tracking row")
	}
	off := newTrackRig(t, func(d *Deps) { d.Jira = nil })
	task := off.task("PR-12360", registry.TaskNeedsYou, "investigate")
	off.pass()
	if _, ok := off.tracking(task.ID); ok {
		t.Error("[jira] off: a tracking row was written")
	}
}

// A batch keeps within the tick's Jira budget, which the intake's tries
// spend first: of 50 tasks a batch reads 3, those never read first, oldest
// first; an ended task's owed last read is one of the 3; a read Jira did
// not answer ends the batch; a tick whose intake try met a Jira that does
// not answer hands no batch, and one whose try Jira refused hands one of
// what is left. The reads of a few tasks are paced across the ticks, so
// they come due apart: 3 tasks read one a tick, and again one a tick 10
// minutes later.
func TestTheJiraReadsKeepWithinTheTicksBudget(t *testing.T) {
	r := newTrackRig(t)
	var keys []string
	for i := range 50 {
		key := fmt.Sprintf("PR-%d", 12400+i)
		r.task(key, registry.TaskNeedsYou, "investigate")
		keys = append(keys, key)
	}
	r.pass()
	if reads := r.jira.reads(); !slices.Equal(reads, keys[:jiraTriesPerTick]) {
		t.Fatalf("first batch read %q, want the 3 oldest", reads)
	}
	r.jira.forget()
	r.clock.Add(45 * time.Second)
	r.pass()
	if reads := r.jira.reads(); !slices.Equal(reads, keys[3:6]) {
		t.Fatalf("second batch read %q, want the next 3", reads)
	}

	stopped, err := r.reg.TaskByKey(r.ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, stopped.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.jira.forget()
	r.clock.Add(lastReadAfter)
	r.pass()
	if reads := r.jira.reads(); !slices.Equal(reads, []string{keys[0], keys[6], keys[7]}) {
		t.Fatalf("a batch with an ended task's last read read %q, want it first and 2 more", reads)
	}
	r.jira.forget()
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.clock.Add(45 * time.Second)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("a batch whose read Jira did not answer read %q, want that one alone", reads)
	}
	r.jira.edit(func(f *trackJira) { f.down = 0 })
	r.clock.Add(trackBackoffFirst)
	r.pass() // a batch Jira answers ends the hold

	waiting, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12370", Kind: "bug", Repo: "example/shop", Title: "PR-12370",
		State: registry.TaskWaiting, WaitsWho: waitJira, WaitsWhat: jiraWaitWhat})
	if err != nil {
		t.Fatal(err)
	}
	r.jira.forget()
	r.jira.edit(func(f *trackJira) { f.ticketCode = 503 }) // the intake's try: Jira does not answer
	r.clock.Add(45 * time.Second)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 0 {
		t.Fatalf("a tick whose intake try Jira did not answer read %q, want none", reads)
	}
	r.jira.edit(func(f *trackJira) { f.ticketCode = 404 }) // the try Jira refuses: the task needs you
	r.e.jiraTries[waiting.ID].next = r.clock.Now()
	r.clock.Add(45 * time.Second)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Fatalf("after a refused intake try the batch read %q, want the 2 reads left", reads)
	}

	few := newTrackRig(t)
	for _, key := range []string{"PR-12360", "PR-12361", "PR-12362"} {
		few.task(key, registry.TaskNeedsYou, "investigate")
	}
	var perTick []int
	for range 3 {
		before := len(few.jira.reads())
		few.pass()
		perTick = append(perTick, len(few.jira.reads())-before)
		few.clock.Add(45 * time.Second)
	}
	few.clock.Add(trackEvery - 3*45*time.Second)
	few.pass()
	if reads := few.jira.reads(); !slices.Equal(perTick, []int{1, 1, 1}) || !slices.Equal(reads, []string{"PR-12360", "PR-12361", "PR-12362", "PR-12360"}) {
		t.Errorf("3 tasks read %v a tick, %q in all; want one a tick, and the first again 10 minutes after its read", perTick, reads)
	}
}

// A task that ended (done, stopped, failed) owes one last read, 2 minutes
// after its end (Jira's own automation moves a merged issue a while
// after), tried again while it fails, up to 5 minutes after the end, and
// none after, a restart of the daemon included. A task that ended before
// any read owes it when it had a PR; one with neither gets none.
func TestAnEndedTaskGetsOneLastJiraRead(t *testing.T) {
	r := newTrackRig(t)
	old, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12361", Kind: "bug", Repo: "example/shop", Title: "old", State: registry.TaskDone})
	if err != nil {
		t.Fatal(err)
	}
	unread, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12362", Kind: "bug", Repo: "example/shop", Title: "unread",
		State: registry.TaskDone, PRNumber: 4562})
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Add(lastReadBound) // both ended 5 minutes ago
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.pass()
	r.clock.Add(time.Minute)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.jira.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Won't do", Category: "done", Since: r.clock.Now(), Searched: true})
	r.clock.Add(time.Minute)
	r.pass()
	if reads := r.jira.reads(); !slices.Equal(reads, []string{"PR-12360"}) {
		t.Fatalf("reads %q within 2 minutes of the end, want the one at work alone", reads)
	}
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.clock.Add(time.Minute) // 2 minutes after the end: the last read, which fails
	r.pass()
	r.jira.edit(func(f *trackJira) { f.down = 0 })
	r.clock.Add(trackBackoffFirst)
	r.pass() // tried again
	if reads := r.jira.reads(); !slices.Equal(reads, []string{"PR-12360", "PR-12360", "PR-12360"}) {
		t.Fatalf("reads %q, want one at work, a last that failed and its retry", reads)
	}
	if got, _ := r.tracking(task.ID); got.Status != "Won't do" || !got.ReadAt.Equal(r.clock.Now()) {
		t.Errorf("the last read: %+v", got)
	}
	for range 3 {
		r.clock.Add(trackEvery)
		r.pass()
	}
	r.restart()
	r.clock.Add(trackEvery)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 3 {
		t.Errorf("reads %q after the last one, want none more", reads)
	}
	if _, ok := r.tracking(old.ID); ok {
		t.Error("a task that ended with no read and no PR was read")
	}
	if _, ok := r.tracking(unread.ID); ok {
		t.Error("a task with a PR that ended over 5 minutes before its first read was read")
	}

	fresh := newTrackRig(t)
	merged, err := fresh.reg.CreateTask(fresh.ctx, registry.Task{JiraKey: "PR-12363", Kind: "bug", Repo: "example/shop", Title: "merged",
		State: registry.TaskDone, PRNumber: 4563})
	if err != nil {
		t.Fatal(err)
	}
	fresh.clock.Add(lastReadAfter)
	fresh.pass()
	if got, ok := fresh.tracking(merged.ID); !ok || got.Status != "To Do" {
		t.Errorf("a task with a PR that ended before its first read: %+v (row %v), want its last read", got, ok)
	}
}

// A failed read never fails the tick or the task: it is recorded on the
// row (the facts of the read before stay) in the jira package's words,
// which carry no ticket text, and logged once while its cause stays; Jira
// down is one cause for every task and ends the batch, an issue Jira
// refuses is its task's own, and a read whose changelog failed keeps the
// status it read, its cause its task's own too. A read that works clears
// the cause, and the next failure is logged again.
func TestAFailedJiraReadIsLoggedOnceAndFailsNothing(t *testing.T) {
	r := newTrackRig(t)
	one := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	two := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.pass()
	r.clock.Add(45 * time.Second)
	r.pass()
	gone := r.task("PR-12362", registry.TaskNeedsYou, "investigate")
	r.jira.edit(func(f *trackJira) { f.refused["PR-12362"] = 404 })
	r.clock.Add(45 * time.Second)
	r.pass()
	if got, ok := r.tracking(gone.ID); !ok || got.JiraKey != "PR-12362" || !got.ReadAt.IsZero() || !got.TriedAt.Equal(r.clock.Now()) ||
		got.LastError != "Jira has no such issue, or the account cannot see it (HTTP 404)" || got.Status != "" {
		t.Fatalf("a first read Jira refused: %+v (row %v), want the try and its cause, no facts", got, ok)
	}
	for range 3 { // one, two, then gone again, refused again
		r.clock.Add(trackEvery)
		r.pass()
	}
	if got, _ := r.tracking(gone.ID); !got.TriedAt.After(t0.Add(90 * time.Second)) {
		t.Fatalf("PR-12362 was not read again: %+v", got)
	}
	if n := strings.Count(r.log.String(), "HTTP 404"); n != 1 {
		t.Errorf("a refusal that stays logged %d times, want once:\n%s", n, r.log.String())
	}
	before, _ := r.tracking(one.ID)
	twoBefore, _ := r.tracking(two.ID)
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	for range 4 {
		r.clock.Add(trackEvery)
		if err := r.e.Tick(r.ctx); err != nil {
			t.Fatalf("a tick with Jira down: %v, want no error", err)
		}
		r.e.waitBackground()
	}
	r.e.takeTracking(r.ctx)
	got, _ := r.tracking(one.ID)
	if got.Status != "In Review" || !got.ReadAt.Equal(before.ReadAt) || got.LastError != "Jira did not answer within 20s" || !got.TriedAt.After(before.TriedAt) {
		t.Errorf("a failed read's row: %+v; want the facts kept and the try recorded", got)
	}
	if task := r.reload(one.ID); task.State != registry.TaskNeedsYou || task.WaitsWho != one.WaitsWho {
		t.Errorf("the task moved: %+v", task)
	}
	if n := strings.Count(r.log.String(), "Jira did not answer within 20s"); n != 1 {
		t.Errorf("Jira down logged %d times, want once:\n%s", n, r.log.String())
	}
	r.jira.edit(func(f *trackJira) { f.down, f.refused["PR-12361"] = 0, 404 })
	for range 6 {
		r.clock.Add(trackEvery)
		r.pass()
	}
	if got, _ := r.tracking(two.ID); got.LastError != "Jira has no such issue, or the account cannot see it (HTTP 404)" || !got.ReadAt.Equal(twoBefore.ReadAt) {
		t.Errorf("a refused read's row: %+v", got)
	}
	if got, _ := r.tracking(one.ID); got.LastError != "" || !got.ReadAt.After(before.ReadAt) {
		t.Errorf("a read that works again: %+v", got)
	}
	if n := strings.Count(r.log.String(), "HTTP 404"); n != 2 {
		t.Errorf("the refusals logged %d times, want once for each task:\n%s", n, r.log.String())
	}
	r.jira.edit(func(f *trackJira) { f.partial["PR-12360"] = true })
	for range 3 {
		r.clock.Add(trackEvery)
		r.pass()
	}
	if got, _ := r.tracking(one.ID); got.Status != "In Review" || !got.Since.IsZero() || got.Searched || !got.ReadAt.Equal(got.TriedAt) ||
		!strings.Contains(got.LastError, "not for its changelog") {
		t.Errorf("a read whose changelog failed: %+v, want its status kept, Since unknown, its cause", got)
	}
	if n := strings.Count(r.log.String(), "not for its changelog"); n != 1 {
		t.Errorf("the changelog's failure logged %d times, want once for its task", n)
	}
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.clock.Add(trackEvery)
	r.pass()
	if n := strings.Count(r.log.String(), "Jira did not answer within 20s"); n != 2 {
		t.Errorf("Jira down again after a read that worked: logged %d times in all, want twice", n)
	}
	if strings.Contains(r.log.String(), "Payouts") {
		t.Errorf("the log carries ticket text:\n%s", r.log.String())
	}
}

// The screen's row of a task and its task card carry its issue's status as
// the last read that worked says it (name, category, since when, the
// assignee, when it was read and tried, the last try's failure); a task
// whose reads all failed carries the failure alone, with no name; none
// before a try, nor for a row of the key the task had before.
func TestTheScreenCarriesATasksJiraStatus(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	unread := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	never := r.task("PR-12362", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.jira.edit(func(f *trackJira) { f.refused["PR-12361"] = 404 })
	r.pass()
	r.clock.Add(45 * time.Second)
	r.pass()
	r.clock.Add(time.Minute)
	if err := r.reg.TransitionTask(r.ctx, never.ID, nil, registry.TaskStopped, nil); err != nil { // never read: off home and its row
		t.Fatal(err)
	}
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	want := &tui.FactoryJiraStatus{Name: "In Review", Category: "indeterminate", Since: t0.Add(-24 * time.Hour), Assigned: true, AssignedToYou: true,
		ReadAt: t0, TriedAt: t0}
	failed := &tui.FactoryJiraStatus{TriedAt: t0.Add(45 * time.Second), LastError: "Jira has no such issue, or the account cannot see it (HTTP 404)"}
	for _, row := range d.Rows {
		switch row.ID {
		case task.ID:
			if row.JiraStatus == nil || *row.JiraStatus != *want {
				t.Errorf("the row's Jira status: %+v, want %+v", row.JiraStatus, want)
			}
			if c := d.Cards[task.ID].Task.JiraStatus; c == nil || *c != *want {
				t.Errorf("the task card's Jira status: %+v, want %+v", c, want)
			}
		case unread.ID:
			if row.JiraStatus == nil || *row.JiraStatus != *failed {
				t.Errorf("a task whose reads failed: %+v, want %+v", row.JiraStatus, failed)
			}
		}
	}
	if err := r.reg.SetTracking(r.ctx, registry.Tracking{TaskID: task.ID, JiraKey: "PR-12399", Status: "Done", Category: "done", ReadAt: t0, TriedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if d, err = ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout}); err != nil {
		t.Fatal(err)
	}
	for _, row := range d.Rows {
		if row.ID == task.ID && row.JiraStatus != nil {
			t.Errorf("a row of another key: %+v, want none", row.JiraStatus)
		}
	}
}

// An issue assigned while the read of your account failed reaches the
// screen as assigned, not known to be you; one whose changelog failed keeps
// what the read of the account said (someone else's). Before, both read as
// someone else's.
func TestTheScreenTellsAnAssigneeNotKnownToBeYou(t *testing.T) {
	r := newTrackRig(t)
	mine := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	theirs := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	other := inReview("PR-12361")
	other.AssigneeYou = false
	r.jira.set("PR-12361", other)
	r.jira.edit(func(f *trackJira) { f.noAccount["PR-12360"], f.partial["PR-12361"] = true, true })
	r.pass()
	r.clock.Add(45 * time.Second)
	r.pass()
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range d.Rows {
		js := row.JiraStatus
		switch row.ID {
		case mine.ID:
			if js == nil || !js.Assigned || js.AssignedToYou || !js.YouUnknown ||
				!strings.HasPrefix(js.LastError, "Jira answered for the issue but not for its account: ") {
				t.Errorf("an issue whose account read failed: %+v, want assigned, not known to be you", js)
			}
			if c := d.Cards[mine.ID].Task.JiraStatus; c == nil || *c != *js {
				t.Errorf("its card's Jira status %+v, want the row's %+v", c, js)
			}
		case theirs.ID:
			if js == nil || !js.Assigned || js.AssignedToYou || js.YouUnknown ||
				!strings.HasPrefix(js.LastError, "Jira answered for the issue but not for its changelog: ") {
				t.Errorf("an issue whose changelog read failed: %+v, want someone else's", js)
			}
		}
	}
}

// While no factory daemon runs, every task's Jira status on the screen
// says so, on its row and its card, as its staging cells say unknown: its
// facts are as old as their read. Before, it read as a fresh one.
func TestTheScreenSaysNoDaemonReadsTheJiraStatuses(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.pass()
	for _, down := range []bool{false, true} {
		d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout, DaemonDown: down})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range d.Rows {
			if row.ID != task.ID {
				continue
			}
			if js := row.JiraStatus; js == nil || js.Name != "In Review" || js.DaemonDown != down {
				t.Errorf("the daemon down %v: the row's Jira status %+v", down, js)
			}
			if c := d.Cards[task.ID].Task.JiraStatus; c == nil || c.DaemonDown != down {
				t.Errorf("the daemon down %v: the card's Jira status %+v", down, c)
			}
		}
	}
}

// The PR's status file carries the task's Jira key and its status in the
// file's words, mapped from the last read (no Jira at all before a try, so
// the first edit of the comment says a status), and is written again only
// when that word changes: a new read of the same status writes nothing.
func TestThePRStatusFileCarriesTheIssuesJiraStatus(t *testing.T) {
	f := newTrackJira(t)
	r := newFollowRig(t, func(d *Deps) { d.Jira = f })
	r.published("jira")
	f.set("PR-12360", inReview("PR-12360"))
	r.pass()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{}) {
		t.Fatalf("before its first try: %+v, want no Jira", got)
	}
	r.clock.Add(45 * time.Second)
	r.pass()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraInCodeReview}) {
		t.Fatalf("after a read of In Review: %+v", got)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(r.statusPath(), old, old); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(trackEvery)
	r.pass()
	r.clock.Add(45 * time.Second)
	r.pass()
	if st, err := os.Stat(r.statusPath()); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("a read of the same status rewrote the file (%v, %v)", st, err)
	}
}

// An ended task's last status write waits for its owed last read, and
// carries it: the PR merged, its file says it published while the read is
// owed, then done with the read's status; the batch that read it kicks a
// tick, so the write need not wait a poll. A last read Jira never answers
// holds the write 5 minutes after the end at most, then it goes out with
// what the row has.
func TestAnEndedTasksLastStatusWriteWaitsForItsLastRead(t *testing.T) {
	f := newTrackJira(t)
	r := newFollowRig(t, func(d *Deps) { d.Jira = f })
	r.published("last")
	f.set("PR-12360", inReview("PR-12360"))
	r.pass()
	f.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Done", Category: "done", StatusID: "4", Since: r.clock.Now(), Searched: true})
	r.prIs(store.GHMerged, r.clock.Now())
	r.clock.Add(45 * time.Second)
	r.pass() // the PR merged: the task is done
	for range 2 {
		r.clock.Add(45 * time.Second)
		r.pass()
		if got := r.mustStatus().Now.Stage; got != prstatus.StagePublished {
			t.Fatalf("the last write went out before the last read: %s", got)
		}
	}
	r.clock.Add(45 * time.Second) // 2 minutes and 15 seconds after the end: the last read
	select {
	case <-r.e.kick:
	default:
	}
	r.pass()
	select {
	case <-r.e.kick:
	default:
		t.Error("the batch that read the last read kicked no tick: the last write would wait a poll")
	}
	r.clock.Add(45 * time.Second)
	r.pass()
	if got := r.mustStatus(); got.Jira != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraDone}) || got.Now.Stage != prstatus.StageDone {
		t.Errorf("the ended task's last write: %+v, %+v; want done with the last read", got.Jira, got.Now)
	}

	g := newTrackJira(t)
	down := newFollowRig(t, func(d *Deps) { d.Jira = g })
	down.published("down")
	g.set("PR-12360", inReview("PR-12360"))
	down.pass()
	g.edit(func(f *trackJira) { f.down = 28 })
	down.prIs(store.GHMerged, down.clock.Now())
	down.clock.Add(45 * time.Second)
	down.pass()
	end := down.clock.Now()
	for down.clock.Now().Before(end.Add(lastReadBound)) {
		if got := down.mustStatus().Now.Stage; got != prstatus.StagePublished {
			t.Fatalf("%v after the end the last write went out: %s", down.clock.Now().Sub(end), got)
		}
		down.clock.Add(45 * time.Second)
		down.pass()
	}
	if got := down.mustStatus(); got.Now.Stage != prstatus.StageDone || got.Jira != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraInCodeReview}) {
		t.Errorf("past the bound: %+v, %+v; want done with the row's status", got.Jira, got.Now)
	}
}

// With [jira] off a task's PR says no Jira issue, whatever an earlier read
// left, and so does a task whose
// key is another tracker's; a task whose reads have all failed says its key
// with its status unknown.
func TestAPRStatusFileSaysNoJiraWithoutAReadAndUnknownAfterFailedOnes(t *testing.T) {
	r := newFollowRig(t)
	r.published("off")
	if err := r.reg.SetTracking(r.ctx, registry.Tracking{TaskID: r.task.ID, JiraKey: "PR-12360", Status: "In Review", Category: "indeterminate",
		ReadAt: t0, TriedAt: t0}); err != nil { // a read from before [jira] was switched off
		t.Fatal(err)
	}
	r.tick()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{}) {
		t.Errorf("[jira] off: %+v, want no Jira issue", got)
	}
	f := newTrackJira(t)
	failing := newFollowRig(t, func(d *Deps) { d.Jira = f })
	failing.published("failing")
	f.edit(func(f *trackJira) { f.refused["PR-12360"] = 404 })
	failing.pass()
	failing.clock.Add(45 * time.Second)
	failing.pass()
	if got := failing.mustStatus().Jira; got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraUnknown}) {
		t.Errorf("reads that failed: %+v, want the key, unknown", got)
	}
	failing.cfg.Board.Trackers = []string{"https://linear.app/example/issue/PR-{num}"}
	failing.clock.Add(time.Minute)
	failing.tick()
	if got := failing.mustStatus().Jira; got != (prstatus.Jira{}) {
		t.Errorf("a key of another tracker: %+v, want no Jira issue", got)
	}
}

// Of the tasks never read, a published one is read first: its PR's status
// file says no Jira until that read.
func TestAPublishedTasksFirstReadComesFirst(t *testing.T) {
	r := newTrackRig(t)
	r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.task("PR-12361", registry.TaskPublished, "publish")
	r.pass()
	if reads := r.jira.reads(); !slices.Equal(reads, []string{"PR-12361"}) {
		t.Errorf("reads %q, want the published task's first", reads)
	}
}

// The batch's answers never wait for good: batches whose answers find the
// place of the loop's next pass taken (started past the guard of one at a
// time, as a bug would) give up after a short wait, say so once, and drop
// them, so neither the loop nor a batch hangs, and their reads come due
// again for the next batch.
func TestABatchWhoseAnswersCannotBeTakenDropsThem(t *testing.T) {
	r := newTrackRig(t)
	r.e.trackSendWait = 20 * time.Millisecond
	one := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	two := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	three := r.task("PR-12362", registry.TaskNeedsYou, "investigate")
	r.e.startTrackBatch([]trackJob{{t: one, key: one.JiraKey}})
	r.e.waitBackground() // its answers wait for the next pass
	r.e.startTrackBatch([]trackJob{{t: two, key: two.JiraKey}})
	r.e.startTrackBatch([]trackJob{{t: three, key: three.JiraKey}})
	ended := make(chan struct{})
	go func() {
		r.e.waitBackground()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("a batch whose answers had no place hangs")
	}
	if n := strings.Count(r.log.String(), "answers were dropped"); n != 1 {
		t.Errorf("two drops logged %d times, want once:\n%s", n, r.log.String())
	}
	r.e.takeTracking(r.ctx) // the loop takes the answers that found their place
	if _, ok := r.tracking(one.ID); !ok {
		t.Fatal("the answers that found their place were not taken")
	}
	for range 2 { // the next batches read the dropped ones' tasks again, one a tick
		r.clock.Add(45 * time.Second)
		r.pass()
	}
	for _, task := range []registry.Task{two, three} {
		if _, ok := r.tracking(task.ID); !ok {
			t.Errorf("%s was not read again", task.JiraKey)
		}
	}
}
