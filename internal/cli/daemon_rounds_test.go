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
	st, err := store.Open(c.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
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
	st, err := store.Open(c.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
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
