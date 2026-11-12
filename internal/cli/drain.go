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
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
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
// lift that compares its value first), its acknowledgement, its pid now and
// whether its build knows the drain (and how to ask again, for a daemon that
// starts while the command waits), the agents' turns it left in herdr while
// it is down, its wake-up, and what its park left as it was.
type drainTarget struct {
	name     string
	pid      int
	knows    bool
	read     func(ctx context.Context) (string, error)
	write    func(ctx context.Context, v string) error
	lift     func(ctx context.Context, v string) error
	ack      func(ctx context.Context) string
	alive    func(ctx context.Context) (int, error)
	knowsPID func(ctx context.Context, pid int) bool
	down     func(ctx context.Context) ([]string, error)
	start    string // what starts it again, for a daemon that is down
	kick     func()
	// prev is the record before this command wrote its own; left what the
	// daemon's last acknowledgement says its park left.
	prev string
	left []string
}

// drainTargets are the daemons of scope sc, the factory first: its record
// is a file, the review daemon's a kv row of a registry that may be busy.
func drainTargets(ctx context.Context, c *Context, d *actDeps, sc controlScope) []*drainTarget {
	var out []*drainTarget
	if sc.factory {
		l := c.Layout
		t := &drainTarget{name: daemonFactory, start: "magnum factory install",
			read:  func(context.Context) (string, error) { return factory.ReadRebootDrain(l) },
			write: func(_ context.Context, v string) error { return factory.WriteRebootDrain(l, v) },
			lift:  func(_ context.Context, v string) error { return factory.LiftRebootDrain(l, v) },
			ack: func(context.Context) string {
				if rec, ok, err := factory.ReadWork(l); err == nil && ok && rec.RebootDrain != nil {
					return rec.RebootDrain.Value()
				}
				return ""
			},
			alive:    func(ctx context.Context) (int, error) { return controlSys.FactoryPID(ctx, l) },
			knowsPID: func(_ context.Context, pid int) bool { return factoryKnowsControls(l, pid) },
			down:     func(ctx context.Context) ([]string, error) { return factoryTurnsDown(ctx, l) },
			kick:     func() { _, _ = controlSys.FactoryKick(l) },
		}
		out = append(out, t)
	}
	if sc.review {
		st := d.Store
		t := &drainTarget{name: daemonReview, start: "magnum daemon-restart",
			read: func(ctx context.Context) (string, error) {
				v, _, err := st.GetKV(ctx, engine.KVDaemonRebootDrain)
				return v, err
			},
			write: func(ctx context.Context, v string) error { return st.SetKV(ctx, engine.KVDaemonRebootDrain, v) },
			lift: func(ctx context.Context, v string) error {
				_, err := st.DeleteKVIf(ctx, engine.KVDaemonRebootDrain, v)
				return err
			},
			ack:      func(ctx context.Context) string { v, _, _ := st.GetKV(ctx, engine.KVDaemonDrainAck); return v },
			alive:    func(context.Context) (int, error) { return reviewPID(d) },
			knowsPID: func(ctx context.Context, pid int) bool { return reviewKnowsControls(ctx, d, pid) },
			down:     func(ctx context.Context) ([]string, error) { return reviewTurnsDown(ctx, st) },
			kick:     func() { _, _ = d.Kick() },
		}
		out = append(out, t)
	}
	for _, t := range out {
		if pid, err := t.alive(ctx); err == nil { // one it cannot tell is said by the wait, which certifies nothing of it
			t.pid = pid
		}
		t.knows = t.pid == 0 || t.knowsPID(ctx, t.pid)
	}
	return out
}

// reviewPID is the running review daemon's pid (0: none), read without
// waking it when the deps can (Running), else through the kick; an error
// when its pidfile cannot be read.
func reviewPID(d *actDeps) (int, error) {
	if d.Running != nil {
		return d.Running()
	}
	return d.Kick()
}

// drainRecord is the drain for a reboot this command writes: a generation of
// its own, and, when a drain already holds, its start, its end (until
// resume after --keep unless --keep says it now), its reason unless --reason
// gives one, and its boot unless this one reads the boot.
func drainRecord(ctx context.Context, c *Context, targets []*drainTarget, now time.Time, o drainOpts) engine.RebootDrain {
	rec := engine.RebootDrain{Generation: now.UnixNano(), Since: now, Until: engine.DrainUntilBoot, Reason: actClean(o.reason)}
	var held *engine.RebootDrain
	for _, t := range targets {
		if v, err := t.read(ctx); err == nil {
			if d, ok := engine.ParseRebootDrain(v); ok {
				held = &d
				break
			}
		}
	}
	if held != nil {
		rec.Since, rec.Until = held.Since, held.Until
		if rec.Reason == "" {
			rec.Reason = held.Reason
		}
	}
	if o.keep {
		rec.Until = engine.DrainUntilResume
	}
	if rec.Until == engine.DrainUntilResume {
		return rec
	}
	if boot, err := controlSys.Boot(ctx); err == nil {
		rec.Boot = boot
	} else if held != nil && held.Boot.Known() {
		rec.Boot = held.Boot
	} else {
		fmt.Fprintf(c.Stderr, "magnum drain: warning: cannot read the Mac's boot (%v): the drain lasts until `magnum resume`\n", err)
	}
	return rec
}

func drainMain(ctx context.Context, c *Context, d *actDeps, o drainOpts) int {
	sc := o.scope.resolve(c)
	out := controlOut{c: c, cmd: "drain", both: sc.both()}
	targets := drainTargets(ctx, c, d, sc)
	if !drainBuildsKnow(out, targets) {
		return 1
	}
	rec := drainRecord(ctx, c, targets, d.now(), o)
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
			out.say(t.name, "draining for a reboot: no daemon runs; its agents' turns in herdr end, and its next start holds the drain")
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

// drainUndo undoes, on the daemons set before one failed, what this command
// wrote, while its record is still this command's: it lifts a drain it
// wrote where none was, and puts back the one it overwrote.
func drainUndo(ctx context.Context, out controlOut, set []*drainTarget, value string) {
	for _, t := range set {
		var err error
		if t.prev == "" {
			if err = t.lift(ctx, value); err == nil {
				out.fail(t.name, "its drain is lifted again: the other daemon's failed")
				continue
			}
		} else {
			var cur string
			if cur, err = t.read(ctx); err == nil && cur == value {
				err = t.write(ctx, t.prev)
			}
			if err == nil {
				out.fail(t.name, "its drain is put back as it was: the other daemon's failed")
				continue
			}
		}
		out.fail(t.name, "its drain stays as this command wrote it (could not undo it: "+err.Error()+"); `magnum resume` lifts it")
	}
}

// drainState is a daemon's side of the wait: done (it acknowledged this
// generation, is quiet and parked its idle agents, or it is down with no
// agent's turn in flight) and what still runs or keeps it from saying so,
// in words.
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

// state reads t's side of the wait for generation gen. A daemon it cannot
// tell runs, or a daemon that is down whose agents' turns it cannot read,
// is never done; one that is down is done only once its agents' turns in
// herdr ended; a daemon that started meanwhile has its build checked again.
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
	pid, err := t.alive(ctx)
	if err != nil {
		return drainState{text: "cannot tell whether the " + t.name + " daemon runs (" + err.Error() + "): nothing is safe to say of it"}, drainGoesOn
	}
	if pid == 0 {
		return t.downState(ctx), drainGoesOn
	}
	if pid != t.pid {
		t.pid, t.knows = pid, t.knowsPID(ctx, pid)
	}
	if !t.knows {
		return drainState{text: oldBuildText(t.name, pid, "`magnum drain`")}, drainGoesOn
	}
	a, ok := engine.ParseDrainAck(t.ack(ctx))
	switch {
	case !ok || a.Generation != gen || a.PID != pid:
		return drainState{text: "has not held the drain yet"}, drainGoesOn
	case a.Done(gen, pid):
		t.left = a.Left
		return drainState{done: true, text: "quiet, its idle agents parked"}, drainGoesOn
	case a.Quiet && len(a.Work) > 0:
		return drainState{text: "quiet; its agents cannot park yet: " + strings.Join(a.Work, ", ")}, drainGoesOn
	case a.Quiet:
		return drainState{text: "quiet; parking its idle agents"}, drainGoesOn
	}
	return drainState{text: "still runs: " + strings.Join(a.Work, ", ")}, drainGoesOn
}

// downState is the state of t's daemon while it is down: its agents' turns
// outlive it in herdr, so it is done only once its store shows none.
func (t *drainTarget) downState(ctx context.Context) drainState {
	work, err := t.down(ctx)
	switch {
	case err != nil:
		return drainState{text: "its daemon is down and its agents' turns cannot be read (" + err.Error() + "): start it (`" + t.start +
			"`) so it says what runs, or `magnum resume`"}
	case len(work) > 0:
		return drainState{text: "its daemon is down while agents' turns run in herdr: " + strings.Join(work, ", ") + ": start it (`" +
			t.start + "`) so it sees them end"}
	}
	return drainState{done: true, text: "no daemon runs, and no agent's turn is in flight"}
}

// drainWait waits until every daemon of targets is done (drainState), saying
// what still runs every drainProgressEvery and waking the daemons every
// rebootDrainKickEvery, for at most o.timeout. Ctrl-C and the timeout keep
// the drain (exit 1); a resume of every daemon ends the wait (exit 0), of
// some (two polls in a row: one resume lifts the daemons one at a time) with
// what still holds (exit 1), and a newer drain too (exit 1: its command
// waits for it).
func drainWait(ctx context.Context, c *Context, d *actDeps, out controlOut, targets []*drainTarget, rec engine.RebootDrain, o drainOpts) int {
	var waited, sinceProgress, sinceKick time.Duration
	states := make([]drainState, len(targets))
	partial := false
	for {
		all := true
		var resumed []bool
		lifted := 0
		for i, t := range targets {
			st, stop := t.state(ctx, rec.Generation)
			switch stop {
			case drainReplaced:
				fmt.Fprintf(c.Stderr, "magnum drain: a newer `magnum drain` took the drain over (%s): its command waits for it\n", t.name)
				return 1
			case drainResumed:
				lifted++
				st = drainState{text: "the drain was lifted (`magnum resume`)"}
			}
			resumed = append(resumed, stop == drainResumed)
			states[i] = st
			all = all && st.done
		}
		switch {
		case lifted == len(targets):
			fmt.Fprintln(c.Stdout, "the drain was lifted (`magnum resume`): new work starts again")
			return 0
		case lifted > 0 && partial:
			return drainPartlyLifted(out, targets, resumed)
		case lifted > 0:
			partial = true
		case all:
			return drainSafe(c, out, targets, states, rec)
		default:
			partial = false
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

// drainSafe ends a wait whose daemons are all done: each one's line, what a
// park left as it was, and the safe line.
func drainSafe(c *Context, out controlOut, targets []*drainTarget, states []drainState, rec engine.RebootDrain) int {
	for i, t := range targets {
		out.say(t.name, states[i].text)
		for _, l := range t.left {
			out.say(t.name, "left as it is: "+l)
		}
	}
	if rec.Until == engine.DrainUntilResume || !rec.Boot.Known() {
		fmt.Fprintln(c.Stdout, drainSafeKeepText)
	} else {
		fmt.Fprintln(c.Stdout, drainSafeText)
	}
	return 0
}

// drainPartlyLifted ends a wait whose drain a resume lifted on some of the
// daemons alone (resumed): it says which, and what still drains.
func drainPartlyLifted(out controlOut, targets []*drainTarget, resumed []bool) int {
	for i, t := range targets {
		if resumed[i] {
			out.fail(t.name, "the drain was lifted (`magnum resume`): new work starts again there")
		} else {
			out.fail(t.name, "still drains: `magnum resume` lifts it, `magnum drain` drains both again")
		}
	}
	return 1
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

// reviewTurnsDown are the review daemon's agents' turns in flight, from its
// registry, while the daemon is down: "codex-judge turn of
// talkable/talkable#12". Its turns outlive it in herdr.
func reviewTurnsDown(ctx context.Context, st *store.Store) ([]string, error) {
	runs, err := st.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range runs {
		switch r.State {
		case store.RunEnded, store.RunVerified, store.RunFailed, store.RunAbandoned:
			continue
		}
		what := fmt.Sprintf("PR %d", r.PRID)
		if pr, err := st.PRByID(ctx, r.PRID); err == nil {
			if repo, err := st.RepoByID(ctx, pr.RepoID); err == nil {
				what = fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
			}
		}
		out = append(out, r.Role+" turn of "+what)
	}
	return out, nil
}

// factoryTurnsDown are the factory daemon's agents' turns in flight, from
// its agents store opened read-only, while the daemon is down: "fx-writer
// turn of task 7" (a task's row and its liaison's are numbered by the task).
// No agents store yet is none.
func factoryTurnsDown(ctx context.Context, l paths.Layout) ([]string, error) {
	path := l.FactoryAgentsDB()
	if !fsx.Exists(path) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT r.role, p.number FROM runs r JOIN prs p ON p.id = r.pr_id
		WHERE r.state IN (?, ?, ?) ORDER BY r.created_at`, store.RunPending, store.RunSubmitted, store.RunWorking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s turn of task %d", role, n))
	}
	return out, rows.Err()
}
