package staging

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyed is an event as the registry keys it, with who asked and its text.
type keyed struct {
	id          EffectID
	seq         int
	step, phase string
	by, text    string
}

// recordKeyed makes the world's Event record keyed events.
func (w *deployWorld) recordKeyed() func() []keyed {
	var mu sync.Mutex
	var got []keyed
	w.deps.Event = func(e Effect, step, phase, text string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, keyed{e.ID, e.Seq, step, phase, e.By, text})
	}
	return func() []keyed {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// Important 1 (the review's R1). A process killed after a run's second
// journal write, before it delivered either write's events: the next client
// delivers both from the journal's outbox, each under its own write's Seq.
func TestEveryEventOfACutRunIsDeliveredFromTheOutbox(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	events := w.recordKeyed()
	a := w.client()
	id := w.deploy(a, 1)
	w.drive(a, id, at(StepVerify))
	saves := 0
	a.hook = func(hid EffectID, step, point string) error {
		if hid == id && step == StepVerify && point == "saved" {
			if saves++; saves == 2 {
				return errHook // killed right after verify's ok is written
			}
		}
		return nil
	}
	_ = a.Step(context.Background())
	kill(a)
	b := w.client()
	defer b.Close()
	w.drive(b, id, at(StepWatch))
	var verify []keyed
	for _, e := range events() {
		if e.step == StepVerify {
			verify = append(verify, e)
		}
	}
	phases := map[string]int{}
	for _, e := range verify {
		phases[e.phase] = e.seq
	}
	if phases[PhaseBegin] == 0 || phases[PhaseOK] == 0 || phases[PhaseBegin] >= phases[PhaseOK] {
		t.Errorf("verify's events: %+v, want its begin and its ok, each under its own write", verify)
	}
}

// Important 1. The outbox holds only what this client has not delivered:
// a write after a delivery drops the delivered events, so the list never
// grows past one effect's undelivered events.
func TestTheOutboxDropsWhatWasDelivered(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 1)
	for _, step := range []string{StepMerge, StepVerify, StepPush, StepWatch} {
		w.drive(c, id, at(step))
		j := w.journalOf(id)
		if len(j.Pending) > 2 {
			t.Errorf("at %s the outbox holds %d events: %+v, want at most the last write's", step, len(j.Pending), j.Pending)
		}
		for _, ev := range j.Pending {
			if ev.Seq <= 0 || ev.Seq > j.Seq {
				t.Errorf("at %s an outbox event of write %d, the journal at %d", step, ev.Seq, j.Seq)
			}
		}
	}
}

// Important 2 (the review's R2). A needs-you effect deployed again keeps
// its id and carries its Seq on: no event of the new run repeats a key of
// the first run's.
func TestAResumedNeedsYouCarriesItsSeqOn(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	events := w.recordKeyed()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepMerge))
	first := len(events())
	seq := w.journalOf(id).Seq
	w.editJournal(id, func(j *journal) { j.State, j.Why = EffectNeedsYou, "needs you" })
	again, err := c.Deploy(context.Background(), w.plan(c, 1), ByLiaison)
	if err != nil || again[0] != id {
		t.Fatalf("resume: %v %v", again, err)
	}
	if got := w.journalOf(id).Seq; got <= seq {
		t.Errorf("the resumed journal's Seq %d, want past the first run's %d", got, seq)
	}
	w.drive(c, id, at(StepMerge))
	all := events()
	type key struct {
		seq         int
		step, phase string
	}
	old := map[key]keyed{}
	for _, e := range all[:first] {
		old[key{e.seq, e.step, e.phase}] = e
	}
	for _, e := range all[first:] {
		if o, ok := old[key{e.seq, e.step, e.phase}]; ok {
			t.Errorf("key (%s, %d, %s, %s) of the first run (by %s, %q) again in the second (by %s, %q)", e.id, e.seq, e.step, e.phase, o.by, o.text, e.by, e.text)
		}
	}
}

// Minor A (the review's R3). An edited BusyFor (negative, or near the
// limit) cannot defeat the lock cap: the effect still ends about
// busyBudget into its lock waits.
func TestAnEditedBusyForCannotDefeatTheLockCap(t *testing.T) {
	t.Parallel()
	for _, busyFor := range []time.Duration{-1000 * time.Hour, 1<<63 - 1} {
		t.Run(busyFor.String(), func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			c := w.client()
			defer c.Close()
			c.lockWait = 5 * time.Millisecond
			id := w.deploy(c, 1)
			w.drive(c, id, at(StepVerify))
			w.editJournal(id, func(j *journal) { j.BusyFor = busyFor })
			unlock, err := lockMirror(context.Background(), w.mirror, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			start := w.deps.Now()
			for !ended(w.effect(c, id)) && w.deps.Now().Sub(start) < 4*time.Hour {
				if err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				w.advance(time.Minute)
			}
			e := w.effect(c, id)
			if !ended(e) || w.deps.Now().Sub(start) > busyBudget+2*time.Minute {
				t.Errorf("after %s of lock waits: %+v, want ended within about %s", w.deps.Now().Sub(start), e, busyBudget)
			}
			if _, held := c.heldFor(w.mirror, "void"); held {
				t.Error("the env is still held")
			}
		})
	}
}

// The review's R4. The lock cap at the push step: failed, nothing pushed,
// the env released, and no push on later Steps once the lock is free.
func TestTheLockCapAtThePushPushesNothingAfter(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	c.lockWait = 5 * time.Millisecond
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepPush))
	unlock, err := lockMirror(context.Background(), w.mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := w.deps.Now()
	for !ended(w.effect(c, id)) && w.deps.Now().Sub(start) < 3*time.Hour {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
	}
	unlock()
	if e := w.effect(c, id); e.State != EffectFailed || !strings.Contains(e.Why, "nothing was pushed") {
		t.Errorf("ended: %+v, want failed, nothing pushed", e)
	}
	if _, held := c.heldFor(w.mirror, "void"); held {
		t.Error("the env is still held")
	}
	for range 5 {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
	}
	w.noPushSince(0, w.tip)
}

// Minor B (the review's R5). Time with no Step (a sleeping laptop, a daemon
// down) is no lock wait: one busy try, 40 minutes with no Step, then the
// lock free, and the lock waits count the one try alone.
func TestTimeWithoutStepsIsNoLockWait(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	c.lockWait = 5 * time.Millisecond
	id := w.deploy(c, 1)
	w.drive(c, id, func(e Effect) bool {
		if e.Step == StepWatch {
			w.passCI(e.Merge)
		}
		return e.Step == StepConfirm || ended(e)
	})
	w.deployed(w.tip) // confirm reads its history under the lock
	unlock, err := lockMirror(context.Background(), w.mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.advance(time.Minute)
	if err := c.Step(context.Background()); err != nil { // a second try, a minute on
		t.Fatal(err)
	}
	unlock()
	w.advance(40 * time.Minute)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j := w.journalOf(id); j.BusyFor > busyGap+time.Second {
		t.Errorf("lock waits %s after two tries a minute apart and 40 minutes without a Step, want at most %s", j.BusyFor, busyGap)
	}
}

// Minor C. While the mirror stays busy, a try after the first waits only
// busyLockWait for the lock, not the whole lock wait: a busy repository
// holds up the other repositories' Steps once, not at every try.
func TestABusyMirrorWaitsTheWholeLockWaitOnlyOnce(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	c.lockWait, c.busyLockWait = 600*time.Millisecond, 10*time.Millisecond
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepVerify))
	unlock, err := lockMirror(context.Background(), w.mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	took := func() time.Duration {
		start := time.Now()
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
		return time.Since(start)
	}
	if first := took(); first < 500*time.Millisecond {
		t.Fatalf("the first busy try took %s, want the whole lock wait", first)
	}
	for range 3 {
		if next := took(); next > 400*time.Millisecond {
			t.Errorf("a later busy try took %s, want about busyLockWait", next)
		}
	}
	if e := w.effect(c, id); e.State != EffectRunning || !strings.Contains(e.Why, "mirror's lock") {
		t.Errorf("waiting: %+v", e)
	}
}

// Minor D (the review's R6). Each flush delivers its events in Seq order;
// across clients the arrival can interleave, and Seq orders them.
func TestEachFlushDeliversInSeqOrder(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	events := w.recordKeyed()
	a := w.client()
	id := w.deploy(a, 1)
	saves := 0
	a.hook = func(hid EffectID, step, point string) error {
		if hid == id && point == "saved" {
			if saves++; saves == 3 {
				return errHook // three writes in the outbox, none delivered
			}
		}
		return nil
	}
	for range 3 {
		_ = a.Step(context.Background())
	}
	kill(a)
	before := len(events())
	b := w.client()
	defer b.Close()
	w.drive(b, id, at(StepPush))
	type key struct {
		seq         int
		step, phase string
	}
	var seqs, firsts []int
	seen := map[key]bool{}
	for i, e := range events() {
		if i >= before {
			seqs = append(seqs, e.seq)
		}
		k := key{e.seq, e.step, e.phase}
		if !seen[k] {
			seen[k] = true
			firsts = append(firsts, e.seq)
		}
	}
	if !slices.IsSorted(seqs) {
		t.Errorf("b delivered out of Seq order: %v", seqs)
	}
	if !slices.IsSorted(firsts) {
		t.Errorf("deduped by key, the events are out of Seq order: %v", firsts)
	}
	if len(seqs) == 0 || seqs[0] > firsts[before-1] {
		t.Errorf("b delivered %v after a's %v: no event of a's cut write again", seqs, firsts[:before])
	}
}

// Important 1 of the second review (its S1). A run that ends the effect,
// killed right after the end's write and before its flush: the scan of a
// fresh client delivers the ended journal's outbox, both events, then
// clears the outbox with one write that leaves the step, the state and
// the why as they were; later Steps write the ended journal no more.
func TestAnEndingRunsEventsAreDeliveredFromItsOutbox(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	events := w.recordKeyed()
	a := w.client()
	id := w.deploy(a, 1)
	w.drive(a, id, at(StepVerify))
	w.editJournal(id, func(j *journal) { j.Merge = "" })
	saves := 0
	a.hook = func(hid EffectID, step, point string) error {
		if hid == id && step == StepVerify && point == "saved" {
			if saves++; saves == 2 {
				return errHook // killed right after the end's write
			}
		}
		return nil
	}
	_ = a.Step(context.Background())
	kill(a)
	cut := w.journalOf(id)
	if cut.State != EffectFailed || len(cut.Pending) != 2 {
		t.Fatalf("the cut: %s with %d events in the outbox, want failed with verify's two", cut.State, len(cut.Pending))
	}
	b := w.client()
	defer b.Close()
	for range 3 {
		if err := b.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
	}
	phases := map[string]bool{}
	for _, e := range events() {
		if e.step == StepVerify && e.id == id {
			phases[e.phase] = true
		}
	}
	if !phases[PhaseBegin] || !phases[PhaseFail] {
		t.Errorf("verify's events delivered: %v, want its begin and its end", phases)
	}
	after := w.journalOf(id)
	if len(after.Pending) != 0 || after.Step != cut.Step || after.State != cut.State || after.Why != cut.Why || after.Seq != cut.Seq || !after.Updated.Equal(cut.Updated) {
		t.Errorf("after the delivery: %s %s %q seq %d, %d in the outbox; want the cut's %s %s %q seq %d, the outbox empty",
			after.Step, after.State, after.Why, after.Seq, len(after.Pending), cut.Step, cut.State, cut.Why, cut.Seq)
	}
	n := len(events())
	for range 3 {
		if err := b.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.journalOf(id); len(events()) != n || !got.Updated.Equal(after.Updated) {
		t.Errorf("an ended effect with an empty outbox was delivered or written again")
	}
}

// Important 1 of the second review (its S1c). A Cancel killed in its flush
// (Event panics at its first delivery): a fresh client's scan delivers the
// cancel's event from the cancelled journal's outbox.
func TestACancelKilledInItsFlushIsDeliveredLater(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	var mu sync.Mutex
	var got []string
	panicNext := false
	w.deps.Event = func(e Effect, step, phase, text string) {
		mu.Lock()
		p := panicNext
		panicNext = false
		mu.Unlock()
		if p {
			panic("killed in the flush")
		}
		mu.Lock()
		got = append(got, step+"/"+phase+"@"+strconv.Itoa(e.Seq)+" "+e.State)
		mu.Unlock()
	}
	a := w.client()
	id := w.deploy(a, 1)
	w.drive(a, id, at(StepVerify))
	kill(a)
	b := w.client()
	mu.Lock()
	panicNext = true
	mu.Unlock()
	func() {
		defer func() { _ = recover() }()
		_ = b.Cancel(context.Background(), id)
	}()
	kill(b)
	c := w.client()
	defer c.Close()
	for range 3 {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(got, func(s string) bool {
		return strings.HasSuffix(s, " "+EffectCancelled) && strings.Contains(s, PhaseFail)
	}) {
		t.Errorf("delivered after the cut: %v, want the cancel's event", got)
	}
	if j := w.journalOf(id); len(j.Pending) != 0 {
		t.Errorf("the cancelled journal's outbox still holds %d events", len(j.Pending))
	}
}

// Minor 2 of the second review (its S5). A Cancel during a run of lock
// waits cannot drop the effect's refs while the mirror is busy; the scan
// drops them once the mirror's flock is free, so none is left for good.
func TestTheRefsOfAnEffectEndedWhileTheMirrorWasBusyAreDroppedLater(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	c.lockWait, c.busyLockWait = 300*time.Millisecond, 20*time.Millisecond
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepVerify))
	unlock, err := lockMirror(context.Background(), w.mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Step(context.Background()); err != nil { // one busy try: BusyAt set
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if j := w.journalOf(id); j.State != EffectCancelled || !j.Held {
		t.Fatalf("cancelled under a busy mirror: %s, held %v; want cancelled and its refs still held", j.State, j.Held)
	}
	unlock()
	for range 3 {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
	}
	if refs := gitIn(t, w.r, w.mirror, "for-each-ref", "--format=%(refname)", "refs/magnum/"); refs != "" {
		t.Errorf("refs left in the mirror: %s", refs)
	}
	if j := w.journalOf(id); j.Held || j.State != EffectCancelled {
		t.Errorf("after the scan: %s, held %v; want cancelled, nothing held", j.State, j.Held)
	}
}
