package cli

// `magnum drain`: a drain for a reboot of both daemons (docs/superpowers/
// specs/2026-11-12-control-commands-design.md). It writes the record
// (engine.RebootDrain: the review daemon's kv row, the factory's
// reboot-drain.json), which no process owns, wakes the daemons, and waits
// until each acknowledged this generation, is quiet and parked its idle
// agents; then it says the reboot is safe. Ctrl-C and the timeout leave
// the drain in force; `magnum resume` lifts it, and the first start of a
// daemon after the reboot ends it (unless --keep).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
)

const (
	drainUsage = "drain [--review] [--factory] [--keep] [--timeout 3h30m] [--no-wait] [--reason text | reason…]"
	// rebootDrainTimeout is drain's default --timeout: past the local review
	// round's cap, as factory install's.
	rebootDrainTimeout = factoryDrainTimeout
	// rebootDrainKickEvery is how often the wait wakes the daemons (the
	// review daemon's tick polls GitHub).
	rebootDrainKickEvery = time.Minute
	// drainSafeText and drainSafeKeepText end a drain that went quiet.
	drainSafeText     = "quiet: safe to reboot. After the reboot, the daemons' first start ends the drain. magnum resume ends it now."
	drainSafeKeepText = "quiet: safe to reboot. The drain lasts until magnum resume."
)

type drainOpts struct {
	scope   controlScope
	keep    bool
	noWait  bool
	timeout time.Duration
	reason  string
}

func newDrainCmd(c *Context) *cobra.Command {
	var o drainOpts
	cmd := newCommand(groupAct, drainUsage, "drain both daemons for a reboot: nothing new starts, then wait until they are quiet",
		"Drain the review daemon and the factory daemon for a reboot of the Mac (the factory only while its launchd job "+
			"is installed; --review or --factory narrows it): nothing new starts, not even what you ask for (a review, "+
			"a fix, a factory launch, prompt, push, check, claim, talk or QA run), while status, resume, abort and stop "+
			"still work. The command waits until each daemon held the drain, nothing it runs is in flight (the review "+
			"daemon's rounds, fix, merge and whiteboard jobs, background jobs, slot jobs and staging resolves; the "+
			"factory's local review rounds, checks, claims, talks, QA runs and every agent's turn, the liaisons' too) "+
			"and it parked its idle agents, printing a line per daemon every 15s, for at most --timeout (default "+
			rebootDrainTimeout.String()+"); then it says \"safe to reboot\" and exits 0. The drain is a record no "+
			"process owns (the review registry's daemon.reboot_drain row, the factory's reboot-drain.json): ctrl+c or "+
			"the timeout leave it in force and exit 1 (`magnum drain` waits again, `magnum resume` allows new work), and "+
			"--no-wait sets it and returns at once. Each daemon's first start after the Mac rebooted ends it; a restart "+
			"on the same boot (a crash, a new build, a restart by command) keeps it, and --keep makes it last until "+
			"`magnum resume`. While it holds, `magnum status`, `magnum factory status`, the board's title and the "+
			"factory screen say \"draining since <time>: magnum resume ends it\", and after an hour quiet with no "+
			"reboot each daemon says once that it still holds. A daemon of an older build does not know the drain: the "+
			"command says to restart it on this build first and drains nothing. A restart's drain (`daemon-restart "+
			"--drain`, `factory install --drain`) is apart: neither lifts the other.",
		func(pos []string) int { return runDrain(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.keep, "keep", false, "keep the drain until magnum resume, past the reboot")
	fs.BoolVar(&o.noWait, "no-wait", false, "set the drain and return at once")
	fs.DurationVar(&o.timeout, "timeout", rebootDrainTimeout, "how long to wait for the daemons to go quiet (the drain stays after it)")
	fs.StringVar(&o.reason, "reason", "", "why (shown in magnum status)")
	addScopeFlags(cmd, &o.scope)
	return cmd
}

func runDrain(c *Context, o drainOpts, pos []string) int {
	for _, p := range pos {
		if _, err := time.ParseDuration(p); err == nil {
			return actUsage(c, "drain", fmt.Sprintf("%q reads as a duration: use --timeout %s (words after drain are the reason)", p, p), drainUsage)
		}
	}
	if len(pos) > 0 {
		o.reason = strings.TrimSpace(o.reason + " " + strings.Join(pos, " "))
	}
	if o.timeout <= 0 {
		return actUsage(c, "drain", "--timeout must be positive", drainUsage)
	}
	if code, refused := refuseInAgentPane(c, "drain"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "drain", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return drainMain(ctx, c, d, o)
}

// drainTarget is one daemon's side of a drain: its record (read, write and a
// lift that compares its value first), its acknowledgement, its pid now,
// whether its build knows the drain, and its wake-up.
type drainTarget struct {
	name  string
	pid   int
	knows bool
	read  func(ctx context.Context) (string, error)
	write func(ctx context.Context, v string) error
	lift  func(ctx context.Context, v string) error
	ack   func(ctx context.Context) string
	alive func(ctx context.Context) int
	kick  func()
	// prev is the record before this command wrote its own.
	prev string
}

// drainTargets are the daemons of scope sc, the factory first: its record
// is a file, the review daemon's a kv row of a registry that may be busy.
func drainTargets(ctx context.Context, c *Context, d *actDeps, sc controlScope) []*drainTarget {
	var out []*drainTarget
	if sc.factory {
		l := c.Layout
		alive := func(ctx context.Context) int { pid, _ := controlSys.FactoryPID(ctx, l); return pid }
		t := &drainTarget{name: daemonFactory, pid: alive(ctx),
			read:  func(context.Context) (string, error) { return factory.ReadRebootDrain(l) },
			write: func(_ context.Context, v string) error { return factory.WriteRebootDrain(l, v) },
			lift:  func(_ context.Context, v string) error { return factory.LiftRebootDrain(l, v) },
			ack: func(context.Context) string {
				if rec, ok, err := factory.ReadWork(l); err == nil && ok && rec.RebootDrain != nil {
					return rec.RebootDrain.Value()
				}
				return ""
			},
			alive: alive,
			kick:  func() { _, _ = controlSys.FactoryKick(l) },
		}
		t.knows = t.pid == 0 || factoryKnowsControls(l, t.pid)
		out = append(out, t)
	}
	if sc.review {
		st := d.Store
		get := func(ctx context.Context, key string) string { v, _, _ := st.GetKV(ctx, key); return v }
		t := &drainTarget{name: daemonReview, pid: controlReviewPID(c, d),
			read: func(ctx context.Context) (string, error) {
				v, _, err := st.GetKV(ctx, engine.KVDaemonRebootDrain)
				return v, err
			},
			write: func(ctx context.Context, v string) error { return st.SetKV(ctx, engine.KVDaemonRebootDrain, v) },
			lift: func(ctx context.Context, v string) error {
				_, err := st.DeleteKVIf(ctx, engine.KVDaemonRebootDrain, v)
				return err
			},
			ack:   func(ctx context.Context) string { return get(ctx, engine.KVDaemonDrainAck) },
			alive: func(context.Context) int { return controlReviewPID(c, d) },
			kick:  func() { _, _ = d.Kick() },
		}
		t.knows = t.pid == 0 || reviewKnowsControls(ctx, d, t.pid)
		out = append(out, t)
	}
	return out
}

func drainMain(ctx context.Context, c *Context, d *actDeps, o drainOpts) int {
	sc := o.scope.resolve(c)
	out := controlOut{c: c, cmd: "drain", both: sc.both()}
	now := d.now()
	rec := engine.RebootDrain{Generation: now.UnixNano(), Since: now, Until: engine.DrainUntilBoot, Reason: actClean(o.reason)}
	if o.keep {
		rec.Until = engine.DrainUntilResume
	} else if boot, err := controlSys.Boot(ctx); err != nil {
		fmt.Fprintf(c.Stderr, "magnum drain: warning: cannot read the Mac's boot (%v): the drain lasts until `magnum resume`\n", err)
	} else {
		rec.Boot = boot
	}
	targets := drainTargets(ctx, c, d, sc)
	if !drainBuildsKnow(out, targets) {
		return 1
	}
	value := rec.Value()
	for i, t := range targets {
		prev, err := t.read(ctx)
		if err == nil {
			err = t.write(ctx, value)
		}
		if err != nil {
			out.fail(t.name, "cannot start the drain: "+err.Error())
			drainUndo(ctx, out, targets[:i], value)
			return 1
		}
		t.prev = prev
		if t.pid > 0 {
			t.kick()
		}
	}
	for _, t := range targets {
		if t.pid > 0 {
			out.say(t.name, "draining for a reboot: nothing new starts, not even what you ask for; the work in flight ends, then the idle agents park")
		} else {
			out.say(t.name, "draining for a reboot: no daemon runs, so nothing is in flight; its next start holds the drain")
		}
	}
	if o.noWait {
		fmt.Fprintf(c.Stdout, "the drain holds %s: `magnum drain` waits for the daemons to go quiet, `magnum resume` ends it\n", rec.Words())
		return 0
	}
	return drainWait(ctx, c, d, out, targets, rec, o)
}

// drainBuildsKnow refuses a drain a running daemon of an older build would
// not hold: it names that daemon and how to go on, says the others are left
// as they were, and reports false.
func drainBuildsKnow(out controlOut, targets []*drainTarget) bool {
	ok := true
	for _, t := range targets {
		if !t.knows {
			out.fail(t.name, oldBuildText(t.name, t.pid, "`magnum drain`"))
			ok = false
		}
	}
	if !ok {
		for _, t := range targets {
			if t.knows {
				out.fail(t.name, "left as it was: nothing is drained")
			}
		}
	}
	return ok
}

// drainUndo lifts the records this command wrote on the daemons set before
// one failed: only where no drain held before, and only while the record is
// still this command's.
func drainUndo(ctx context.Context, out controlOut, set []*drainTarget, value string) {
	for _, t := range set {
		if t.prev != "" {
			out.fail(t.name, "keeps the drain it held before this command")
			continue
		}
		if err := t.lift(ctx, value); err != nil {
			out.fail(t.name, "its drain stays (could not lift it: "+err.Error()+"); `magnum resume` lifts it")
			continue
		}
		out.fail(t.name, "its drain is lifted again: the other daemon's failed")
	}
}

// drainState is a daemon's side of the wait: done (it acknowledged this
// generation, is quiet and parked its idle agents, or it does not run) and
// what still runs, in words.
type drainState struct {
	done bool
	text string
}

// drainStop is why the wait ends before the daemons went quiet: the record
// is gone (`magnum resume`) or another generation's (a newer `magnum drain`).
type drainStop int

const (
	drainGoesOn drainStop = iota
	drainResumed
	drainReplaced
)

// state reads t's side of the wait for generation gen.
func (t *drainTarget) state(ctx context.Context, gen int64) (drainState, drainStop) {
	v, err := t.read(ctx)
	if err != nil {
		return drainState{text: "its drain cannot be read now (" + err.Error() + ")"}, drainGoesOn
	}
	d, ok := engine.ParseRebootDrain(v)
	switch {
	case v == "":
		return drainState{}, drainResumed
	case !ok || d.Generation != gen:
		return drainState{}, drainReplaced
	}
	pid := t.alive(ctx)
	if pid == 0 {
		return drainState{done: true, text: "no daemon runs: nothing in flight"}, drainGoesOn
	}
	a, ok := engine.ParseDrainAck(t.ack(ctx))
	switch {
	case !ok || a.Generation != gen || a.PID != pid:
		return drainState{text: "has not held the drain yet"}, drainGoesOn
	case a.Done(gen, pid):
		return drainState{done: true, text: "quiet, its idle agents parked"}, drainGoesOn
	case a.Quiet && len(a.Work) > 0:
		return drainState{text: "quiet; its agents cannot park yet: " + strings.Join(a.Work, ", ")}, drainGoesOn
	case a.Quiet:
		return drainState{text: "quiet; parking its idle agents"}, drainGoesOn
	}
	return drainState{text: "still runs: " + strings.Join(a.Work, ", ")}, drainGoesOn
}

// drainWait waits until every daemon of targets is done (drainState), saying
// what still runs every drainProgressEvery and waking the daemons every
// rebootDrainKickEvery, for at most o.timeout. Ctrl-C and the timeout keep
// the drain (exit 1); a resume ends the wait (exit 0), a newer drain too
// (exit 1: its command waits for it).
func drainWait(ctx context.Context, c *Context, d *actDeps, out controlOut, targets []*drainTarget, rec engine.RebootDrain, o drainOpts) int {
	var waited, sinceProgress, sinceKick time.Duration
	states := make([]drainState, len(targets))
	for {
		all := true
		for i, t := range targets {
			st, stop := t.state(ctx, rec.Generation)
			switch stop {
			case drainResumed:
				fmt.Fprintln(c.Stdout, "the drain was lifted (`magnum resume`): new work starts again")
				return 0
			case drainReplaced:
				fmt.Fprintf(c.Stderr, "magnum drain: a newer `magnum drain` took the drain over (%s): its command waits for it\n", t.name)
				return 1
			}
			states[i] = st
			all = all && st.done
		}
		if all {
			for i, t := range targets {
				out.say(t.name, states[i].text)
			}
			if rec.Until == engine.DrainUntilResume || !rec.Boot.Known() {
				fmt.Fprintln(c.Stdout, drainSafeKeepText)
			} else {
				fmt.Fprintln(c.Stdout, drainSafeText)
			}
			return 0
		}
		switch {
		case ctx.Err() != nil:
			return drainKept(c, out, targets, states, "interrupted after "+waited.String())
		case waited >= o.timeout:
			return drainKept(c, out, targets, states, "still not quiet after "+o.timeout.String())
		}
		if waited == 0 || sinceProgress >= drainProgressEvery {
			sinceProgress = 0
			for i, t := range targets {
				if !states[i].done {
					out.say(t.name, "draining: "+states[i].text)
				}
			}
		}
		if sinceKick >= rebootDrainKickEvery {
			sinceKick = 0
			for _, t := range targets {
				t.kick()
			}
		}
		if d.sleep(ctx, drainPollEvery) != nil {
			return drainKept(c, out, targets, states, "interrupted after "+waited.String())
		}
		waited += drainPollEvery
		sinceProgress += drainPollEvery
		sinceKick += drainPollEvery
	}
}

// drainKept ends a wait that gave up (why) with the drain in force: what
// still runs on each daemon, and how to go on.
func drainKept(c *Context, out controlOut, targets []*drainTarget, states []drainState, why string) int {
	fmt.Fprintf(c.Stderr, "magnum drain: %s; the drain holds\n", why)
	for i, t := range targets {
		if !states[i].done {
			out.fail(t.name, states[i].text)
		}
	}
	fmt.Fprintln(c.Stderr, "`magnum drain` waits for it again; `magnum resume` allows new work")
	return 1
}
