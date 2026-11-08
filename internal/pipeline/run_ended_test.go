package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// endSignal is the observe pass's run-ended broadcast as agents.Manager.
// Ended hands it out: a channel closed at the next end, then a new one.
type endSignal struct {
	mu    sync.Mutex
	ch    chan struct{}
	taken int // Ended calls
}

func (s *endSignal) Ended() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken++
	if s.ch == nil {
		s.ch = make(chan struct{})
	}
	return s.ch
}

func (s *endSignal) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
}

func (s *endSignal) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taken
}

// submittedJudgeRun records a judge run in flight on the judge's session.
func (e *env) submittedJudgeRun() store.Run {
	e.t.Helper()
	sess := e.sess[agents.RoleJudge]
	r, err := e.st.CreateRun(e.ctx, store.Run{PRID: e.pr.ID, Round: 1, Role: string(agents.RoleJudge), Kind: store.RunInitial,
		TargetSHA: target, Identity: "talkable-app", State: store.RunSubmitted, SessionID: &sess.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// endRun ends run r as the observe pass does.
func (e *env) endRun(r store.Run) {
	e.t.Helper()
	if err := e.st.TransitionRun(context.WithoutCancel(e.ctx), r.ID, []string{store.RunSubmitted, store.RunWorking}, store.RunEnded,
		func(u *store.RunUpdate) { u.Set("ended_at", e.clock.Now()) }); err != nil {
		e.t.Errorf("end run %s: %v", r.ID, err)
	}
}

// pollNeverEnds makes the runner's poll sleep last until its context ends:
// a waiter that returns did not wait for its poll. naps receives each
// sleep's duration as it begins.
func (e *env) pollNeverEnds() chan time.Duration {
	naps := make(chan time.Duration, 16)
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		naps <- d
		<-ctx.Done()
		return ctx.Err()
	}
	return naps
}

// waitAsync runs the round's wait for run r on its own goroutine.
func (e *env) waitAsync(ctx context.Context, r store.Run) <-chan turn {
	rd := &round{r: e.r, in: e.input(KindInitial)}
	done := make(chan turn, 1)
	go func() { done <- rd.wait(ctx, r.ID, 0, "", nil) }()
	return done
}

// failGuard bounds a wait that only a missed wake would make long.
const failGuard = 20 * time.Second

// The observe pass that ends a run wakes its waiter at once: the next
// stage starts within a second of the end, not up to a poll (10 s) later.
func TestAWaiterReturnsOnTheRunEndWithoutWaitingForItsPoll(t *testing.T) {
	e := newEnv(t)
	sig := &endSignal{}
	e.r.Ended = sig.Ended
	naps := e.pollNeverEnds()
	r := e.submittedJudgeRun()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := e.waitAsync(ctx, r)

	select {
	case d := <-naps:
		if d != DefaultPollInterval {
			t.Fatalf("the waiter slept %s, want its poll %s", d, DefaultPollInterval)
		}
	case <-time.After(failGuard):
		t.Fatal("the waiter never began its poll")
	}
	e.endRun(r)
	sig.wake()
	select {
	case got := <-done:
		if got.kind != waitEnded || got.run.State != store.RunEnded {
			t.Fatalf("wait = kind %d, run %+v; want ended", got.kind, got.run)
		}
	case <-time.After(failGuard):
		t.Fatal("the run ended and the waiter still slept its poll")
	}
}

// An end that comes after the waiter read its run as at work, but before
// it began to sleep, still wakes it: it took the channel before the read
// (a waiter that took it as it began to sleep would get a new one, which
// nothing closes, and sleep its whole poll).
func TestAnEndBeforeTheWaiterSleepsIsNotLost(t *testing.T) {
	e := newEnv(t)
	sig := &endSignal{}
	e.r.Ended = sig.Ended
	e.pollNeverEnds()
	r := e.submittedJudgeRun()
	var once sync.Once
	clock := e.r.Clock
	e.r.Clock = func() time.Time {
		// The wait reads the clock after it read the run's row: once it
		// took its channel, the end comes there.
		if sig.calls() > 0 {
			once.Do(func() {
				e.endRun(r)
				sig.wake()
			})
		}
		return clock()
	}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	select {
	case got := <-e.waitAsync(ctx, r):
		if got.kind != waitEnded {
			t.Fatalf("wait = kind %d, run %+v; want ended", got.kind, got.run)
		}
	case <-time.After(failGuard):
		t.Fatal("an end before the waiter slept was lost: it sleeps its whole poll")
	}
}

// A wake that ends no run of the waiter's (another run ended) has it look
// again and sleep on: it returns only on its own run's end.
func TestAnotherRunsEndHasTheWaiterLookAgain(t *testing.T) {
	e := newEnv(t)
	sig := &endSignal{}
	e.r.Ended = sig.Ended
	naps := e.pollNeverEnds()
	r := e.submittedJudgeRun()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := e.waitAsync(ctx, r)
	<-naps
	sig.wake() // another run's end
	select {
	case <-naps:
	case got := <-done:
		t.Fatalf("the waiter returned on another run's end: %+v", got)
	case <-time.After(failGuard):
		t.Fatal("the waiter did not look again after the wake")
	}
	e.endRun(r)
	sig.wake()
	select {
	case got := <-done:
		if got.kind != waitEnded {
			t.Fatalf("wait = kind %d; want ended", got.kind)
		}
	case <-time.After(failGuard):
		t.Fatal("the waiter missed its run's end")
	}
}
