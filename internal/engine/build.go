package engine

// The daemon's build. A rebuild without a restart left the daemon running
// an older binary for hours with nothing saying so (a post-merge review the
// new build allowed was refused by the old one). The daemon records what it
// runs at startup (KVDaemonBuild), so `magnum status`, the CLI's request
// replies and the screens can tell the binary on disk or the CLI is newer
// (SkewNote). With [daemon] restart_on_new_build every reconcile also looks
// at the binary on disk, and once a new one passes its configuration check
// the daemon exits at the first tick with no round in flight, for launchd to
// start the new build; dispatch never stops for it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

const (
	// KVDaemonBuild is the running daemon's Build as JSON, written at
	// startup.
	KVDaemonBuild = "daemon.build"
	// kvRestartForBuild is the label of the build a daemon exited from for
	// a new one (restart_on_new_build); the next start reports and deletes
	// it (daemon.restarted_for_build).
	kvRestartForBuild = "daemon.restart_for_build"
)

// ErrRestartForBuild is what Tick and Run return when the daemon exits for a
// new build on disk ([daemon] restart_on_new_build): the exit is non-zero, so
// launchd starts the new binary.
var ErrRestartForBuild = errors.New("a new build is on disk and no round is in flight; exiting for launchd to start it")

// Build identifies a magnum binary: the version `make build` stamps
// (main.version), the commit Go recorded (vcs.revision), and the binary
// launchd starts (paths.Layout.Binary) with its modification time.
type Build struct {
	Version   string    `json:"version"`
	Revision  string    `json:"revision,omitempty"`
	Path      string    `json:"path,omitempty"`
	ModTime   time.Time `json:"mtime,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// CurrentBuild is this process's build: version as stamped, the revision
// from the binary's build info, and path with its current mtime (both left
// empty when path is "" or cannot be read).
func CurrentBuild(version, path string) Build {
	b := Build{Version: version}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				b.Revision = s.Value
			}
		}
	}
	if path != "" {
		if fi, err := os.Stat(path); err == nil {
			b.Path, b.ModTime = path, fi.ModTime()
		}
	}
	return b
}

// Label is how messages name the build: its version, with the commit when
// the version does not say it ("dev (8ad5bb2)").
func (b Build) Label() string {
	v := b.Version
	if v == "" {
		v = "unknown"
	}
	if b.Revision != "" && (v == "dev" || v == "unknown") {
		r := b.Revision
		if len(r) > 7 {
			r = r[:7]
		}
		v += " (" + r + ")"
	}
	return v
}

// ReadDaemonBuild reads the build the daemon recorded at startup (false
// when none did: a daemon older than the record, or none ever ran).
func ReadDaemonBuild(ctx context.Context, st *store.Store) (Build, bool) {
	v, ok, err := st.GetKV(ctx, KVDaemonBuild)
	if err != nil || !ok || v == "" {
		return Build{}, false
	}
	var b Build
	if json.Unmarshal([]byte(v), &b) != nil {
		return Build{}, false
	}
	return b, true
}

// SkewNote says when the running daemon is older than what is built: the
// CLI's version (cliVersion) differs from the daemon's, or the daemon's
// binary on disk changed since it started (its mtime; modTime reads it, false
// when it cannot). "" when neither. A version that is empty or "dev" (a build
// `make build` did not stamp) is not compared.
func SkewNote(daemon Build, cliVersion string, modTime func(path string) (time.Time, bool)) string {
	head := "daemon runs " + daemon.Label()
	if !daemon.StartedAt.IsZero() {
		head += " since " + daemon.StartedAt.Local().Format("2006-01-02 15:04")
	}
	fix := ": `magnum daemon-restart`"
	stamped := func(v string) bool { return v != "" && v != "dev" }
	if stamped(cliVersion) && stamped(daemon.Version) && cliVersion != daemon.Version {
		return head + "; " + cliVersion + " is built" + fix
	}
	if daemon.Path != "" && !daemon.ModTime.IsZero() && modTime != nil {
		if mt, ok := modTime(daemon.Path); ok && !mt.Equal(daemon.ModTime) {
			return head + fmt.Sprintf("; a new build is on disk (%s, built %s)", daemon.Path, mt.Local().Format("2006-01-02 15:04")) + fix
		}
	}
	return ""
}

// FileModTime is SkewNote's reader of a binary's mtime on disk.
func FileModTime(path string) (time.Time, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// recordBuild writes the build at startup and reports a restart for a new
// build the previous daemon exited for.
func (e *Engine) recordBuild(ctx context.Context) {
	b := e.build
	b.StartedAt = e.now()
	if raw, err := json.Marshal(b); err == nil {
		e.setKV(ctx, KVDaemonBuild, string(raw))
	}
	if from, ok := e.getKV(ctx, kvRestartForBuild); ok {
		e.delKV(ctx, kvRestartForBuild)
		e.event(ctx, "info", "", "daemon.restarted_for_build",
			fmt.Sprintf("restarted for a new build: %s → %s (restart_on_new_build)", from, b.Label()), nil)
	}
}

// buildWatch is what the daemon knows about new builds on disk
// (restart_on_new_build).
type buildWatch struct {
	seen    time.Time // the mtime last looked at (checked or not)
	pending time.Time // the mtime of the build that passed its check; zero = no restart pending
	version string    // that build's version
	manual  bool      // the restart was left to the user (no launchd)
}

// noteBuild looks at the daemon's binary on disk (each reconcile). A new one
// is checked once (Options.CheckBuild: its version, then its configuration
// check); when it passes and [daemon] restart_on_new_build is on, the restart
// is pending (daemon.restart_pending) until restartForBuild finds no round in
// flight. Without launchd to start it again the event says to restart by hand.
func (e *Engine) noteBuild(ctx context.Context) {
	b := e.build
	if !e.cfg.Daemon.RestartOnNewBuild || e.d.DryRun || e.once || b.Path == "" || b.ModTime.IsZero() || e.checkBuild == nil {
		return
	}
	mt, ok := FileModTime(b.Path)
	if !ok || mt.Equal(b.ModTime) || mt.Equal(e.builds.seen) {
		return
	}
	e.builds = buildWatch{seen: mt}
	version, err := e.checkBuild(ctx, b.Path)
	if err != nil {
		e.event(ctx, "warn", "", "daemon.build_rejected",
			fmt.Sprintf("a new build is on disk (%s, built %s) but fails its check, so the daemon keeps running %s: %v",
				b.Path, mt.Local().Format("15:04:05"), b.Label(), err), nil)
		return
	}
	e.builds.pending, e.builds.version = mt, version
	msg := fmt.Sprintf("a new build is on disk (%s %s, built %s) and passed its check: the daemon restarts on it at the first tick with no round in flight",
		b.Path, version, mt.Local().Format("15:04:05"))
	if !e.supervised {
		e.builds.manual = true
		msg = fmt.Sprintf("a new build is on disk (%s %s, built %s) and passed its check, but launchd does not run this daemon, so nothing would start it again: restart it by hand (`magnum daemon-restart`)",
			b.Path, version, mt.Local().Format("15:04:05"))
	}
	e.event(ctx, "info", "", "daemon.restart_pending", msg, map[string]any{"from": b.Label(), "to": version})
}

// restartForBuild reports ErrRestartForBuild when a checked new build waits
// (noteBuild) and no round is claiming, reviewing or verifying, in the
// registry or in this process. The binary must still be the one checked: a
// newer one is checked again on the next reconcile.
func (e *Engine) restartForBuild(ctx context.Context) error {
	w := e.builds
	if w.pending.IsZero() || w.manual {
		return nil
	}
	if mt, ok := FileModTime(e.build.Path); !ok || !mt.Equal(w.pending) {
		e.builds = buildWatch{}
		return nil
	}
	if e.activeRounds() > 0 {
		return nil
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: store.InFlightStates})
	if err != nil || len(prs) > 0 {
		return nil
	}
	e.setKV(ctx, kvRestartForBuild, e.build.Label())
	e.log.Info(ErrRestartForBuild.Error(), "from", e.build.Label(), "to", w.version, "path", e.build.Path)
	return ErrRestartForBuild
}
