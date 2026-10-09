package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// targetKind is one of the PR/slot verbs that become daemon requests.
type targetKind struct {
	name, req, summary string
	slotOK             bool   // a slot without a PR is a valid target
	long               string // --help text
}

var targetKinds = []targetKind{
	{"pin", engine.ReqPin, "keep a PR's slot and sessions for you (automation leaves them alone)", true,
		"Keep a PR's slot and agent sessions for you: automation does not claim, check out, release or remove " +
			"them until `magnum unpin`. A slot name pins that slot."},
	{"unpin", engine.ReqUnpin, "hand a pinned PR's slot back to automation", true,
		"Hand a pinned PR's slot (or a pinned slot) back to automation."},
	{"release", engine.ReqRelease, "hand back a PR's slot now (sessions parked, worktree reset)", true,
		"Hand back a PR's slot now: its sessions are parked and the worktree reset, so the slot is free for the " +
			"next round. The daemon does it; without one the release runs here under the lock. --force discards " +
			"tracked changes and releases inside the close grace, but never overrides pins or running agents. A " +
			"terminal asks y/N first (--yes does not; there --json needs --yes)."},
	{"mute", engine.ReqMute, "stop automatic reviews of a PR", false,
		"Stop automatic reviews of a PR until `magnum unmute`; a forced `magnum review` still runs. A PR waiting " +
			"for an automatic round leaves the queue at once. The words after the PR are the mute's reason, kept in " +
			"the request and its pr.muted event (`magnum mute talkable#9 waits for the rework`). On a PR GitHub " +
			"merged before magnum reviewed its last push it dismisses the merged-unreviewed flag instead (`magnum " +
			"unmute` restores it, `magnum review` still runs a post-merge review)."},
	{"unmute", engine.ReqUnmute, "resume automatic reviews of a PR (also undoes magnum ignore)", false,
		"Resume automatic reviews of a muted PR. For a PR `magnum ignore` muted, the ignore mark goes too and " +
			"its eligibility is decided again; it is reviewed on its next push."},
}

func targetKindByName(name string) targetKind {
	for _, k := range targetKinds {
		if k.name == name {
			return k
		}
	}
	panic("cli: unknown target kind " + name)
}

// newTargetCmd is `magnum pin|unpin|release|mute|unmute`.
func newTargetCmd(c *Context, k targetKind) *cobra.Command {
	var o targetOpts
	cmd := newCommand(groupAct, targetUsage(k), k.summary, k.long, func(pos []string) int { return runTarget(c, k, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.workspace, "workspace", "", "herdr workspace id instead of a PR (plugin context)")
	fs.StringVar(&o.cwd, "cwd", "", "directory inside a magnum slot instead of a PR (plugin context)")
	fs.BoolVar(&o.json, "json", false, "print the request outcome as JSON")
	if k.name == "release" {
		fs.BoolVar(&o.force, "force", false, "discard tracked changes and release inside the close grace (never overrides pins or running agents)")
		fs.BoolVar(&o.wait, "wait", false, "wait until the daemon has released it")
		fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	}
	completePluginContext(cmd)
	if k.slotOK {
		cmd.ValidArgsFunction = completeFirst(c.completePRs, c.completeSlots)
	} else {
		cmd.ValidArgsFunction = completeFirst(c.completePRs)
	}
	return cmd
}

type targetOpts struct {
	workspace, cwd         string
	reason                 string // mute: the words after the PR
	json, force, wait, yes bool
}

func targetUsage(k targetKind) string {
	u := k.name + " <ref"
	if k.slotOK {
		u += "|slot"
	}
	u += ">"
	if k.req == engine.ReqMute {
		u += " [reason…]"
	}
	u += " [--workspace id] [--cwd path] [--json]"
	if k.name == "release" {
		u += " [--force] [--wait] [--yes]"
	}
	return u
}

func runTarget(c *Context, k targetKind, o targetOpts, pos []string) int {
	usage := targetUsage(k)
	if len(pos) > 1 && k.req != engine.ReqMute {
		return actUsage(c, k.name, "one target at a time", usage)
	}
	ref := ""
	if len(pos) > 0 {
		ref, o.reason = pos[0], strings.Join(pos[1:], " ")
	}
	if ref == "" && o.workspace == "" && o.cwd == "" {
		return actUsage(c, k.name, "which PR?", usage)
	}
	if k.req == engine.ReqMute {
		if code, refused := refuseInAgentPane(c, k.name); refused {
			return code
		}
	}
	mode := actFull
	if k.name == "release" {
		mode = actVerbose // an in-process release logs its steps
	}
	d, err := actNewDeps(c, mode)
	if err != nil {
		return cmdFail(c, k.name, err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return targetMain(ctx, c, d, k, ref, o)
}

// targetMain resolves the target and hands it to the daemon (release runs
// in-process under the lock when no daemon runs).
func targetMain(ctx context.Context, c *Context, d *actDeps, k targetKind, ref string, o targetOpts) int {
	t, err := targetResolve(ctx, d, ref, o.workspace, o.cwd)
	if err != nil {
		return cmdFail(c, k.name, verbFix(k.name, err))
	}
	var payload engine.TargetPayload
	label := ""
	switch {
	case t.BySlot && k.slotOK:
		payload.Slot = t.Slot.Name
		label = "slot " + t.Slot.Name
	case t.hasPR():
		payload.PRTarget = t.prTarget()
		payload.Reason = o.reason
		label = d.actLabel(t.full(), t.PR.Number)
	case k.slotOK:
		payload.Slot = t.Slot.Name
		label = "slot " + t.Slot.Name
	default:
		return cmdFail(c, k.name, fmt.Errorf("slot %s holds no PR: %s takes a PR", t.Slot.Name, k.name))
	}
	if k.name == "release" {
		payload.Force = o.force
		return releaseMain(ctx, c, d, t, label, payload, o)
	}
	out, err := d.reqs().send(ctx, k.req, payload, d.quick())
	if err != nil {
		return cmdFail(c, k.name, err)
	}
	if o.json {
		_ = writeJSON(c.Stdout, out.view())
		return out.jsonCode()
	}
	if out.Pending() && out.PID == 0 {
		out.printNotes(c.Stderr)
		fmt.Fprintf(c.Stderr, "%s %s is queued as request %d and applies when the daemon starts: %s\n", k.name, label, out.ID(), actDaemonFix)
		return 0
	}
	return out.print(c.Stdout, c.Stderr)
}

// targetResolve is resolve plus slot names (review3, owner/name#N) as
// targets.
func targetResolve(ctx context.Context, d *actDeps, ref, workspace, cwd string) (actTarget, error) {
	if ref != "" {
		if _, _, _, perr := d.refs().ResolvePR(ctx, ref); perr != nil {
			slot, err := d.Store.SlotByName(ctx, ref)
			if errors.Is(err, store.ErrNotFound) {
				return actTarget{}, fmt.Errorf("%q is neither a PR (URL, owner/repo#N, repo#N or N) nor a magnum slot name", ref)
			}
			if err != nil {
				return actTarget{}, err
			}
			if slot.PRID == nil {
				return actTarget{Slot: &slot, BySlot: true}, nil
			}
			t, err := d.targetByPRID(ctx, *slot.PRID)
			t.Slot, t.BySlot = &slot, true
			return t, err
		}
	}
	return d.resolve(ctx, ref, workspace, cwd)
}

// releaseMain hands a release to the daemon, or runs it here under the
// ops and daemon locks (acquireOps) when no daemon runs (it may reset a worktree and reload a
// schema, so it asks first on a terminal).
func releaseMain(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, payload engine.TargetPayload, o targetOpts) int {
	w, ew := c.Stdout, c.Stderr
	slot := t.Slot
	if slot == nil && t.hasPR() {
		s, err := slotOfPR(ctx, d.Store, t.PR.ID)
		if err != nil {
			return cmdFail(c, "release", err)
		}
		slot = s
	}
	what := label
	switch {
	case t.BySlot && t.hasPR():
		what += " (" + d.actLabel(t.full(), t.PR.Number) + ")"
	case slot != nil && t.hasPR():
		what += " (slot " + slot.Name + ")"
	}
	// Ask only on an interactive terminal: the herdr plugin captures the
	// output of its `here release` action, so a prompt there would hang unseen.
	// There --json needs --yes, as cleanup's does: the question would land in
	// the JSON, and skipping it would reset the worktree unasked.
	if d.StdinTTY && d.StdoutTTY && !o.yes {
		if o.json {
			return actUsage(c, "release", "--json needs --yes to apply (a terminal asks y/N first); nothing released",
				targetUsage(targetKindByName("release")))
		}
		if !d.confirm(ctx, ew, fmt.Sprintf("release %s? its sessions are parked and the worktree is reset (tracked changes discarded)", what)) {
			fmt.Fprintln(ew, "nothing released")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
	}
	unlock, who, err := d.Lock()
	switch {
	case err != nil:
		return cmdFail(c, "release", err)
	case who == opsBusy:
		return cmdFail(c, "release", opsBusyErr(d.Layout))
	case who == opsDaemon:
		return releaseHandOff(ctx, c, d, t, label, what, payload, o)
	case who != opsMine || unlock == nil:
		return cmdFail(c, "release", opsHolderErr(who))
	}
	defer unlock()

	if d.Cleanup == nil {
		return cmdFail(c, "release", errors.New("cleanup is not available"))
	}
	opts := cleanup.Options{Force: payload.Force}
	if payload.Slot != "" {
		opts.Slot = payload.Slot
	} else {
		opts.PR = &cleanup.PRRef{Repo: payload.Repo, Number: payload.Number}
	}
	plan, err := d.Cleanup.Plan(ctx, opts)
	if err != nil {
		return cmdFail(c, "release", err)
	}
	if len(plan.Actions) == 0 {
		if o.json {
			_ = writeJSON(w, struct {
				Plan cleanup.Plan `json:"plan"`
			}{plan})
		} else {
			fmt.Fprintln(w, strings.TrimSpace(cleanup.Render(plan)))
			fmt.Fprintf(ew, "nothing to release for %s\n", what)
		}
		if len(plan.Skipped) > 0 {
			return 1
		}
		return 0
	}
	if !o.json {
		fmt.Fprintln(w, strings.TrimSpace(cleanup.Render(plan)))
		fmt.Fprintln(ew, "no daemon is running: releasing here…")
	}
	rep, err := d.Cleanup.Apply(ctx, plan, false)
	if o.json {
		out := struct {
			Plan   cleanup.Plan   `json:"plan"`
			Report cleanup.Report `json:"report"`
			Error  string         `json:"error,omitempty"`
		}{Plan: plan, Report: rep}
		if err != nil {
			out.Error = err.Error()
		}
		_ = writeJSON(w, out)
	} else {
		fmt.Fprintln(w, strings.TrimSpace(cleanup.RenderReport(rep)))
	}
	if err != nil {
		if !o.json {
			fmt.Fprintf(ew, "magnum release: %v\n", err)
		}
		return 1
	}
	return 0
}

// releaseHandOff queues the release for the running daemon (it holds its
// lock) and, with --wait, follows the request to its end.
func releaseHandOff(ctx context.Context, c *Context, d *actDeps, t actTarget, label, what string, payload engine.TargetPayload, o targetOpts) int {
	w, ew := c.Stdout, c.Stderr
	rc := d.reqs()
	out, err := rc.send(ctx, engine.ReqRelease, payload, reqSend{})
	if err != nil {
		return cmdFail(c, "release", err)
	}
	id := out.ID()
	if !o.wait || out.PID == 0 {
		if o.json {
			_ = writeJSON(w, out.view())
		} else {
			out.printNotes(ew)
			fmt.Fprintf(w, "release of %s queued as request %d; the daemon runs it on its heavy worker (`magnum release %s --wait` or `magnum logs request:%d` follows it)\n", what, id, releaseRef(t, label), id)
		}
		if o.wait {
			// The daemon holds its lock but has no usable pidfile, so it was
			// not woken and may be gone: do not wait without bound.
			fmt.Fprintf(ew, "magnum release: not waiting: the daemon could not be woken (no usable pidfile %s); it picks request %d up on its next tick\n", d.Layout.Pid(), id)
			return 1
		}
		return 0
	}
	if !o.json {
		fmt.Fprintf(ew, "waiting for the daemon (pid %d) to release %s (request %d)…\n", out.PID, what, id)
	}
	if err := rc.wait(ctx, &out, -1, d.Poll); err != nil {
		fmt.Fprintf(ew, "stopped waiting; request %d goes on (`magnum logs request:%d`)\n", id, id)
		return 130
	}
	if o.json {
		_ = writeJSON(w, out.view())
		return out.jsonCode()
	}
	return out.print(w, ew)
}

func releaseRef(t actTarget, label string) string {
	if t.hasPR() && !t.BySlot {
		return label
	}
	return t.Slot.Name
}
