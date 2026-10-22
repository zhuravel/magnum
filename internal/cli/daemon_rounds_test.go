package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func TestActiveRoundsReadsAnOddHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "my home #1 100%")
	c, _, _ := bareContext(t)
	c.Layout = paths.Layout{Home: home}
	if rounds, err := c.activeRounds(context.Background()); err != nil || !rounds.none() {
		t.Fatalf("no registry yet: %v %v", rounds, err)
	}
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	inspSeedPR(t, st, "talkable/talkable", 7, store.PRReviewing, nil)
	inspSeedPR(t, st, "talkable/talkable", 8, store.PRReviewed, nil)
	rounds, err := c.activeRounds(context.Background())
	if err != nil || rounds.String() != "talkable/talkable#7 (reviewing)" {
		t.Fatalf("rounds %v err %v", rounds, err)
	}
	if c.refuseWhileRoundsRun("daemon-stop", false) {
		t.Fatal("a round in flight must refuse")
	}
	if !c.refuseWhileRoundsRun("daemon-stop", true) {
		t.Fatal("--now must override")
	}
}

// A stop counted only review rounds as in flight, so daemon-restart (and
// install, uninstall, daemon-stop, the drain) cut a running notes curation
// or retro short. Both count: the stop is refused and names them.
func TestARunningCurationOrRetroRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-3 * time.Minute)
	mark, _ := json.Marshal(engine.CurateMark{Repo: "talkable/talkable", Trigger: "request", Started: started})
	if err := st.SetKV(ctx, engine.KVNotesCurating, string(mark)); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "notes curation of talkable/talkable (since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if err := st.SetKV(ctx, engine.KVRetroRunning, store.FormatTime(started)); err != nil {
		t.Fatal(err)
	}
	inspSeedPR(t, st, "talkable/talkable", 7, store.PRReviewing, nil)
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a curation and a retro in flight must refuse")
	}
	actContains(t, errb.String(), "1 review round(s) and 2 background job(s) in flight: talkable/talkable#7 (reviewing), retro (since "+clock+"), notes curation of talkable/talkable (since "+clock+")",
		"cuts the background work short", "--when-idle")
	if !c.refuseWhileRoundsRun("daemon-restart", true) {
		t.Fatal("--now must override")
	}
	if err := errors.Join(st.DeleteKV(ctx, engine.KVNotesCurating), st.DeleteKV(ctx, engine.KVRetroRunning)); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || len(running.work) != 0 {
		t.Fatalf("after they ended: %q, %v", running, err)
	}
}

func TestRefuseWhileRoundsRunFailsClosed(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Not a database: the read fails, so the stop is refused.
	if err := os.WriteFile(c.Layout.DB(), []byte("not a sqlite file, not at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("an unreadable registry must refuse")
	}
	actContains(t, errb.String(), "cannot tell whether review rounds are in flight", "--now")
	if !c.refuseWhileRoundsRun("daemon-restart", true) {
		t.Fatal("--now must override an unreadable registry")
	}
}

// A running skill edit is background work too: a stop is refused and names
// it, since the edit would propose nothing.
func TestARunningSkillEditRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-5 * time.Minute)
	mark, _ := json.Marshal(engine.EditMark{Run: "20261009-070000", Trigger: "request", Started: started})
	if err := st.SetKV(ctx, engine.KVLearnEditing, string(mark)); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "skill edit (since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a skill edit in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: skill edit (since "+clock+")", "a skill edit proposes nothing")
}

// A revert runs in the skill edit's slot and its mark: a stop is refused
// and names the revert.
func TestARunningRevertRefusesAStopAndIsNamed(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-5 * time.Minute)
	mark, _ := json.Marshal(engine.EditMark{Run: "20261009-070000", Trigger: "regression", Started: started, Revert: 3})
	if err := st.SetKV(ctx, engine.KVLearnEditing, string(mark)); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "revert of skill version 3 (since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a revert in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: revert of skill version 3 (since "+clock+")")
}

// The gate of a skill proposal is background work too: a stop is refused
// and names it (the proposal stays gating and its gate starts again).
func TestARunningSkillGateRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-5 * time.Minute)
	mark, _ := json.Marshal(engine.GateMark{Proposal: 3, Started: started})
	if err := st.SetKV(ctx, engine.KVLearnGating, string(mark)); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "skill gate of proposal 3 (since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a skill gate in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: skill gate of proposal 3 (since "+clock+")", "a skill gate runs again")
}

// A fix loop whose fixer works is background work too: the PR stays
// reviewed while it runs, so a stop counted nothing and a drain restarted
// mid-fix, losing the loop. A stop is refused and names it; a loop waiting
// for its re-review is not counted (the round is, once it runs).
func TestARunningFixRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-4 * time.Minute)
	_, fixing := inspSeedPR(t, st, "talkable/talkable", 7, store.PRReviewed, nil)
	_, reviewing := inspSeedPR(t, st, "talkable/talkable", 8, store.PRRereviewPending, nil)
	for pr, s := range map[int64]engine.FixState{
		fixing.ID:    {Cycle: 2, Cap: 3, Phase: engine.FixPhaseFixing, StartedAt: started},
		reviewing.ID: {Cycle: 1, Cap: 3, Phase: engine.FixPhaseReviewing, StartedAt: started},
	} {
		b, _ := json.Marshal(s)
		if err := st.SetKV(ctx, engine.KVPRFix(pr), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "fix of talkable/talkable#7 (cycle 2, since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a fix in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: fix of talkable/talkable#7 (cycle 2, since "+clock+")",
		"a fix loop ends and needs you", "--drain")
	if err := st.DeleteKV(ctx, engine.KVPRFix(fixing.ID)); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || !running.none() {
		t.Fatalf("after the fix ended: %q, %v", running, err)
	}
}

// A whiteboard whose writer works is background work too: the PR stays
// reviewed while it runs, and a stop loses the whiteboard (its run fails as
// lost in a restart). A stop is refused and names it; a whiteboard run that
// ended counts no more.
func TestARunningWhiteboardRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 7, store.PRReviewed, nil)
	run, err := st.CreateRun(ctx, store.Run{ID: "r-wb", PRID: pr.ID, Round: 1, Role: "claude-whiteboard", Kind: store.RunWhiteboard,
		TargetSHA: pr.HeadSHA, Identity: pr.Identity, ReviewerLogin: "x", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-3 * time.Minute)
	if err := st.TransitionRun(ctx, run.ID, nil, store.RunWorking, func(u *store.RunUpdate) { u.Set("submitted_at", started) }); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if err != nil || running.String() != "whiteboard of talkable/talkable#7 (since "+clock+")" {
		t.Fatalf("in flight %q, err %v", running, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a whiteboard in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: whiteboard of talkable/talkable#7 (since "+clock+")",
		"a whiteboard is lost", "--drain")
	if err := st.TransitionRun(ctx, run.ID, nil, store.RunVerified, nil); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || !running.none() {
		t.Fatalf("after the whiteboard ended: %q, %v", running, err)
	}
}

// A running notes critique is background work: a stop is refused and names
// the repository and the proposal, since the critique would be cut short.
func TestARunningCritiqueRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-4 * time.Minute)
	mark, _ := json.Marshal(engine.CriticMark{Repo: "talkable/talkable", Proposal: 3, Model: "fable", Started: started})
	if err := st.SetKV(ctx, engine.KVNotesCritic, string(mark)); err != nil {
		t.Fatal(err)
	}
	running, err := c.activeRounds(ctx)
	clock := inspClock(time.Now(), started)
	if want := "notes critique of talkable/talkable (proposal 3, since " + clock + ")"; err != nil || running.String() != want {
		t.Fatalf("in flight %q, want %q, err %v", running, want, err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("a critique in flight must refuse")
	}
	actContains(t, errb.String(), "1 background job(s) in flight: notes critique of talkable/talkable (proposal 3, since "+clock+")",
		"a notes critique runs again later")
	if err := st.DeleteKV(ctx, engine.KVNotesCritic); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || len(running.work) != 0 {
		t.Fatalf("after it ended: %q, %v", running, err)
	}
}
