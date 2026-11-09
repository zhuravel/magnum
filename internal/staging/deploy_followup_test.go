package staging

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink the tests read while clients write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// logged gives the clients made after it a log the test reads.
func (w *deployWorld) logged() *syncBuffer {
	b := &syncBuffer{}
	w.deps.Log = slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return b
}

// seqEvent is an event with the journal write it came from.
type seqEvent struct {
	id               EffectID
	seq              int
	step, phase, txt string
}

// Minor 8. Every event carries the journal write it records (Effect.Seq):
// a second client that delivers the outbox again (its Cancel loads the
// journal the first one wrote) repeats the keys, each with the same event,
// so the registry can drop the repeats by (effect id, Seq, step, phase);
// distinct events never share a key.
func TestEveryEventCarriesItsJournalWritesSeq(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	var mu sync.Mutex
	var got []seqEvent
	w.deps.Event = func(e Effect, step, phase, text string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, seqEvent{e.ID, e.Seq, step, phase, text})
	}
	a := w.client()
	defer a.Close()
	id := w.deploy(a, 1)
	w.drive(a, id, at(StepVerify))
	b := w.client()
	defer b.Close()
	if err := b.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	texts := map[seqEvent]string{}
	repeats := 0
	for _, ev := range got {
		if ev.seq <= 0 {
			t.Errorf("an event without its write: %+v", ev)
		}
		k := seqEvent{id: ev.id, seq: ev.seq, step: ev.step, phase: ev.phase}
		if text, ok := texts[k]; ok {
			repeats++
			if text != ev.txt {
				t.Errorf("key %+v delivered as %q and as %q: two events share it", k, text, ev.txt)
			}
		}
		texts[k] = ev.txt
	}
	if repeats == 0 {
		t.Errorf("no repeat after b loaded a's journal: %+v", got)
	}
}

// Minor 2. A journal whose merge is no sha fails at once, at verify or at
// push: no retry, nothing pushed.
func TestAMergeThatIsNoShaFailsAtOnce(t *testing.T) {
	t.Parallel()
	for _, step := range []string{StepVerify, StepPush} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 1)
			w.drive(c, id, at(step))
			w.editJournal(id, func(j *journal) { j.Merge = "" })
			if err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			e := w.effect(c, id)
			if e.State != EffectFailed || e.Step != step || !strings.Contains(e.Why, "no sha") || !w.journalOf(id).RetryAt.IsZero() {
				t.Errorf("after one Step: %+v, want failed at %s, no sha, no retry", e, step)
			}
			w.noPushSince(0, w.tip)
		})
	}
}

// Minor 3. A FailingSince in the future (an edit, a clock stepped back) is
// taken as now, once logged, so the budget still ends the effect.
func TestAFailingSinceInTheFutureIsTakenAsNow(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	log := w.logged()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepVerify))
	w.run.failing("fetch")
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.editJournal(id, func(j *journal) { j.FailingSince = w.deps.Now().Add(365 * 24 * time.Hour) })
	start := w.deps.Now()
	for range 12 {
		if ended(w.effect(c, id)) {
			break
		}
		if j := w.journalOf(id); j.RetryAt.After(w.deps.Now()) {
			w.advance(j.RetryAt.Sub(w.deps.Now()))
		}
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	e := w.effect(c, id)
	if e.State != EffectFailed || w.deps.Now().Sub(start) > failBudget+10*time.Minute {
		t.Errorf("after %s: %+v, want failed within the budget", w.deps.Now().Sub(start), e)
	}
	if n := log.count("in the future"); n != 1 {
		t.Errorf("log lines about the future: %d, want 1", n)
	}
}

// Minor 4. A live journal whose env name fails the id rule is passed over
// by the scan with one log line per file, never an error at each tick, and
// nothing is pushed; Cancel still ends it by id.
func TestAJournalWithAHostileEnvNameIsPassedOver(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../../evil", "a\x00b", ".deploys"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			log := w.logged()
			c := w.client()
			defer c.Close()
			root, err := openDeploys(w.mirror, true)
			if err != nil {
				t.Fatal(err)
			}
			now := w.deps.Now()
			id := newID("void", 7, now)
			j := &journal{V: journalVersion, ID: id, Repo: "acme/app", Env: Env{Name: name, Branch: "staging-void"},
				PR: PR{Repo: "acme/app", Number: 7, Head: w.heads[1]}, By: ByCLI, Step: StepClaim, State: EffectQueued,
				PlanTip: w.tip, Started: now, Updated: now}
			if err := writeJournal(root, j); err != nil {
				t.Fatal(err)
			}
			root.Close()
			for range 3 {
				if err := c.Step(context.Background()); err != nil {
					t.Errorf("Step: %v, want the journal passed over", err)
				}
			}
			if n := log.count("passed over"); n != 1 {
				t.Errorf("log lines: %d, want 1 for the file", n)
			}
			if e := w.effect(c, id); e.State != EffectQueued {
				t.Errorf("the journal: %+v, want it left queued", e)
			}
			if err := c.Cancel(context.Background(), id); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if e := w.effect(c, id); e.State != EffectCancelled {
				t.Errorf("after Cancel: %+v, want cancelled", e)
			}
			w.noPushSince(0, w.tip)
		})
	}
}

// Minor 5. No Deps.Event call runs while the deploys dir's flock or an
// effect's flock is held (a slow registry write would hold up the other
// daemon's claims and cancels): each event is delivered after the unlock.
func TestNoEventIsDeliveredUnderTheDirOrEffectFlock(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	inner := w.deps.Event
	w.deps.Event = func(e Effect, step, phase, text string) {
		root, err := openDeploys(w.mirror, false)
		if err != nil {
			t.Errorf("the deploys dir: %v", err)
			return
		}
		defer root.Close()
		for _, name := range []string{dirLockName, effectLockName(e.ID)} {
			f, ok, err := flockIn(context.Background(), root, name, false)
			if err != nil || !ok {
				t.Errorf("%s %s %q delivered under %s", step, phase, text, name)
				continue
			}
			unflock(f)
		}
		inner(e, step, phase, text)
	}
	c := w.client()
	defer c.Close()
	if e := w.finish(c, w.deploy(c, 1)); e.State != EffectDone {
		t.Fatalf("clean: %+v", e)
	}
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

// Minor 6. A resolver's merge the mirror has already (an earlier try's
// hand-off) is written again and held at refs/magnum/resolved/<id>, so a
// prune before the push cannot take it.
func TestAResolvedMergeTheMirrorHoldsAlreadyIsHeldByItsRef(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
	dir, merge := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "staging and theirs\n"})
	// The mirror holds the merge already, on no ref.
	gitIn(t, w.r, w.mirror, "fetch", "-q", dir, merge+":refs/probe/x")
	gitIn(t, w.r, w.mirror, "update-ref", "-d", "refs/probe/x")
	res.answer(ResolveResult{State: ResolveDone, MergeSHA: merge, Dir: dir})
	e := w.drive(c, id, at(StepVerify))
	if e.State != EffectRunning || e.Merge != merge {
		t.Fatalf("after the resolver: %+v, want at verify with its merge", e)
	}
	if got := gitIn(t, w.r, w.mirror, "for-each-ref", "--format=%(objectname)", resolvedRef(id)); got != merge {
		t.Errorf("%s points at %q, want the merge %s", resolvedRef(id), got, short(merge))
	}
	gitIn(t, w.r, w.mirror, "prune", "--expire=now")
	if e := w.finish(c, id); e.State != EffectDone || e.Merge != merge {
		t.Errorf("after a prune: %+v, want done with the resolver's merge", e)
	}
}

// Minor 7. A wait for the mirror's flock (a Refresh's fetch, a clone) is
// no failure of the step: past failBudget of waiting the effect still
// runs, with no failure counted, and it goes on once the flock is free.
func TestAWaitForTheMirrorsLockIsNoFailure(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	c.lockWait = 20 * time.Millisecond
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepVerify))
	unlock, err := lockMirror(context.Background(), w.mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := w.deps.Now()
	for w.deps.Now().Sub(start) <= failBudget+time.Minute {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		j := w.journalOf(id)
		if j.State != EffectRunning || j.Fails != 0 || !strings.Contains(j.Why, "mirror's lock") {
			t.Fatalf("waiting for the lock: %s fails %d %q, want running, no failure, the lock named", j.State, j.Fails, j.Why)
		}
		w.advance(time.Minute)
	}
	unlock()
	if e := w.finish(c, id); e.State != EffectDone {
		t.Errorf("once the lock is free: %+v, want done", e)
	}
}

// The lock waits' own cap: an effect that waits on the mirror's lock for
// busyBudget in all ends, failed before the push ("the mirror stayed busy
// for 60 min", nothing pushed), done, "pushed, not confirmed ... the push
// stands", after it; the env is released either way, with one fail event.
func TestLockWaitsEndTheEffectAfterAnHour(t *testing.T) {
	t.Parallel()
	for _, step := range []string{StepVerify, StepConfirm} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			c := w.client()
			defer c.Close()
			c.lockWait = 20 * time.Millisecond
			id := w.deploy(c, 1)
			e := w.drive(c, id, func(e Effect) bool {
				if e.Step == StepWatch {
					w.passCI(e.Merge)
				}
				return e.Step == step || ended(e)
			})
			if step == StepConfirm {
				w.deployed(e.Merge) // confirm needs the mirror to tell it holds the merge
			}
			unlock, err := lockMirror(context.Background(), w.mirror, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			events := w.eventCount()
			start := w.deps.Now()
			for !ended(w.effect(c, id)) && w.deps.Now().Sub(start) < 2*busyBudget {
				if w.deps.Now().Sub(start) < busyBudget-time.Minute {
					if e := w.effect(c, id); e.State != EffectRunning {
						t.Fatalf("after %s of lock waits: %+v, want still running", w.deps.Now().Sub(start), e)
					}
				}
				if err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				w.advance(time.Minute)
			}
			e = w.effect(c, id)
			if waited := w.deps.Now().Sub(start); waited < busyBudget || waited > busyBudget+2*time.Minute {
				t.Errorf("ended after %s of lock waits, want about %s", waited, busyBudget)
			}
			if !strings.Contains(e.Why, "the mirror stayed busy for 60 min") {
				t.Errorf("ended: %+v, want the mirror stayed busy for 60 min", e)
			}
			switch step {
			case StepVerify:
				if e.State != EffectFailed || !strings.Contains(e.Why, "nothing was pushed") {
					t.Errorf("before the push: %+v, want failed, nothing pushed", e)
				}
				w.noPushSince(0, w.tip)
			case StepConfirm:
				if e.State != EffectDone || !strings.HasPrefix(e.Why, NotConfirmed) || !strings.Contains(e.Why, "the push stands") {
					t.Errorf("after the push: %+v, want done, pushed, not confirmed, the push stands", e)
				}
			}
			if _, held := c.heldFor(w.mirror, "void"); held {
				t.Error("the env is still held")
			}
			if got := w.failEvents(events); got != 1 {
				t.Errorf("fail events: %d, want the end's alone", got)
			}
		})
	}
}
