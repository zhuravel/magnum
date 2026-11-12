package cli

// The daemons' lines of `magnum status`, `magnum factory status`, the
// titles of the board and the dashboard, and the factory screen's blocker
// line (control.go has their state): one line per daemon, with `magnum
// pause`'s pause and a drain in force, loud while a drain holds.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/tui"
)

// statusFactoryControl is the factory daemon's line for `magnum status`
// (statusDeps.Factory): shown while its launchd job is installed, it runs,
// or a pause or a drain of its holds.
func statusFactoryControl(l paths.Layout) func(ctx context.Context) (daemonControl, bool) {
	return func(ctx context.Context) (daemonControl, bool) {
		pid, _ := controlSys.FactoryPID(ctx, l)
		dc := factoryControlOf(l, pid)
		return dc, pid > 0 || dc.Pause != nil || dc.Drain != nil || controlSys.FactoryInstalled(l) || fsx.Exists(l.FactoryWork())
	}
}

// statusGatherControls fills the report's daemons: the review daemon's from
// its kv rows and the daemon block, the factory daemon's when the deps read
// it.
func statusGatherControls(ctx context.Context, d statusDeps, r *statusReport) {
	r.Daemons = []daemonControl{reviewControlOf(storeKV(ctx, d.Store), r.Daemon.Running, r.Daemon.PID)}
	if d.Factory != nil {
		if dc, ok := d.Factory(ctx); ok {
			r.Daemons = append(r.Daemons, dc)
		}
	}
}

// statusRenderDaemons prints one line per daemon.
func statusRenderDaemons(w io.Writer, r statusReport) {
	for _, dc := range r.Daemons {
		fmt.Fprintln(w, statusSafe(dc.line(r.GeneratedAt), 0))
	}
}

// screenControlFacts are the titles' facts of the daemons' controls: a drain
// for a reboot of either daemon ("review draining since 14:02: magnum resume
// ends it"), and the factory's pause (the review daemon's is the Paused
// fact).
func screenControlFacts(ctx context.Context, d statusDeps, r statusReport, now time.Time) []tui.ControlFact {
	dcs := []daemonControl{reviewControlOf(storeKV(ctx, d.Store), r.Daemon.Running, r.Daemon.PID)}
	dcs[0].Pause = nil
	if dcs[0].Drain != nil && dcs[0].Drain.For != drainForReboot {
		dcs[0].Drain = nil // the Draining fact says a restart's
	}
	if d.Factory != nil {
		if dc, ok := d.Factory(ctx); ok {
			dcs = append(dcs, dc)
		}
	}
	var out []tui.ControlFact
	for _, dc := range dcs {
		full := dc.holdText(now, false)
		if full == "" {
			continue
		}
		short := dc.Name + " paused"
		if dc.Drain != nil {
			short = dc.Name + " draining"
		}
		out = append(out, tui.ControlFact{Full: dc.Name + " " + full, Short: short})
	}
	return out
}

// screenFactoryControl is the factory daemon's line for the screens'
// titles: its pidfile checked with a signal 0 (no ps with every refresh).
func screenFactoryControl(l paths.Layout) func(ctx context.Context) (daemonControl, bool) {
	return func(context.Context) (daemonControl, bool) {
		dc := factoryControlOf(l, controlSys.FactoryAlive(l))
		return dc, dc.Pause != nil || dc.Drain != nil
	}
}

// factoryScreenControlLines are the factory screen's blocker lines of the
// daemons with a pause or a drain, short and long, the review daemon's read
// from its registry read-only, the factory daemon's (fpid: 0 when none
// runs) from its files.
func factoryScreenControlLines(ctx context.Context, c *Context, fpid int, now time.Time) (short, long []string) {
	dcs := []daemonControl{reviewControlRO(ctx, c), factoryControlOf(c.Layout, fpid)}
	return controlHoldLines(dcs, now)
}

// factoryStatusDaemons are `magnum factory status`'s daemons: the review
// daemon's from its registry read-only, the factory's from its files.
func factoryStatusDaemons(ctx context.Context, c *Context, pid int) []daemonControl {
	return []daemonControl{reviewControlRO(ctx, c), factoryControlOf(c.Layout, pid)}
}

// factoryHoldShown reports whether factory status prints hold h on a line
// of its own: the pause and the drain for a reboot are the daemons' lines.
func factoryHoldShown(h factory.Hold) bool {
	return h.Kind != factory.HoldPause && h.Kind != factory.HoldReboot
}
