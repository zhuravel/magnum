package cli

// `magnum fix`: ask the daemon to start a fix loop on the operator's own PR
// (a fixer agent fixes magnum's findings, then the push is re-reviewed, up to
// [pipeline] fix_cycles times; engine/fix.go), to leave the PR out of autofix
// (--off) or to put it back (--on). The daemon decides and answers with the
// sentence this prints, the refusals included; --wait follows the loop with
// the review follower until the loop's row is gone and says how it ended.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const fixUsage = "fix <url|owner/repo#N|repo#N|N> [--off | --on] [--wait] [--timeout <duration>] [--dry-run] [--json] [--yes]"

// fixDefaultTimeout is how long --wait follows a loop by default.
const fixDefaultTimeout = reviewDefaultTimeout

// fixEndGrace is how many polls --wait gives the daemon to record the end of a
// loop whose row is gone: the row goes first, then the event that says why.
const fixEndGrace = 3

// fixEventWindow is how many of the PR's newest events --wait reads for the
// end of its loop.
const fixEventWindow = 200

type fixOpts struct {
	off, on, wait, dryRun, json, yes bool
	timeout                          time.Duration // --timeout: stop following after this long (0 = no limit)
}

func newFixCmd(c *Context) *cobra.Command {
	var o fixOpts
	cmd := newCommand(groupAct, fixUsage, "fix magnum's findings on your own PR now (--wait follows the loop to its end)",
		"Fix magnum's findings on your own PR now. A fixer agent (the fix role, claude-fix by default) fixes the findings "+
			"of magnum's review on the PR's current head, in the PR's slot and as you: it pushes, answers the review threads "+
			"and writes a report. Magnum then reviews what it pushed and, while findings are left, starts the next cycle, up "+
			"to [pipeline] fix_cycles. A loop ends clean, or needing you when the fixer leaves findings to you, the cap is "+
			"reached or a run fails. The daemon refuses, with the reason as its answer, a PR magnum has not reviewed at its "+
			"current head, a review that found nothing and left nothing open, a PR that is not yours or lives in a fork, a round in "+
			"flight, a snooze, a loop already running and a watch without a fix role. A pinned PR (magnum open, magnum pin) is unpinned by the fix, as "+
			"by a review; its slot's guards still keep a person's changes or agent safe, and the refusal names the guard. "+
			"While the daemon drains for a restart (daemon-restart --drain) a fix it would start is held instead: it "+
			"starts after the restart, or once the drain is lifted without one, if the review still stands at the same "+
			"head, and is dropped otherwise (the fix.dropped event and a toast say why; `magnum abort` takes it back). "+
			"A terminal asks y/N first (--yes does not; there --json needs "+
			"--yes), and a review agent's pane is refused. --wait follows the loop until its end and says how it ended "+
			"(clean after N fix cycles, needs you, pushed nothing, aborted or ended); --timeout (default 2h, 0 = no limit) "+
			"stops following it after that long, and the loop goes on; `magnum abort` stops a loop. --off takes the PR out "+
			"of autofix ([pipeline] autofix = \"own\"), so its findings stay yours (a `magnum fix` you ask for still runs, "+
			"with a note), and --on puts it back; both ask y/N and start no loop. --dry-run only asks the daemon what would happen.",
		func(pos []string) int { return runFix(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.off, "off", false, "take the PR out of autofix: its findings stay yours (starts no loop)")
	fs.BoolVar(&o.on, "on", false, "put the PR back into autofix (starts no loop)")
	fs.BoolVar(&o.wait, "wait", false, "follow the loop until it ends: clean, needs you, pushed nothing, aborted or ended")
	fs.DurationVar(&o.timeout, "timeout", fixDefaultTimeout, "with --wait: stop following after this `duration` (0 = no limit); the loop goes on")
	fs.BoolVar(&o.dryRun, "dry-run", false, "ask the daemon what would happen; nothing is started or changed")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON (progress goes to stderr)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runFix(c *Context, o fixOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, "fix", "which PR?", fixUsage)
	case len(pos) > 1:
		return actUsage(c, "fix", "one PR at a time", fixUsage)
	case o.off && o.on:
		return actUsage(c, "fix", "--off and --on exclude each other", fixUsage)
	case (o.off || o.on) && o.wait:
		return actUsage(c, "fix", "--off and --on start no loop, so there is nothing for --wait to follow", fixUsage)
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
)

// fixEnding is how a fix loop ended and the sentence that says so.
type fixEnding struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// code is the exit code of a command that followed the loop to this end.
func (e fixEnding) code() int {
	if e.Kind == fixEndClean || e.Kind == fixEndNothing {
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
	State   string             `json:"state"`
	End     *fixEnding         `json:"end,omitempty"` // with --wait: how the loop ended
	Error   string             `json:"error,omitempty"`
}

// fixProgress is where progress lines go: stdout, or stderr with --json.
func fixProgress(c *Context, o fixOpts) io.Writer { return reviewProgress(c, reviewOpts{json: o.json}) }

// fixMain resolves the PR, asks on a terminal, hands the request to the
// daemon, prints its answer and, with --wait, follows the loop it started.
// The screens call it with yes set, the board and the dashboard having asked.
func fixMain(ctx context.Context, c *Context, d *actDeps, ref string, o fixOpts) int {
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "fix", verbFix("fix", err))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	out := fixJSON{PR: label, URL: t.PR.URL, DryRun: o.dryRun, State: t.PR.State}
	if !o.json {
		reviewDescribe(ctx, c.Stdout, d, t, label, false, o.dryRun)
	}
	payload := engine.FixPayload{PRTarget: t.prTarget(), DryRun: o.dryRun, Off: o.off, On: o.on}
	if o.dryRun {
		out.Payload = &payload
		b, _ := json.Marshal(payload)
		fmt.Fprintf(fixProgress(c, o), "dry run: the daemon is asked what would happen (request %s %s); nothing is changed\n", engine.ReqFix, b)
	}
	if t.PR.Pinned && !o.off && !o.on {
		fmt.Fprintf(fixProgress(c, o), "%s is pinned: this fix unpins it (a person's changes in its slot still hold the fix)\n", label)
	}
	if code, ok := fixConfirm(ctx, c, d, label, o); !ok {
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
	if !o.json {
		res.print(c.Stdout, c.Stderr)
	}
	if !o.wait || o.dryRun || o.off || o.on || fixHeldForRestart(ctx, c, d, t, label, o) { // no loop was started (yet)
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

// fixHeldForRestart reports whether the daemon held the fix through a drain
// for a restart (engine.KVPRFixHeld): no loop runs before the restart, so
// --wait has nothing to follow yet, and it says so.
func fixHeldForRestart(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, o fixOpts) bool {
	if _, held, err := d.Store.GetKV(ctx, engine.KVPRFixHeld(t.PR.ID)); err != nil || !held {
		return false
	}
	fmt.Fprintf(fixProgress(c, o), "%s: nothing to follow yet: the fix starts after the restart (`magnum status %s` shows it then)\n", label, label)
	return true
}

// fixConfirm asks y/N on an interactive terminal, naming what is about to
// happen; ok is false when the command ends here (code is its exit code). A
// dry run changes nothing and asks nothing, and --yes and a script skip the
// question. --json on a terminal needs --yes: the question would land in the
// JSON, and skipping it would apply unasked.
func fixConfirm(ctx context.Context, c *Context, d *actDeps, label string, o fixOpts) (code int, ok bool) {
	if o.dryRun || o.yes || !d.StdinTTY || !d.StdoutTTY {
		return 0, true
	}
	question, declined := fmt.Sprintf("Fix the findings of %s as you?", label), "nothing queued"
	switch {
	case o.off:
		question, declined = fmt.Sprintf("Leave the findings of %s to you (no autofix)?", label), "nothing changed"
	case o.on:
		question, declined = fmt.Sprintf("Autofix %s again?", label), "nothing changed"
	}
	if o.json {
		return actUsage(c, "fix", "--json needs --yes to apply (a terminal asks y/N first); "+declined, fixUsage), false
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
	return cmdFail(c, "fix", err)
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
			Cycle int    `json:"cycle"`
			Run   string `json:"run"`
			Fix   bool   `json:"fix"`
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
		case "fix.needs_you", "fix.capped":
			e = fixEnding{fixEndNeedsYou, "needs you: " + strings.TrimPrefix(msg, "fix: ")}
		case "fix.failed", "fix.ended":
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
