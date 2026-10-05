package cli

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// TestScreenFactsReadWhatHoldsTheDaemon: the titles' facts come from the
// registry as `magnum status` reads it: the build skew (this binary's newer
// version), the pause with when it began and the requests it holds, the
// drain with its drainer, and the Codex budget's projection to codex_soft;
// launchctl is not run for them.
func TestScreenFactsReadWhatHoldsTheDaemon(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	recordBuild(t, st, engine.Build{Version: "v1.4.0", StartedAt: now.Add(-3 * time.Hour)})
	d.Version = "v1.5.0"
	d.Launchd = func(context.Context) (launchd.Info, error) {
		t.Error("launchctl ran for the titles")
		return launchd.Info{}, nil
	}
	// A weekly window 18% elapsed at 50% used: 80% comes 0.288 of a week in,
	// before the reset.
	window := 7 * 24 * time.Hour
	resets := now.Add(window - time.Duration(0.18*float64(window)))
	for k, v := range map[string]string{
		engine.KVDaemonDraining:     engine.Drain{Since: now.Add(-time.Minute), PID: 31337}.Value(),
		engine.KVUsageCodexPercent:  "50",
		engine.KVUsageCodexWindow:   strconv.Itoa(7 * 24 * 60),
		engine.KVUsageCodexResetsAt: store.FormatTime(resets),
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	f := screenFacts(ctx, d)
	want := tui.DaemonFacts{SkewOld: "v1.4.0", SkewNew: "v1.5.0 built", SkewSince: now.Add(-3 * time.Hour),
		Paused: true, PausedSince: now.Add(-19 * time.Hour), Held: 6, Draining: true, DrainerPID: 31337}
	codex := f.Codex
	f.Codex = nil
	f.SkewSince, f.PausedSince = f.SkewSince.UTC(), f.PausedSince.UTC()
	if f != want {
		t.Fatalf("facts %+v\nwant  %+v", f, want)
	}
	start := resets.Add(-window)
	at := start.Add(time.Duration(float64(now.Sub(start)) * 80 / 50))
	if codex == nil || codex.Used != 50 || codex.Cap != 80 || codex.At.Sub(at).Abs() > time.Second {
		t.Fatalf("codex %+v, want 50%% reaching 80%% at %s", codex, at)
	}

	// Once the pace reaches the soft cap only after the reset, the titles say
	// nothing of it; past it, they name the hard cap.
	if err := st.SetKV(ctx, engine.KVUsageCodexPercent, "10"); err != nil {
		t.Fatal(err)
	}
	if f := screenFacts(ctx, d); f.Codex != nil {
		t.Errorf("a pace within the window shows: %+v", f.Codex)
	}
	if err := st.SetKV(ctx, engine.KVUsageCodexPercent, "85"); err != nil {
		t.Fatal(err)
	}
	if f := screenFacts(ctx, d); f.Codex == nil || f.Codex.Cap != 95 {
		t.Errorf("past the soft cap: %+v", f.Codex)
	}
}

// TestScreenFactsNameTheBinaryOnDisk: with the same version, a binary
// rebuilt on disk is named by when it was built.
func TestScreenFactsNameTheBinaryOnDisk(t *testing.T) {
	b := engine.Build{Version: "dev", Path: "/opt/magnum/bin/magnum", ModTime: time.Date(2026, 10, 5, 17, 40, 0, 0, time.Local)}
	built := time.Date(2026, 10, 5, 18, 48, 0, 0, time.Local)
	modTime := func(string) (time.Time, bool) { return built, true }
	if got := screenSkewNew(b, "dev", modTime); got != "new build Oct 5 18:48" {
		t.Errorf("got %q", got)
	}
	if got := screenSkewNew(b, "dev", func(string) (time.Time, bool) { return time.Time{}, false }); got != "a new build" {
		t.Errorf("unreadable binary: %q", got)
	}
}

// TestStatusDashCarriesTheFacts: the dashboard's data carries the facts its
// title says.
func TestStatusDashCarriesTheFacts(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	data, err := statusDashSource(d, statusOptions{})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !data.Facts.Paused || data.Facts.Held != 6 {
		t.Fatalf("facts %+v", data.Facts)
	}
}
