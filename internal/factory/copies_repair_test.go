package factory

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// breakAll claims a copy for a new task while every copy's setup fails
// twice, so each copy the repository may have breaks, and fails the test
// unless the claim waits with none left.
func (r *copiesRig) breakAll(names ...string) {
	r.t.Helper()
	for _, n := range names {
		r.failSetup[n] = 2
	}
	if _, err := r.c.Claim(r.ctx, r.task("PR-1")); !errors.Is(err, ErrNoCopy) {
		r.t.Fatalf("Claim with every setup failing = %v, want ErrNoCopy", err)
	}
	for _, n := range names {
		if sl := r.slot(n); sl.State != store.SlotBroken {
			r.t.Fatalf("%s = %s, want broken", n, sl.State)
		}
	}
}

// due moves the clock by d and fails the test unless RepairDue then names
// want ("" for none).
func (r *copiesRig) due(d time.Duration, want string) {
	r.t.Helper()
	r.clock.Add(d)
	got, err := r.c.RepairDue(r.ctx)
	if err != nil || got != want {
		r.t.Fatalf("RepairDue at %s = %q, %v; want %q", r.clock.Now().Sub(t0), got, err, want)
	}
}

// copyEvents are copy name's events of kind, by phase ("" for one with
// none).
func (r *copiesRig) copyEvents(name, kind string) map[string][]store.Event {
	r.t.Helper()
	evs, err := r.st.EventsBySubject(r.ctx, "slot:"+name, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string][]store.Event{}
	for _, e := range evs {
		if e.Kind == kind {
			out[store.Deref(e.Phase)] = append(out[store.Deref(e.Phase)], e)
		}
	}
	return out
}

// A copy that broke while the daemon runs is repaired again on a backoff:
// 10 minutes after it broke, then 30 minutes after that repair failed, then
// every hour. RepairDue names it only once its try is due; each try runs the
// start's repair (its setup again) with a begin and a fail or ok event, and
// the one that works puts it back in the pool, where a claim takes it.
func TestABrokenCopyIsRepairedAfter10MinutesThen30ThenEveryHour(t *testing.T) {
	r := newCopiesRig(t, 1)
	one := filepath.Join(r.root, "talkable.factory1")
	setups := func(want int) {
		t.Helper()
		if got := len(r.calls("bin/worktree-setup", one)); got != want {
			t.Fatalf("factory1's setup ran %d times, want %d", got, want)
		}
	}
	repair := func(works bool) {
		t.Helper()
		err := r.c.Repair(r.ctx, "factory1")
		if b, broke := errors.AsType[copyBroke](err); works && err != nil || !works && (!broke || b.cause == "") {
			t.Fatalf("Repair = %v, want works %v (or the cause it broke with)", err, works)
		}
	}
	r.breakAll("factory1")
	r.failSetup["factory1"] = 3 // the next three repairs fail too
	setups(2)

	r.due(10*time.Minute-time.Second, "")
	r.due(time.Second, "factory1")
	repair(false)
	setups(3)
	r.due(30*time.Minute-time.Second, "")
	r.due(time.Second, "factory1")
	repair(false)
	r.due(time.Hour-time.Second, "")
	r.due(time.Second, "factory1")
	repair(false)
	setups(5)
	r.due(time.Hour-time.Second, "")
	r.due(time.Second, "factory1")
	repair(true)
	setups(6)
	if sl := r.slot("factory1"); sl.State != store.SlotFree {
		t.Fatalf("factory1 = %s after the repair that worked, want free", sl.State)
	}
	r.due(2*time.Hour, "")

	evs := r.copyEvents("factory1", "factory.copy_repair")
	if len(evs[store.PhaseBegin]) != 4 || len(evs[store.PhaseFail]) != 3 || len(evs[store.PhaseOK]) != 1 {
		t.Fatalf("repair events %d begin, %d fail, %d ok; want 4, 3 and 1", len(evs[store.PhaseBegin]), len(evs[store.PhaseFail]),
			len(evs[store.PhaseOK]))
	}
	if c := r.claim(r.task("PR-2")); c.Name != "factory1" {
		t.Fatalf("claimed %s after the repair, want factory1", c.Name)
	}
}

// A copy whose try is due waits for it as the claims say: the wait names
// the next repair, and the restart that repairs them now.
func TestTheWaitForABrokenCopyNamesItsNextRepair(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.breakAll("factory1")
	next := r.clock.Now().Add(10 * time.Minute)
	_, err := r.c.Claim(r.ctx, r.task("PR-2"))
	want := "copy: 0 busy, 1 broken (the next repair at " + clock(next) + ", or a restart: magnum factory install)"
	if !errors.Is(err, ErrNoCopy) || err.Error() != want {
		t.Fatalf("Claim = %v, want %s", err, want)
	}
	r.clock.Add(10 * time.Minute)
	_, err = r.c.Claim(r.ctx, r.task("PR-3"))
	want = "copy: 0 busy, 1 broken (its repair is due, or a restart: magnum factory install)"
	if !errors.Is(err, ErrNoCopy) || err.Error() != want {
		t.Fatalf("Claim once the repair is due = %v, want %s", err, want)
	}
}

// The backoff never repairs a copy a claim works on or a task holds: a
// claim's own repair in flight (a restart's first claim repairs a copy that
// broke before it), or a broken copy whose row still names a task's owner
// row (a pinned break keeps its PR). RepairDue passes over them and Repair
// refuses them.
func TestTheBackoffRepairsNoCopyAClaimWorksOnOrATaskHolds(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.breakAll("factory1")
	sl := r.slot("factory1")
	pr, err := r.owner(r.ctx, r.task("PR-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.UpdateSlotFields(r.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("pr_id", pr.ID) }); err != nil {
		t.Fatal(err)
	}
	r.due(10*time.Minute, "")
	if err := r.c.Repair(r.ctx, "factory1"); !errors.Is(err, errNotDue) {
		t.Fatalf("Repair of a copy a task holds = %v, want errNotDue", err)
	}
	if err := r.st.UpdateSlotFields(r.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("pr_id", nil) }); err != nil {
		t.Fatal(err)
	}
	r.due(0, "factory1")

	r.c = r.newCopies() // a restart: the first claim that finds no copy repairs it at once
	block := make(chan struct{})
	r.blocks["bin/worktree-setup factory1"] = block
	claimed := make(chan error, 1)
	go func() {
		_, err := r.c.Claim(r.ctx, r.task("PR-3"))
		claimed <- err
	}()
	if got := <-r.entered; got != "bin/worktree-setup factory1" {
		t.Fatalf("entered %q, want the start's repair", got)
	}
	r.due(time.Hour, "")
	if err := r.c.Repair(r.ctx, "factory1"); !errors.Is(err, errNotDue) {
		t.Fatalf("Repair during a claim's repair = %v, want errNotDue", err)
	}
	close(block)
	if err := <-claimed; err != nil {
		t.Fatalf("the claim that repaired it = %v, want its copy", err)
	}
	if got := len(r.calls("bin/worktree-setup", filepath.Join(r.root, "talkable.factory1"))); got != 3 {
		t.Fatalf("factory1's setup ran %d times, want 3: the backoff ran none", got)
	}
}

// The daemon's stop cuts a backoff repair in flight (its setup ends with the
// context), and nothing half-done is recorded: the copy is broken again with
// the cause it broke with, the cut counts as no try (no ok, no copy_broken,
// its try still due), and the repair's begin ends with a fail that says the
// stop cut it.
func TestAStopCutsABackoffRepairAndLeavesTheCopyAsItWas(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.setupStderr = "redis-server: command not found"
	r.breakAll("factory1")
	before := r.slot("factory1")
	r.due(10*time.Minute, "factory1")

	block := make(chan struct{})
	r.blocks["bin/worktree-setup factory1"] = block
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan error, 1)
	go func() { done <- r.c.Repair(ctx, "factory1") }()
	<-r.entered
	cancel()
	close(block)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut repair = %v, want context.Canceled", err)
	}

	sl := r.slot("factory1")
	if sl.State != store.SlotBroken || store.Deref(sl.LastError) != store.Deref(before.LastError) {
		t.Fatalf("factory1 = %s (%q) after the cut, want broken with %q", sl.State, store.Deref(sl.LastError), store.Deref(before.LastError))
	}
	evs := r.copyEvents("factory1", "factory.copy_repair")
	if len(evs[store.PhaseOK]) != 0 || len(evs[store.PhaseFail]) != 1 || !strings.Contains(evs[store.PhaseFail][0].Message, "stopped") {
		t.Fatalf("repair events %+v, want one fail that names the stop and no ok", evs)
	}
	if n := len(r.copyEvents("factory1", "factory.copy_broken")[""]); n != 1 {
		t.Fatalf("%d copy_broken events, want only the break's: the cut is no failure", n)
	}
	r.due(0, "factory1") // the cut counted no try
}

// Every copy of a repository broken with one cause is an outage of the
// repository (Outages), whatever their own words (the path, the name and the
// port of each, its databases), the temporary files and the times say, and
// whether a copy broke at its setup or at a repair. A repair in flight keeps
// it; a repair that works ends it.
func TestEveryCopyBrokenTheSameWayIsAnOutageUntilOneIsRepaired(t *testing.T) {
	r := newCopiesRig(t, 2)
	r.setupStderr = "mise WARN Config files in {dir}/.mise.toml are not trusted.\n" +
		"createdb talkable_test__{copy} on port {port} at {time} ({took}): redis-server: command not found\n"
	if outs, err := r.c.Outages(r.ctx); err != nil || len(outs) != 0 {
		t.Fatalf("Outages with no copy = %+v, %v; want none", outs, err)
	}
	r.breakAll("factory1", "factory2")
	outage := func(what string) Outage {
		t.Helper()
		outs, err := r.c.Outages(r.ctx)
		if err != nil || len(outs) != 1 || outs[0].Repo != "talkable/talkable" {
			t.Fatalf("Outages %s = %+v, %v; want talkable/talkable's", what, outs, err)
		}
		return outs[0]
	}
	o := outage("with both copies broken the same way")
	if !strings.Contains(o.Cause, "redis-server: command not found") {
		t.Errorf("the outage's cause %q does not say why", o.Cause)
	}
	for _, own := range []string{"factory1", "factory2", r.root, "3010", "3020", "2026-", "ms)", "mise WARN", "provision-", "setup failed twice"} {
		if strings.Contains(o.Cause, own) {
			t.Errorf("the outage's cause %q has %q, a copy's own word", o.Cause, own)
		}
	}

	r.failSetup["factory1"] = 1
	r.due(10*time.Minute, "factory1")
	if err := r.c.Repair(r.ctx, "factory1"); err == nil {
		t.Fatal("Repair worked, want it to fail")
	}
	if got := outage("after a repair failed the same way"); got.Cause != o.Cause {
		t.Fatalf("the outage's cause after the repair %q, want %q", got.Cause, o.Cause)
	}

	block := make(chan struct{})
	r.blocks["bin/worktree-setup factory2"] = block
	done := make(chan error, 1)
	r.due(0, "factory2")
	go func() { done <- r.c.Repair(r.ctx, "factory2") }()
	<-r.entered
	outage("while a repair runs")
	close(block)
	if err := <-done; err != nil {
		t.Fatalf("Repair = %v, want it repaired", err)
	}
	if outs, err := r.c.Outages(r.ctx); err != nil || len(outs) != 0 {
		t.Fatalf("Outages once factory2 is repaired = %+v, %v; want none", outs, err)
	}
}

// Copies broken with two causes are no outage, nor are broken copies beside
// one that works or beside room for another (a claim adds it), nor one copy
// alone.
func TestCopiesBrokenTwoWaysOrWithACopyLeftAreNoOutage(t *testing.T) {
	two := newCopiesRig(t, 2)
	two.stderrFor["factory1"] = "redis-server: command not found"
	two.stderrFor["factory2"] = "createdb: could not connect to the server"
	two.breakAll("factory1", "factory2")
	if outs, err := two.c.Outages(two.ctx); err != nil || len(outs) != 0 {
		t.Errorf("Outages of copies broken two ways = %+v, %v; want none", outs, err)
	}

	left := newCopiesRig(t, 3)
	left.failSetup["factory1"], left.failSetup["factory2"] = 2, 2
	if c := left.claim(left.task("PR-1")); c.Name != "factory3" {
		t.Fatalf("claimed %s, want factory3", c.Name)
	}
	if outs, err := left.c.Outages(left.ctx); err != nil || len(outs) != 0 {
		t.Errorf("Outages with a copy that works = %+v, %v; want none", outs, err)
	}

	room := newCopiesRig(t, 2)
	room.breakAll("factory1", "factory2")
	room.cfg.Factory.Repos[0].MaxCopies = 3
	if outs, err := room.c.Outages(room.ctx); err != nil || len(outs) != 0 {
		t.Errorf("Outages with room for a third copy = %+v, %v; want none", outs, err)
	}

	alone := newCopiesRig(t, 1)
	alone.breakAll("factory1")
	if outs, err := alone.c.Outages(alone.ctx); err != nil || len(outs) != 0 {
		t.Errorf("Outages of a single copy = %+v, %v; want none: one copy says nothing of a shared cause", outs, err)
	}
}

// A copy that fails its repairs with the cause it broke with says so once:
// each try has its fail event, but only a new cause is another copy_broken
// warning (the copy's log), however many tries fail.
func TestAFailedRepairWarnsOncePerCause(t *testing.T) {
	r := newCopiesRig(t, 1)
	r.setupStderr = "redis-server: command not found at {time}"
	r.breakAll("factory1")
	r.failSetup["factory1"] = 4
	warns := func(want int) {
		t.Helper()
		evs := r.copyEvents("factory1", "factory.copy_broken")[""]
		if len(evs) != want || slices.ContainsFunc(evs, func(e store.Event) bool { return e.Level != "warn" }) {
			t.Fatalf("copy_broken events %+v, want %d warnings", evs, want)
		}
	}
	tryAgain := func(d time.Duration) {
		t.Helper()
		r.due(d, "factory1")
		if err := r.c.Repair(r.ctx, "factory1"); err == nil {
			t.Fatal("Repair worked, want it to fail")
		}
	}
	warns(1)
	tryAgain(10 * time.Minute)
	tryAgain(30 * time.Minute)
	warns(1)
	r.mu.Lock()
	r.setupStderr = "createdb: could not connect to the server"
	r.mu.Unlock()
	tryAgain(time.Hour)
	warns(2)
	tryAgain(time.Hour)
	warns(2)
	if n := len(r.copyEvents("factory1", "factory.copy_repair")[store.PhaseFail]); n != 4 {
		t.Fatalf("%d failed repair events, want one per try", n)
	}
}

// A copy's signature is the cause its last error words, past what failed
// ("setup failed twice", "its repair failed") and without its setup's log,
// with the copy's own words (its path, its name in its databases, the env
// values only it has) and what changes from run to run (temporary files,
// times, durations) taken out: two copies broken by one cause read the
// same, and two causes do not.
func TestACopysSignatureKeepsTheCauseAndDropsItsOwnWords(t *testing.T) {
	repo := config.FactoryRepo{Repo: "talkable/talkable", CopyName: "factory{n}", Copies: "/work/talkable.factory{n}",
		Env: map[string]string{"PORT": "30{n}0", "WT_BRANCH": "{copy}", "REDIS_DB_OFFSET": "{n*16}", "RAILS_ENV": "development"}}
	slot := func(n int) store.Slot {
		name := repo.Pool().Slot(n)
		return store.Slot{Name: name, Path: repo.Pool().Path(n), DBSlug: new(slots.DBSlug(name))}
	}
	tmp := t.TempDir() // under the TMPDIR the daemon's commands write their temporary files to
	why := func(n int, what, line string) string {
		sl := slot(n)
		return what + ": bin/worktree-setup exited 1: " + strings.NewReplacer("{dir}", sl.Path, "{copy}", sl.Name,
			"{port}", copyEnv(repo, n)["PORT"], "{tmp}", filepath.Join(tmp, "setup."+sl.Name+".log"),
			"{at}", t0.Add(time.Duration(n)*time.Minute).Format(time.RFC3339), "{clock}", t0.Add(time.Duration(n)*time.Second).Format(time.TimeOnly),
		).Replace(line) + " (its setup's log: /home/logs/provision-" + sl.Name + ".log)"
	}
	line := "{dir}/bin/worktree-setup: createdb talkable_test__{copy} on port {port} at {at} ({clock}, 1.{port}s, see {tmp} " +
		"and /tmp/x.{copy}.sock): role \"development\" does not exist"
	one := copySignature(repo, slot(1), why(1, "setup failed twice", line))
	two := copySignature(repo, slot(2), why(2, "its repair failed", line))
	if one != two {
		t.Fatalf("signatures of one cause differ:\n%s\n%s", one, two)
	}
	for _, want := range []string{"bin/worktree-setup exited 1", `role "development" does not exist`, "createdb", "on port"} {
		if !strings.Contains(one, want) {
			t.Errorf("signature %q lost %q", one, want)
		}
	}
	for _, own := range []string{"factory1", "/work/", "3010", "2026-", "provision-", "setup failed twice", tmp, "/tmp/"} {
		if strings.Contains(one, own) {
			t.Errorf("signature %q keeps %q", one, own)
		}
	}
	if other := copySignature(repo, slot(2), why(2, "its repair failed", "createdb: could not connect to the server")); other == one {
		t.Errorf("two causes read the same: %q", one)
	}
	if got := copySignature(repo, slot(1), ""); got != "" {
		t.Errorf("signature of no error %q, want none", got)
	}
}

// replaceWord replaces a word where it stands alone, never inside another
// word, and ends whatever the replacement holds.
func TestReplaceWordReplacesOnlyAWholeWord(t *testing.T) {
	for _, tc := range []struct{ s, w, by, want string }{
		{"port 3010 in use (3010)", "3010", "<copy>", "port <copy> in use (<copy>)"},
		{"13010 and 30100 and 3010_x and x3010", "3010", "<copy>", "13010 and 30100 and 3010_x and x3010"},
		{"3010", "3010", "<copy>", "<copy>"},
		{"a copy, copy", "copy", "<copy>", "a <copy>, <copy>"},
		{"", "3010", "<copy>", ""},
	} {
		if got := replaceWord(tc.s, tc.w, tc.by); got != tc.want {
			t.Errorf("replaceWord(%q, %q) = %q, want %q", tc.s, tc.w, got, tc.want)
		}
	}
}
