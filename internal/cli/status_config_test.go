package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/launchd"
)

// A running daemon whose user config differs from the one it loaded at its
// start shows a config line: when the file changed and how the change
// applies (the daemon's idle restart on a saved config when launchd runs
// it, else a restart by hand). Nothing when the file is the one loaded,
// when no daemon recorded a sum or when no daemon runs.
func TestStatusSaysTheConfigChangedSinceTheDaemonStarted(t *testing.T) {
	f, st, d, now := statusFixture(t)
	ctx := context.Background()
	user := filepath.Join(f.Home, "user-config.toml")
	d.Layout.UserConfig = user
	write := func(text string) string {
		t.Helper()
		if err := os.WriteFile(user, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := now.Add(-25 * time.Minute)
		if err := os.Chtimes(user, modified, modified); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])
	}
	setKV := func(k, v string) {
		t.Helper()
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	render := func() (statusReport, string, string) {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return r, b.String(), string(raw)
	}
	noLine := func(what string) {
		t.Helper()
		if r, out, raw := render(); r.Daemon.Config != nil || strings.Contains(out, "config:") || strings.Contains(raw, "config_changed") {
			t.Fatalf("%s: config = %+v\n%s", what, r.Daemon.Config, out)
		}
	}

	loaded := write("[daemon]\nmax_concurrent_reviews = 3\n")
	noLine("no sum recorded")
	setKV(engine.KVDaemonConfigSum, loaded)
	noLine("the file the daemon loaded")

	saved := write("[daemon]\nmax_concurrent_reviews = 2\n")
	head := "config:   " + inspTilde(user) + " changed since the daemon started (modified " + inspClock(now, now.Add(-25*time.Minute)) + "); "
	r, out, raw := render()
	actContains(t, out, head+"applies after a restart: `magnum daemon-restart --when-idle`\n")
	if c := r.Daemon.Config; c == nil || c.Path != user || c.ModTime == nil || c.Restart != nil || c.Supervised == nil || !*c.Supervised ||
		!strings.Contains(raw, `"config_changed":{"path":`) {
		t.Fatalf("config = %+v\n%s", r.Daemon.Config, raw)
	}
	if i, j := strings.Index(out, "config:"), strings.Index(out, "daemon:"); i < j {
		t.Errorf("the config line comes before the daemon line:\n%s", out)
	}

	// A save asked for the idle restart, and launchd runs the daemon.
	mark, _ := json.Marshal(engine.ConfigRestartMark{SHA256: saved, SavedAt: now.Add(-25 * time.Minute)})
	setKV(engine.KVConfigRestart, string(mark))
	r, out, raw = render()
	actContains(t, out, head+"the daemon restarts on it when nothing is in flight\n")
	if c := r.Daemon.Config; c == nil || c.Restart == nil || c.Restart.SHA256 != saved || !strings.Contains(raw, `"restart":{"sha256":"`+saved+`"`) {
		t.Fatalf("config = %+v\n%s", r.Daemon.Config, raw)
	}

	// The daemon refused to restart on this very file (it does not load with
	// the build on disk): the line says to fix it, never to restart onto it.
	refusal := func(sha string) string {
		raw, _ := json.Marshal(engine.ConfigRestartRefusal{SHA256: sha, At: now.Add(-time.Minute)})
		return string(raw)
	}
	setKV(engine.KVConfigRestartRefused, refusal(saved))
	r, out, raw = render()
	actContains(t, out, head+"the daemon will not restart on it: it does not load with the build on disk; fix it (`magnum config` shows why)\n")
	if strings.Contains(out, "--when-idle") || r.Daemon.Config == nil || r.Daemon.Config.Refused == nil ||
		!strings.Contains(raw, `"refused":{"sha256":"`+saved+`"`) {
		t.Fatalf("config = %+v\n%s\n%s", r.Daemon.Config, out, raw)
	}
	// A refusal of another file (an earlier one) says nothing about this one.
	setKV(engine.KVConfigRestartRefused, refusal(loaded))
	r, out, _ = render()
	actContains(t, out, head+"the daemon restarts on it when nothing is in flight\n")
	if r.Daemon.Config.Refused != nil {
		t.Fatalf("an older refusal shown: %+v", r.Daemon.Config.Refused)
	}

	// launchd does not run this daemon: nothing starts it again.
	d.Launchd = func(context.Context) (launchd.Info, error) { return launchd.Info{State: launchd.NotLoaded}, nil }
	_, out, _ = render()
	actContains(t, out, head+"applies after a restart: `magnum daemon-restart --when-idle`\n")
	// launchd cannot be asked.
	d.Launchd = nil
	r, out, _ = render()
	actContains(t, out, head+"the daemon restarts on it when idle (if launchd runs it)\n")
	if r.Daemon.Config == nil || r.Daemon.Config.Supervised != nil {
		t.Fatalf("config = %+v", r.Daemon.Config)
	}

	// A file removed since the start differs from the one loaded; a daemon
	// that loaded none matches a missing file.
	if err := os.Remove(user); err != nil {
		t.Fatal(err)
	}
	_, out, _ = render()
	actContains(t, out, "config:   "+inspTilde(user)+" changed since the daemon started (removed); ")
	setKV(engine.KVDaemonConfigSum, "")
	noLine("no user config, none loaded")

	// No daemon runs: the next one loads the file afresh.
	write("[daemon]\nmax_concurrent_reviews = 2\n")
	d.DaemonPID = func() (int, error) { return 0, nil }
	noLine("no daemon")
}
