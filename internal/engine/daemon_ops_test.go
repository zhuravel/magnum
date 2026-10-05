package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// events are the events of kind, oldest first.
func (h *harness) events(kind string) []store.Event {
	h.t.Helper()
	rows, err := h.st.DB().QueryContext(h.ctx, "SELECT level, message FROM events WHERE kind = ? ORDER BY id", kind)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		ev := store.Event{Kind: kind}
		if err := rows.Scan(&ev.Level, &ev.Message); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// radarHookGH runs onRadar before each Radar call (the start of a poll).
type radarHookGH struct {
	*fakeGH
	onRadar *func()
}

func (g radarHookGH) Radar(ctx context.Context, org string) ([]github.RepoRadar, github.RateLimit, error) {
	if f := *g.onRadar; f != nil {
		f()
	}
	return g.fakeGH.Radar(ctx, org)
}

// The CLI waits only so long for an answer (about 12 s of GitHub poll used
// to come first): a tick answers the requests already queued before it
// polls, and the ones queued during the poll after it, in the same tick.
func TestTickAnswersRequestsBeforeItPolls(t *testing.T) {
	var onRadar func()
	h := newHarness(t, func(h *harness) {
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return radarHookGH{fakeGH: h.gh, onRadar: &onRadar}
			}
			return nil
		}
	})
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()

	early := h.enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
	var atPoll string
	var late int64
	onRadar = func() {
		if late == 0 {
			atPoll = h.request(early).State
			late = h.enqueue(ReqUnmute, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
		}
	}
	h.tick()
	if atPoll != store.RequestDone {
		t.Fatalf("the request queued before the tick was %q when the poll started, want done", atPoll)
	}
	if r := h.request(late); r.State != store.RequestDone || deref(r.Result) != "unmuted talkable/talkable#1" {
		t.Fatalf("the request queued during the poll: %+v %q", r, deref(r.Result))
	}
}

// An older daemon used to drop a payload field it did not know (a dry run's
// dry_run would post): the request fails and says to restart the daemon.
func TestRequestWithAFieldTheDaemonPredatesFails(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	id := h.enqueue(ReqMute, map[string]any{"ref": "1", "dry_run": true})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), `this daemon predates "dry_run": restart it`) {
		t.Fatalf("request = %+v %q", r, deref(r.Result))
	}
	if h.pr(1).Muted {
		t.Fatal("the PR was muted without the field")
	}
}

// A daemon records its build at startup, so the CLI can tell when it runs an
// older one than the CLI or the binary on disk.
func TestStartupRecordsTheBuildAndSkewIsReported(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.build = CurrentBuild("v1.2.0", bin)
	h.startup()
	b, ok := ReadDaemonBuild(h.ctx, h.st)
	if !ok || b.Version != "v1.2.0" || b.Path != bin || b.ModTime.IsZero() || !b.StartedAt.Equal(h.clock.Now()) {
		t.Fatalf("recorded build = %+v %v", b, ok)
	}
	if note := SkewNote(b, "v1.2.0", FileModTime); note != "" {
		t.Fatalf("same build: %q", note)
	}
	if note := SkewNote(b, "dev", FileModTime); note != "" {
		t.Fatalf("an unstamped CLI (go run) compared its version: %q", note)
	}
	note := SkewNote(b, "v1.3.0", FileModTime)
	if !strings.HasPrefix(note, "daemon runs v1.2.0 since ") || !strings.HasSuffix(note, "; v1.3.0 is built: `magnum daemon-restart`") {
		t.Fatalf("CLI newer: %q", note)
	}
	later := b.ModTime.Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if note := SkewNote(b, "v1.2.0", FileModTime); !strings.Contains(note, "a new build is on disk ("+bin) {
		t.Fatalf("binary rebuilt: %q", note)
	}
}

// restartOnNewBuild sets up a daemon under launchd with restart_on_new_build
// whose binary is then rebuilt; check answers the new binary's check.
func restartOnNewBuild(h *harness, check func() error) string {
	h.t.Helper()
	bin := filepath.Join(h.t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("v1"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.cfg.Daemon.RestartOnNewBuild = true
	h.e.build = CurrentBuild("v1", bin)
	h.e.supervised = true
	h.e.checkBuild = func(_ context.Context, path string) (string, error) {
		if path != bin {
			h.t.Fatalf("checked %s, want %s", path, bin)
		}
		return "v2", check()
	}
	return bin
}

func rebuild(t *testing.T, bin string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(bin, at, at); err != nil {
		t.Fatal(err)
	}
}

// With restart_on_new_build, a checked new build restarts the daemon at the
// first tick with no round in flight; rounds already running finish first and
// dispatch is never held for it.
func TestNewBuildRestartsTheDaemonOnceNoRoundRuns(t *testing.T) {
	h := newHarness(t)
	bin := restartOnNewBuild(h, func() error { return nil })
	h.runningRound(2, "h1", false)
	rebuild(t, bin, time.Now().Add(time.Hour))
	h.e.lastReconcile = time.Time{}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick with a round in flight: %v", err)
	}
	if evs := h.events("daemon.restart_pending"); len(evs) != 1 || !strings.Contains(evs[0].Message, "passed its check") {
		t.Fatalf("restart_pending events = %+v", evs)
	}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("second tick with the round still in flight: %v", err)
	}
	h.rd.mu.Lock()
	close(h.rd.gate)
	h.rd.gate = nil
	h.rd.mu.Unlock()
	h.settle()
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForBuild) {
		t.Fatalf("tick after the round: %v, want ErrRestartForBuild", err)
	}

	// The next daemon reports the restart.
	h.e.build = CurrentBuild("v2", bin)
	h.startup()
	if evs := h.events("daemon.restarted_for_build"); len(evs) != 1 || !strings.Contains(evs[0].Message, "v1 → v2") {
		t.Fatalf("restarted_for_build events = %+v", evs)
	}
}

// A new build that fails its check never restarts the daemon, and neither
// does one when launchd does not run the daemon (nothing would start it
// again) or when restart_on_new_build is off.
func TestNewBuildDoesNotRestartWhenItFailsOrNothingRestartsTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name       string
		check      error
		supervised bool
		off        bool
		event      string
	}{
		{name: "fails its check", check: errors.New("config: bad key"), supervised: true, event: "daemon.build_rejected"},
		{name: "no launchd", event: "daemon.restart_pending"},
		{name: "off", supervised: true, off: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bin := restartOnNewBuild(h, func() error { return tc.check })
			h.e.supervised = tc.supervised
			h.cfg.Daemon.RestartOnNewBuild = !tc.off
			h.startup()
			rebuild(t, bin, time.Now().Add(time.Hour))
			h.e.lastReconcile = time.Time{}
			for range 2 {
				if err := h.e.Tick(h.ctx); err != nil {
					t.Fatalf("tick: %v", err)
				}
				h.settle()
			}
			if tc.event != "" && len(h.events(tc.event)) != 1 {
				t.Fatalf("%s events = %+v", tc.event, h.events(tc.event))
			}
			if tc.off && len(h.events("daemon.restart_pending"))+len(h.events("daemon.build_rejected")) > 0 {
				t.Fatal("a build was checked with restart_on_new_build off")
			}
		})
	}
}

// A drain whose drainer died (its terminal closed: SIGHUP) used to hold
// every round until the next restart: the daemon lifts it and says so.
func TestDrainWhoseDrainerIsGoneIsLifted(t *testing.T) {
	h := newHarness(t)
	h.startup()
	defer func(orig func(int) (string, string, error)) { processCommand = orig }(processCommand)
	processCommand = func(pid int) (string, string, error) {
		return "/Users/example/bin/magnum", "/Users/example/bin/magnum daemon-restart --drain", nil
	}
	live := Drain{Since: h.clock.Now(), PID: os.Getpid()}
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, live.Value()); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if v, _, _ := h.st.GetKV(h.ctx, KVDaemonDraining); v != live.Value() {
		t.Fatalf("a live drainer's drain was lifted: %q", v)
	}

	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	gone := Drain{Since: h.clock.Now(), PID: dead.Process.Pid}
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, gone.Value()); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if v, ok, _ := h.st.GetKV(h.ctx, KVDaemonDraining); ok {
		t.Fatalf("the drain of a dead drainer stays: %q", v)
	}
	if evs := h.events("daemon.drain_lifted"); len(evs) != 1 || !strings.Contains(evs[0].Message, "is gone") {
		t.Fatalf("drain_lifted events = %+v", evs)
	}
}

func TestParseDrainReadsBothFormats(t *testing.T) {
	at := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	if d, ok := ParseDrain(Drain{Since: at, PID: 42}.Value()); !ok || !d.Since.Equal(at) || d.PID != 42 {
		t.Fatalf("new format: %+v %v", d, ok)
	}
	if d, ok := ParseDrain(store.FormatTime(at)); !ok || !d.Since.Equal(at) || d.PID != 0 {
		t.Fatalf("an older CLI's bare time: %+v %v", d, ok)
	}
	if _, ok := ParseDrain("nonsense"); ok {
		t.Fatal("nonsense parsed")
	}
}

// Requests queued while no daemon ran used to fire whenever one started,
// days later: at startup those older than an hour fail. One queued while a
// daemon ran (a heavy request it was running when it stopped) stays.
func TestStartupExpiresRequestsQueuedWithoutADaemon(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick() // the daemon's last tick
	seen := h.enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
	if err := h.st.CompleteRequest(h.ctx, seen, store.RequestDone, "muted"); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	stale := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "1"}})
	h.advance(2 * time.Hour)
	fresh := h.enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: "1"}})
	h.startup()
	if r := h.request(stale); r.State != store.RequestFailed || deref(r.Result) != "expired: queued while no daemon ran" {
		t.Fatalf("stale request = %+v %q", r, deref(r.Result))
	}
	if r := h.request(fresh); r.State != store.RequestPending {
		t.Fatalf("fresh request = %+v", r)
	}
	if h.pr(1).Forced {
		t.Fatal("the expired review forced the PR")
	}
}

func TestStartupKeepsOldRequestsADaemonSaw(t *testing.T) {
	h := newHarness(t)
	h.startup()
	old := h.enqueue(ReqProvision, ProvisionPayload{Count: 1})
	h.advance(time.Minute)
	h.e.setKV(h.ctx, kvLastTick, store.FormatTime(h.clock.Now())) // a daemon ticked after it was queued
	h.advance(3 * time.Hour)
	h.e.expireRequests(h.ctx, h.clock.Now().Add(-3*time.Hour))
	if r := h.request(old); r.State != store.RequestPending {
		t.Fatalf("a request a daemon saw expired: %+v %q", r, deref(r.Result))
	}
}
