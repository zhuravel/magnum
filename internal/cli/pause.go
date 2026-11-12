package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	pauseUsage  = "pause [--review] [--factory] [--for 2h | --until 15:30] [--reason text | reason…]"
	resumeUsage = "resume [--review] [--factory] [--keep-drain] | resume [--tool <kind>|all] [--watch owner]"
)

type pauseOpts struct {
	dur    time.Duration
	until  string
	reason string
	scope  controlScope
}

func newPauseCmd(c *Context) *cobra.Command {
	var o pauseOpts
	cmd := newCommand(groupAct, pauseUsage, "pause new automatic work on both daemons (what runs finishes; what you ask for still runs)",
		"Pause new automatic work on the review daemon and the factory daemon (the factory only while its launchd job is "+
			"installed); --review or --factory narrows it to one. It waits for nothing: what runs finishes. "+
			"Review: no automatic round starts, while a review you ask for "+
			"(`magnum review`, the board's r/R/i, the picker) still runs. Under autofix = \"open\" it also holds the fix "+
			"loops the look would start (a loop that runs goes on, and `magnum fix` or the board's f still starts one), and "+
			"a review it holds is not late (magnum_late_after counts from its end). "+
			"Factory: no launch of a stage's roles, claim of a copy, local review round, check or QA run starts, while your "+
			"`magnum factory start` queues its task and your answers, retries, talks and pushes run; the factory's pause is "+
			"pause.json beside its drain.json, and its screen and `magnum factory status` show it. "+
			"The pause lasts until `magnum resume`, "+
			"for --for, or until --until (a local time like 15:30 or an RFC3339 timestamp), across restarts and reboots; "+
			"--reason, or the words "+
			"after `pause`, say why and show in `magnum status` (a word that reads as a duration is refused: use "+
			"--for). It is recorded in the review registry and the factory's file, so it applies even when no daemon runs "+
			"yet. A factory daemon of an older build does not know its pause: the command says to restart it on this build "+
			"first and pauses nothing. `magnum drain` also holds what you ask for and waits for the work in flight.",
		func(pos []string) int { return runPause(c, o, pos) })
	fs := cmd.Flags()
	fs.DurationVar(&o.dur, "for", 0, "pause for this long (default: until magnum resume)")
	fs.StringVar(&o.until, "until", "", "pause until this local time (15:30) or RFC3339 timestamp")
	fs.StringVar(&o.reason, "reason", "", "why (shown in magnum status)")
	addScopeFlags(cmd, &o.scope)
	return cmd
}

func runPause(c *Context, o pauseOpts, pos []string) int {
	// `magnum pause 2h` would pause indefinitely with the reason "2h": a word
	// that reads as a duration is refused, never taken for the reason.
	for _, p := range pos {
		if _, err := time.ParseDuration(p); err == nil {
			return actUsage(c, "pause", fmt.Sprintf("%q reads as a duration: use --for %s (words after pause are the reason)", p, p), pauseUsage)
		}
	}
	if len(pos) > 0 {
		o.reason = strings.TrimSpace(o.reason + " " + strings.Join(pos, " "))
	}
	if msg := o.refusal(); msg != "" {
		return actUsage(c, "pause", msg, pauseUsage)
	}
	if code, refused := refuseInAgentPane(c, "pause"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "pause", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return pauseMain(ctx, c, d, o)
}

// refusal is why pause refuses o's length ("" = it does not): a negative
// --for, or --for with --until.
func (o pauseOpts) refusal() string {
	if o.dur < 0 || (o.dur > 0 && o.until != "") {
		return "use either --for (positive) or --until"
	}
	return ""
}

func pauseMain(ctx context.Context, c *Context, d *actDeps, o pauseOpts) int {
	now := d.now()
	var until time.Time
	switch {
	case o.dur > 0:
		until = now.Add(o.dur)
	case o.until != "":
		t, err := pauseParseUntil(o.until, now)
		if err != nil {
			return actUsage(c, "pause", err.Error(), pauseUsage)
		}
		until = t
	}
	sc := o.scope.resolve(c)
	if !sc.factory {
		return pauseReview(ctx, c, d, o, now, until)
	}
	out := controlOut{c: c, cmd: "pause", both: sc.both()}
	fpid, err := controlSys.FactoryPID(ctx, c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum pause: warning: %v\n", err)
	}
	if fpid > 0 && !factoryKnowsControls(c.Layout, fpid) {
		out.fail(daemonFactory, oldBuildText(daemonFactory, fpid, "`magnum pause`"))
		if sc.review {
			out.fail(daemonReview, "left as it was: nothing is paused")
		}
		return 1
	}
	undo, code := pauseFactory(c, out, o, now, until, fpid)
	if code != 0 || !sc.review {
		if code != 0 && sc.review {
			out.fail(daemonReview, "left as it was: nothing is paused")
		}
		return code
	}
	rc := *c
	rc.Stdout, rc.Stderr = prefixLines(c.Stdout, daemonReview+": "), prefixLines(c.Stderr, daemonReview+": ")
	if code := pauseReview(ctx, &rc, d, o, now, until); code != 0 {
		if msg := undo(); msg != "" {
			out.fail(daemonFactory, msg+": the review daemon's failed")
		}
		return code
	}
	return 0
}

// pauseFactory writes the factory's pause (pause.json): a pause renewed
// while it holds keeps its start. It wakes the running factory daemon
// (fpid), which holds the pause at its next tick, and returns the undo of
// what it wrote, while the file still holds it: it lifts a pause written
// where none was and puts back the one it overwrote (factory.ReplacePause),
// and says which ("" when it undid nothing).
func pauseFactory(c *Context, out controlOut, o pauseOpts, now, until time.Time, fpid int) (undo func() string, code int) {
	none := func() string { return "" }
	l := c.Layout
	prev, err := factory.ReadPause(l)
	if err != nil {
		out.fail(daemonFactory, fmt.Sprintf("cannot read %s: %v", inspTilde(l.FactoryPause()), err))
		return none, 1
	}
	p := factory.Pause{Generation: now.UnixNano(), Since: now, Until: until, Reason: actClean(o.reason)}
	if old, ok := factory.ParsePause(prev); ok {
		p.Since = old.Since
	}
	value := p.Value()
	if err := factory.WritePause(l, value); err != nil {
		out.fail(daemonFactory, fmt.Sprintf("cannot write %s: %v", inspTilde(l.FactoryPause()), err))
		return none, 1
	}
	out.say(daemonFactory, p.Words()+": no launch, claim, local review round, check or QA run starts; your start, answer, retry, "+
		"talk and push still run; `magnum resume` lifts it")
	if fpid > 0 {
		_, _ = controlSys.FactoryKick(l)
	} else {
		out.say(daemonFactory, "no factory daemon is running; the pause applies when it starts")
	}
	return func() string {
		switch cur, err := factory.ReadPause(l); {
		case err != nil || cur != value:
			return "" // another pause took its place: it stays
		case prev == "":
			if factory.LiftPause(l, value) == nil {
				return "its pause is lifted again"
			}
		case factory.ReplacePause(l, value, prev) == nil:
			return "its pause is put back as it was"
		}
		return "its pause stays as this command wrote it (could not undo it); `magnum resume --factory` lifts it"
	}, 0
}

// pauseReview pauses the review daemon's automatic reviews: through its
// request handler while it runs, else in the registry.
func pauseReview(ctx context.Context, c *Context, d *actDeps, o pauseOpts, now, until time.Time) int {
	msg := "paused automation"
	if !until.IsZero() {
		msg += " until " + until.Local().Format("Jan 2 15:04")
	}
	if o.reason != "" {
		msg += " (" + actClean(o.reason) + ")"
	}
	msg += ": running rounds finish, nothing new starts; `magnum resume` lifts it"
	if pid := pauseDaemonPID(c, d); pid > 0 {
		p := engine.PausePayload{Reason: o.reason}
		if !until.IsZero() {
			p.Until = &until
		}
		return pauseRequest(ctx, c, d, "pause", engine.ReqPause, p, msg)
	}
	// No daemon: write the registry directly. The expiry and reason go in
	// before the flag, so a daemon starting meanwhile never pairs the new
	// flag with an expired until left by an earlier pause (its health check
	// would lift the new pause at once).
	untilVal := ""
	if !until.IsZero() {
		untilVal = store.FormatTime(until)
	}
	// A pause renewed while it runs keeps its start (an older build's pause
	// has none: the daemon takes it from its event).
	startVal := store.FormatTime(now)
	if v, _, err := d.Store.GetKV(ctx, store.KVDaemonPaused); err != nil {
		return cmdFail(c, "pause", err)
	} else if v == "1" {
		if startVal, _, err = d.Store.GetKV(ctx, engine.KVDaemonPausedAt); err != nil {
			return cmdFail(c, "pause", err)
		}
	}
	for _, kv := range [][2]string{{store.KVDaemonPausedReason, o.reason}, {store.KVDaemonPausedUntil, untilVal},
		{engine.KVDaemonPausedAt, startVal}, {store.KVDaemonPaused, "1"}} {
		if err := pauseSetKV(ctx, d.Store, kv[0], kv[1]); err != nil {
			return cmdFail(c, "pause", err)
		}
	}
	_, _ = d.Store.AppendEvent(ctx, store.Event{Level: "info", Kind: "daemon.paused",
		Message: strings.TrimSpace("automation paused by magnum pause " + o.reason)})
	fmt.Fprintln(c.Stdout, msg)
	fmt.Fprintf(c.Stderr, "no daemon is running; the change applies when it starts (%s)\n", actDaemonFix)
	return 0
}

// pauseSetKV sets key, or deletes it for an empty value.
func pauseSetKV(ctx context.Context, st *store.Store, key, value string) error {
	if value == "" {
		return st.DeleteKV(ctx, key)
	}
	return st.SetKV(ctx, key, value)
}

// pauseDaemonPID is the pid of the running daemon (0 when none runs). It
// wakes the daemon (SIGUSR1 after the ps check); a pidfile naming another
// process is reported and counts as no daemon.
func pauseDaemonPID(c *Context, d *actDeps) int {
	pid, err := d.Kick()
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum: %v\n", err)
		return 0
	}
	return pid
}

// pauseRequest hands a pause or resume to the running daemon, whose request
// handler applies it with the engine's own transitions (event, toast
// dedup), and waits for it briefly. done is printed when the daemon applied
// it ("" = the daemon's own result).
func pauseRequest(ctx context.Context, c *Context, d *actDeps, cmd, kind string, p engine.PausePayload, done string) int {
	q := d.quick()
	q.Held = true // the caller found the daemon running
	out, err := d.reqs().send(ctx, kind, p, q)
	if err != nil {
		return cmdFail(c, cmd, err)
	}
	if out.Req.State == store.RequestDone && done != "" {
		out.printNotes(c.Stderr)
		fmt.Fprintln(c.Stdout, done)
		return 0
	}
	return out.print(c.Stdout, c.Stderr)
}

// pauseParseUntil reads "15:30" (today, or tomorrow if past) or RFC3339.
func pauseParseUntil(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("--until %s is in the past", s)
		}
		return t, nil
	}
	hm, err := time.ParseInLocation("15:04", s, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q: want HH:MM (local) or an RFC3339 timestamp", s)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

type resumeOpts struct {
	tool, watch string
	scope       controlScope
	// keepDrain lifts the pauses alone: a drain for a reboot stays (the
	// settings screen's resume, a script's own pause around a restart).
	keepDrain bool
}

func newResumeCmd(c *Context) *cobra.Command {
	var o resumeOpts
	cmd := newCommand(groupAct, resumeUsage, "lift the pause and the drain on both daemons, or an agent kind's or a watch's pause",
		"Resume new work on the review daemon and the factory daemon (the factory only while its launchd job is "+
			"installed; --review or --factory narrows it): lift `magnum pause` and the drain for a reboot (`magnum drain`) "+
			"on each, and on the review daemon the infrastructure pause too (whose probe would otherwise lift it). "+
			"A `magnum drain` that still waits says the drain was lifted and exits 0; --keep-drain lifts the pauses "+
			"alone and leaves a drain for a reboot as it is. It never lifts a restart's drain, "+
			"an agent kind's or a watch's pause, and never touches mutes, snoozes or pins: when one of them still holds "+
			"work it says so, with its own remedy. "+
			"--tool lifts a usage-limit, login, Codex-budget or hooks-review pause of an agent kind (codex, claude or "+
			"any [kinds.<name>]; `magnum roles --kinds` lists them) or of all, and forgets the kind's per-model "+
			"limits, so its sessions go back to their roles' models. For codex (or all), with Codex's last reading at "+
			"either [usage] cap, a Codex-budget pause or not, it also overrules that reading, which is stale after a "+
			"switch to another Codex account: both caps wait for Codex's next reading, from the next Codex session; "+
			"and while hooks in a Codex home's hooks.json have no trust entry (the hooks-review pause), new Codex "+
			"sessions start anyway until that hooks.json changes again. "+
			"--watch lifts the identity-leak pause of a watch owner.",
		func(pos []string) int { return runResume(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.tool, "tool", "", "lift a pause of this agent kind (codex, claude, ...) or all, and forget its model limits")
	fs.StringVar(&o.watch, "watch", "", "lift the identity-leak pause of a watch owner")
	fs.BoolVar(&o.keepDrain, "keep-drain", false, "lift the pauses alone: a drain for a reboot (`magnum drain`) stays")
	_ = cmd.RegisterFlagCompletionFunc("tool", completeFlag(c.completeKinds))
	_ = cmd.RegisterFlagCompletionFunc("watch", completeFlag(c.completeWatches))
	addScopeFlags(cmd, &o.scope)
	return cmd
}

func runResume(c *Context, o resumeOpts, pos []string) int {
	if len(pos) > 0 {
		return actUsage(c, "resume", "takes no arguments", resumeUsage)
	}
	if o.tool != "" && o.watch != "" {
		return actUsage(c, "resume", "use either --tool or --watch", resumeUsage)
	}
	if (o.tool != "" || o.watch != "") && o.scope.factory {
		return actUsage(c, "resume", "--tool and --watch lift the review daemon's pauses: drop --factory", resumeUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "resume", err)
	}
	if msg := resumeToolRefusal(d.Cfg, o.tool); msg != "" {
		d.Close()
		return actUsage(c, "resume", msg, resumeUsage)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return resumeMain(ctx, c, d, o)
}

// resumeToolRefusal is why resume refuses --tool tool under cfg ("" = it
// does not): a name that is no agent kind and not "all".
func resumeToolRefusal(cfg *config.Config, tool string) string {
	if kinds := rolesAllKinds(cfg); tool != "" && tool != "all" && !slices.Contains(kinds, tool) {
		return fmt.Sprintf("unknown tool %q (%s or all)", tool, strings.Join(kinds, ", "))
	}
	return ""
}

func resumeMain(ctx context.Context, c *Context, d *actDeps, o resumeOpts) int {
	if o.tool != "" || o.watch != "" {
		return resumeReview(ctx, c, d, o)
	}
	sc := o.scope.resolve(c)
	out := controlOut{c: c, cmd: "resume", both: sc.both()}
	code := 0
	if sc.factory {
		code = resumeFactory(ctx, c, out, o.keepDrain)
	}
	if sc.review {
		rc := c
		if sc.both() {
			cp := *c
			cp.Stdout, cp.Stderr = prefixLines(c.Stdout, daemonReview+": "), prefixLines(c.Stderr, daemonReview+": ")
			rc = &cp
		}
		code = max(code, resumeReviewAll(ctx, rc, d, o.keepDrain))
	}
	return code
}

// keptDrainText is the still-held line of a drain for a reboot that a
// resume with keepDrain leaves.
const keptDrainText = "a drain for a reboot (`magnum drain`): `magnum resume` lifts it"

// resumeReviewAll lifts the review daemon's drain for a reboot (unless
// keepDrain) and its pause (resumeReview), and names the holds it leaves,
// each with its own remedy.
func resumeReviewAll(ctx context.Context, c *Context, d *actDeps, keepDrain bool) int {
	lifted := false
	if !keepDrain {
		var err error
		if lifted, err = liftReviewRebootDrain(ctx, d); err != nil {
			return cmdFail(c, "resume", err)
		}
	}
	offline := controlReviewPID(c, d) == 0
	code := resumeReview(ctx, c, d, resumeOpts{})
	if lifted {
		fmt.Fprintln(c.Stdout, "lifted the drain for a reboot: new work starts again")
	}
	held := reviewStillHeld(ctx, d, offline)
	if v, ok, err := d.Store.GetKV(ctx, engine.KVDaemonRebootDrain); keepDrain && (err != nil || (ok && v != "")) {
		held = append([]string{keptDrainText}, held...)
	}
	for _, s := range held {
		fmt.Fprintln(c.Stdout, "still held: "+s)
	}
	return code
}

// liftReviewRebootDrain deletes the review daemon's drain for a reboot it
// read, and then its acknowledgement (the daemon says the lift in its
// event); false when none held. A drain that changed between the read and
// the delete (a newer `magnum drain`) is read again, a few times, and then
// it is an error: nothing is said lifted that was not.
func liftReviewRebootDrain(ctx context.Context, d *actDeps) (bool, error) {
	for range 3 {
		v, ok, err := d.Store.GetKV(ctx, engine.KVDaemonRebootDrain)
		if err != nil || !ok || v == "" {
			return false, err
		}
		gone, err := d.Store.DeleteKVIf(ctx, engine.KVDaemonRebootDrain, v)
		if err != nil {
			return false, err
		}
		if gone {
			return true, d.Store.DeleteKV(ctx, engine.KVDaemonDrainAck)
		}
	}
	return false, errors.New("the drain for a reboot changed while it was lifted (a newer `magnum drain`?): run `magnum resume` again")
}

// reviewStillHeld are the review daemon's holds a plain resume leaves, each
// with its remedy: a restart's drain, a watch's identity-leak pause, and,
// with no daemon to name them in its answer (offline), an agent kind's
// pause.
func reviewStillHeld(ctx context.Context, d *actDeps, offline bool) []string {
	var out []string
	if v, ok, err := d.Store.GetKV(ctx, engine.KVDaemonDraining); err == nil && ok && v != "" {
		out = append(out, "a drain for a restart (`magnum daemon-restart --drain`): it ends with that restart, or ctrl+c in its terminal")
	}
	for _, w := range d.Cfg.Watches {
		if v, ok, err := d.Store.GetKV(ctx, store.KVWatchPaused(w.Owner)); err == nil && ok && v != "" {
			out = append(out, fmt.Sprintf("watch %s is paused (identity leak): `magnum resume --watch %s`", w.Owner, w.Owner))
		}
	}
	if !offline {
		return out
	}
	now := d.now()
	for _, kind := range rolesAllKinds(d.Cfg) {
		v, ok, err := d.Store.GetKV(ctx, store.KVToolPausedUntil(kind))
		if err != nil || !ok {
			continue
		}
		if until, err := store.ParseTime(v); err == nil && now.Before(until) {
			reason, _, _ := d.Store.GetKV(ctx, store.KVToolPausedReason(kind))
			out = append(out, fmt.Sprintf("%s is paused (%s) until %s: `magnum resume --tool %s`", kind, reason,
				inspClock(now, until), kind))
		}
	}
	return out
}

// resumeFactory lifts the factory's pause and its drain for a reboot (their
// files; the pause alone with keepDrain), wakes the running factory daemon,
// and names the holds it leaves: a restart's drain, a kept drain for a
// reboot and the machine's holds the daemon keeps. A file it cannot read is
// said, then removed all the same: the daemon holds what it cannot read, and
// only this removes it. A file it cannot lift keeps it from neither the
// other file nor its lines: it says each result and returns 1.
func resumeFactory(ctx context.Context, c *Context, out controlOut, keepDrain bool) int {
	l := c.Layout
	var lifted, kept []string
	failed := false
	for _, f := range []struct {
		what   string
		read   func(paths.Layout) (string, error)
		lift   func(paths.Layout, string) error
		remove func(paths.Layout) error
		path   string
		drain  bool
	}{
		{"the pause", factory.ReadPause, factory.LiftPause, factory.RemovePause, l.FactoryPause(), false},
		{"the drain for a reboot", factory.ReadRebootDrain, factory.LiftRebootDrain, factory.RemoveRebootDrain, l.FactoryRebootDrain(), true},
	} {
		v, err := f.read(l)
		switch {
		case f.drain && keepDrain:
			if err != nil || v != "" {
				kept = append(kept, keptDrainText)
			}
			continue
		case err != nil:
			out.say(daemonFactory, fmt.Sprintf("cannot read %s (%v): it is removed, since the daemon holds what it cannot read", inspTilde(f.path), err))
			err = f.remove(l)
		case v != "":
			err = f.lift(l, "")
		default:
			continue
		}
		if err != nil { // the next record is lifted all the same, and both results are said
			out.fail(daemonFactory, fmt.Sprintf("cannot lift %s (%s: %v)", f.what, inspTilde(f.path), err))
			failed = true
			continue
		}
		lifted = append(lifted, f.what)
	}
	pid, _ := controlSys.FactoryPID(ctx, l)
	if pid > 0 {
		_, _ = controlSys.FactoryKick(l)
	}
	switch {
	case len(lifted) > 0 && (len(kept) > 0 || failed):
		out.say(daemonFactory, "lifted "+strings.Join(lifted, " and "))
	case len(lifted) > 0:
		out.say(daemonFactory, "lifted "+strings.Join(lifted, " and ")+": new work starts again")
	case failed:
	case keepDrain:
		out.say(daemonFactory, "was not paused")
	default:
		out.say(daemonFactory, "was not paused or draining")
	}
	for _, s := range append(kept, factoryStillHeld(l, pid)...) {
		out.say(daemonFactory, "still held: "+s)
	}
	if failed {
		return 1
	}
	return 0
}

// factoryStillHeld are the factory's holds a resume leaves, with their
// remedies: a restart's drain, and the holds of the machine the running
// daemon pid keeps (holds.json).
func factoryStillHeld(l paths.Layout, pid int) []string {
	var out []string
	if v, _ := factory.ReadDrain(l); v != "" {
		out = append(out, "a drain for a restart (`magnum factory install --drain`): it ends with that restart, or ctrl+c in its terminal")
	}
	if pid <= 0 {
		return out
	}
	hs, _ := factory.ReadHolds(l)
	for _, h := range hs {
		switch h.Kind {
		case factory.HoldHerdr, factory.HoldInfra:
			out = append(out, h.Kind+": "+h.Reason+": the factory tries again by itself")
		case factory.HoldDisk, factory.HoldMemory:
			out = append(out, h.Kind+": "+h.Reason+": it lifts once the Mac has room")
		}
	}
	return out
}

// resumeReview is the review daemon's resume of o: an agent kind's pause
// (--tool), a watch's (--watch), or its pause and its infrastructure pause.
func resumeReview(ctx context.Context, c *Context, d *actDeps, o resumeOpts) int {
	if pid := pauseDaemonPID(c, d); pid > 0 {
		return pauseRequest(ctx, c, d, "resume", engine.ReqResume, engine.PausePayload{Tool: o.tool, Watch: o.watch}, "")
	}
	msg, err := resumeOffline(ctx, d, o)
	if err != nil {
		return cmdFail(c, "resume", err)
	}
	_, _ = d.Store.AppendEvent(ctx, store.Event{Level: "info", Kind: "daemon.resumed", Message: msg + " (magnum resume)"})
	fmt.Fprintln(c.Stdout, msg)
	fmt.Fprintf(c.Stderr, "no daemon is running; the change applies when it starts (%s)\n", actDaemonFix)
	return 0
}

// resumeOffline lifts a pause in the registry when no daemon runs, as the
// engine's request handler would: a lifted agent-kind pause also forgets its
// toast's dedup record, so the next pause notifies at once.
func resumeOffline(ctx context.Context, d *actDeps, o resumeOpts) (string, error) {
	del := func(keys ...string) (bool, error) {
		had := false
		for _, k := range keys {
			if _, ok, err := d.Store.GetKV(ctx, k); err != nil {
				return had, err
			} else if ok {
				had = true
			}
			if err := d.Store.DeleteKV(ctx, k); err != nil {
				return had, err
			}
		}
		return had, nil
	}
	switch {
	case o.watch != "":
		had, err := del(store.KVWatchPaused(o.watch))
		if err != nil {
			return "", err
		}
		if !had {
			return "watch " + o.watch + " was not paused", nil
		}
		return "resumed watch " + o.watch, nil
	case o.tool != "":
		tools, quiet := rolesAllKinds(d.Cfg), rolesKindNames(d.Cfg)
		if o.tool != "all" {
			tools, quiet = []string{o.tool}, []string{o.tool}
		}
		var lifted, limits []string
		budget := false
		for _, t := range tools {
			reason, _, err := d.Store.GetKV(ctx, store.KVToolPausedReason(t))
			if err != nil {
				return "", err
			}
			if reason != "" {
				if err := d.Store.ForgetSend(ctx, "pause:"+t+":"+reason); err != nil {
					return "", err
				}
			}
			had, err := del(store.KVToolPausedUntil(t), store.KVToolPausedReason(t), store.KVToolPausedDetail(t))
			if err != nil {
				return "", err
			}
			held, err := del(engine.KVKindHold(t)) // a shared launch failure's hold, as the daemon's liftToolPause lifts it
			if err != nil {
				return "", err
			}
			if held {
				if err := d.Store.SetKV(ctx, engine.KVKindHoldLifted(t), store.FormatTime(d.now())); err != nil {
					return "", err
				}
				had = true
			}
			// A lift, a pause or not, as the daemon's resume records it: a
			// reviewer's failure from before pauses the kind no more at a settle.
			if err := d.Store.SetKV(ctx, store.KVToolPauseLiftedAt(t), store.FormatTime(d.now())); err != nil {
				return "", err
			}
			if had {
				lifted = append(lifted, t)
				budget = budget || reason == engine.BudgetPauseReason
			}
			models, err := engine.ClearModelLimits(ctx, d.Store, t)
			if err != nil {
				return "", err
			}
			for _, m := range models {
				limits = append(limits, t+"/"+m)
			}
		}
		if !budget && slices.ContainsFunc(tools, func(t string) bool { return slices.Contains(engine.BudgetKinds(d.Cfg), t) }) {
			// No budget-cap pause: the reading the daemon recorded is
			// overruled at the soft or the hard cap, as its liftToolPause does.
			atCap, err := engine.RecordedBudgetAtCap(ctx, d.Store, d.Cfg.Usage, d.now())
			if err != nil {
				return "", err
			}
			budget = atCap
		}
		note := ""
		if budget { // the daemon takes the mark over at its start (engine.overruleBudget)
			if err := d.Store.SetKV(ctx, engine.KVUsageCodexOverruledAt, store.FormatTime(d.now())); err != nil {
				return "", err
			}
			note = " (" + engine.BudgetOverruledNote + ")"
		}
		if slices.Contains(tools, config.KindCodex) { // as the daemon's liftToolPause overrules the hooks hold
			overruled, err := engine.OverruleHooksHold(ctx, d.Store)
			if err != nil {
				return "", err
			}
			if overruled {
				note += " (" + engine.HooksOverruledNote + ")"
			}
		}
		if len(limits) > 0 {
			note += " (model limits cleared: " + strings.Join(limits, ", ") + ")"
		}
		if len(lifted) == 0 {
			return strings.Join(quiet, " and ") + " had no pause" + note, nil
		}
		return "resumed " + strings.Join(lifted, " and ") + note, nil
	}
	had, err := del(store.KVDaemonPaused, store.KVDaemonPausedReason, store.KVDaemonPausedUntil)
	if err != nil {
		return "", err
	}
	if _, err := del(engine.KVDaemonPausedAt, engine.KVDaemonPausedHeld); err != nil {
		return "", err
	}
	infra, err := del(engine.KVInfraPausedUntil, engine.KVInfraPausedReason, engine.KVInfraPausedDetail)
	if err != nil {
		return "", err
	}
	if infra {
		if err := d.Store.ForgetSend(ctx, "infra"); err != nil {
			return "", err
		}
	}
	switch {
	case had && infra:
		return "resumed automation and lifted the infrastructure pause", nil
	case infra:
		return "lifted the infrastructure pause", nil
	case !had:
		return "automation was not paused", nil
	}
	return "resumed automation", nil
}
