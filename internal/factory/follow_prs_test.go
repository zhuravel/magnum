package factory

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// followRef is the PR the follow tests' task published.
var followRef = PRRef{Repo: "example/shop", Number: 4530}

// syncBuffer is a log sink the engine and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// followRig is the harness with magnum's registry (the fake) and a task
// published as example/shop#4530, whose PR magnum has open; log is what
// the engine logged.
type followRig struct {
	*harness
	magnum *fakeMagnum
	task   registry.Task
	log    *syncBuffer
}

func newFollowRig(t *testing.T, edit ...func(d *Deps)) *followRig {
	t.Helper()
	m, log := &fakeMagnum{}, &syncBuffer{}
	h := newHarness(t, append([]func(d *Deps){func(d *Deps) {
		d.Magnum, d.Log = m, slog.New(slog.NewTextHandler(log, nil))
	}}, edit...)...)
	task := h.task("PR-12360", registry.TaskActive, "publish")
	if err := h.reg.LinkPR(h.ctx, registry.PRLink{TaskID: task.ID, Repo: followRef.Repo, Number: followRef.Number, Branch: "zhuravel-PR-12360"}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.TransitionTask(h.ctx, task.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", followRef.Number) }); err != nil {
		t.Fatal(err)
	}
	r := &followRig{harness: h, magnum: m, task: h.reload(task.ID), log: log}
	r.prIs(store.GHOpen, time.Time{})
	return r
}

// prIs makes magnum's registry say the PR is in GitHub state gh, merged
// or closed at at.
func (r *followRig) prIs(gh string, at time.Time) {
	v := PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: gh, Head: prHead}
	switch gh {
	case store.GHMerged:
		v.MergedAt, v.ClosedAt = &at, &at
	case store.GHClosed:
		v.ClosedAt = &at
	}
	r.magnum.setPR(followRef, &v)
}

// A merged PR makes its task done, the first writer of done: its wait
// clears, and its event says why.
func TestAMergedPRMovesItsTaskToDone(t *testing.T) {
	r := newFollowRig(t)
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskPublished {
		t.Fatalf("an open PR's task is %s, want it published", got.State)
	}
	r.prIs(store.GHMerged, r.clock.Now())
	r.tick()
	got := r.reload(r.task.ID)
	if got.State != registry.TaskDone || got.WaitsWho != "" || got.WaitsWhat != "" {
		t.Fatalf("a merged PR's task: %s, waits %q %q; want done", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if evs := r.events(r.task.ID, "pr"); len(evs) != 1 || evs[0].Phase != registry.PhaseOK || !strings.Contains(evs[0].Text, "PR #4530 merged") {
		t.Errorf("pr events %+v, want one that says the PR merged", evs)
	}
}

// A PR closed without a merge keeps its task published: within [daemon]
// close_grace nothing moves (a reopen may come), and after it the task
// waits on you, alerted once, never back in a live stage.
func TestAClosedPRNeedsYouAfterTheGrace(t *testing.T) {
	r := newFollowRig(t)
	r.prIs(store.GHClosed, r.clock.Now())
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskPublished || got.WaitsWho != "" {
		t.Fatalf("within the grace: %s, waits on %q; want published, waiting on nobody yet", got.State, got.WaitsWho)
	}
	r.clock.Add(r.cfg.Daemon.CloseGrace.Duration + time.Minute)
	r.tick()
	r.tick()
	got := r.reload(r.task.ID)
	if got.State != registry.TaskPublished || got.WaitsWho != waitsYou || got.WaitsWhat != "PR #4530 closed without a merge: reopen it, or x drops the task" {
		t.Fatalf("past the grace: %s, waits %q %q; want published, waiting on you", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if evs := r.events(r.task.ID, "alert"); len(evs) != 1 || !strings.Contains(evs[0].Text, "PR-12360: you: PR #4530 closed without a merge") {
		t.Errorf("alerts %+v, want one that says the PR closed", evs)
	}
	if retryRefusal(r.ctx, r.reg, r.layout, got) == nil {
		t.Error("a retry would move the published task back into a live stage")
	}
}

// A PR reopened after its close cleared its task's wait on you: the task
// waits on magnum's verdict again.
func TestAReopenedPRWaitsOnMagnumAgain(t *testing.T) {
	r := newFollowRig(t)
	r.prIs(store.GHClosed, r.clock.Now().Add(-time.Hour))
	r.tick()
	if got := r.reload(r.task.ID); got.WaitsWho != waitsYou {
		t.Fatalf("closed an hour ago: waits on %q, want you", got.WaitsWho)
	}
	r.prIs(store.GHOpen, time.Time{})
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskPublished || got.WaitsWho != "" || got.WaitsWhat != "" {
		t.Fatalf("reopened: %s, waits %q %q; want published, waiting on nobody", got.State, got.WaitsWho, got.WaitsWhat)
	}
	if evs := r.events(r.task.ID, "pr"); len(evs) != 1 || !strings.Contains(evs[0].Text, "PR #4530 reopened") {
		t.Errorf("pr events %+v, want the reopen", evs)
	}
}

// A registry that cannot be read, a PR magnum does not have and magnum's
// daemon down move no task, whatever the PR did: unknown is never a merge
// or a close. The failed read is logged once while it stays the same.
func TestAnUnreadableMagnumRegistryMovesNoTask(t *testing.T) {
	r := newFollowRig(t)
	r.prIs(store.GHMerged, r.clock.Now())
	r.magnum.setPRErr(errors.New("magnum's registry is at schema 40 and this build needs 41"))
	r.tick()
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskPublished {
		t.Fatalf("an unreadable registry moved the task to %s", got.State)
	}
	if n := strings.Count(r.log.String(), "schema 40"); n != 1 {
		t.Errorf("the failed read is logged %d times over 2 ticks, want once:\n%s", n, r.log.String())
	}
	r.magnum.setPRErr(nil)
	merged := PRState{Found: true, DaemonUp: false, Watched: true, GHState: store.GHMerged}
	r.magnum.setPR(followRef, &merged)
	r.tick()
	r.magnum.setPR(followRef, nil)
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskPublished {
		t.Fatalf("magnum's daemon down or the PR not found moved the task to %s", got.State)
	}
}

// The follow needs neither herdr nor MySQL: with herdr down the merge
// still makes the task done, and the park of its agents waits for a tick
// with herdr up.
func TestFollowPRsRunsWhileHerdrIsDown(t *testing.T) {
	var down sync.Mutex
	herdrDown := true
	r := newFollowRig(t, func(d *Deps) {
		d.HerdrUp = func(context.Context) error {
			down.Lock()
			defer down.Unlock()
			if herdrDown {
				return errors.New("herdr: no socket")
			}
			return nil
		}
	})
	r.prIs(store.GHMerged, r.clock.Now())
	r.tick()
	if got := r.reload(r.task.ID); got.State != registry.TaskDone {
		t.Fatalf("with herdr down the merged PR's task is %s, want done", got.State)
	}
	if parked := r.rt.parked(); len(parked) != 0 {
		t.Fatalf("parks %v while herdr is down", parked)
	}
	down.Lock()
	herdrDown = false
	down.Unlock()
	r.tick()
	r.tick()
	if parked := r.rt.parked(); !slices.Equal(parked, []int64{r.task.ID}) {
		t.Errorf("parks %v once herdr is up, want the done task's, once", parked)
	}
}

// Done frees the task's Jira key: a new task may take it, which a
// published task forbids.
func TestADoneTaskFreesItsJiraKey(t *testing.T) {
	r := newFollowRig(t)
	again := registry.Task{JiraKey: r.task.JiraKey, Kind: "bug", Repo: "example/shop", Title: "again", State: registry.TaskQueued}
	if _, err := r.reg.CreateTask(r.ctx, again); !errors.As(err, new(registry.ErrActiveKey)) {
		t.Fatalf("a second task of a published task's key: %v, want the key held", err)
	}
	r.prIs(store.GHMerged, r.clock.Now())
	r.tick()
	if _, err := r.reg.CreateTask(r.ctx, again); err != nil {
		t.Fatalf("a task of a done task's key: %v, want it made", err)
	}
}

// A merged task gets the end a stopped one gets: its agents parked, its
// open attempts failed and its copy released; your input is not recorded,
// as you did not stop it.
func TestAMergedTasksAgentsAreParkedAndItsCopyReleased(t *testing.T) {
	r := newFollowRig(t)
	a, err := r.reg.BeginAttempt(r.ctx, r.task.ID, "publish")
	if err != nil {
		t.Fatal(err)
	}
	r.prIs(store.GHMerged, r.clock.Now())
	r.tick()
	if parked := r.rt.parked(); !slices.Equal(parked, []int64{r.task.ID}) {
		t.Errorf("parks %v, want the merged task's", parked)
	}
	if got := r.copies.releases(); !slices.Contains(got, r.task.ID) {
		t.Errorf("releases %v, want the merged task's copy", got)
	}
	as, err := r.reg.Attempts(r.ctx, r.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(as, func(x registry.Attempt) bool { return x.ID == a.ID }); i < 0 || as[i].State != registry.AttemptFailed {
		t.Errorf("attempts %+v, want %s failed", as, a.ID)
	}
	ins, err := r.reg.Inputs(r.ctx, r.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 0 {
		t.Errorf("inputs %+v, want none: nobody stopped the task", ins)
	}
}

// A stop shares the merge's end (closeTask): with herdr down its park waits
// for a tick with herdr up, rather than failing against herdr.
func TestAStopWhileHerdrIsDownParksOnceHerdrIsUp(t *testing.T) {
	var mu sync.Mutex
	herdrDown := true
	h := newHarness(t, func(d *Deps) {
		d.HerdrUp = func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if herdrDown {
				return errors.New("herdr: no socket")
			}
			return nil
		}
	})
	task := h.task("PR-12361", registry.TaskNeedsYou, "report")
	h.tick()
	if _, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: task.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.reload(task.ID); got.State != registry.TaskStopped {
		t.Fatalf("the task is %s after its stop, want stopped", got.State)
	}
	if parked := h.rt.parked(); len(parked) != 0 {
		t.Fatalf("parks %v while herdr is down", parked)
	}
	mu.Lock()
	herdrDown = false
	mu.Unlock()
	h.tick()
	if parked := h.rt.parked(); !slices.Equal(parked, []int64{task.ID}) {
		t.Errorf("parks %v once herdr is up, want the stopped task's", parked)
	}
}

// ciIs makes magnum's registry say the open PR is at head with its CI in
// state (stale: of an older commit than the head).
func (r *followRig) ciIs(head, state string, stale bool) {
	v := PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: head,
		CI: &tui.CIInfo{State: state, Stale: stale}}
	r.magnum.setPR(followRef, &v)
}

// The PR's CI after the publish goes into the task's checks, kind ci from
// source github, once per head and result: a failure, then a pass of the
// same head after a rerun, both; a second tick writes no duplicate, and a
// pending head, CI of an older commit than the head and a PR without CI
// write nothing. No row has a cause: nothing says a failure is not ours.
func TestThePRsCIAfterThePublishIsCopiedOncePerHeadAndResult(t *testing.T) {
	r := newFollowRig(t)
	next, older := "d00d"+strings.Repeat("1", 36), "beef"+strings.Repeat("2", 36)
	for _, step := range []struct {
		head, state string
		stale       bool
	}{
		{prHead, "failed", false},
		{prHead, "failed", false},
		{prHead, "pending", false},
		{prHead, "passed", false},
		{prHead, "passed", false},
		{older, "failed", true}, // CI of a commit before this head
		{next, "pending", false},
		{next, "none", false},
		{next, "skipped", false},
		{next, "failed", false},
		{prHead, "failed", false}, // the older head again: its row stands
	} {
		r.ciIs(step.head, step.state, step.stale)
		r.tick()
	}
	r.prIs(store.GHOpen, time.Time{}) // magnum read no CI
	r.tick()
	cs, err := r.reg.Checks(r.ctx, r.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		if c.Kind != registry.CheckCI || c.Source != "github" || c.Cause != "" || c.AttemptID != "" {
			t.Errorf("check %+v, want a CI check of GitHub's with no cause", c)
		}
		got = append(got, c.HeadSHA[:4]+" "+c.Result)
	}
	if want := []string{"a1b2 fail", "a1b2 pass", "d00d fail"}; !slices.Equal(got, want) {
		t.Errorf("CI checks %q, want %q", got, want)
	}
}
