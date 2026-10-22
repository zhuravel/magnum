package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// seedWork writes the daemon's mark of its work in flight that no row shows
// (engine.KVDaemonWork) into the test registry; no work deletes it, as the
// daemon does once nothing is in flight.
func seedWork(t *testing.T, dt *daemonGroupTest, work ...engine.WorkItem) {
	t.Helper()
	if err := dt.ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	storetest.Seed(t, dt.ctx.Layout.DB())
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if len(work) == 0 {
		if err := st.DeleteKV(context.Background(), engine.KVDaemonWork); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := json.Marshal(engine.WorkMark{Work: work})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVDaemonWork, string(b)); err != nil {
		t.Fatal(err)
	}
}

// --when-idle waits for the daemon's work that no registry row shows: a job
// on the heavy worker (a slot's provision of 20 to 45 minutes) and a GitHub
// write as the operator (the withdrawal of an automatic approval), whose
// answer a restart would lose. It restarts only once both ended.
func TestDaemonRestartWhenIdleWaitsForAHeavyJobAndAnApprovalWrite(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("running", 100), launchctlPrint("running", 200)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	started := time.Now().Add(-20 * time.Minute)
	provision := engine.WorkItem{What: "heavy job provision:talkable/talkable", Started: started}
	withdrawal := engine.WorkItem{What: "withdrawal of approval 9001 of talkable/talkable#765 by zhuravel", Started: started}
	seedWork(t, dt, provision, withdrawal)
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
			t.Errorf("sleep %d: restarted while work was in flight", sleeps)
		}
		switch sleeps {
		case 2:
			seedWork(t, dt, withdrawal) // the provision ended
		case 4:
			seedWork(t, dt) // the withdrawal's answer is recorded
		}
		return nil
	}
	if code := dt.run("daemon-restart", "--when-idle"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	clock := inspClock(time.Now(), started)
	actContains(t, dt.stdout.String(), "waiting for 2 background job(s) in flight to end",
		"heavy job provision:talkable/talkable (since "+clock+")",
		"withdrawal of approval 9001 of talkable/talkable#765 by zhuravel (since "+clock+")", "restarted zhuravel.magnum")
	if sleeps != 4 {
		t.Fatalf("restarted after %d waits, want 4", sleeps)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d", n)
	}
}

// A stop is refused while the daemon's work no row shows is in flight, and
// names it: an approval as the operator, a heavy job; a mark the CLI cannot
// read still counts. --now overrides.
func TestTheDaemonsWorkInFlightRefusesAStop(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, c.Layout.DB())
	ctx := context.Background()
	started := time.Now().Add(-time.Minute)
	b, _ := json.Marshal(engine.WorkMark{Work: []engine.WorkItem{
		{What: "approval of talkable/talkable#2 as zhuravel", Started: started},
		{What: "heavy job park:12", Started: started},
	}})
	if err := st.SetKV(ctx, engine.KVDaemonWork, string(b)); err != nil {
		t.Fatal(err)
	}
	clock := inspClock(time.Now(), started)
	running, err := c.activeRounds(ctx)
	if want := "approval of talkable/talkable#2 as zhuravel (since " + clock + "), heavy job park:12 (since " + clock + ")"; err != nil ||
		running.String() != want {
		t.Fatalf("in flight %q, err %v; want %q", running, err, want)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("work in flight must refuse")
	}
	actContains(t, errb.String(), "2 background job(s) in flight: approval of talkable/talkable#2 as zhuravel", "--when-idle")
	if !c.refuseWhileRoundsRun("daemon-restart", true) {
		t.Fatal("--now must override")
	}
	if err := st.SetKV(ctx, engine.KVDaemonWork, "not json"); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || running.String() != "daemon work" {
		t.Fatalf("an unreadable mark: %q, %v", running, err)
	}
	if err := st.DeleteKV(ctx, engine.KVDaemonWork); err != nil {
		t.Fatal(err)
	}
	if running, err := c.activeRounds(ctx); err != nil || !running.none() {
		t.Fatalf("after it ended: %q, %v", running, err)
	}
}
