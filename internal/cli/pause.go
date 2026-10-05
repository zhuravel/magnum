package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	pauseUsage  = "pause [--for 2h | --until 15:30] [--reason text]"
	resumeUsage = "resume [--tool <kind>|all] [--watch owner]"
)

type pauseOpts struct {
	dur    time.Duration
	until  string
	reason string
}

func newPauseCmd(c *Context) *cobra.Command {
	var o pauseOpts
	cmd := newCommand(groupAct, pauseUsage, "pause automatic reviews (running rounds finish; reviews you ask for still run)",
		"Pause automatic reviews: running rounds finish and no automatic round starts, while a review you ask for "+
			"(`magnum review`, the board's r/R/i, the picker) still runs. The pause lasts until `magnum resume`, "+
			"for --for, or until --until (a local time like 15:30 or an RFC3339 timestamp); --reason shows in "+
			"`magnum status`. It is recorded in the registry, so it applies even when no daemon runs yet.",
		func(pos []string) int { return runPause(c, o, pos) })
	fs := cmd.Flags()
	fs.DurationVar(&o.dur, "for", 0, "pause for this long (default: until magnum resume)")
	fs.StringVar(&o.until, "until", "", "pause until this local time (15:30) or RFC3339 timestamp")
	fs.StringVar(&o.reason, "reason", "", "why (shown in magnum status)")
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
	if o.dur < 0 || (o.dur > 0 && o.until != "") {
		return actUsage(c, "pause", "use either --for (positive) or --until", pauseUsage)
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
}

func newResumeCmd(c *Context) *cobra.Command {
	var o resumeOpts
	cmd := newCommand(groupAct, resumeUsage, "resume automation, or lift an agent kind's or a watch's pause",
		"Resume automation after `magnum pause` (and lift an infrastructure pause, whose probe would otherwise "+
			"lift it). --tool lifts a usage-limit, login or Codex-budget pause of an agent kind (codex, claude or "+
			"any [kinds.<name>]; `magnum roles --kinds` lists them) or of all, and forgets the kind's per-model "+
			"limits, so its sessions go back to their roles' models; --watch lifts the identity-leak pause of a "+
			"watch owner.",
		func(pos []string) int { return runResume(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.tool, "tool", "", "lift a pause of this agent kind (codex, claude, ...) or all, and forget its model limits")
	fs.StringVar(&o.watch, "watch", "", "lift the identity-leak pause of a watch owner")
	_ = cmd.RegisterFlagCompletionFunc("tool", completeFlag(c.completeKinds))
	_ = cmd.RegisterFlagCompletionFunc("watch", completeFlag(c.completeWatches))
	return cmd
}

func runResume(c *Context, o resumeOpts, pos []string) int {
	if len(pos) > 0 {
		return actUsage(c, "resume", "takes no arguments", resumeUsage)
	}
	if o.tool != "" && o.watch != "" {
		return actUsage(c, "resume", "use either --tool or --watch", resumeUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "resume", err)
	}
	if kinds := rolesAllKinds(d.Cfg); o.tool != "" && o.tool != "all" && !slices.Contains(kinds, o.tool) {
		d.Close()
		return actUsage(c, "resume", fmt.Sprintf("unknown tool %q (%s or all)", o.tool, strings.Join(kinds, ", ")), resumeUsage)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return resumeMain(ctx, c, d, o)
}

func resumeMain(ctx context.Context, c *Context, d *actDeps, o resumeOpts) int {
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
			if had {
				lifted = append(lifted, t)
			}
			models, err := engine.ClearModelLimits(ctx, d.Store, t)
			if err != nil {
				return "", err
			}
			for _, m := range models {
				limits = append(limits, t+"/"+m)
			}
		}
		note := ""
		if len(limits) > 0 {
			note = " (model limits cleared: " + strings.Join(limits, ", ") + ")"
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
