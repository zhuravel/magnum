package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// workingRun records a turn of role at work on the PR #n of repo in st, as
// an agent's turn that outlives its daemon in herdr.
func workingRun(t *testing.T, st *store.Store, repo string, n int, role string) store.Run {
	t.Helper()
	ctx := context.Background()
	owner, name, _ := strings.Cut(repo, "/")
	r, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_" + repo, Owner: owner, Name: name, Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: r.ID, NodeID: "PR_" + repo + "_" + role, Number: n,
		URL: "https://github.com/" + repo + "/pull/1", HeadSHA: "abc1234def5678", GHState: store.GHOpen, InitialState: store.PRReviewing,
		Identity: "talkable-app"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, store.Run{ID: "run-" + role, PRID: up.PR.ID, Role: role, Kind: store.RunInitial, TargetSHA: "abc1234def5678",
		Identity: "talkable-app", State: store.RunWorking, PromptText: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// endRun ends run id in st, as its turn ending does.
func endRun(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.TransitionRun(context.Background(), id, nil, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
}

// A daemon that does not run is not quiet while an agent's turn it started
// still runs in herdr: the drain reads its own store (the review registry,
// the factory's agents store) and waits for those turns, saying the daemon
// is down; once they ended, it is safe.
func TestADrainWaitsForTheTurnsOfADaemonThatIsDown(t *testing.T) {
	h, _ := newControlHarness(t)
	run := workingRun(t, h.st, "talkable/talkable", 12, "codex-judge")
	agents := storetest.OpenFile(t, h.c.Layout.FactoryAgentsDB())
	frun := workingRun(t, agents, "factory-owner/tasks", 7, "fx-writer")
	sleeps := 0
	h.onSleep = func(h *actHarness) {
		if sleeps++; sleeps == 2 {
			endRun(t, h.st, run.ID)
			endRun(t, agents, frun.ID)
		}
	}
	if code := h.cmd("drain"); code != 0 {
		t.Fatalf("drain: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "review: draining: its daemon is down while agents' turns run in herdr: codex-judge turn of talkable/talkable#12",
		"factory: draining: its daemon is down while agents' turns run in herdr: fx-writer turn of task 7", drainSafeText)
}

// A daemon whose state cannot be read is never certified: a pidfile that
// cannot be read, or the store of a daemon that is down that cannot be read,
// keeps the drain unsafe, saying why.
func TestADrainNeverCertifiesADaemonItCannotRead(t *testing.T) {
	h, _ := newControlHarness(t)
	h.d.Running = func() (int, error) { return 0, errors.New("pidfile state/daemon.pid: bad content \"x\"") }
	if err := os.MkdirAll(filepath.Dir(h.c.Layout.FactoryAgentsDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.c.Layout.FactoryAgentsDB(), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("drain", "--timeout", "10s"); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "review: cannot tell whether the review daemon runs", "factory: its daemon is down and its agents' turns cannot be read")
	if strings.Contains(h.out.String(), "safe to reboot") {
		t.Fatalf("it said safe with a daemon it cannot read:\n%s", h.out.String())
	}
}

// A drain run again while one holds keeps its start and its end (until
// resume after --keep) unless a flag changes them, and a failure on the other
// daemon puts back the drain it overwrote.
func TestADrainRunAgainKeepsItsStartAndEndAndAFailurePutsTheOldOneBack(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("drain", "--keep", "--no-wait"); code != 0 {
		t.Fatalf("drain --keep: exit %d: %s", code, h.errb.String())
	}
	first, _ := h.reviewDrain()
	h.now = h.now.Add(time.Hour)
	if code := h.cmd("drain", "--no-wait"); code != 0 {
		t.Fatalf("drain again: exit %d: %s", code, h.errb.String())
	}
	again, _ := h.reviewDrain()
	if !again.Since.Equal(first.Since) || again.Until != engine.DrainUntilResume || again.Generation == first.Generation {
		t.Fatalf("the drain run again %+v, want the first's start and end (%+v), a generation of its own", again, first)
	}

	before, err := factory.ReadRebootDrain(h.c.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Close(); err != nil { // the review side fails
		t.Fatal(err)
	}
	if code := drainMain(h.ctx, h.c, h.d, drainOpts{timeout: time.Hour}); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	if after, _ := factory.ReadRebootDrain(h.c.Layout); after != before {
		t.Fatalf("the factory's drain is %q after the failure, want the one it overwrote put back (%q)", after, before)
	}
	actContains(t, h.errb.String(), "factory: its drain is put back as it was")
}

// A resume of one daemon ends a drain that waits for both with what still
// holds, never as if both were lifted.
func TestAPartialResumeEndsAWaitingDrainSayingWhatStillHolds(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	sleeps := 0
	h.onSleep = func(h *actHarness) {
		if sleeps++; sleeps == 2 {
			_ = h.st.DeleteKV(h.ctx, engine.KVDaemonRebootDrain) // magnum resume --review
		}
	}
	if code := h.cmd("drain"); code != 1 {
		t.Fatalf("drain then resume --review: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "review: the drain was lifted (`magnum resume`)", "factory: still drains")
	if strings.Contains(h.out.String(), "new work starts again") && !strings.Contains(h.errb.String(), "still drains") {
		t.Fatalf("a partial resume read as a full one:\n%s%s", h.out.String(), h.errb.String())
	}
}

// A daemon that started on an older build while the drain waits is named,
// and the drain is not certified for it.
func TestADrainSaysWhenAnOlderBuildStartsWhileItWaits(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	h.onSleep = func(h *actHarness) {
		h.pid = 9 // a daemon of an older build: its start recorded its pid, and no record of the controls
		_ = h.st.SetKV(h.ctx, store.KVDaemonPid, "9")
		if d, ok := h.reviewDrain(); ok {
			h.ackBoth(f, d.Generation, true, "")
		}
	}
	if code := h.cmd("drain", "--timeout", "10s"); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "the review daemon (pid 9) runs a build that does not know `magnum drain`: restart it on this build first")
}

// resume acts on what it compared: a drain for a reboot that changed while
// it was lifted is not said to be lifted.
func TestResumeSaysNothingLiftedWhenTheDrainChangedUnderIt(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("drain", "--review", "--no-wait"); code != 0 {
		t.Fatalf("drain: exit %d", code)
	}
	if _, err := h.st.DB().Exec(`CREATE TRIGGER keep_drain BEFORE DELETE ON kv WHEN OLD.key = 'daemon.reboot_drain'
		BEGIN UPDATE kv SET value = value || ' ' WHERE key = OLD.key; SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	h.out.Reset()
	if code := h.cmd("resume", "--review"); code != 1 {
		t.Fatalf("resume: exit %d, want 1", code)
	}
	if strings.Contains(h.out.String(), "lifted the drain") {
		t.Fatalf("resume said it lifted a drain it did not:\n%s", h.out.String())
	}
	if _, ok := h.reviewDrain(); !ok {
		t.Fatal("the drain went")
	}
	actContains(t, h.errb.String(), "changed while it was lifted")
}

// A drain record a daemon cannot read holds it until resume, and the status
// lines say so.
func TestAnUnreadableDrainRecordShowsInStatus(t *testing.T) {
	h, _ := newControlHarness(t)
	if err := h.st.SetKV(h.ctx, engine.KVDaemonRebootDrain, "{not a drain"); err != nil {
		t.Fatal(err)
	}
	if err := factory.WriteRebootDrain(h.c.Layout, "{not a drain"); err != nil {
		t.Fatal(err)
	}
	if got := reviewControlOf(storeKV(h.ctx, h.st), false, 0).holdText(h.now, false); got != "draining (unreadable record): magnum resume ends it" {
		t.Errorf("review holds %q, want the unreadable drain said", got)
	}
	if got := factoryControlOf(h.c.Layout, 0).holdText(h.now, false); got != "draining (unreadable record): magnum resume --factory ends it" {
		t.Errorf("factory holds %q, want the unreadable drain said", got)
	}
}
