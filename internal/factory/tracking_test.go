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

// trackJira is the factory's Jira in the tracking tests: Status answers
// each key's preset status (To Do, new, for a key with none) and records
// the keys, in order; while down is set every read fails as curl's exit
// code down does, and a key refused fails with its HTTP status, each
// through a real jira.Client over a fake curl, so the errors are the jira
// package's own. Ticket fails as down or ticketCode says (an intake try).
type trackJira struct {
	mu         sync.Mutex
	statuses   map[string]jira.IssueStatus
	refused    map[string]int
	down       int
	ticketCode int
	asked      []string
	tickets    []string
	client     *jira.Client
}

func newTrackJira(t *testing.T) *trackJira {
	t.Helper()
	f := &trackJira{statuses: map[string]jira.IssueStatus{}, refused: map[string]int{}}
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

func (f *trackJira) Status(ctx context.Context, key string) (jira.IssueStatus, error) {
	f.mu.Lock()
	f.asked = append(f.asked, key)
	_, refused := f.refused[key]
	fails := f.down != 0 || refused
	s, ok := f.statuses[key]
	f.mu.Unlock()
	if fails {
		return f.client.Status(ctx, key)
	}
	if !ok {
		s = jira.IssueStatus{Key: key, Name: "To Do", Category: "new", Since: t0.Add(-72 * time.Hour)}
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

// reads are the keys Status was asked for, in order, and forget forgets
// them.
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
	return jira.IssueStatus{Key: key, Name: "In Review", Category: "indeterminate", Since: t0.Add(-24 * time.Hour), Assigned: true, AssigneeYou: true}
}

// A task's issue is read on the first tick, then again only once 10
// minutes passed since the last read, and each read replaces the row's
// facts: the status's name and category, since when, the assignee.
func TestATasksJiraStatusIsReadAgainAfterTenMinutesAndNotBefore(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.tick()
	got, ok := r.tracking(task.ID)
	want := registry.Tracking{TaskID: task.ID, JiraKey: "PR-12360", Status: "In Review", Category: "indeterminate",
		Since: t0.Add(-24 * time.Hour), Assigned: true, AssigneeYou: true, ReadAt: t0, TriedAt: t0}
	if !ok || got != want {
		t.Fatalf("after the first tick: %+v (row %v)\nwant %+v", got, ok, want)
	}
	r.jira.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Done", Category: "done", Since: t0.Add(5 * time.Minute)})
	r.clock.Add(trackEvery - time.Second)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("reads %q within 10 minutes, want the first alone", reads)
	}
	r.clock.Add(time.Second)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Fatalf("reads %q at 10 minutes, want a second", reads)
	}
	got, _ = r.tracking(task.ID)
	if got.Status != "Done" || got.Category != "done" || got.Assigned || got.AssigneeYou || !got.ReadAt.Equal(r.clock.Now()) ||
		!got.Since.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("after the second read: %+v", got)
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
		r.tick()
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
	off.tick()
	if _, ok := off.tracking(task.ID); ok {
		t.Error("[jira] off: a tracking row was written")
	}
}

// The reads keep within the tick's Jira budget, which the intake's tries
// spend first: of 50 tasks a tick reads 3, those never read first, oldest
// first; an ended task's last read is one of the 3; a read Jira did not
// answer ends the tick's reads; a tick whose intake try met a Jira that
// does not answer reads none, and one whose try Jira refused reads what is
// left. The reads of a
// few tasks are paced across the ticks, so they come due apart: 3 tasks
// read one a tick, and again one a tick 10 minutes later.
func TestTheJiraReadsKeepWithinTheTicksBudget(t *testing.T) {
	r := newTrackRig(t)
	var keys []string
	for i := range 50 {
		key := fmt.Sprintf("PR-%d", 12400+i)
		r.task(key, registry.TaskNeedsYou, "investigate")
		keys = append(keys, key)
	}
	r.tick()
	if reads := r.jira.reads(); !slices.Equal(reads, keys[:jiraTriesPerTick]) {
		t.Fatalf("first tick read %q, want the 3 oldest", reads)
	}
	r.jira.forget()
	r.clock.Add(45 * time.Second)
	r.tick()
	if reads := r.jira.reads(); !slices.Equal(reads, keys[3:6]) {
		t.Fatalf("second tick read %q, want the next 3", reads)
	}

	r.jira.forget()
	stopped, err := r.reg.TaskByKey(r.ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, stopped.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(45 * time.Second)
	r.tick()
	if reads := r.jira.reads(); !slices.Equal(reads, []string{keys[0], keys[6], keys[7]}) {
		t.Fatalf("a tick with an ended task's last read read %q, want it and 2 more", reads)
	}
	r.jira.forget()
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.clock.Add(45 * time.Second)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("a tick whose read Jira did not answer read %q, want that one alone", reads)
	}
	r.jira.edit(func(f *trackJira) { f.down = 0 })

	waiting, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12370", Kind: "bug", Repo: "example/shop", Title: "PR-12370",
		State: registry.TaskWaiting, WaitsWho: waitJira, WaitsWhat: jiraWaitWhat})
	if err != nil {
		t.Fatal(err)
	}
	r.jira.forget()
	r.jira.edit(func(f *trackJira) { f.ticketCode = 503 }) // the intake's try: Jira does not answer
	r.clock.Add(45 * time.Second)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 0 {
		t.Fatalf("a tick whose intake try Jira did not answer read %q, want none", reads)
	}
	r.jira.edit(func(f *trackJira) { f.ticketCode = 404 }) // the try Jira refuses: the task needs you
	r.e.jiraTries[waiting.ID].next = r.clock.Now()
	r.clock.Add(45 * time.Second)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Fatalf("after a refused intake try the tick read %q, want the 2 reads left", reads)
	}

	few := newTrackRig(t)
	for _, key := range []string{"PR-12360", "PR-12361", "PR-12362"} {
		few.task(key, registry.TaskNeedsYou, "investigate")
	}
	var perTick []int
	for range 3 {
		before := len(few.jira.reads())
		few.tick()
		perTick = append(perTick, len(few.jira.reads())-before)
		few.clock.Add(45 * time.Second)
	}
	few.clock.Add(trackEvery - 3*45*time.Second)
	few.tick()
	if reads := few.jira.reads(); !slices.Equal(perTick, []int{1, 1, 1}) || !slices.Equal(reads, []string{"PR-12360", "PR-12361", "PR-12362", "PR-12360"}) {
		t.Errorf("3 tasks read %v a tick, %q in all; want one a tick, and the first again 10 minutes after its read", perTick, reads)
	}
}

// A task that ended (done, stopped, failed) gets one last read after its
// end, before its PR's last status write, and none after, a restart of the
// daemon included; one that ended before any read of it gets none.
func TestAnEndedTaskGetsOneLastJiraRead(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	r.tick()
	old, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12361", Kind: "bug", Repo: "example/shop", Title: "old", State: registry.TaskDone})
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Add(time.Minute)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.jira.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Won't do", Category: "done", Since: r.clock.Now()})
	r.clock.Add(time.Minute)
	r.tick()
	if reads := r.jira.reads(); !slices.Equal(reads, []string{"PR-12360", "PR-12360"}) {
		t.Fatalf("reads %q, want one at work and one last after the end", reads)
	}
	if got, _ := r.tracking(task.ID); got.Status != "Won't do" || !got.ReadAt.Equal(r.clock.Now()) {
		t.Errorf("the last read: %+v", got)
	}
	for range 3 {
		r.clock.Add(trackEvery)
		r.tick()
	}
	r.restart()
	r.clock.Add(trackEvery)
	r.tick()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Errorf("reads %q after the last one, want none more", reads)
	}
	if _, ok := r.tracking(old.ID); ok {
		t.Error("a task that ended before any read was read")
	}
}

// A failed read never fails the tick or the task: it is recorded on the
// row (the facts of the read before stay) in the jira package's words,
// which carry no ticket text, and logged once while its cause stays; Jira
// down is one cause for every task and stops the tick's reads, an issue
// Jira refuses is its task's own. A read that works clears the cause, and
// the next failure is logged again.
func TestAFailedJiraReadIsLoggedOnceAndFailsNothing(t *testing.T) {
	r := newTrackRig(t)
	one := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	two := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.tick()
	r.clock.Add(45 * time.Second)
	r.tick()
	gone := r.task("PR-12362", registry.TaskNeedsYou, "investigate")
	r.jira.edit(func(f *trackJira) { f.refused["PR-12362"] = 404 })
	r.clock.Add(45 * time.Second)
	r.tick()
	if got, ok := r.tracking(gone.ID); !ok || got.JiraKey != "PR-12362" || !got.ReadAt.IsZero() || !got.TriedAt.Equal(r.clock.Now()) ||
		got.LastError != "Jira has no such issue, or the account cannot see it (HTTP 404)" || got.Status != "" {
		t.Fatalf("a first read Jira refused: %+v (row %v), want the try and its cause, no facts", got, ok)
	}
	if err := r.reg.TransitionTask(r.ctx, gone.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(45 * time.Second)
	r.tick() // its last read, refused again
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	for range 4 {
		r.clock.Add(trackEvery)
		if err := r.e.Tick(r.ctx); err != nil {
			t.Fatalf("a tick with Jira down: %v, want no error", err)
		}
	}
	got, _ := r.tracking(one.ID)
	if got.Status != "In Review" || !got.ReadAt.Equal(t0) || got.LastError != "Jira did not answer within 20s" || !got.TriedAt.After(t0) {
		t.Errorf("a failed read's row: %+v; want the facts kept and the try recorded", got)
	}
	if task := r.reload(one.ID); task.State != registry.TaskNeedsYou || task.WaitsWho != one.WaitsWho {
		t.Errorf("the task moved: %+v", task)
	}
	if n := strings.Count(r.log.String(), "Jira did not answer within 20s"); n != 1 {
		t.Errorf("Jira down logged %d times, want once:\n%s", n, r.log.String())
	}
	r.jira.edit(func(f *trackJira) { f.down, f.refused["PR-12361"] = 0, 404 })
	for range 5 { // one tick, then two, then one, then two, then one
		r.clock.Add(trackEvery)
		r.tick()
	}
	if got, _ := r.tracking(two.ID); got.LastError != "Jira has no such issue, or the account cannot see it (HTTP 404)" || !got.ReadAt.Equal(t0.Add(45*time.Second)) {
		t.Errorf("a refused read's row: %+v", got)
	}
	if got, _ := r.tracking(one.ID); got.LastError != "" || !got.ReadAt.Equal(r.clock.Now()) {
		t.Errorf("a read that works again: %+v", got)
	}
	if n := strings.Count(r.log.String(), "HTTP 404"); n != 2 {
		t.Errorf("the refusals logged %d times, want once for each task:\n%s", n, r.log.String())
	}
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.clock.Add(trackEvery)
	r.tick()
	if n := strings.Count(r.log.String(), "Jira did not answer within 20s"); n != 2 {
		t.Errorf("Jira down again after a read that worked: logged %d times in all, want twice", n)
	}
	if strings.Contains(r.log.String(), "Payouts") {
		t.Errorf("the log carries ticket text:\n%s", r.log.String())
	}
}

// The screen's row of a task and its task card carry its issue's status as
// the last read that worked says it (name, category, since when, the
// assignee, when it was read and the last try's failure): none before a
// read worked, nor for a row of the key the task had before.
func TestTheScreenCarriesATasksJiraStatus(t *testing.T) {
	r := newTrackRig(t)
	task := r.task("PR-12360", registry.TaskNeedsYou, "investigate")
	unread := r.task("PR-12361", registry.TaskNeedsYou, "investigate")
	r.jira.set("PR-12360", inReview("PR-12360"))
	r.jira.edit(func(f *trackJira) { f.refused["PR-12361"] = 404 })
	r.tick()
	r.clock.Add(45 * time.Second)
	r.tick()
	r.clock.Add(time.Minute)
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	want := &tui.FactoryJiraStatus{Name: "In Review", Category: "indeterminate", Since: t0.Add(-24 * time.Hour), Assigned: true, AssignedToYou: true,
		ReadAt: t0}
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
			if row.JiraStatus != nil {
				t.Errorf("a task never read: %+v, want none", row.JiraStatus)
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

// The PR's status file carries the task's Jira key and its status in the
// file's words, mapped from the last read (unknown before one), and is
// written again only when that word changes: a new read of the same
// status writes nothing. An ended task's last write carries its last read.
func TestThePRStatusFileCarriesTheIssuesJiraStatus(t *testing.T) {
	f := newTrackJira(t)
	r := newFollowRig(t, func(d *Deps) { d.Jira = f })
	r.published("jira")
	r.tick()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraToDo}) &&
		got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraUnknown}) {
		t.Fatalf("the first tick's Jira: %+v", got)
	}
	f.set("PR-12360", inReview("PR-12360"))
	r.clock.Add(trackEvery)
	r.tick()
	r.clock.Add(45 * time.Second)
	r.tick()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraInCodeReview}) {
		t.Fatalf("after a read of In Review: %+v", got)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(r.statusPath(), old, old); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(trackEvery)
	r.tick()
	r.clock.Add(45 * time.Second)
	r.tick()
	if st, err := os.Stat(r.statusPath()); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("a read of the same status rewrote the file (%v, %v)", st, err)
	}
	f.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "Done", Category: "done", Since: r.clock.Now()})
	r.prIs(store.GHMerged, r.clock.Now())
	r.clock.Add(time.Minute)
	r.tick() // the PR merged: the task is done
	r.clock.Add(time.Minute)
	r.tick() // its last read, then its last write
	if got := r.mustStatus(); got.Jira != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraDone}) || got.Now.Stage != prstatus.StageDone {
		t.Errorf("the ended task's last write: %+v, %+v", got.Jira, got.Now)
	}
}

// Without a read of its issue the file says the task's key with its status
// unknown; a task whose key is another tracker's says no Jira issue.
func TestAPRStatusFileWithoutAReadSaysTheKeyUnknown(t *testing.T) {
	r := newFollowRig(t)
	r.published("unknown")
	r.tick()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{Key: "PR-12360", Status: prstatus.JiraUnknown}) {
		t.Errorf("no read: %+v, want the key, unknown", got)
	}
	r.cfg.Board.Trackers = []string{"https://linear.app/example/issue/PR-{num}"}
	r.clock.Add(time.Minute)
	r.tick()
	if got := r.mustStatus().Jira; got != (prstatus.Jira{}) {
		t.Errorf("a key of another tracker: %+v, want no Jira issue", got)
	}
}
