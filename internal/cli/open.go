package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const openUsage = "open <ref> [--role <role>] [--no-reveal] [--new-window] [--json] [--yes] [--timeout 5m] [--workspace id] [--cwd path]"

type openOpts struct {
	role, workspace, cwd           string
	noReveal, newWindow, json, yes bool
	timeout                        time.Duration
}

func newOpenCmd(c *Context) *cobra.Command {
	var o openOpts
	cmd := newCommand(groupAct, openUsage, "focus a PR's agent pane in herdr and bring herdr to the front",
		"Focus a PR's agent pane (its watch's judge by default, the fixer's while a fix loop's fixer works, or any role of the watch with --role; "+
			"`magnum roles` lists them) in herdr and bring the terminal to the front; without a herdr client a "+
			"new terminal tab (or --new-window) is opened. A parked PR is restored by the daemon first: a slot "+
			"is claimed, reviewed_sha checked out, the judge resumed, and the PR and slot held for you as a pin "+
			"holds them, until the later of [daemon] open_hold (30m) after the open and human_cooldown after your "+
			"last input in its panes, when the hold ends by itself (open_hold = \"0\" pins them until `magnum unpin`; "+
			"`magnum pin` makes the hold lasting). A terminal asks y/N before a restore (--yes does not; nor do "+
			"--json and a script, which restore at once); a focus of a live pane asks nothing. When the "+
			"role's pane is gone (its session lost or closed, as magnum closes a retired session's pane), a live agent "+
			"pane of the PR is focused instead, the judge's first; with none, `magnum review` lays out new panes.",
		func(pos []string) int { return runOpen(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.role, "role", "", "pane to focus: "+roleFlagHelp)
	fs.BoolVar(&o.noReveal, "no-reveal", false, "only focus inside herdr; do not bring the terminal to the front")
	fs.BoolVar(&o.newWindow, "new-window", false, "open a new terminal window (not a tab) when no herdr client is found")
	fs.BoolVar(&o.json, "json", false, "print what was focused and revealed as JSON (a restore does not ask first)")
	fs.BoolVar(&o.yes, "yes", false, "restore a parked PR without asking first")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long to wait for the daemon to restore a parked PR")
	fs.StringVar(&o.workspace, "workspace", "", "herdr workspace id instead of a PR (plugin context)")
	fs.StringVar(&o.cwd, "cwd", "", "directory inside a magnum slot instead of a PR (plugin context)")
	c.completeRole(cmd)
	completePluginContext(cmd)
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runOpen(c *Context, o openOpts, pos []string) int {
	if len(pos) > 1 {
		return actUsage(c, "open", "one PR at a time", openUsage)
	}
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	}
	if ref == "" && o.workspace == "" && o.cwd == "" {
		return actUsage(c, "open", "which PR?", openUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "open", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return openMain(ctx, c, d, ref, o)
}

func openMain(ctx context.Context, c *Context, d *actDeps, ref string, o openOpts) int {
	if err := actKnownRole(d.Cfg, o.role); err != nil {
		return actUsage(c, "open", err.Error(), openUsage)
	}
	t, err := d.resolve(ctx, ref, o.workspace, o.cwd)
	if err != nil {
		return cmdFail(c, "open", verbFix("open", err))
	}
	if !t.hasPR() {
		return cmdFail(c, "open", fmt.Errorf("slot %s holds no PR", t.Slot.Name))
	}
	if strings.TrimSpace(o.role) == "" {
		// While a fix loop's fixer works in the slot, its pane is the one to
		// see: the judge's may be parked, or laid out and never started.
		if fs, _ := statusFix(ctx, d.Store, t.PR.ID); fs != nil && fs.Phase == engine.FixPhaseFixing && fs.Role != "" {
			o.role = fs.Role
		}
	}
	spec, err := actRoleFor(d.Cfg, t.full(), o.role)
	if err != nil {
		return cmdFail(c, "open", err)
	}
	role := spec.Name
	label := d.actLabel(t.full(), t.PR.Number)
	res := actFocusResult{PR: label, URL: t.PR.URL, Role: role}

	judge := actJudgeFor(d.Cfg, t.full())
	sess, err := d.Store.LiveSessionByPRRole(ctx, t.PR.ID, role)
	if err == nil && store.Deref(sess.HerdrPaneID) == "" && store.Deref(sess.AgentName) == "" {
		err = store.ErrNotFound // still starting: wait for its pane like a restore
	}
	focused := false
	if errors.Is(err, store.ErrNotFound) {
		sess, err = openRestore(ctx, c, d, t, role, label, o)
		if gone, ok := errors.AsType[*openGoneError](err); ok {
			// The role's pane was closed with its session (lost, or closed):
			// any live pane of the PR beats an error.
			note := fmt.Sprintf("the %s session is %s and its pane gone; focused %%s (`magnum review %s` lays out new panes)",
				actRoleName(role), gone.state, label)
			if ok, ferr := d.focusLivePane(ctx, &res, t.PR.ID, judge, role, note, o); ok {
				err, focused = ferr, true
			}
		}
	}
	if err == nil && !focused {
		err = d.focus(ctx, &res, sess, o.noReveal, o.newWindow)
		if err != nil && herdr.IsCode(err, herdr.CodeAgentNotFound) {
			// herdr focuses agents only: a shell role's pane is a plain shell
			// beside the judge, and an agent role's pane may be gone (closed
			// by hand before a tick saw it), so focus a live agent of the PR.
			note := fmt.Sprintf("the %s pane is gone; focused %%s", actRoleName(role))
			if spec.IsShell() {
				note = fmt.Sprintf("herdr focuses agents only; focused %%s beside the %s pane", actRoleName(role))
			}
			if ok, ferr := d.focusLivePane(ctx, &res, t.PR.ID, judge, role, note, o); ok {
				err = ferr
			}
		}
	}
	if errors.Is(err, errOpenDeclined) {
		fmt.Fprintln(c.Stderr, "nothing restored")
		if ctx.Err() != nil {
			return 130
		}
		return 1
	}
	if err != nil {
		return openFail(c, o, res, err)
	}
	if o.json {
		_ = writeJSON(c.Stdout, res)
		return 0
	}
	fmt.Fprintln(c.Stdout, actFocusLine(res))
	return 0
}

func openFail(c *Context, o openOpts, res actFocusResult, err error) int {
	if o.json {
		_ = writeJSON(c.Stdout, struct {
			actFocusResult
			Error string `json:"error"`
		}{res, err.Error()})
		return 1
	}
	return cmdFail(c, "open", err)
}

// errOpenDeclined is openRestore's answer when the operator said no to the
// restore's question.
var errOpenDeclined = errors.New("restore declined")

// openSessions is what the PR's sessions of role say an open of it does:
// latest is the newest, parked the newest parked one, and starting says the
// newest is starting or live (the open waits for its pane). An open
// restores when one is parked and none is starting (openWouldRestore).
func openSessions(all []store.Session, role string) (parked, latest *store.Session, starting bool) {
	for i := range all {
		s := &all[i]
		if s.Role != role {
			continue
		}
		latest = s
		if s.State == store.SessionParked {
			parked = s
		}
	}
	starting = latest != nil && (latest.State == store.SessionStarting || latest.State == store.SessionLive)
	return parked, latest, starting
}

// openWouldRestore reports whether `magnum open` of role asks the daemon to
// restore the PR (claim a slot, resume its sessions, hold it): no session of
// role is up (starting, or live with a pane or an agent: the open focuses
// it), and one is parked. The board's o asks first when it does.
func openWouldRestore(all []store.Session, role string) bool {
	for _, s := range all {
		if s.Role == role && (s.State == store.SessionStarting || s.State == store.SessionLive) {
			return false
		}
	}
	parked, _, starting := openSessions(all, role)
	return parked != nil && !starting
}

// openRoleOf is the role `magnum open` of the PR opens without --role, as
// openMain resolves it: the fixer's while a fix loop's fixer works, else the
// watch's judge.
func openRoleOf(ctx context.Context, st *store.Store, cfg *config.Config, prID int64, full string) string {
	asked := ""
	if fs, _ := statusFix(ctx, st, prID); fs != nil && fs.Phase == engine.FixPhaseFixing && fs.Role != "" {
		asked = fs.Role
	}
	if cfg != nil {
		if r, err := actRoleFor(cfg, full, asked); err == nil {
			return r.Name
		}
	}
	return actJudgeFor(cfg, full)
}

// openHoldEnd is when h, the hold `magnum open` set on PR prID, ends: its
// Until, or human_cooldown after the PR's last input when that is later
// (engine.OpenHold.End; d is the zero Daemon without a config).
func openHoldEnd(ctx context.Context, st *store.Store, d config.Daemon, h engine.OpenHold, prID int64) time.Time {
	var at *time.Time
	if pr, err := st.PRByID(ctx, prID); err == nil {
		at = pr.HumanActiveAt
	}
	return h.End(d, at)
}

// openRestore handles a PR without a live session for role: a parked one is
// handed to the daemon (request "open": claim a slot, resume the sessions,
// hold the PR) and waited for, after a y on a terminal (not with --yes or
// --json: integrations call open, and a declined question is
// errOpenDeclined); anything else gets the exact next step.
func openRestore(ctx context.Context, c *Context, d *actDeps, t actTarget, role, label string, o openOpts) (store.Session, error) {
	all, err := d.Store.SessionsByPR(ctx, t.PR.ID)
	if err != nil {
		return store.Session{}, err
	}
	parked, latest, starting := openSessions(all, role)
	if parked == nil && !starting {
		if latest != nil {
			return store.Session{}, &openGoneError{state: latest.State,
				msg: fmt.Sprintf("the %s session of %s is %s: `magnum review %s` starts a new round with fresh sessions", actRoleName(role), label, latest.State, label)}
		}
		return store.Session{}, fmt.Errorf("%s has no %s session yet (PR state %s): `magnum review %s` starts one", label, actRoleName(role), t.PR.State, label)
	}
	hint := openResumeHint(d.Cfg, parked)
	reqID := int64(0)
	hold := d.Cfg.Daemon.OpenHold.Duration
	if !starting && d.StdinTTY && d.StdoutTTY && !o.yes && !o.json && !d.confirm(ctx, c.Stderr, tui.OpenRestoreQuestion(label, hold)) {
		return store.Session{}, errOpenDeclined
	}
	if !starting {
		// Restoring is for now: with no daemon nothing stays queued to
		// restore and hold the PR whenever one starts.
		out, err := d.reqs().send(ctx, actReqOpen, engine.OpenPayload{PRTarget: t.prTarget(), Role: role}, reqSend{NeedDaemon: true})
		switch {
		case errors.Is(err, errNoDaemon):
			return store.Session{}, fmt.Errorf("%s is parked and no daemon is running to restore it (%s)%s", label, actDaemonFix, hint)
		case err != nil:
			return store.Session{}, err
		}
		reqID = out.ID()
		if !o.json {
			out.printNotes(c.Stderr)
			fmt.Fprintf(c.Stderr, "%s is parked; asked the daemon to restore its sessions in a slot, held for you %s (request %d)…\n",
				label, tui.OpenHoldPhrase(hold), reqID)
		}
	} else if !o.json {
		fmt.Fprintf(c.Stderr, "waiting for the %s session of %s to start…\n", actRoleName(role), label)
	}
	deadline := d.now().Add(o.timeout)
	for {
		sess, err := d.Store.LiveSessionByPRRole(ctx, t.PR.ID, role)
		if err == nil && (store.Deref(sess.HerdrPaneID) != "" || store.Deref(sess.AgentName) != "") {
			return sess, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.Session{}, err
		}
		if reqID != 0 {
			req, err := d.Store.RequestByID(ctx, reqID)
			if err != nil {
				return store.Session{}, err
			}
			if req.State == store.RequestFailed {
				why := store.Deref(req.Result)
				if strings.Contains(why, "unknown request kind") {
					return store.Session{}, fmt.Errorf("this daemon cannot restore parked sessions yet: `magnum review %s` restores them with a new round%s", label, hint)
				}
				return store.Session{}, fmt.Errorf("the daemon could not restore %s: %s%s", label, why, hint)
			}
		}
		if o.timeout > 0 && !d.now().Before(deadline) {
			return store.Session{}, fmt.Errorf("timed out after %s waiting for the %s session of %s (`magnum status %s` shows why)", o.timeout, actRoleName(role), label, label)
		}
		if err := d.sleep(ctx, d.Poll); err != nil {
			return store.Session{}, err
		}
	}
}

// openGoneError is openRestore's answer for a role whose newest session is
// lost or closed, with no parked conversation to restore: its pane is gone
// (magnum closes a retired session's pane), and openMain focuses a live
// pane of the PR instead when there is one.
type openGoneError struct {
	state string // the session's state: lost or closed
	msg   string
}

func (e *openGoneError) Error() string { return e.msg }

// focusLivePane focuses a live agent pane of the PR other than except's
// (the judge's first, then the others in the order they started) when
// except's own pane cannot be focused, with note, whose %s is the role
// focused, in res.Note. herdr focuses agents only, so a pane without a live
// agent (a workspace holding nothing but the operator's shell) cannot take
// the focus. focused reports that herdr took it; err is then what the
// reveal said (unfocused, why the sessions could not be read, if so).
// Nothing focused leaves res as it was.
func (d *actDeps) focusLivePane(ctx context.Context, res *actFocusResult, prID int64, judge, except, note string, o openOpts) (focused bool, err error) {
	all, err := d.Store.SessionsByPR(ctx, prID)
	if err != nil {
		return false, err
	}
	var live []store.Session
	for _, first := range []bool{true, false} {
		for _, s := range all {
			if s.State == store.SessionLive && s.Role != except && store.Deref(s.AgentName) != "" && (s.Role == judge) == first {
				live = append(live, s)
			}
		}
	}
	for _, s := range live {
		saved := *res
		res.Note = fmt.Sprintf(note, actRoleName(s.Role))
		err := d.focus(ctx, res, s, o.noReveal, o.newWindow)
		if herdr.IsCode(err, herdr.CodeAgentNotFound) {
			*res = saved
			continue
		}
		return true, err
	}
	return false, nil
}

// openResumeHint is the manual resume command of a parked session.
func openResumeHint(cfg *config.Config, s *store.Session) string {
	if s == nil {
		return ""
	}
	argv := actResumeArgv(cfg, *s)
	if argv == nil {
		return ""
	}
	for i, a := range argv {
		argv[i] = actShellQuote(a)
	}
	cmd := strings.Join(argv, " ")
	if cwd := store.Deref(s.Cwd); cwd != "" {
		cmd = "cd " + actShellQuote(cwd) + " && " + cmd
	}
	return "\nresume by hand: " + cmd
}
