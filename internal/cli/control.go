package cli

// What `magnum pause`, `magnum drain`, `magnum resume` and the status lines
// share (docs/superpowers/specs/2026-11-12-control-commands-design.md): the
// scope flags, the seams of the factory daemon and of the boot time, the
// check that a running daemon knows the controls, and each daemon's control
// state as one line (daemonControl).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// The daemons' names in the scope and the lines.
const (
	daemonReview  = "review"
	daemonFactory = "factory"
)

// controlScope is the daemons a control command acts on.
type controlScope struct{ review, factory bool }

// addScopeFlags adds --review and --factory.
func addScopeFlags(cmd *cobra.Command, s *controlScope) {
	cmd.Flags().BoolVar(&s.review, "review", false, "act on the review daemon (with --factory: both)")
	cmd.Flags().BoolVar(&s.factory, "factory", false, "act on the factory daemon (with --review: both)")
}

// resolve is the scope a command acts on: the daemons its flags name, or,
// with neither, the review daemon and the factory daemon when its launchd
// job is installed.
func (s controlScope) resolve(c *Context) controlScope {
	if s.review || s.factory {
		return s
	}
	return controlScope{review: true, factory: controlSys.FactoryInstalled(c.Layout)}
}

// both reports whether the scope names both daemons: each line then says
// which.
func (s controlScope) both() bool { return s.review && s.factory }

// controlSeams are the factory daemon's and the Mac's seams of the control
// commands; tests replace controlSys.
type controlSeams struct {
	// FactoryInstalled reports whether the factory daemon's launchd job is
	// installed (its plist exists).
	FactoryInstalled func(l paths.Layout) bool
	// FactoryPID is the running factory daemon's pid (0: none), its pidfile
	// checked with ps; FactoryAlive the cheap check of the screens (the
	// pidfile and a signal 0).
	FactoryPID   func(ctx context.Context, l paths.Layout) (int, error)
	FactoryAlive func(l paths.Layout) int
	// FactoryKick wakes the factory daemon (SIGUSR1).
	FactoryKick func(l paths.Layout) (int, error)
	// Boot reads the Mac's boot (kern.bootsessionuuid, kern.boottime).
	Boot func(ctx context.Context) (engine.BootID, error)
}

var controlSys = controlSeams{
	FactoryInstalled: func(paths.Layout) bool {
		home, err := daemonSys.UserHome()
		return err == nil && fsx.Exists(launchd.AgentPath(home, factory.LaunchLabel))
	},
	FactoryPID: func(ctx context.Context, l paths.Layout) (int, error) {
		pid, err := daemonSys.factoryPID(l)
		if err != nil || pid == 0 {
			return 0, err
		}
		comm, args, err := engine.ProcessCommand(ctx, daemonSys.runner(false, nil), pid)
		if err == nil && !factory.LooksLikeFactoryDaemon(comm, args) {
			return 0, nil
		}
		return pid, nil
	},
	FactoryAlive: factoryAlive,
	FactoryKick:  factoryKickNow,
	Boot: func(ctx context.Context) (engine.BootID, error) {
		return engine.ReadBoot(ctx, daemonSys.runner(false, nil))
	},
}

// factoryKickNow wakes the factory daemon through daemonSys as it is at the
// call (a method value would keep the one of the package's start).
func factoryKickNow(l paths.Layout) (int, error) { return daemonSys.factoryKick(l) }

// controlReviewPID is the running review daemon's pid (0: none), read
// without waking it when the deps can (Running), else through the kick.
func controlReviewPID(c *Context, d *actDeps) int {
	if d.Running != nil {
		pid, err := d.Running()
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum: %v\n", err)
			return 0
		}
		return pid
	}
	return pauseDaemonPID(c, d)
}

// reviewKnowsControls reports whether the review daemon pid recorded that it
// knows the controls (engine.KVDaemonControls).
func reviewKnowsControls(ctx context.Context, d *actDeps, pid int) bool {
	v, _, err := d.Store.GetKV(ctx, engine.KVDaemonControls)
	return err == nil && engine.KnowsControls(v, pid, engine.ControlsVersion)
}

// factoryKnowsControls reports whether the factory daemon pid's record
// (work.json) says that its build knows the controls.
func factoryKnowsControls(l paths.Layout, pid int) bool {
	rec, ok, err := factory.ReadWork(l)
	return err == nil && ok && rec.PID == pid && rec.Controls >= engine.ControlsVersion
}

// oldBuildText says that daemon name (pid) runs a build that does not know
// what (`magnum drain`, the factory's pause) and how to go on: the command
// claims no hold that would not be in force.
func oldBuildText(name string, pid int, what string) string {
	fix := "magnum daemon-restart --drain"
	if name == daemonFactory {
		fix = "magnum factory install --drain"
	}
	return fmt.Sprintf("the %s daemon (pid %d) runs a build that does not know %s: restart it on this build first (`%s`)",
		name, pid, what, fix)
}

// controlOut prints a control command's lines, each with its daemon's name
// when the scope names both.
type controlOut struct {
	c    *Context
	cmd  string
	both bool
}

// say prints msg about daemon name on stdout.
func (o controlOut) say(name, msg string) {
	if o.both {
		msg = name + ": " + msg
	}
	fmt.Fprintln(o.c.Stdout, msg)
}

// fail prints msg about daemon name on stderr, as the command's.
func (o controlOut) fail(name, msg string) {
	if o.both {
		msg = name + ": " + msg
	}
	fmt.Fprintf(o.c.Stderr, "magnum %s: %s\n", o.cmd, msg)
}

// prefixLines writes to w with prefix at the start of each line: the review
// daemon's own lines when a command acts on both daemons.
func prefixLines(w io.Writer, prefix string) io.Writer {
	return &prefixWriter{w: w, prefix: prefix, start: true}
}

type prefixWriter struct {
	w      io.Writer
	prefix string
	start  bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	var out []byte
	for _, ch := range b {
		if p.start {
			out = append(out, p.prefix...)
			p.start = false
		}
		out = append(out, ch)
		if ch == '\n' {
			p.start = true
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// daemonControl is a daemon's state as `magnum status --json` and `magnum
// factory status --json` list it (daemons): whether it runs, its pid and
// build, `magnum pause`'s pause and a drain (for a reboot, else for a
// restart) in force.
type daemonControl struct {
	Name    string        `json:"name"`
	Running bool          `json:"running"`
	PID     int           `json:"pid,omitempty"`
	Build   string        `json:"build,omitempty"`
	Pause   *controlPause `json:"pause,omitempty"`
	Drain   *controlDrain `json:"drain,omitempty"`
	// Controls says what the running daemon knows of the controls when it is
	// not known to hold them ("" when it does): controlsOlder, its build does
	// not know them and ignores the factory's pause and a drain for a reboot;
	// controlsStarting, it has not recorded yet what it knows (its pidfile
	// comes first); controlsUnknown, its record cannot be read.
	Controls string `json:"controls,omitempty"`
}

// The values of daemonControl.Controls.
const (
	controlsOlder    = "older"
	controlsStarting = "starting"
	controlsUnknown  = "unknown"
)

// controlPause is a pause in force: since when, until when (nil: until
// `magnum resume`) and why; Unreadable, a factory pause.json the CLI cannot
// read or parse (the daemon holds a pause for it).
type controlPause struct {
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Unreadable bool       `json:"unreadable,omitempty"`
}

// controlDrain is a drain in force: for a reboot (`magnum drain`) or a
// restart (`daemon-restart --drain`, `factory install --drain`), since
// when, until what (a reboot's: "boot" or "resume"), why, and the
// daemon's acknowledgement of a reboot's: whether the running daemon holds
// it, is quiet and parked its idle agents, and the work that still runs.
type controlDrain struct {
	For          string    `json:"for"`
	Unreadable   bool      `json:"unreadable,omitempty"`
	Since        time.Time `json:"since"`
	Until        string    `json:"until,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Acknowledged bool      `json:"acknowledged"`
	Quiet        bool      `json:"quiet"`
	Parked       bool      `json:"parked"`
	Work         []string  `json:"work,omitempty"`
}

// The drains' kinds (controlDrain.For).
const (
	drainForReboot  = "reboot"
	drainForRestart = "restart"
)

// reviewControlOf is the review daemon's state from its kv rows (get) and
// its process (running, pid).
func reviewControlOf(get func(key string) (string, bool), running bool, pid int) daemonControl {
	dc := daemonControl{Name: daemonReview, Running: running, PID: pid}
	if v, ok := get(engine.KVDaemonBuild); ok {
		var b engine.Build
		if json.Unmarshal([]byte(v), &b) == nil {
			dc.Build = b.Label()
		}
	}
	if v, _ := get(store.KVDaemonPaused); v == "1" {
		p := &controlPause{}
		p.Reason, _ = get(store.KVDaemonPausedReason)
		for _, f := range []struct {
			key string
			to  **time.Time
		}{{store.KVDaemonPausedUntil, &p.Until}, {engine.KVDaemonPausedAt, &p.Since}} {
			if v, ok := get(f.key); ok {
				if t, err := store.ParseTime(v); err == nil {
					*f.to = &t
				}
			}
		}
		dc.Pause = p
	}
	reboot, _ := get(engine.KVDaemonRebootDrain)
	ack, _ := get(engine.KVDaemonDrainAck)
	controls, _ := get(engine.KVDaemonControls)
	if running && pid > 0 && !engine.KnowsControls(controls, pid, engine.ControlsVersion) {
		dc.Controls = controlsOlder
		if v, _ := get(store.KVDaemonPid); v != strconv.Itoa(pid) { // its start has not recorded its pid yet
			dc.Controls = controlsStarting
		}
	}
	if dr := rebootDrainOf(reboot, ack, pid); dr != nil {
		dc.Drain = dr
	} else if v, ok := get(engine.KVDaemonDraining); ok {
		if d, ok := engine.ParseDrain(v); ok {
			dc.Drain = &controlDrain{For: drainForRestart, Since: d.Since}
		}
	}
	return dc
}

// factoryControlOf is the factory daemon's state from its files and its
// process (pid: 0 when it does not run).
func factoryControlOf(l paths.Layout, pid int) daemonControl {
	dc := daemonControl{Name: daemonFactory, Running: pid > 0, PID: pid}
	rec, ok, err := factory.ReadWork(l)
	known := err == nil && ok && rec.PID == pid
	switch {
	case pid <= 0:
	case err != nil || (ok && rec.PID != pid):
		dc.Controls = controlsUnknown // a record it cannot read, or another daemon's
	case !ok || rec.Controls < engine.ControlsVersion:
		dc.Controls = controlsOlder
	}
	if known {
		dc.Build = rec.Build
	}
	if v, err := factory.ReadPause(l); err != nil {
		dc.Pause = &controlPause{Unreadable: true} // the daemon holds a pause for it, as it says: shown, with its remedy
	} else if p, ok := factory.ParsePause(v); ok {
		cp := &controlPause{Since: &p.Since, Reason: p.Reason}
		if !p.Until.IsZero() {
			cp.Until = &p.Until
		}
		dc.Pause = cp
	} else if v != "" {
		dc.Pause = &controlPause{Unreadable: true} // content it cannot parse holds a pause too
	}
	ack := ""
	if known && rec.RebootDrain != nil {
		ack = rec.RebootDrain.Value()
	}
	reboot, err := factory.ReadRebootDrain(l)
	if err != nil {
		dc.Drain = &controlDrain{For: drainForReboot, Unreadable: true} // the daemon holds a drain it cannot read
	} else if dr := rebootDrainOf(reboot, ack, pid); dr != nil {
		dc.Drain = dr
	} else if v, _ := factory.ReadDrain(l); v != "" {
		if d, ok := factory.ParseDrain(v); ok {
			dc.Drain = &controlDrain{For: drainForRestart, Since: d.Since}
		}
	}
	return dc
}

// rebootDrainOf is a drain for a reboot (its record's value) with the
// acknowledgement of the daemon pid (ack's value); nil when none holds. A
// record the daemons cannot read holds them until `magnum resume`: it is an
// unreadable drain.
func rebootDrainOf(record, ack string, pid int) *controlDrain {
	d, ok := engine.ParseRebootDrain(record)
	if !ok {
		if strings.TrimSpace(record) != "" {
			return &controlDrain{For: drainForReboot, Unreadable: true}
		}
		return nil
	}
	cd := &controlDrain{For: drainForReboot, Since: d.Since, Until: d.Until, Reason: d.Reason}
	if a, ok := engine.ParseDrainAck(ack); ok && a.Generation == d.Generation && a.PID == pid && pid > 0 {
		cd.Acknowledged, cd.Quiet, cd.Parked, cd.Work = true, a.Quiet, a.Parked, a.Work
	}
	return cd
}

// holdText says dc's pause and drain, as the status lines, the titles and the
// factory screen's blocker line say them ("" for none): "paused by you
// until 15:30 (lunch): magnum resume", "draining since 14:02: magnum resume
// ends it", with what still runs when long (the drain's work, and whether
// it is quiet).
func (dc daemonControl) holdText(now time.Time, long bool) string {
	var parts []string
	resume := "magnum resume"
	if dc.Name == daemonFactory {
		resume += " --factory" // it removes a record the daemon cannot read, its plist installed or not
	}
	if d := dc.Drain; d != nil {
		switch {
		case d.Unreadable:
			parts = append(parts, "draining (unreadable record): "+resume+" ends it")
		case d.For == drainForReboot:
			s := "draining since " + inspClock(now, d.Since) + ": magnum resume ends it"
			switch {
			case !long:
			case !dc.Running:
			case !d.Acknowledged:
				s += " · not held yet"
			case d.Quiet && d.Parked:
				s += " · quiet: safe to reboot"
			case d.Quiet && len(d.Work) > 0:
				s += " · quiet; cannot park yet: " + strings.Join(d.Work, ", ")
			case d.Quiet:
				s += " · quiet, parking its idle agents"
			default:
				s += " · waits for " + strings.Join(d.Work, ", ")
			}
			parts = append(parts, s)
		default:
			parts = append(parts, "draining for a restart since "+inspClock(now, d.Since))
		}
	}
	if p := dc.Pause; p != nil && p.Unreadable {
		parts = append(parts, "paused (unreadable record): "+resume+" removes it")
	} else if p != nil {
		s := "paused by you"
		if p.Until != nil {
			s += " until " + inspClock(now, *p.Until)
		}
		if p.Reason != "" {
			s += " (" + p.Reason + ")"
		}
		parts = append(parts, s+": magnum resume")
	}
	if dc.controlled() {
		switch dc.Controls {
		case controlsOlder:
			parts = append(parts, oldBuildText(dc.Name, dc.PID, "the pause and the drain for a reboot, so it holds neither"))
		case controlsStarting:
			parts = append(parts, fmt.Sprintf("the %s daemon (pid %d) is starting: whether it holds this shows once it recorded what it knows",
				dc.Name, dc.PID))
		case controlsUnknown:
			parts = append(parts, fmt.Sprintf("the build of the %s daemon (pid %d) cannot be read: whether it holds this is unknown", dc.Name, dc.PID))
		}
	}
	return actClean(strings.Join(parts, " · "))
}

// controlled reports whether dc's line names a hold of the controls (the
// factory's pause or a drain for a reboot on either daemon; the review
// daemon's pause is older than the controls) while its daemon is not known
// to hold them (Controls).
func (dc daemonControl) controlled() bool {
	if dc.Controls == "" {
		return false
	}
	reboot := dc.Drain != nil && dc.Drain.For == drainForReboot
	return reboot || (dc.Name == daemonFactory && dc.Pause != nil)
}

// line is dc as `magnum status` and `magnum factory status` print it:
// "review   running · pid 4242 · paused by you until 15:30 (lunch): magnum
// resume".
func (dc daemonControl) line(now time.Time) string {
	parts := []string{"not running"}
	if dc.Running {
		parts = []string{"running", fmt.Sprintf("pid %d", dc.PID)}
	}
	if h := dc.holdText(now, true); h != "" {
		parts = append(parts, h)
	}
	return fmt.Sprintf("%-8s %s", dc.Name, strings.Join(parts, " · "))
}

// controlHoldLines are the screens' lines of the daemons with a pause or a
// drain: short ("factory draining since 14:02: magnum resume ends it") and
// long (with what still runs).
func controlHoldLines(dcs []daemonControl, now time.Time) (short, long []string) {
	for _, dc := range dcs {
		if s := dc.holdText(now, false); s != "" {
			short = append(short, dc.Name+" "+s)
			long = append(long, dc.Name+" "+dc.holdText(now, true))
		}
	}
	return short, long
}

// storeKV reads st's kv rows for reviewControlOf.
func storeKV(ctx context.Context, st *store.Store) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if st == nil {
			return "", false
		}
		v, ok, err := st.GetKV(ctx, key)
		return v, ok && err == nil
	}
}

// controlKVKeys are the review daemon's kv rows reviewControlOf reads.
var controlKVKeys = []string{engine.KVDaemonBuild, store.KVDaemonPaused, store.KVDaemonPausedReason, store.KVDaemonPausedUntil,
	engine.KVDaemonPausedAt, engine.KVDaemonRebootDrain, engine.KVDaemonDrainAck, engine.KVDaemonDraining, engine.KVDaemonControls,
	store.KVDaemonPid}

// reviewControlRO is the review daemon's state read from its registry
// opened read-only (the factory's status and screen, which open no
// registry of magnum's); no registry yet is no pause and no drain.
func reviewControlRO(ctx context.Context, c *Context) daemonControl {
	pid, _ := daemonSys.DaemonPID(c.Layout)
	if pid > 0 && errors.Is(daemonSys.Kill(pid, 0), syscall.ESRCH) {
		pid = 0
	}
	rows := map[string]string{}
	if db, err := c.registryReadOnly(); err == nil && db != nil {
		defer db.Close()
		readControlKV(ctx, db, rows)
	}
	return reviewControlOf(func(k string) (string, bool) { v, ok := rows[k]; return v, ok }, pid > 0, pid)
}

// readControlKV reads controlKVKeys from db into rows (each a daemon.* row);
// a failed read leaves what it read.
func readControlKV(ctx context.Context, db *sql.DB, rows map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	rs, err := db.QueryContext(ctx, "SELECT key, value FROM kv WHERE key GLOB 'daemon.*'")
	if err != nil {
		return
	}
	defer rs.Close()
	for rs.Next() {
		var k, v string
		if rs.Scan(&k, &v) == nil && slices.Contains(controlKVKeys, k) {
			rows[k] = v
		}
	}
}
