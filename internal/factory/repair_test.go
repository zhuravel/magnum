package factory

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

func (f *fakeCopies) RepairDue(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dueAsked++
	if len(f.due) == 0 {
		return "", nil
	}
	name := f.due[0]
	f.due = f.due[1:]
	return name, nil
}

func (f *fakeCopies) Repair(ctx context.Context, name string) error {
	f.mu.Lock()
	f.repaired = append(f.repaired, name)
	block := f.repairBlock
	var err error
	if len(f.repairErrs) > 0 {
		err, f.repairErrs = f.repairErrs[0], f.repairErrs[1:]
	}
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	f.mu.Lock()
	f.repairCtxErr = append(f.repairCtxErr, ctx.Err())
	f.mu.Unlock()
	return err
}

func (f *fakeCopies) Outages(context.Context) ([]Outage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.outages), nil
}

// repairs is what the fake copies' repairs saw so far: how often RepairDue
// was asked, the copies Repair ran, and the context's error as each ended.
func (f *fakeCopies) repairs() (asked int, repaired []string, ctxErrs []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dueAsked, slices.Clone(f.repaired), slices.Clone(f.repairCtxErr)
}

func (f *fakeCopies) setDue(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.due = names
}

func (f *fakeCopies) setOutages(o ...Outage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outages = o
}

// startedRepair waits until the fake's Repair has run n times.
func (h *harness) startedRepair(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, got, _ := h.copies.repairs(); len(got) >= n {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("Repair ran fewer than %d times", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A broken copy's backoff repair runs off the loop, one at a time: a tick
// starts the one RepairDue names, the ticks while it runs ask for no other,
// and it is named in the record of the work a restart would cut. None
// starts under the disk hold or while the daemon drains, as no claim (whose
// repair is the start's) does; the daemon's stop cancels the one in flight.
func TestABackoffRepairRunsOffTheLoopOneAtATimeAndNeverUnderTheDiskHoldOrADrain(t *testing.T) {
	var free atomic.Uint64
	free.Store(1 * gb)
	h, _ := diskHarness(t, &free)
	h.copies.setDue("factory1", "factory2")
	h.copies.repairBlock = make(chan struct{})

	h.tick()
	if asked, got, _ := h.copies.repairs(); asked != 0 || len(got) != 0 {
		t.Fatalf("under the disk hold RepairDue was asked %d times and Repair ran %v, want neither", asked, got)
	}
	free.Store(100 * gb)
	h.e.draining.Store(true)
	h.tick()
	if asked, got, _ := h.copies.repairs(); asked != 0 || len(got) != 0 {
		t.Fatalf("while the daemon drains RepairDue was asked %d times and Repair ran %v, want neither", asked, got)
	}
	h.e.draining.Store(false)

	h.tick()
	h.startedRepair(1)
	rec, ok, err := ReadWork(h.layout)
	if err != nil || !ok || !slices.ContainsFunc(rec.Work, func(w WorkItem) bool {
		return w.Kind == WorkRepair && strings.Contains(w.What, "factory1")
	}) {
		t.Fatalf("work.json %+v (%v, %v), want the repair of factory1 in it", rec, ok, err)
	}
	h.clock.Add(time.Hour)
	h.tick()
	if asked, got, _ := h.copies.repairs(); asked != 1 || !slices.Equal(got, []string{"factory1"}) {
		t.Fatalf("with a repair in flight RepairDue was asked %d times and Repair ran %v, want one of factory1", asked, got)
	}

	h.e.stopBackground()
	if _, _, errs := h.copies.repairs(); len(errs) != 1 || errs[0] == nil {
		t.Fatalf("the repair's context at its end %v, want it cancelled by the stop", errs)
	}
	if rec, _, _ := ReadWork(h.layout); slices.ContainsFunc(rec.Work, func(w WorkItem) bool { return w.Kind == WorkRepair }) {
		t.Fatalf("work.json %+v still names the repair the stop ended", rec)
	}
}

// When every copy of a repository broke with one cause (Copies.Outages),
// the factory raises one infrastructure hold of that repository, which says
// the cause once: its event, the screen's holds.json and, after 30 minutes,
// a NEEDS YOU blocker with one alert. It holds no stage (the repository's
// tasks wait for a copy anyway, and the repairs go on), and it lifts itself
// once a copy repaired.
func TestEveryCopyOfARepositoryBrokenTheSameWayRaisesOneInfraHoldThatARepairLifts(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12420", registry.TaskActive, "investigate")
	var seen calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		seen.add(t.ID, h.clock.Now())
		return false, nil
	})
	cause := "bin/worktree-setup exited 1: redis-server: command not found"
	h.copies.setOutages(Outage{Repo: "example/shop", Cause: cause})
	reason := "every copy of example/shop broke the same way: " + cause

	h.tick()
	h.tick()
	hold := h.e.holds[repoHoldKey("example/shop")]
	if hold == nil || hold.Kind != HoldInfra || hold.Repo != "example/shop" || hold.Reason != reason {
		t.Fatalf("the hold %+v, want infra of example/shop: %s", hold, reason)
	}
	if h.e.holds[HoldInfra] != nil {
		t.Fatal("the machine's infrastructure hold is on: a repository's copies hold only that repository")
	}
	if got := seen.list(); len(got) != 2 || got[0] != task.ID {
		t.Fatalf("the stage ran for %v, want twice for the task: the hold holds no stage", got)
	}
	begins := func() []registry.Event {
		var out []registry.Event
		for _, ev := range h.events(0, "hold") {
			if ev.Phase == registry.PhaseBegin {
				out = append(out, ev)
			}
		}
		return out
	}
	if b := begins(); len(b) != 1 || !strings.HasPrefix(b[0].Text, "infra: "+reason) {
		t.Fatalf("hold begin events %+v, want one that says the cause", b)
	}
	hs, err := ReadHolds(h.layout)
	if err != nil || len(hs) != 1 || hs[0].Repo != "example/shop" || hs[0].Reason != reason {
		t.Fatalf("holds.json %+v (%v), want the repository's hold", hs, err)
	}

	h.clock.Add(holdNeedsYou)
	h.tick()
	h.clock.Add(time.Minute)
	h.tick()
	if got := h.e.Blocking(); len(got) != 1 || !strings.HasPrefix(got[0], "infra: "+reason+", held since ") {
		t.Fatalf("blocking %v, want the repository's hold after 30 minutes", got)
	}
	if n := len(h.events(0, "alert")); n != 1 {
		t.Fatalf("%d alerts, want one", n)
	}

	h.copies.setOutages()
	h.tick()
	if h.e.holds[repoHoldKey("example/shop")] != nil || len(h.e.Blocking()) != 0 {
		t.Fatalf("holds %v, blocking %v once a copy repaired, want none", h.e.holds, h.e.Blocking())
	}
	if !slices.ContainsFunc(h.events(0, "hold"), func(ev registry.Event) bool {
		return ev.Phase == registry.PhaseOK && strings.HasPrefix(ev.Text, "infra: lifted after ") && strings.Contains(ev.Text, reason)
	}) {
		t.Fatalf("hold events %+v, want the lift", h.events(0, "hold"))
	}
	if hs, err := ReadHolds(h.layout); err != nil || len(hs) != 0 {
		t.Fatalf("holds.json %+v (%v) once lifted, want none", hs, err)
	}
}

// A backoff repair that keeps failing for one cause is logged once, at WARN,
// and again only when its cause changes; a repair that works is logged and
// forgets the cause.
func TestAFailedBackoffRepairLogsOncePerCause(t *testing.T) {
	log := &syncBuffer{}
	h := newHarness(t, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(log, nil)) })
	broke := func(cause string) error { return copyBroke{name: "factory1", cause: cause} }
	h.copies.setDue("factory1", "factory1", "factory1", "factory1", "factory1", "factory1", "factory1")
	h.copies.repairErrs = []error{broke("redis-server: command not found"), broke("redis-server: command not found"),
		broke("createdb: could not connect"), broke("createdb: could not connect"), broke("createdb: could not connect"), nil,
		broke("createdb: could not connect")}
	for range 8 {
		h.tick()
		h.e.waitBackground()
	}
	var warns, oks []string
	for line := range strings.SplitSeq(log.String(), "\n") {
		switch {
		case strings.Contains(line, "level=WARN") && strings.Contains(line, "repair failed"):
			warns = append(warns, line)
		case strings.Contains(line, "level=INFO") && strings.Contains(line, "is repaired"):
			oks = append(oks, line)
		}
	}
	if len(warns) != 3 || !strings.Contains(warns[0], "redis-server") || !strings.Contains(warns[1], "createdb") ||
		!strings.Contains(warns[2], "createdb") {
		t.Fatalf("WARN lines %q, want one per cause, and the cause again after the repair that worked", warns)
	}
	if len(oks) != 1 {
		t.Fatalf("INFO lines of a repair %q, want one", oks)
	}
}

// A repository's infrastructure hold is a blocker line of home that names
// the repository, for its stages do not all wait: short, and with the cause
// on a wide screen.
func TestScreenDataShowsARepositorysInfraHoldAsABlocker(t *testing.T) {
	r := newScreenRig(t)
	reason := "every copy of example/shop broke the same way: bin/worktree-setup exited 1: redis-server: command not found"
	b, err := json.Marshal([]Hold{{Kind: HoldInfra, Repo: "example/shop", Reason: reason, Since: screenNow.Add(-5 * time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.layout.FactoryHolds()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.layout.FactoryHolds(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	d := r.data(screenNow)
	short := "infra down: example/shop's tasks wait for a copy"
	if want := []string{short}; !slices.Equal(d.Blocking, want) {
		t.Errorf("blocking %q, want %q", d.Blocking, want)
	}
	if want := []string{short + " (" + reason + ")"}; !slices.Equal(d.BlockingLong, want) {
		t.Errorf("blocking long %q, want %q", d.BlockingLong, want)
	}
}
