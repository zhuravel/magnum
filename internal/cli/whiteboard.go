package cli

// `magnum whiteboard`: ask the daemon to write the whiteboard of a PR magnum
// reviewed (a dev.fast Whiteboard of the PR at its reviewed head, which the
// whiteboard role writes in the PR's slot; engine/whiteboard.go), or open the
// one it wrote (--open, which asks the daemon nothing). The daemon decides
// and answers with the sentence this prints, the refusals included; --wait
// follows the job with the review follower until its end event and says how
// it ended.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const whiteboardUsage = "whiteboard <url|owner/repo#N|repo#N|N> [--open] [--wait] [--timeout <duration>] [--json] [--yes]"

// whiteboardDefaultTimeout is how long --wait follows a job by default.
const whiteboardDefaultTimeout = reviewDefaultTimeout

// whiteboardEventWindow is how many of the PR's newest events --wait reads
// for the end of its job.
const whiteboardEventWindow = 200

type whiteboardOpts struct {
	open, wait, json, yes bool
	timeout               time.Duration // --timeout: stop following after this long (0 = no limit)
}

func newWhiteboardCmd(c *Context) *cobra.Command {
	var o whiteboardOpts
	cmd := newCommand(groupAct, whiteboardUsage, "write the whiteboard of a PR magnum reviewed (--open opens it in Whiteboard Desktop)",
		"Write the whiteboard of a PR magnum reviewed at its current head: an interactive document of the PR (its "+
			"diagrams, code links and the review's findings) in dev.fast Whiteboard, which the whiteboard role "+
			"(claude-whiteboard by default) writes in the PR's slot from the review magnum posted. Whiteboards are "+
			"optional: with [pipeline] whiteboard = true (a [[watch]] may override) the daemon writes one by itself "+
			"after every review it posts on a new head, and this command writes one when you ask; a PR whose "+
			"whiteboard describes an earlier head gets the same whiteboard moved to the current one. It needs "+
			"dev.fast Whiteboard: Whiteboard Desktop running, or `whiteboard server start`, and the `whiteboard` CLI "+
			"on PATH. The daemon refuses, with the reason as its answer, a PR magnum has not reviewed at its current "+
			"head, a round or a fix loop in flight, a head that has its whiteboard already, a watch without a "+
			"whiteboard role and a Whiteboard that is not running. A terminal asks y/N first (--yes does not; there "+
			"--json needs --yes), and a review agent's pane is refused. --wait follows the job until its end and says "+
			"how it ended (the whiteboard written, the run failed, or stopped by a push, a close or `magnum abort`); "+
			"--timeout (default 2h, 0 = no limit) stops following it after that long, and the job goes on. --open "+
			"asks the daemon nothing: it opens the whiteboard magnum wrote of the PR in Whiteboard Desktop, focused "+
			"(`whiteboard app pick`), and refuses a PR without one.",
		func(pos []string) int { return runWhiteboard(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.open, "open", false, "open the whiteboard magnum wrote in Whiteboard Desktop (writes nothing)")
	fs.BoolVar(&o.wait, "wait", false, "follow the job until it ends: the whiteboard written, failed or stopped")
	fs.DurationVar(&o.timeout, "timeout", whiteboardDefaultTimeout, "with --wait: stop following after this `duration` (0 = no limit); the job goes on")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON (progress goes to stderr)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runWhiteboard(c *Context, o whiteboardOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, "whiteboard", "which PR?", whiteboardUsage)
	case len(pos) > 1:
		return actUsage(c, "whiteboard", "one PR at a time", whiteboardUsage)
	case o.open && o.wait:
		return actUsage(c, "whiteboard", "--open opens the whiteboard magnum wrote and starts nothing, so there is nothing for --wait to follow", whiteboardUsage)
	}
	if code, refused := refuseInAgentPane(c, "whiteboard"); refused {
		return code
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "whiteboard", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return whiteboardMain(ctx, c, d, pos[0], o)
}

// The kinds of end whiteboardJSON reports: the job's end events without
// their prefix.
const (
	whiteboardEndDone    = "done"    // the whiteboard was written
	whiteboardEndFailed  = "failed"  // the writer's run failed
	whiteboardEndStopped = "stopped" // a push, a close, `magnum abort`
)

// whiteboardEnding is how a job ended and the sentence that says so; a
// written whiteboard names its session and the head it describes.
type whiteboardEnding struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Session string `json:"-"`
	Head    string `json:"-"`
}

// code is the exit code of a command that followed the job to this end.
func (e whiteboardEnding) code() int {
	if e.Kind == whiteboardEndDone {
		return 0
	}
	return 1
}

// whiteboardJSON is what --json prints.
type whiteboardJSON struct {
	PR      string            `json:"pr"`
	URL     string            `json:"url"`
	Request *actRequestJSON   `json:"request,omitempty"`
	State   string            `json:"state"`
	Session string            `json:"session,omitempty"` // the whiteboard opened (--open) or written (--wait)
	Head    string            `json:"head,omitempty"`    // the head it describes
	End     *whiteboardEnding `json:"end,omitempty"`     // with --wait: how the job ended
	Error   string            `json:"error,omitempty"`
}

// whiteboardProgress is where progress lines go: stdout, or stderr with --json.
func whiteboardProgress(c *Context, o whiteboardOpts) io.Writer {
	return reviewProgress(c, reviewOpts{json: o.json})
}

// whiteboardMain resolves the PR and, with --open, opens the whiteboard
// magnum wrote; otherwise it asks on a terminal, hands the request to the
// daemon, prints its answer and, with --wait, follows the job it started.
// The screens call it with yes set, having asked.
func whiteboardMain(ctx context.Context, c *Context, d *actDeps, ref string, o whiteboardOpts) int {
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "whiteboard", verbFix("whiteboard", err))
	}
	label := d.actLabel(t.full(), t.PR.Number)
	out := whiteboardJSON{PR: label, URL: t.PR.URL, State: t.PR.State}
	if o.open {
		return whiteboardOpen(ctx, c, d, t, label, o, out)
	}
	if !o.json {
		reviewDescribe(ctx, c.Stdout, d, t, label, false, false)
	}
	if code, ok := whiteboardConfirm(ctx, c, d, label, o); !ok {
		return code
	}

	// A job that starts must not start later, when nobody waits for it: with
	// no daemon to run it now the request is refused, never left queued.
	q := d.quick()
	q.NeedDaemon = true
	start := d.now()
	payload := engine.WhiteboardPayload{PRTarget: t.prTarget()}
	res, err := d.reqs().send(ctx, engine.ReqWhiteboard, payload, q)
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("%s: nothing was queued: %w", label, err)
		}
		return whiteboardFail(c, o, out, err)
	}
	rv := res.view()
	out.Request = &rv
	if res.Failed() {
		res.printNotes(c.Stderr)
		return whiteboardFail(c, o, out, errors.New(res.Result()))
	}
	if !o.json {
		res.print(c.Stdout, c.Stderr)
	}
	if !o.wait {
		if o.json {
			_ = writeJSON(c.Stdout, out)
		}
		return 0
	}
	return whiteboardFollow(ctx, c, d, t, label, res, start, o, out)
}

// whiteboardConfirm asks y/N on an interactive terminal; ok is false when
// the command ends here (code is its exit code). --yes and a script skip the
// question. --json on a terminal needs --yes: the question would land in the
// JSON, and skipping it would apply unasked.
func whiteboardConfirm(ctx context.Context, c *Context, d *actDeps, label string, o whiteboardOpts) (code int, ok bool) {
	if o.yes || !d.StdinTTY || !d.StdoutTTY {
		return 0, true
	}
	const declined = "nothing queued"
	if o.json {
		return actUsage(c, "whiteboard", "--json needs --yes to apply (a terminal asks y/N first); "+declined, whiteboardUsage), false
	}
	if !d.confirm(ctx, c.Stdout, fmt.Sprintf("Write the whiteboard of %s?", label)) {
		fmt.Fprintln(c.Stderr, declined)
		if ctx.Err() != nil {
			return 130, false
		}
		return 1, false
	}
	return 0, true
}

func whiteboardFail(c *Context, o whiteboardOpts, out whiteboardJSON, err error) int {
	if o.json {
		out.Error = err.Error()
		_ = writeJSON(c.Stdout, out)
		return 1
	}
	return cmdFail(c, "whiteboard", err)
}

// whiteboardOpen opens the whiteboard magnum wrote of the PR (its
// KVPRWhiteboard record) in Whiteboard Desktop through the whiteboard CLI.
// It changes nothing of magnum's and asks no question, as `magnum open`
// focuses a pane without one. A whiteboard of an earlier head opens with a
// note; a PR without one is refused.
func whiteboardOpen(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, o whiteboardOpts, out whiteboardJSON) int {
	v, _, err := d.Store.GetKV(ctx, engine.KVPRWhiteboard(t.PR.ID))
	if err != nil {
		return whiteboardFail(c, o, out, err)
	}
	rec, ok := engine.ParseWhiteboardRecord(v)
	if !ok {
		return whiteboardFail(c, o, out, fmt.Errorf("no whiteboard of %s yet", label))
	}
	out.Session, out.Head = rec.Session, rec.Head
	if _, err := d.Run.Run(ctx, engine.WhiteboardOpenCmd(rec.Session)); err != nil {
		return whiteboardFail(c, o, out, fmt.Errorf("could not open the whiteboard %s: %s", rec.Session, whiteboardRunError(err)))
	}
	if o.json {
		_ = writeJSON(c.Stdout, out)
		return 0
	}
	where := textx.ShortSHA(rec.Head)
	if rec.Head != t.PR.HeadSHA {
		where += "; its head is " + textx.ShortSHA(t.PR.HeadSHA) + " now"
	}
	fmt.Fprintf(c.Stdout, "opened the whiteboard %s of %s (%s)\n", rec.Session, label, where)
	return 0
}

// whiteboardRunError is why the whiteboard CLI failed: the first line of
// its stderr, redacted, else the error.
func whiteboardRunError(err error) string {
	var ee *execx.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
		return textx.FirstLine(execx.Redact(ee.Stderr))
	}
	return err.Error()
}

// whiteboardFollow follows the job the answered request started (res,
// queued at start) with the review follower: it prints the PR's states and
// the writer's run, and it is over at the job's end event.
func whiteboardFollow(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, res reqOutcome, start time.Time, o whiteboardOpts, out whiteboardJSON) int {
	w := &whiteboardWatch{}
	// The follower prints no JSON itself (its options carry no --json): the
	// command does, with how the job ended.
	w.f = &reviewFollower{c: c, d: d, t: t, label: label, reqID: res.ID(), start: start, o: reviewOpts{timeout: o.timeout},
		progress: whiteboardProgress(c, o), runs: map[string]string{}, verb: "whiteboard", end: w.done,
		goesOn: fmt.Sprintf("the whiteboard job goes on (`magnum status %s` shows it, `magnum abort %s` stops it)", label, label)}
	code := w.f.run(ctx)
	if req, err := d.Store.RequestByID(ctx, res.ID()); err == nil {
		res.Req = req
		rv := res.view()
		out.Request = &rv
	}
	if w.f.out.State != "" {
		out.State = w.f.out.State
	}
	out.Error = w.f.out.Error
	if w.ending != nil {
		out.End = w.ending
		out.Session, out.Head = w.ending.Session, w.ending.Head
		if code != 0 {
			out.Error = w.ending.Text
		}
	}
	if o.json {
		_ = writeJSON(c.Stdout, out)
	}
	return code
}

// whiteboardWatch is the end of the follower of a whiteboard job.
type whiteboardWatch struct {
	f      *reviewFollower
	ending *whiteboardEnding // how the job ended, once it did
}

// done is the follower's completion: the job is over when one of its end
// events is recorded at or after the request (the PR's state does not move
// for it: the job is no round).
func (w *whiteboardWatch) done(ctx context.Context, _ store.PR) (code int, done bool) {
	f := w.f
	if !f.reqDone {
		return 0, false
	}
	end, found := whiteboardEndOf(ctx, f.d, f.t, f.start)
	if !found {
		return 0, false
	}
	w.ending = &end
	if end.code() == 0 {
		fmt.Fprintf(f.progress, "%s: %s\n", f.label, end.Text)
		return 0, true
	}
	fmt.Fprintf(f.c.Stderr, "%s: %s\n", f.label, end.Text)
	return end.code(), true
}

// whiteboardEndOf reads how the PR's job ended from the events at or after
// start: the latest of whiteboard.done (whose data names the session and the
// head), whiteboard.failed and whiteboard.stopped.
func whiteboardEndOf(ctx context.Context, d *actDeps, t actTarget, start time.Time) (whiteboardEnding, bool) {
	evs, err := d.Store.EventsBySubject(ctx, fmt.Sprintf("pr:%s#%d", t.full(), t.PR.Number), whiteboardEventWindow)
	if err != nil {
		return whiteboardEnding{}, false
	}
	var end whiteboardEnding
	found := false
	for _, ev := range evs {
		if ev.At.Before(start) {
			continue
		}
		kind, ok := strings.CutPrefix(ev.Kind, "whiteboard.")
		if !ok || (kind != whiteboardEndDone && kind != whiteboardEndFailed && kind != whiteboardEndStopped) {
			continue
		}
		e := whiteboardEnding{Kind: kind, Text: actClean(ev.Message)}
		if kind == whiteboardEndDone {
			var data struct {
				Session string `json:"session"`
				Head    string `json:"head"`
			}
			_ = json.Unmarshal(ev.Data, &data)
			e.Session, e.Head = data.Session, data.Head
		}
		end, found = e, true
	}
	return end, found
}
