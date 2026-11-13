package cli

// `magnum fix`: ask the daemon to start a fix loop on the operator's own PR
// (a fixer agent fixes magnum's findings, then the push is re-reviewed, up to
// [pipeline] fix_cycles times; engine/fix.go), to leave the PR out of autofix
// (--off) or to put it back (--on), or to say that the operator did what a
// loop left them (--done: back to reviewed, no round). --decide posts the
// operator's decision in a thread the fixer left them, as them, and starts
// the loop that builds it (engine/fix_decide.go). The daemon decides and answers with the
// sentence this prints, the refusals included; --wait follows the loop with
// the review follower until the loop's row is gone and says how it ended.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const fixUsage = "fix <url|owner/repo#N|repo#N|N> [--off | --on | --done | --ci | --drop-push | --decide <thread>=<reply>...] [--wait] [--timeout <duration>] [--dry-run] [--json] [--yes]" +
	" | fix --drained [--json] | fix --handover all|<pr>|back <pr|all> [--dry-run] [--json] [--yes]"

// fixDefaultTimeout is how long --wait follows a loop by default.
const fixDefaultTimeout = reviewDefaultTimeout

// fixEndGrace is how many polls --wait gives the daemon to record the end of a
// loop whose row is gone: the row goes first, then the event that says why.
const fixEndGrace = 3

// fixEventWindow is how many of the PR's newest events --wait reads for the
// end of its loop.
const fixEventWindow = 200

type fixOpts struct {
	off, on, done, wait, dryRun, json, yes bool
	// merge is `magnum merge-parent`: a merge of the PR's parent alone
	// (engine.FixModeMerge), asked y/N with the daemon's plan. asked is the
	// plan a screen's y/N named (its P), which the merge is bound to.
	merge bool
	asked *engine.MergeAsked
	// ci is `magnum fix --ci`: a CI fix of the PR's failed required CI
	// (engine.FixModeCI), asked y/N with the daemon's plan. ciAsked is the
	// plan a screen's y/N named (its c), which the start is bound to.
	ci      bool
	ciAsked *engine.CIFixAsked
	timeout time.Duration // --timeout: stop following after this long (0 = no limit)
	// decide are the --decide values as typed, decisions what they say
	// (parseDecide).
	decide    []string
	decisions []engine.FixDecision
	// seenSum is, with done, the digest of the attention the operator saw
	// where a screen asked (attentionSum of the board's row); nil = the PR's
	// last_error, which the command shows before its y/N.
	seenSum *string
	// drained is --drained, the morning list (fix_handover in this file);
	// handover is --handover's value (all, back or a PR); dropPush is
	// --drop-push, and drop merge-parent's --drop: the morning list's drops.
	drained        bool
	handover       string
	dropPush, drop bool
}

func newFixCmd(c *Context) *cobra.Command {
	var o fixOpts
	cmd := newCommand(groupAct, fixUsage, "fix magnum's findings on your own PR now (--wait follows the loop to its end)",
		"Fix magnum's findings on your own PR now. A fixer agent (the fix role, claude-fix by default) fixes the findings "+
			"of magnum's review on the PR's current head, in the PR's slot and as the fix role's login (a fixer App's bot, or you; the question names it) (when the pool took that slot back, the fix "+
			"claims one at the reviewed head first, as magnum open does but without the pin; with no slot free it refuses and "+
			"says to run it again shortly): it pushes, answers the review threads "+
			"and what other reviewers wrote that nobody answered, and writes a report. A review that found nothing and left "+
			"nothing open starts a fix only when another reviewer's open thread, review body or comment waits for an answer: "+
			"the fixer then answers those alone. Magnum then reviews what it pushed and, while findings are left, starts the next cycle, up "+
			"to [pipeline] fix_cycles. A loop ends clean, or needing you when the fixer leaves findings to you, the cap is "+
			"reached or a run fails. The daemon refuses, with the reason as its answer, a PR magnum has not reviewed at its "+
			"current head, a review that found nothing while no other reviewer waits for an answer (or whose other reviewers "+
			"GitHub cannot list), a PR that is not yours or lives in a fork, a round in "+
			"flight, a snooze, a loop already running and a watch without a fix role. A pinned PR (magnum open, magnum pin) is unpinned by the fix, as "+
			"by a review; its slot's guards still keep a person's changes or agent safe, and the refusal names the guard. "+
			"While the daemon drains for a restart (daemon-restart --drain) or a reboot (magnum drain) a fix it would start "+
			"is held instead: it starts after the restart or the reboot, or once the drain is lifted without one, if the "+
			"review still stands at the same "+
			"head, and is dropped otherwise (the fix.dropped event and a toast say why; `magnum abort` takes it back). "+
			"While the GitHub budget of the identity the fix reads as is below [github] budget_floor, the fix is held the "+
			"same way and starts once the budget recovers, if the review still stands; so is a fix whose read of the other "+
			"reviewers' threads and comments, made before it starts, GitHub refuses for its rate limit. A held fix that would "+
			"start while the fix role's agent kind is paused (a usage limit) waits for the pause to end instead of being "+
			"dropped (a fix you ask for during the pause is refused with it). "+
			"A terminal asks y/N first (--yes does not; there --json needs "+
			"--yes), and a review agent's pane is refused. --wait follows the loop until its end and says how it ended "+
			"(clean after N fix cycles, needs you, pushed nothing, aborted or ended, and when the last fixer did not get the "+
			"other reviewers' threads and comments, that they were not read); --timeout (default 2h, 0 = no limit) "+
			"stops following it after that long, and the loop goes on; `magnum abort` stops a loop. --off takes the PR out "+
			"of autofix ([pipeline] autofix = \"own\"), so its findings stay yours (a `magnum fix` you ask for still runs, "+
			"with a note), and --on puts it back; both ask y/N and start no loop. --done says you did what a loop left you "+
			"(its Needs you, such as a change to the PR's description you made on GitHub): it shows that attention, asks y/N, "+
			"and the PR goes back from needs attention to reviewed at the head magnum reviewed, with no review and no loop. "+
			"The daemon refuses it for an attention that is not the fixer's calls (its cap, a failed run, a round's), one whose "+
			"fixer did not get the other reviewers' items (`magnum fix` reads them again), one that changed since you looked, "+
			"a head magnum has not reviewed, a loop that runs and a fix held for later. "+
			"--decide <thread>=\"<reply>\" (repeatable; the thread is the id or the link of a thread the fixer left you a call in) "+
			"posts your reply there as you (the gh identity the fix role uses), then starts the loop in one step: its fixer reads "+
			"your reply as your decision and builds it (the fix, or the won't-fix reply you chose), citing it, or leaves it to you "+
			"again. It shows each reply and asks y/N; the daemon refuses, with nothing posted, a thread the fixer made no call in "+
			"(or one it answered since), a PR that is not yours, a loop that runs, a fix held for later and every refusal of "+
			"`magnum fix` (a pinned PR is unpinned first, as for `magnum fix`). A reply of yours typed on GitHub in such a "+
			"thread, while no fix run of the PR is at work, is a decision too, for the next `magnum fix`; under autofix = "+
			"\"open\" the loop starts by itself once the PR settles only when every call the fixer left is a decision you "+
			"made: with anything else asked of you, run `magnum fix` or --decide. "+
			"--ci fixes your PR's failed required CI instead (a CI fix, as autofix = \"open\" starts by itself): the fixer "+
			"gets why the check failed (ci-failures.json beside its report), fixes the PR's own code, runs the failing tests and "+
			"pushes, at most 3 CI fixes a PR across its heads; it goes alone (no --off, --on, --done or --decide), asks y/N "+
			"with the daemon's plan (--yes does not; with no terminal it needs --yes), and the daemon refuses another's PR, a "+
			"head where no required check failed, a check red or never run on the base, a PR that changes its own CI config, "+
			"the cap, a head magnum has not reviewed or whose review leaves work (fix that first), a drain and a low GitHub "+
			"budget (a CI fix is never held); its plan reads why the check failed (the base's CI, the failed jobs and the end of "+
			"their logs, once for the head) and keeps it for the start. "+
			"--dry-run only asks the daemon what would happen.\n\n"+
			"The factory takes the fix loop over (the handover): --handover <pr> hands one own PR's fixes to the factory, at "+
			"once when none of magnum's fix work is in flight on it, else once that ends (magnum starts no new loop on it "+
			"meanwhile, and a loop in flight ends at its cycle boundary, with no next cycle); --handover all does so for every "+
			"own PR, now and later; both are refused while [factory] fix is not \"on\". --handover back <pr> returns a PR only while the factory has not worked it since its "+
			"import, and calls off a handover that waits; --handover back all lifts --handover all and returns every PR it may. "+
			"Each asks y/N on a terminal (--yes does not). --drained prints the morning list: every PR whose fix work keeps it "+
			"from handing over (a loop, a fixer's push record, a held merge, magnum's push waiting for its CI, a "+
			"pending request, a fixer's run), with your action for each; a fix held for a restart, a pause or the GitHub budget "+
			"holds no PR back: it goes with the PR, and the factory imports it. --drop-push drops the record of a fixer's push that "+
			"magnum keeps for its late settle (up to 24h), which the morning list names when the PR's head moved past it.",
		func(pos []string) int { return runFix(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.off, "off", false, "take the PR out of autofix: its findings stay yours (starts no loop)")
	fs.BoolVar(&o.on, "on", false, "put the PR back into autofix (starts no loop)")
	fs.BoolVar(&o.done, "done", false, "you did what the fix loop left you: back to reviewed with no review (starts no loop)")
	fs.BoolVar(&o.ci, "ci", false, "fix your PR's failed required CI (a CI fix, at most 3 a PR; asks y/N with the daemon's plan)")
	fs.StringArrayVar(&o.decide, "decide", nil, "post your decision in a `thread=reply` the fixer left you, as you, then fix (repeatable)")
	fs.BoolVar(&o.wait, "wait", false, "follow the loop until it ends: clean, needs you, pushed nothing, aborted or ended")
	fs.DurationVar(&o.timeout, "timeout", fixDefaultTimeout, "with --wait: stop following after this `duration` (0 = no limit); the loop goes on")
	fs.BoolVar(&o.dryRun, "dry-run", false, "ask the daemon what would happen; nothing is started or changed")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON (progress goes to stderr)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&o.drained, "drained", false, "list every PR whose fix work keeps it from handing over to the factory, with your action for each")
	fs.StringVar(&o.handover, "handover", "", "hand a PR's fixes (`all`, `<pr>`) to the factory, or `back` one (`back <pr|all>`)")
	fs.BoolVar(&o.dropPush, "drop-push", false, "drop the record of a fixer's push magnum keeps for its late settle")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

// verb is the command fixMain runs as: fix, or merge-parent.
func (o fixOpts) verb() string {
	if o.merge {
		return "merge-parent"
	}
	return "fix"
}

// usage is the command's usage line.
func (o fixOpts) usage() string {
	if o.merge {
		return mergeParentUsage
	}
	return fixUsage
}

func runFix(c *Context, o fixOpts, pos []string) int {
	switch {
	case o.drained:
		return runFixDrained(c, o, pos)
	case o.handover != "":
		return runFixHandover(c, o, pos)
	case o.dropPush && (o.done || o.off || o.on || o.ci || len(o.decide) > 0 || o.wait):
		return actUsage(c, "fix", "--drop-push goes alone, without --off, --on, --done, --ci, --decide or --wait", fixUsage)
	}
	switch {
	case len(pos) == 0:
		return actUsage(c, "fix", "which PR?", fixUsage)
	case len(pos) > 1:
		return actUsage(c, "fix", "one PR at a time", fixUsage)
	case o.done && (o.off || o.on):
		return actUsage(c, "fix", "--done goes alone, without --off or --on", fixUsage)
	case o.done && o.wait:
		return actUsage(c, "fix", "--done starts no loop, so there is nothing for --wait to follow", fixUsage)
	case o.off && o.on:
		return actUsage(c, "fix", "--off and --on exclude each other", fixUsage)
	case (o.off || o.on) && o.wait:
		return actUsage(c, "fix", "--off and --on start no loop, so there is nothing for --wait to follow", fixUsage)
	case len(o.decide) > 0 && (o.done || o.off || o.on):
		return actUsage(c, "fix", "--decide goes without --done, --off and --on", fixUsage)
	case o.ci && (o.done || o.off || o.on || len(o.decide) > 0):
		return actUsage(c, "fix", "--ci goes alone, without --off, --on, --done or --decide", fixUsage)
	}
	if len(o.decide) > 0 {
		decisions, err := parseDecide(o.decide)
		if err != nil {
			return actUsage(c, "fix", err.Error(), fixUsage)
		}
		o.decisions = decisions
	}
	if code, refused := refuseInAgentPane(c, "fix"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "fix", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return fixMain(ctx, c, d, pos[0], o)
}

// The kinds of end fixJSON reports.
const (
	fixEndClean    = "clean"          // the last re-review found nothing
	fixEndNothing  = "pushed_nothing" // the fixer pushed nothing and replied
	fixEndNeedsYou = "needs_you"      // findings left to the author, or the cap
	fixEndAborted  = "aborted"        // magnum abort
	fixEndEnded    = "ended"          // a failed run or re-review, a PR that closed
	fixEndGone     = "gone"           // the row went and no event says why
	fixEndMerged   = "merged"         // a merge of the parent (magnum merge-parent) merged and pushed it
)

// fixMergeEnds is how the end of a merge of the parent starts
// (engine endParentMerged, parentPushed).
const fixMergeEnds = "the merge of the parent ends: "

// fixEnding is how a fix loop ended and the sentence that says so.
type fixEnding struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// code is the exit code of a command that followed the loop to this end.
func (e fixEnding) code() int {
	if e.Kind == fixEndClean || e.Kind == fixEndNothing || e.Kind == fixEndMerged {
		return 0
	}
	return 1
}

// fixJSON is what --json prints.
type fixJSON struct {
	PR      string             `json:"pr"`
	URL     string             `json:"url"`
	DryRun  bool               `json:"dry_run,omitempty"`
	Payload *engine.FixPayload `json:"payload,omitempty"`
	Request *actRequestJSON    `json:"request,omitempty"`
	// Plan is the daemon's plan of a merge of the parent (its answer to
	// `magnum merge-parent --dry-run`, or to any merge under a daemon run
	// with --dry-run); Request's result is then the plan's words.
	Plan *engine.MergePlan `json:"plan,omitempty"`
	// CIPlan is the daemon's plan of a CI fix (its answer to `magnum fix
	// --ci --dry-run`).
	CIPlan *engine.CIFixPlan `json:"ci_plan,omitempty"`
	State  string            `json:"state"`
	End    *fixEnding        `json:"end,omitempty"` // with --wait: how the loop ended
	Error  string            `json:"error,omitempty"`
}

// fixProgress is where progress lines go: stdout, or stderr with --json.
func fixProgress(c *Context, o fixOpts) io.Writer { return reviewProgress(c, reviewOpts{json: o.json}) }

// fixMain resolves the PR, asks on a terminal, hands the request to the
// daemon, prints its answer and, with --wait, follows the loop it started.
// The screens call it with yes set, the board and the dashboard having asked.
// A merge of the parent without a terminal to ask on (an agent's shell, a
// pipe, a script) needs --yes, as a staging deploy does: it never starts on
// nobody's word; so does its drop (merge-parent --drop), which says so.
func fixMain(ctx context.Context, c *Context, d *actDeps, ref string, o fixOpts) int {
	if o.drop && !o.dryRun && !o.yes && (!d.StdinTTY || !d.StdoutTTY) {
		return actUsage(c, o.verb(), "a drop of the held merge asks y/N on a terminal, and there is none: pass --yes to drop it without asking, "+
			"or --dry-run to see what it drops; nothing queued", o.usage())
	}
	if o.merge && !o.drop && !o.dryRun && !o.yes && (!d.StdinTTY || !d.StdoutTTY) {
		return actUsage(c, o.verb(), "a merge of the parent asks y/N on a terminal, and there is none: pass --yes to merge without asking, "+
			"or --dry-run to see the plan; nothing queued", o.usage())
	}
	if o.ci && !o.dryRun && !o.yes && (!d.StdinTTY || !d.StdoutTTY) {
		return actUsage(c, o.verb(), "a CI fix asks y/N on a terminal, and there is none: pass --yes to start it without asking, "+
			"or --dry-run to see the plan; nothing queued", o.usage())
	}
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, o.verb(), verbFix(o.verb(), err))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	out := fixJSON{PR: label, URL: t.PR.URL, DryRun: o.dryRun, State: t.PR.State}
	if !o.json {
		reviewDescribe(ctx, c.Stdout, d, t, label, false, o.dryRun)
	}
	payload := engine.FixPayload{PRTarget: t.prTarget(), DryRun: o.dryRun, Off: o.off, On: o.on, Decide: o.decisions, DropPush: o.dropPush, Drop: o.drop}
	if o.merge {
		payload.Mode, payload.Asked = engine.FixModeMerge, o.asked
	}
	if o.ci {
		payload.Mode, payload.CIAsked = engine.FixModeCI, o.ciAsked
	}
	if !o.json {
		for _, d := range o.decisions { // what goes out as you, before the y/N
			fmt.Fprintf(c.Stdout, "  reply in thread %d: %s\n", d.Thread, d.Text)
		}
	}
	if o.done { // the attention you mark done: the daemon refuses one that changed since, and one you did not see
		payload.Done, payload.Seen = true, store.Deref(t.PR.LastError)
		if o.seenSum != nil && *o.seenSum != attentionSum(payload.Seen) { // the row was read before the attention changed
			return fixFail(c, o, out, fmt.Errorf("PR #%d needs you for something else since you looked: `magnum status %s` says what; nothing changed",
				t.PR.Number, label))
		}
		if !o.json && payload.Seen != "" {
			fmt.Fprintln(c.Stdout, "  needs you: "+payload.Seen)
		}
	}
	if o.dryRun {
		out.Payload = &payload
		b, _ := json.Marshal(payload)
		changed := "nothing is changed"
		if o.ci { // the plan reads the failed CI and keeps it for the start (the engine's fix_ci.go)
			changed = "nothing starts: the daemon reads why the CI failed, once for the head, and keeps it for the start"
		}
		fmt.Fprintf(fixProgress(c, o), "dry run: the daemon is asked what would happen (request %s %s); %s\n", engine.ReqFix, b, changed)
	}
	if t.PR.Pinned && !o.off && !o.on && !o.done && !o.merge && !o.ci && !o.dropPush { // a drop unpins nothing
		fmt.Fprintf(fixProgress(c, o), "%s is pinned: this fix unpins it (a person's changes in its slot still hold the fix)\n", label)
	}
	question := ""
	if o.merge && !o.drop && fixAsks(d, o) && !o.json { // the y/N names what the daemon would merge, into which branch, as whom
		plan, answer, err := mergeParentPlan(ctx, c.Stderr, d, payload)
		if err != nil {
			return fixFail(c, o, out, err)
		}
		if answer != "" { // nothing to merge, said as an answer
			fmt.Fprintln(c.Stdout, answer)
			return 0
		}
		question = mergeParentQuestion(plan)
		payload.Asked = &plan.Asked // y binds the merge to what the question named
	}
	if o.ci && fixAsks(d, o) && !o.json { // the y/N names the check, the head, the fixer and the count of CI fixes
		plan, err := ciFixPlan(ctx, c.Stderr, d, payload)
		if err != nil {
			return fixFail(c, o, out, err)
		}
		question = ciFixQuestion(plan)
		payload.CIAsked = &plan.Asked // y binds the start to what the question named
	}
	if code, ok := fixConfirm(ctx, c, d, label, cmp.Or(fixerLogin(d.Cfg, t.full()), "you"), question, o); !ok {
		return code
	}

	// A loop that starts must not start later, when nobody waits for it: with
	// no daemon to run it now the request is refused, never left queued.
	q := d.quick()
	q.NeedDaemon = true
	start := d.now()
	res, err := d.reqs().send(ctx, engine.ReqFix, payload, q)
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("%s: nothing was queued: %w", label, err)
		}
		return fixFail(c, o, out, err)
	}
	rv := res.view()
	out.Request = &rv
	if res.Failed() {
		res.printNotes(c.Stderr)
		return fixFail(c, o, out, errors.New(res.Result()))
	}
	// a merge's plan, the answer to a dry run or to any merge under a daemon
	// run with --dry-run, shows its words, not its JSON line, and starts
	// nothing
	plan, planned := engine.ParseMergePlan(res.Result())
	if planned = planned && o.merge && !res.Pending(); planned {
		out.Plan, rv.Result = &plan, plan.Text
	}
	ciPlan, ciPlanned := engine.ParseCIFixPlan(res.Result())
	if ciPlanned = ciPlanned && o.ci && !res.Pending(); ciPlanned {
		out.CIPlan, rv.Result, plan.Text = &ciPlan, ciPlan.Text, ciPlan.Text
		planned = true
	}
	if !o.json {
		if planned {
			res.printNotes(c.Stderr)
			fmt.Fprintln(c.Stdout, actClean(plan.Text))
		} else {
			res.print(c.Stdout, c.Stderr)
		}
	}
	if !o.wait || o.dryRun || planned || o.off || o.on || o.done || fixHeldForRestart(ctx, c, d, t, label, o) { // no loop was started (yet)
		if o.json {
			if pr, err := d.Store.PRByID(ctx, t.PR.ID); err == nil {
				out.State = pr.State
			}
			_ = writeJSON(c.Stdout, out)
		}
		return 0
	}
	return fixFollow(ctx, c, d, t, label, res, start, o, out)
}

// decideThreadRe reads the thread of a --decide value given by its link:
// "…#discussion_r<id>".
var decideThreadRe = regexp.MustCompile(`#discussion_r(\d{1,20})$`)

// parseDecide reads the --decide values, each "<thread>=<reply>": the
// thread by its first comment's id or its link (…#discussion_r<id>), the
// reply everything after the first "=" (it keeps its own commas and equals
// signs), never empty. The daemon checks the rest (engine.FixDecision).
func parseDecide(values []string) ([]engine.FixDecision, error) {
	out := make([]engine.FixDecision, 0, len(values))
	for _, v := range values {
		thread, text, ok := strings.Cut(v, "=")
		thread = strings.TrimSpace(thread)
		if m := decideThreadRe.FindStringSubmatch(thread); m != nil {
			thread = m[1]
		}
		id, err := strconv.ParseInt(thread, 10, 64)
		if !ok || err != nil || id <= 0 {
			return nil, fmt.Errorf("--decide takes <thread id or link>=\"<your reply>\", not %q", v)
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("--decide %d: the reply is empty", id)
		}
		out = append(out, engine.FixDecision{Thread: id, Text: text})
	}
	return out, nil
}

// attentionSum is a digest of a PR's stored error (its last_error) that a
// screen can carry through its sanitizers unchanged and never draws: the
// board's d sends it back as the attention you saw (PRBoardRow.AttentionSum),
// and fixMain refuses --done when the PR's attention no longer has it. ""
// for none.
func attentionSum(msg string) string {
	if msg == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(msg))
	return hex.EncodeToString(sum[:8])
}

// fixHeldForRestart reports whether the daemon held the fix, through a drain
// for a restart, while the GitHub budget is below the floor or while the fix
// role's agent kind is paused (engine.KVPRFixHeld): no loop runs before the
// restart, the budget's recovery or the pause's end, so --wait has nothing
// to follow yet, and it says when the fix starts.
func fixHeldForRestart(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, o fixOpts) bool {
	raw, held, err := d.Store.GetKV(ctx, engine.KVPRFixHeld(t.PR.ID))
	if err != nil || !held {
		return false
	}
	fmt.Fprintf(fixProgress(c, o), "%s: nothing to follow yet: the fix starts %s (`magnum status %s` shows it then)\n", label, engine.HeldFixStarts(raw), label)
	return true
}

// fixAsks reports whether the command asks y/N: on an interactive
// terminal, for anything but a dry run, without --yes.
func fixAsks(d *actDeps, o fixOpts) bool { return !o.dryRun && !o.yes && d.StdinTTY && d.StdoutTTY }

// fixerLogin is the login the fixer of repo's watch pushes and replies as
// (its fix role's identity: a fixer App's bot, or your own login), ""
// when the config does not say.
func fixerLogin(cfg *config.Config, repo string) string {
	if cfg == nil {
		return ""
	}
	if r, ok := cfg.FixRoleFor(cfg.WatchFor(repo)); ok {
		if id := cfg.IdentityByName(r.Identity); id != nil {
			return id.Login
		}
	}
	return ""
}

// mergeParentDry is how the words of the daemon's plan of a merge of the
// parent start (engine.MergePlan's Text); what follows names the PR, who
// merges which tip of which base into which branch, and as whom.
const mergeParentDry = "dry run: would merge the parent of "

// mergeParentPlan asks the daemon, in a dry run, what a merge of the
// parent of payload's PR would do, for its y/N: its plan, or answer, the
// daemon's words when it has none (nothing to merge); a refusal, an answer
// still pending, or the words of a daemon from before the plan
// (errMergePlanPredates) is the error. Without a plan it prints the
// request's notes (a build skew) on ew; with one the merge's request does,
// after the y.
func mergeParentPlan(ctx context.Context, ew io.Writer, d *actDeps, payload engine.FixPayload) (plan engine.MergePlan, answer string, err error) {
	payload.DryRun = true
	q := d.quick()
	q.NeedDaemon = true
	res, err := d.reqs().send(ctx, engine.ReqFix, payload, q)
	if err != nil {
		return engine.MergePlan{}, "", err
	}
	if plan, ok := engine.ParseMergePlan(res.Result()); ok && res.Req.State == store.RequestDone {
		return plan, "", nil
	}
	res.printNotes(ew)
	switch {
	case res.Failed():
		return engine.MergePlan{}, "", errors.New(res.Result())
	case res.Pending():
		return engine.MergePlan{}, "", fmt.Errorf("the daemon has not planned the merge within %s (request %d): ask again later", q.Wait, res.ID())
	case mergePlanPredates(res.Result()):
		return engine.MergePlan{}, "", errMergePlanPredates
	}
	return engine.MergePlan{}, actClean(res.Result()), nil
}

// ciFixPlan asks the daemon, in a dry run, what a CI fix of payload's PR
// would do, for its y/N: its plan; a refusal (nothing failed, the base, the
// cap, …), an answer still pending, or the words of a daemon from before
// CI fix mode is the error.
func ciFixPlan(ctx context.Context, ew io.Writer, d *actDeps, payload engine.FixPayload) (engine.CIFixPlan, error) {
	payload.DryRun = true
	q := d.quick()
	q.NeedDaemon = true
	res, err := d.reqs().send(ctx, engine.ReqFix, payload, q)
	if err != nil {
		return engine.CIFixPlan{}, err
	}
	if plan, ok := engine.ParseCIFixPlan(res.Result()); ok && res.Req.State == store.RequestDone {
		return plan, nil
	}
	res.printNotes(ew)
	switch {
	case res.Failed():
		return engine.CIFixPlan{}, errors.New(res.Result())
	case res.Pending():
		return engine.CIFixPlan{}, fmt.Errorf("the daemon has not planned the CI fix within %s (request %d): ask again later", q.Wait, res.ID())
	}
	return engine.CIFixPlan{}, errCIFixPlanPredates
}

// errCIFixPlanPredates refuses the answer to a dry run of a CI fix that is
// no plan: a daemon from before CI fix mode, which would start a findings
// fix of its own.
var errCIFixPlanPredates = errors.New("this daemon predates the CI fix's plan: restart it (`magnum daemon-restart`), then ask again; nothing queued")

// ciFixQuestion is the y/N of plan: "Fix the failed CI of <PR>: claude-fix
// fixes required check Completion at a1a1a1a (2 failed jobs) in review1 as
// fixer-app[bot], CI fix 1 of 3?".
func ciFixQuestion(plan engine.CIFixPlan) string {
	return "Fix the failed CI of " + actClean(strings.TrimPrefix(plan.Text, engine.CIFixDry)) + "?"
}

// errMergePlanPredates refuses the answer to a dry run of a merge of the
// parent from a daemon whose build predates the merge's plan
// (engine.MergePlan): its words alone name nothing the y could bind the
// merge to.
var errMergePlanPredates = errors.New("this daemon predates the merge's plan: restart it (`magnum daemon-restart`), then ask again; nothing queued")

// mergePlanPredates reports whether result, the answer to a dry run of a
// merge of the parent, is the words of a daemon from before the plan: the
// plan's words with no plan before them.
func mergePlanPredates(result string) bool {
	_, ok := engine.ParseMergePlan(result)
	return !ok && strings.HasPrefix(strings.TrimSpace(result), mergeParentDry)
}

// mergeParentQuestion is the y/N of plan: "Merge the parent of <PR>:
// magnum merges master at c0c0c0c into feature as fixer-app[bot], with no
// conflict?".
func mergeParentQuestion(plan engine.MergePlan) string {
	return "Merge the parent of " + actClean(strings.TrimPrefix(plan.Text, mergeParentDry)) + "?"
}

// fixConfirm asks y/N on an interactive terminal, naming what is about to
// happen (question, else the fix's own as login, the fixer's); ok is false
// when the command ends here (code is its exit code). A dry run changes
// nothing and asks nothing, and --yes and a script skip the question.
// --json on a terminal needs --yes: the question would land in the JSON,
// and skipping it would apply unasked.
func fixConfirm(ctx context.Context, c *Context, d *actDeps, label, login, question string, o fixOpts) (code int, ok bool) {
	if !fixAsks(d, o) {
		return 0, true
	}
	declined := "nothing queued"
	if question == "" {
		question = fmt.Sprintf("Fix the findings of %s as %s?", label, login)
	}
	switch {
	case o.off:
		question, declined = fmt.Sprintf("Leave the findings of %s to you (no autofix)?", label), "nothing changed"
	case o.on:
		question, declined = fmt.Sprintf("Autofix %s again?", label), "nothing changed"
	case o.done:
		question, declined = fmt.Sprintf("Mark what the fixer left on %s done (back to reviewed, no review)?", label), "nothing changed"
	case o.dropPush:
		question, declined = fmt.Sprintf("Drop the record of the fixer's push on %s (magnum settles it no more)?", label), "nothing changed"
	case o.drop:
		question, declined = fmt.Sprintf("Drop the merge of the parent magnum holds for your y on %s (nothing is pushed)?", label), "nothing changed"
	case len(o.decisions) == 1:
		question = fmt.Sprintf("Post this reply as you in %s and fix it?", label)
	case len(o.decisions) > 1:
		question = fmt.Sprintf("Post these %d replies as you in %s and fix it?", len(o.decisions), label)
	}
	if o.json {
		return actUsage(c, o.verb(), "--json needs --yes to apply (a terminal asks y/N first); "+declined, o.usage()), false
	}
	if !d.confirm(ctx, c.Stdout, question) {
		fmt.Fprintln(c.Stderr, declined)
		if ctx.Err() != nil {
			return 130, false
		}
		return 1, false
	}
	return 0, true
}

func fixFail(c *Context, o fixOpts, out fixJSON, err error) int {
	if o.json {
		out.Error = err.Error()
		_ = writeJSON(c.Stdout, out)
		return 1
	}
	return cmdFail(c, o.verb(), err)
}

// fixFollow follows the loop the answered request started (res, queued at
// start) with the review follower: it prints the PR's states and the fixer's
// and the re-reviews' runs, and it is over when the loop's row is gone.
func fixFollow(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, res reqOutcome, start time.Time, o fixOpts, out fixJSON) int {
	w := &fixWatch{}
	// The follower prints no JSON itself (its options carry no --json): the
	// command does, with how the loop ended.
	w.f = &reviewFollower{c: c, d: d, t: t, label: label, reqID: res.ID(), start: start, o: reviewOpts{timeout: o.timeout},
		progress: fixProgress(c, o), runs: map[string]string{}, verb: "fix", end: w.done,
		goesOn: fmt.Sprintf("the fix loop goes on (`magnum status %s` shows it, `magnum abort %s` stops it)", label, label)}
	code := w.f.run(ctx)
	if req, err := d.Store.RequestByID(ctx, res.ID()); err == nil {
		res.Req = req
		rv := res.view()
		out.Request = &rv
	}
	out.State, out.Error = cmp.Or(w.f.out.State, out.State), w.f.out.Error
	if w.ending != nil {
		out.End = w.ending
		if code != 0 {
			out.Error = w.ending.Text
		}
	}
	if o.json {
		_ = writeJSON(c.Stdout, out)
	}
	return code
}

// fixWatch is the end of the follower of a fix loop.
type fixWatch struct {
	f      *reviewFollower
	gone   int        // polls that found the loop's row gone and no event saying why
	ending *fixEnding // how the loop ended, once it did
}

// done is the follower's completion: the loop is over when its row is gone
// (it stays through the forced re-review, so the PR being reviewed again is
// no end), and its end is read from the events and the PR.
func (w *fixWatch) done(ctx context.Context, pr store.PR) (code int, done bool) {
	f := w.f
	if !f.reqDone {
		return 0, false
	}
	if _, ok, err := f.d.Store.GetKV(ctx, engine.KVPRFix(pr.ID)); err != nil || ok {
		return 0, false // alive, or a registry that cannot say: no end
	}
	end, found := fixEndOf(ctx, f.d, f.t, f.start)
	if !found {
		// The row goes first and its event follows: give the daemon a moment.
		if w.gone++; w.gone < fixEndGrace {
			return 0, false
		}
		end = fixEnding{Kind: fixEndGone, Text: fmt.Sprintf("the fix loop is gone, but magnum recorded no end for it (`magnum logs pr:%s` has its events)", f.label)}
	}
	w.ending = &end
	if end.code() == 0 {
		fmt.Fprintf(f.progress, "%s: %s\n", f.label, end.Text)
		return 0, true
	}
	ew := f.c.Stderr
	why, ok := prAttention(ctx, f.d.Store, pr, f.label)
	switch {
	case ok && end.Kind == fixEndNeedsYou && len(why.Steps) > 0:
		// A call per finding: counted on the end's line, each on its own below.
		fmt.Fprintf(ew, "%s: needs you: %s\n", f.label, why.Cause)
		for i, s := range why.Steps {
			label := "     "
			if i == 0 {
				label = "fix: "
			}
			fmt.Fprintf(ew, "%s%s\n", label, s)
		}
	case ok:
		fmt.Fprintf(ew, "%s: %s\nfix: %s\n", f.label, end.Text, why.Fix)
	default:
		fmt.Fprintf(ew, "%s: %s\n", f.label, end.Text)
	}
	return end.code(), true
}

// fixEndOf reads how the PR's loop ended from the events at or after start:
// the latest of the loop's own end events. A fixer that pushed nothing ends
// the loop after its replies (fix.replied), and a cycle that pushed does not,
// its replies come before the re-review. Replies are the end "pushed nothing"
// only when the cycle's fix run recorded the outcome "replied": settle writes
// fix.replied first, and a fixer that left findings to the author without a
// push records "needs_you" and then writes fix.needs_you after the loop's
// row is gone, so the replies alone are no success. A fixer stopped while it
// works ends with pr.aborted (an abort) or pr.ignored (magnum ignore) whose
// data says fix: the abort of a review round has no such flag and is no end
// of the loop.
func fixEndOf(ctx context.Context, d *actDeps, t actTarget, start time.Time) (fixEnding, bool) {
	evs, err := d.Store.EventsBySubject(ctx, fmt.Sprintf("pr:%s#%d", t.full(), t.PR.Number), fixEventWindow)
	if err != nil {
		return fixEnding{}, false
	}
	var end fixEnding
	found := false
	pushed := map[int]bool{} // cycles that pushed
	for _, ev := range evs {
		if ev.At.Before(start) {
			continue
		}
		var data struct {
			Cycle        int    `json:"cycle"`
			Run          string `json:"run"`
			Fix          bool   `json:"fix"`
			OthersUnread string `json:"others_unread"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		msg := actClean(ev.Message)
		var e fixEnding
		switch ev.Kind {
		case "fix.pushed":
			pushed[data.Cycle] = true
			continue
		case "fix.replied":
			if pushed[data.Cycle] || !fixRunReplied(ctx, d, data.Run) {
				continue
			}
			e = fixEnding{fixEndNothing, "pushed nothing: " + msg}
		case "fix.done":
			e = fixEnding{fixEndClean, "clean"}
			if data.Cycle > 0 {
				e.Text += " after " + textx.Count(data.Cycle, "fix cycle", "fix cycles")
			}
			if data.OthersUnread != "" { // the last fixer did not get all the other reviewers' items
				e.Text += fmt.Sprintf(" for magnum's findings; the other reviewers' threads and comments were %s: `magnum fix %s#%d` reads them again",
					actClean(data.OthersUnread), t.full(), t.PR.Number)
			}
		case "fix.needs_you", "fix.capped":
			e = fixEnding{fixEndNeedsYou, "needs you: " + strings.TrimPrefix(msg, "fix: ")}
		case "fix.ended":
			if rest, ok := strings.CutPrefix(msg, fixMergeEnds); ok {
				e = fixEnding{fixEndMerged, rest}
				break
			}
			fallthrough
		case "fix.failed":
			for _, prefix := range []string{"fix loop ended: ", "the fix loop ends: "} {
				msg = strings.TrimPrefix(msg, prefix)
			}
			e = fixEnding{fixEndEnded, "ended: " + msg}
		case "fix.stopped":
			e = fixEnding{fixEndAborted, "aborted"}
		case "pr.aborted", "pr.ignored":
			if !data.Fix {
				continue
			}
			e = fixEnding{fixEndAborted, "aborted"}
			if ev.Kind == "pr.ignored" {
				e.Text += ": the PR was ignored"
			}
		default:
			continue
		}
		end, found = e, true
	}
	return end, found
}

// fixRunReplied reports whether the fix run runID ended with the outcome
// "replied": the fixer pushed nothing and left nothing to the author. A run
// that cannot be read, or that recorded no outcome yet, is not that.
func fixRunReplied(ctx context.Context, d *actDeps, runID string) bool {
	if runID == "" {
		return false
	}
	r, err := d.Store.RunByID(ctx, runID)
	return err == nil && r.Kind == store.RunFix && store.Deref(r.Outcome) == "replied"
}

// runFixDrained is `magnum fix --drained`, the morning list: every PR whose
// fix work in flight keeps it from handing over to the factory
// (engine.FixInFlight, from the registry), each record with your action.
// It takes no PR and changes nothing; empty is the deletion's gate.
func runFixDrained(c *Context, o fixOpts, pos []string) int {
	switch {
	case len(pos) > 0:
		return actUsage(c, "fix", "--drained lists every PR: it takes none", fixUsage)
	case o.handover != "" || o.off || o.on || o.done || o.ci || o.dropPush || o.wait || o.dryRun || len(o.decide) > 0:
		return actUsage(c, "fix", "--drained goes alone (--json aside)", fixUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "fix", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	list, err := engine.FixInFlight(ctx, d.Store, d.Cfg, d.now())
	if err != nil {
		return cmdFail(c, "fix", err)
	}
	if o.json {
		if list == nil {
			list = []engine.UndrainedPR{}
		}
		if err := writeJSON(c.Stdout, list); err != nil {
			return cmdFail(c, "fix", err)
		}
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintln(c.Stdout, "none of magnum's fix work is in flight: every PR can hand over to the factory")
	}
	for _, u := range list {
		line := d.actLabel(u.Repo, u.Number) + "  " + u.URL
		if u.Handing {
			line += "  (hands over to the factory once this ends)"
		}
		fmt.Fprintln(c.Stdout, line)
		for _, r := range u.Records {
			fmt.Fprintf(c.Stdout, "  %s\n    → %s\n", actClean(r.What), actClean(r.Action))
		}
	}
	// A held fix holds no PR back: it goes with the PR, its row kept for the
	// factory's import (engine/fix_drained.go).
	if held, err := d.Store.KVKeys(ctx, "pr.*.fix_held"); err == nil && len(held) > 0 {
		fmt.Fprintf(c.Stdout, "%s no PR back: each goes with its PR, and the factory imports it\n", textx.Count(len(held), "held fix holds", "held fixes hold"))
	}
	return 0
}

// runFixHandover is `magnum fix --handover all|<pr>|back <pr|all>`: it asks
// y/N on a terminal (--yes, a dry run and a script do not), then the
// daemon, which flips a quiet PR at once (engine.ReqFixHandover); with no
// daemon nothing is queued.
func runFixHandover(c *Context, o fixOpts, pos []string) int {
	if o.off || o.on || o.done || o.ci || o.dropPush || o.wait || len(o.decide) > 0 {
		return actUsage(c, "fix", "--handover goes alone (--dry-run, --json and --yes aside)", fixUsage)
	}
	var p engine.HandoverPayload
	ref := ""
	switch {
	case o.handover == "all" && len(pos) == 0:
		p.All = true
	case o.handover == "back" && len(pos) == 1 && pos[0] == "all":
		p.Back, p.All = true, true
	case o.handover == "back" && len(pos) == 1:
		p.Back, ref = true, pos[0]
	case o.handover != "all" && o.handover != "back" && len(pos) == 0:
		ref = o.handover
	default:
		return actUsage(c, "fix", "--handover takes all, one PR, or back with one PR or all", fixUsage)
	}
	p.DryRun = o.dryRun
	if code, refused := refuseInAgentPane(c, "fix"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "fix", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	what := "every own PR"
	if ref != "" {
		t, err := d.resolveRef(ctx, ref)
		if err != nil {
			return cmdFail(c, "fix", verbFix("fix", err))
		}
		p.PRTarget, what = t.prTarget(), d.actLabel(t.full(), t.PR.Number)
	}
	if fixAsks(d, o) {
		if o.json {
			return actUsage(c, "fix", "--json needs --yes to apply (a terminal asks y/N first); nothing queued", fixUsage)
		}
		question := fmt.Sprintf("Hand the fixes of %s to the factory (at once while none of magnum's fix work is in flight, else once it ends)?", what)
		switch {
		case p.Back && p.All:
			question = "Stop handing own PRs to the factory, and return every PR it has not worked since its import to magnum?"
		case p.Back:
			question = fmt.Sprintf("Return the fixes of %s to magnum (only while the factory has not worked it since its import)?", what)
		}
		if !d.confirm(ctx, c.Stdout, question) {
			fmt.Fprintln(c.Stderr, "nothing queued")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
	}
	q := d.quick()
	q.NeedDaemon = true
	res, err := d.reqs().send(ctx, engine.ReqFixHandover, p, q)
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("%s: nothing was queued: %w", what, err)
		}
		return cmdFail(c, "fix", err)
	}
	if o.json {
		_ = writeJSON(c.Stdout, res.view())
		return res.jsonCode()
	}
	return res.print(c.Stdout, c.Stderr)
}
