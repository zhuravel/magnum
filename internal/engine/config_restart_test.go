package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
)

// userConfigText is a user layer that changes nothing the harness relies on.
const userConfigText = "[daemon]\nmax_concurrent_reviews = 3 # the harness's own value\n"

// withUserConfig gives the harness a user config (text) beside its base
// config.toml, loaded as the daemon loads it, so the engine's config knows
// the user layer's SHA-256.
func withUserConfig(text string) func(*harness) {
	return func(h *harness) {
		h.t.Helper()
		user := filepath.Join(h.layout.Home, "user-config.toml")
		if err := os.WriteFile(user, []byte(text), 0o600); err != nil {
			h.t.Fatal(err)
		}
		h.layout.UserConfig = user
		cfg, err := config.Load(h.layout, filepath.Join(h.layout.Home, "config.toml"))
		if err != nil {
			h.t.Fatal(err)
		}
		h.cfg, h.d.Config, h.d.Layout = cfg, cfg, h.layout
		h.ag.layout, h.ag.cfg = h.layout, cfg
		h.d.Cleanup.(*cleanup.Planner).Config = cfg
	}
}

func hexSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// saveUserConfig writes text over the harness's user config and marks the
// restart on it, as the settings screen's save does.
func (h *harness) saveUserConfig(text string, at time.Time) string {
	h.t.Helper()
	if err := os.WriteFile(h.layout.UserConfig, []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
	raw, err := json.Marshal(ConfigRestartMark{SHA256: hexSum(text), SavedAt: at})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, KVConfigRestart, string(raw)); err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func (h *harness) configMark() (ConfigRestartMark, bool) {
	h.t.Helper()
	return ReadConfigRestart(h.ctx, h.st)
}

// The daemon records the SHA-256 of the user config it loaded at its start,
// and "" when it read none, so `magnum status` and the settings screen can
// tell a saved config it does not run yet by content.
func TestTheDaemonRecordsTheUserConfigItLoaded(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	if _, ok := ReadDaemonConfigSum(h.ctx, h.st); ok {
		t.Fatal("a sum before any start")
	}
	h.startup()
	if got, ok := ReadDaemonConfigSum(h.ctx, h.st); !ok || got != hexSum(userConfigText) {
		t.Fatalf("recorded %q, %v; want %s", got, ok, hexSum(userConfigText))
	}

	plain := newHarness(t)
	plain.startup()
	if got, ok := ReadDaemonConfigSum(plain.ctx, plain.st); !ok || got != "" {
		t.Fatalf("without a user config: %q, %v; want \"\", true", got, ok)
	}
}

// A saved config restarts the daemon at the first tick with nothing in
// flight; a round already running finishes first. The next start clears the
// mark and says it runs the saved config.
func TestASavedConfigRestartsTheDaemonOnceNothingIsInFlight(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.runningRound(2, "h1", false) // starts the daemon
	saved := "[daemon]\nmax_concurrent_reviews = 2 # saved by the settings screen\n"
	savedAt := h.clock.Now()
	h.saveUserConfig(saved, savedAt)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick with a round in flight: %v", err)
	}
	h.rd.mu.Lock()
	close(h.rd.gate)
	h.rd.gate = nil
	h.rd.mu.Unlock()
	h.settle()
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick after the round: %v, want ErrRestartForConfig", err)
	}
	if _, ok := h.configMark(); !ok {
		t.Fatal("the exit cleared the mark: the next start could not report it")
	}

	// The next daemon loads the saved file.
	next, err := config.Load(h.layout, filepath.Join(h.layout.Home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	h.e.cfg = next
	h.startup()
	if _, ok := h.configMark(); ok {
		t.Fatal("the mark outlived the restart")
	}
	if got, _ := ReadDaemonConfigSum(h.ctx, h.st); got != hexSum(saved) {
		t.Fatalf("recorded %q, want the saved file's %s", got, hexSum(saved))
	}
	evs := h.events("daemon.restarted_for_config")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "runs the config saved at "+savedAt.Local().Format("15:04:05")) ||
		!strings.Contains(evs[0].Message, hexSum(saved)[:7]) || strings.Contains(evs[0].Message, "saved by the settings screen") {
		t.Fatalf("restarted_for_config events = %+v", evs)
	}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("a tick after the restart: %v", err)
	}
}

// reload makes the harness's engine run the user config on disk, as the
// next daemon would load it.
func (h *harness) reload() {
	h.t.Helper()
	cfg, err := config.Load(h.layout, filepath.Join(h.layout.Home, "config.toml"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.cfg = cfg
}

func (h *harness) writeUserConfig(text string) {
	h.t.Helper()
	if err := os.WriteFile(h.layout.UserConfig, []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// A file that changed again after the save (by hand, before the restart) is
// what the next start runs, and its event says so.
func TestTheRestartedDaemonSaysWhenTheFileChangedAfterTheSave(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.startup()
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	edited := "[daemon]\nmax_concurrent_reviews = 1 # edited by hand after the save\n"
	h.writeUserConfig(edited)
	h.reload()
	h.startup()
	evs := h.events("daemon.restarted_for_config")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "changed again after the save") ||
		!strings.Contains(evs[0].Message, hexSum(edited)[:7]) || strings.Contains(evs[0].Message, "edited by hand") {
		t.Fatalf("restarted_for_config events = %+v", evs)
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("the mark outlived the start")
	}
}

// A save made after the daemon command loaded its config but before the
// engine's start is not what the daemon runs: the start keeps its mark,
// and the idle restart takes it.
func TestASaveBetweenTheLoadAndTheStartIsRestartedOnAtIdle(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	saved := "[daemon]\nmax_concurrent_reviews = 2\n"
	h.saveUserConfig(saved, h.clock.Now())
	h.startup()
	if _, ok := h.configMark(); !ok {
		t.Fatal("the start dropped a save it does not run")
	}
	if got, _ := ReadDaemonConfigSum(h.ctx, h.st); got != hexSum(userConfigText) {
		t.Fatalf("recorded %q, want the loaded file's", got)
	}
	if evs := h.events("daemon.restarted_for_config"); len(evs) != 0 {
		t.Fatalf("restarted_for_config events = %+v", evs)
	}
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick: %v, want ErrRestartForConfig", err)
	}
}

// A save the operator reverted by hand leaves the file the daemon runs:
// its mark is cleared and nothing restarts.
func TestAMarkWhoseFileWasRevertedByHandIsClearedWithoutARestart(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	h.writeUserConfig(userConfigText)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v, want no restart", err)
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("the reverted save's mark stayed")
	}
}

// A binary rebuilt since the start (restart_on_new_build off) is what
// launchd would start: before a restart on a saved config the daemon checks
// it (its version and `<bin> config`), and a binary that refuses the config
// holds the restart, with one event however many ticks, until the file or
// the binary changes.
func TestAChangedBinaryIsCheckedBeforeAConfigRestart(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	bin := filepath.Join(t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.build = CurrentBuild("v1", bin)
	checks, refuse := 0, true
	h.e.checkBuild = func(_ context.Context, path string) (string, error) {
		checks++
		if path != bin {
			t.Errorf("checked %s, want %s", path, bin)
		}
		if refuse {
			return "", errors.New("config: unknown keys: [daemon.new_key]")
		}
		return "v2", nil
	}
	h.startup()
	saved := "[daemon]\nmax_concurrent_reviews = 2\n"
	h.saveUserConfig(saved, h.clock.Now())
	rebuilt := time.Now().Add(time.Hour)
	rebuild(t, bin, rebuilt)
	for range 3 {
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v, want the daemon to keep running", err)
		}
		h.settle()
	}
	if checks != 1 {
		t.Fatalf("the binary was checked %d times, want once", checks)
	}
	evs := h.events("daemon.config_restart_refused")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, bin) ||
		!strings.Contains(evs[0].Message, "daemon.new_key") || !strings.Contains(evs[0].Message, "a saved config") {
		t.Fatalf("config_restart_refused events = %+v", evs)
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("the refused mark stayed")
	}
	rec, ok := ReadConfigRestartRefused(h.ctx, h.st)
	if !ok || rec.SHA256 != hexSum(saved) || rec.BinaryMTime.IsZero() {
		t.Fatalf("refusal record = %+v, %v", rec, ok)
	}

	// A binary that accepts the config, and a new save: the restart goes on.
	refuse = false
	rebuild(t, bin, rebuilt.Add(time.Minute))
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 1\n", h.clock.Now())
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick: %v, want ErrRestartForConfig", err)
	}
	if checks != 2 {
		t.Fatalf("checks = %d, want 2", checks)
	}
}

// A user config on disk that no longer loads holds every idle restart, a
// landed skill version's too: launchd could not start the daemon on it.
// Fixing the file lets the restart go on.
func TestABrokenConfigOnDiskHoldsALearnRestart(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	raw, err := json.Marshal(LearnRestartMark{Version: 3, Proposal: 7, Landed: h.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, KVLearnRestart, string(raw)); err != nil {
		t.Fatal(err)
	}
	h.writeUserConfig("[daemon]\nnot_a_key = 1\n")
	for range 2 {
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v, want the daemon to keep running", err)
		}
		h.settle()
	}
	evs := h.events("daemon.config_restart_refused")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "a landed skill version") || !strings.Contains(evs[0].Message, "not_a_key") ||
		!strings.Contains(evs[0].Message, "the restart goes on once it loads") {
		t.Fatalf("config_restart_refused events = %+v, want the skill version's restart to go on once the file loads", evs)
	}
	h.writeUserConfig("[daemon]\nmax_concurrent_reviews = 2\n")
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForLearn) {
		t.Fatalf("tick on the fixed file: %v, want ErrRestartForLearn", err)
	}
}

// A refusal is this daemon's: the next start checks afresh.
func TestTheNextStartClearsARestartRefusal(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	if err := h.st.SetKV(h.ctx, KVConfigRestartRefused, `{"sha256":"0123456789","at":"2026-10-20T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadConfigRestartRefused(h.ctx, h.st); !ok {
		t.Fatal("the record does not read")
	}
	h.startup()
	if rec, ok := ReadConfigRestartRefused(h.ctx, h.st); ok {
		t.Fatalf("the refusal outlived the start: %+v", rec)
	}
}

// A saved config that no longer loads (a hand edit broke it after the save)
// is never restarted on: the daemon keeps running, says why and drops the
// mark. A newer save made meanwhile keeps its own mark.
func TestASavedConfigThatNoLongerLoadsIsNotRestartedOn(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\nnot_a_key = 1\n", h.clock.Now())
	for range 2 {
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v, want the daemon to keep running", err)
		}
		h.settle()
	}
	evs := h.events("daemon.config_restart_refused")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "no longer loads") ||
		!strings.Contains(evs[0].Message, "not_a_key") {
		t.Fatalf("config_restart_refused events = %+v", evs)
	}
	// With the mark gone nothing restarts on the file fixed by hand: the
	// event says what does, and promises no restart.
	if msg := evs[0].Message; !strings.Contains(msg, "`magnum daemon-restart --when-idle`") || !strings.Contains(msg, "save it again") ||
		strings.Contains(msg, "the restart goes on") {
		t.Fatalf("config_restart_refused says %q, want the command that restarts on the fixed file and no promise", msg)
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("the refused mark stayed")
	}

	// The refusal deletes only the mark it read.
	fresh := h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	h.e.refuseRestart(h.ctx, restartCheck{key: "x"}, "a saved config", false, `{"sha256":"0123456789"}`, errors.New("broken"), true)
	if v, ok, _ := h.st.GetKV(h.ctx, KVConfigRestart); !ok || v != fresh {
		t.Fatalf("a newer save's mark = %q, %v", v, ok)
	}
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick on the newer, valid save: %v, want ErrRestartForConfig", err)
	}
}

// A mark for the config the daemon runs already restarts nothing and is
// cleared; without launchd to start it again, in a dry run or with --once
// the daemon never exits for a saved config, and the mark stays for
// `magnum status` (outside the dry run and --once, whose start clears it).
func TestASavedConfigRestartsOnlyWhenItIsNewAndLaunchdRunsTheDaemon(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	h.saveUserConfig(userConfigText, h.clock.Now())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick on the running config's mark: %v", err)
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("the mark for the running config stayed")
	}

	for _, c := range []struct {
		name string
		mod  func(*harness)
	}{
		{"unsupervised", func(h *harness) { h.e.supervised = false }},
		{"dry run", func(h *harness) { h.e.supervised = true; h.e.d.DryRun = true }},
		{"once", func(h *harness) { h.e.supervised = true; h.e.once = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withUserConfig(userConfigText))
			h.startup()
			c.mod(h)
			h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
			for range 2 {
				if err := h.e.Tick(h.ctx); err != nil {
					t.Fatalf("tick: %v, want the daemon to keep running", err)
				}
				h.settle()
			}
			if _, ok := h.configMark(); !ok {
				t.Fatal("the mark was dropped")
			}
		})
	}
}

// withRebuiltBinary gives the daemon a launch binary that changed since its
// start, checked by check.
func withRebuiltBinary(t *testing.T, h *harness, check func() (string, error)) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.build = CurrentBuild("v1", bin)
	h.e.checkBuild = func(_ context.Context, path string) (string, error) {
		if path != bin {
			t.Errorf("checked %s, want %s", path, bin)
		}
		return check()
	}
	rebuild(t, bin, time.Now().Add(time.Hour))
	return bin
}

// A round drops its reservation before its fix cycle or whiteboard takes
// the PR again, so a tick can find nothing in flight, spend the binary's
// check in that gap and find a job running after it: the daemon stays
// until the next tick with nothing in flight.
func TestARestartWaitsForAJobThatStartsDuringItsCheck(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	started := false
	withRebuiltBinary(t, h, func() (string, error) {
		if !started {
			started = true // the fix cycle after a round reserves the PR while the check runs
			if !h.e.reserve(42, &roundHandle{cancel: func() {}, fix: true}) {
				t.Error("could not reserve the PR")
			}
		}
		return "v2", nil
	})
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v, want the daemon to stay while the fix job runs", err)
	}
	if _, ok := h.configMark(); !ok {
		t.Fatal("the mark was dropped")
	}
	h.e.unreserve(42)
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForConfig) {
		t.Fatalf("tick with nothing in flight: %v, want ErrRestartForConfig", err)
	}
}

// A refusal is checked again after restartRefuseRetry even when neither the
// file nor the binary changed (the binary's check renders the prompts, which
// can be fixed apart from both), and said once.
func TestARefusedRestartIsCheckedAgainAfterItsBackoff(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	checks, refuse := 0, true
	withRebuiltBinary(t, h, func() (string, error) {
		checks++
		if refuse {
			return "", errors.New("prompts do not render with this build: judge-initial.md")
		}
		return "v2", nil
	})
	raw, err := json.Marshal(LearnRestartMark{Version: 3, Proposal: 7, Landed: h.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, KVLearnRestart, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v, want the restart refused", err)
	}
	refuse = false // the prompt fixed: the file and the binary are as they were
	h.clock.Add(restartRefuseRetry - time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick within the backoff: %v, want no exit", err)
	}
	if checks != 1 {
		t.Fatalf("checks = %d within the backoff, want 1", checks)
	}
	h.clock.Add(2 * time.Minute)
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForLearn) {
		t.Fatalf("tick after the backoff: %v, want ErrRestartForLearn", err)
	}
	if checks != 2 {
		t.Fatalf("checks = %d, want 2", checks)
	}
	if evs := h.events("daemon.config_restart_refused"); len(evs) != 1 {
		t.Fatalf("config_restart_refused events = %d, want 1", len(evs))
	}
}

// A refusal that keeps failing past its backoff is said once for its key.
func TestARefusalThatKeepsFailingIsSaidOnce(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	checks := 0
	withRebuiltBinary(t, h, func() (string, error) { checks++; return "", errors.New("prompts do not render") })
	for range 3 {
		h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now()) // the same file saved (again)
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v, want no exit", err)
		}
		h.settle() // the reconcile it queued runs, as the heavy worker runs it: a queued job holds the restart
		h.clock.Add(restartRefuseRetry + time.Minute)
	}
	if checks != 3 {
		t.Fatalf("checks = %d, want one per backoff", checks)
	}
	if evs := h.events("daemon.config_restart_refused"); len(evs) != 1 {
		t.Fatalf("config_restart_refused events = %d, want 1", len(evs))
	}
	if _, ok := h.configMark(); ok {
		t.Fatal("a refused save's mark stayed")
	}
}

// A launch binary that is gone or cannot be read is a refusal: launchd
// could not start the daemon again.
func TestARestartIsRefusedWhenTheLaunchBinaryIsMissing(t *testing.T) {
	h := newHarness(t, withUserConfig(userConfigText))
	h.e.supervised = true
	h.startup()
	bin := withRebuiltBinary(t, h, func() (string, error) { return "v2", nil })
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	h.saveUserConfig("[daemon]\nmax_concurrent_reviews = 2\n", h.clock.Now())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v, want the restart refused", err)
	}
	evs := h.events("daemon.config_restart_refused")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "missing or cannot be read") || !strings.Contains(evs[0].Message, bin) {
		t.Fatalf("config_restart_refused events = %+v", evs)
	}
}
