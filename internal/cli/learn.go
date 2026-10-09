package cli

// `magnum learn`: the self-improving loop's window (DECISIONS "magnum learn
// is the review window"). Without flags it lists the landed skill versions,
// the proposals that wait (for the gate, the landing or the operator), the
// last failed and invalid ones and when the editor runs next; --propose asks
// the daemon for a skill edit now (engine.ReqLearnPropose); --review shows
// the oldest held proposal and asks y/N (learn_review.go). Reading never
// asks GitHub; the daemon does the deciding.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const learnUsage = "[--review [--reason <text>] | --propose] [--json]"

// learnFlags are the parsed `magnum learn` flags.
type learnFlags struct {
	review, propose, json bool
	reason                string
}

func newLearnCmd(c *Context) *cobra.Command {
	var f learnFlags
	cmd := newCommand(groupInspect, "learn "+learnUsage, "the self-improving loop: skill versions, proposals, and the review of a held one",
		"Magnum improves its own review skill and prompts from what reviews got wrong: a skill editor (an agent, "+
			"[learn] editor_*) drafts a change from the general misses the retro found and the findings authors declined "+
			"as wrong or ranked too high, in a git worktree of magnum's checkout on a branch learn/<run>; magnum checks it "+
			"and runs the format gate (the golden files regenerated, the agents, pipeline and config tests, `make "+
			"check-private`) and commits it on that branch as a proposal. The daemon's gate replays eval cases on it and "+
			"passes, holds or fails it; a passed one lands on the checkout's master as a local commit and the daemon restarts "+
			"on it ([learn] restart). Nothing is pushed.\n\n"+
			"Without flags: the landed versions newest first (and how many origin/master does not hold yet), the proposals "+
			"that wait (gating: for the eval's gate; passed: for the landing; held: for you), the last failed and invalid ones "+
			"with their reasons, and when the editor runs next. --json prints the same.\n\n"+
			"--propose asks the daemon for a skill edit now, whatever [learn] editor says; it is refused while one runs, "+
			"while a proposal is gating, passed or held (the refusal names it), or when nothing new is left to learn from.\n\n"+
			"--review shows the oldest held proposal: the inputs it answers with what it did with each, the diff of every "+
			"file it changes, its design-log entry and the gate's table, and asks y/N on a terminal: y asks the daemon to "+
			"land it at once (the answer says why when it stays held), n rejects it (--reason says why, and the next edit reads it; an input in two rejected proposals is "+
			"dismissed and leaves the learned eval corpus at once), anything else leaves it for later. --review --json "+
			"prints the proposal and asks nothing.",
		func(pos []string) int { return runLearn(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.review, "review", false, "review the oldest held proposal and land (y) or reject (n) it")
	fs.StringVar(&f.reason, "reason", "", "with --review: why the proposal is rejected (the next edit reads it)")
	fs.BoolVar(&f.propose, "propose", false, "ask the daemon for a skill edit now")
	fs.BoolVar(&f.json, "json", false, "print JSON (the list, --review, --propose)")
	return cmd
}

func runLearn(c *Context, f learnFlags, pos []string) int {
	switch {
	case len(pos) > 0:
		return inspUsage(c, "learn", "it takes no arguments", learnUsage)
	case f.review && f.propose:
		return inspUsage(c, "learn", "--review and --propose go one at a time", learnUsage)
	case f.reason != "" && !f.review:
		return inspUsage(c, "learn", "--reason goes with --review", learnUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "learn", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	switch {
	case f.propose:
		return learnPropose(ctx, c, d, f)
	case f.review:
		return learnReview(ctx, c, d, f)
	}
	return learnList(ctx, c, d, f)
}

// learnProposalRow is a proposal as the list shows it: with how many
// inputs it was given and noted.
type learnProposalRow struct {
	store.LearnProposal
	Inputs int `json:"inputs"`
	Noted  int `json:"noted"`
}

// learnListData is what `magnum learn` shows (and --json prints).
type learnListData struct {
	Versions    []store.LearnVersion `json:"versions"`
	Waiting     []learnProposalRow   `json:"waiting"` // gating, passed, held; oldest first
	LastFailed  *learnProposalRow    `json:"last_failed,omitempty"`
	LastInvalid *learnProposalRow    `json:"last_invalid,omitempty"`
	Editing     *engine.EditMark     `json:"editing,omitempty"` // the skill edit running now
	Next        string               `json:"next"`
	// Ahead counts the landed versions origin/master does not hold yet
	// (magnum pushes nothing); nil when the checkout cannot say.
	Ahead *int `json:"ahead,omitempty"`
}

// learnRow counts proposal p's inputs.
func learnRow(ctx context.Context, st *store.Store, p store.LearnProposal) learnProposalRow {
	row := learnProposalRow{LearnProposal: p}
	if ins, err := st.LearnProposalInputs(ctx, p.ID); err == nil {
		row.Inputs = len(ins)
		for _, in := range ins {
			if in.Outcome == store.MissNoted {
				row.Noted++
			}
		}
	}
	return row
}

// learnGather reads the list from the registry.
func learnGather(ctx context.Context, d *actDeps) (learnListData, error) {
	data := learnListData{Versions: []store.LearnVersion{}, Waiting: []learnProposalRow{}}
	vs, err := d.Store.LearnVersions(ctx)
	if err != nil {
		return data, err
	}
	if vs != nil {
		data.Versions = vs
	}
	all, err := d.Store.LearnProposals(ctx, nil)
	if err != nil {
		return data, err
	}
	for _, p := range slices.Backward(all) { // oldest first
		switch p.State {
		case store.LearnGating, store.LearnPassed, store.LearnHeld:
			data.Waiting = append(data.Waiting, learnRow(ctx, d.Store, p))
		}
	}
	for _, p := range all { // newest first
		switch {
		case p.State == store.LearnFailed && data.LastFailed == nil:
			r := learnRow(ctx, d.Store, p)
			data.LastFailed = &r
		case p.State == store.LearnInvalid && data.LastInvalid == nil:
			r := learnRow(ctx, d.Store, p)
			data.LastInvalid = &r
		}
	}
	if v, ok, err := d.Store.GetKV(ctx, engine.KVLearnEditing); err == nil && ok && v != "" {
		var m engine.EditMark
		if json.Unmarshal([]byte(v), &m) == nil {
			data.Editing = &m
		}
	}
	data.Ahead = learnAhead(ctx, d, data.Versions)
	data.Next = "on request: `magnum learn --propose` runs the editor now"
	if d.Cfg != nil && d.Cfg.Learn.Editor {
		data.Next = "on request (`magnum learn --propose`); [learn] editor's runs after a retro come with the loop's gate"
	}
	return data, nil
}

// learnAhead counts the landed versions whose commit origin/master does not
// hold, read from magnum's checkout; nil without a checkout, without
// origin/master or without versions.
func learnAhead(ctx context.Context, d *actDeps, vs []store.LearnVersion) *int {
	checkout := d.Layout.Home
	if len(vs) == 0 || checkout == "" || !gitx.IsRepo(checkout) {
		return nil
	}
	git := gitx.New(d.Run)
	origin, err := git.RevParse(ctx, checkout, "refs/remotes/origin/master")
	if err != nil {
		return nil
	}
	n := 0
	for _, v := range vs {
		if base, err := git.MergeBase(ctx, checkout, v.LandedSHA, origin); err != nil || base != v.LandedSHA {
			n++
		}
	}
	return &n
}

func learnList(ctx context.Context, c *Context, d *actDeps, f learnFlags) int {
	data, err := learnGather(ctx, d)
	if err != nil {
		return cmdFail(c, "learn", err)
	}
	if f.json {
		if err := writeJSON(c.Stdout, data); err != nil {
			return cmdFail(c, "learn", err)
		}
		return 0
	}
	learnPrintList(c.Stdout, d, data)
	return 0
}

// learnPrintList writes the list: the versions, the waiting proposals, the
// last failed and invalid ones, the edit running and the next run.
func learnPrintList(w io.Writer, d *actDeps, data learnListData) {
	now := notesNow(d)
	if len(data.Versions) == 0 {
		fmt.Fprintln(w, "skill versions: none landed yet")
	} else {
		fmt.Fprintln(w, "skill versions, newest first:")
		tw := inspTable(w)
		fmt.Fprintln(tw, "  VERSION\tLANDED\tRESTARTED\tCOMMIT\tPROPOSAL")
		for _, v := range data.Versions {
			restarted := "-"
			if v.RestartedAt != nil {
				restarted = inspClock(now, *v.RestartedAt)
			}
			proposal := "-"
			if v.ProposalID != 0 {
				proposal = strconv.FormatInt(v.ProposalID, 10)
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n", v.ID, inspClock(now, v.LandedAt), restarted, learnShort(v.LandedSHA), proposal)
		}
		_ = tw.Flush()
		if n := data.Ahead; n != nil && *n > 0 {
			fmt.Fprintf(w, "  %s not on origin/master: magnum pushes nothing, so push master when you want them there\n",
				textx.Count(*n, "landed version is", "landed versions are"))
		}
	}
	if len(data.Waiting) == 0 {
		fmt.Fprintln(w, "proposals waiting: none")
	} else {
		fmt.Fprintln(w, "proposals waiting, oldest first:")
		tw := inspTable(w)
		fmt.Fprintln(tw, "  ID\tSTATE\tCREATED\tTRIGGER\tBRANCH\tINPUTS")
		for _, r := range data.Waiting {
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%d (%d noted)\n", r.ID, r.State, inspClock(now, r.CreatedAt), r.Trigger, r.Branch, r.Inputs, r.Noted)
		}
		_ = tw.Flush()
	}
	for _, last := range []struct {
		what string
		row  *learnProposalRow
	}{{"last failed", data.LastFailed}, {"last invalid", data.LastInvalid}} {
		if r := last.row; r != nil {
			fmt.Fprintf(w, "%s: proposal %d, %s: %s\n", last.what, r.ID, inspClock(now, r.CreatedAt), textx.Clip(notes.Printable(oneLineText(r.Reason)), 300))
		}
	}
	if m := data.Editing; m != nil {
		fmt.Fprintf(w, "a skill edit runs (run %s, since %s)\n", m.Run, inspClock(now, m.Started))
	}
	fmt.Fprintln(w, "next edit: "+data.Next)
	for _, r := range data.Waiting {
		if r.State == store.LearnHeld {
			fmt.Fprintf(w, "`magnum learn --review` reviews proposal %d\n", r.ID)
			break
		}
	}
}

// oneLineText folds s onto one line.
func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// learnShort is a commit's first 7 characters.
func learnShort(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// learnProposeJSON is what --propose --json prints.
type learnProposeJSON struct {
	Request *actRequestJSON `json:"request,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// learnPropose asks the daemon for a skill edit now (engine.ReqLearnPropose),
// unless a proposal already waits or an edit runs, which it names first:
// the daemon would refuse it.
func learnPropose(ctx context.Context, c *Context, d *actDeps, f learnFlags) int {
	var out learnProposeJSON
	fail := func(err error) int {
		if f.json {
			out.Error = err.Error()
			_ = writeJSON(c.Stdout, out)
			return 1
		}
		return cmdFail(c, "learn", err)
	}
	data, err := learnGather(ctx, d)
	if err != nil {
		return fail(err)
	}
	if len(data.Waiting) > 0 {
		p := data.Waiting[0]
		next := map[string]string{store.LearnGating: "the gate decides it first", store.LearnPassed: "it lands first",
			store.LearnHeld: "`magnum learn --review` lands or rejects it first"}[p.State]
		return fail(fmt.Errorf("proposal %d is %s: %s", p.ID, p.State, next))
	}
	if m := data.Editing; m != nil {
		return fail(fmt.Errorf("a skill edit is running (run %s, since %s); `magnum learn` lists it", m.Run, inspClock(notesNow(d), m.Started)))
	}
	res, err := d.reqs().send(ctx, engine.ReqLearnPropose, engine.LearnProposePayload{}, d.quick())
	if err != nil {
		return fail(err)
	}
	rv := res.view()
	out.Request = &rv
	switch {
	case res.PID == 0 && res.Pending():
		res.printNotes(c.Stderr)
		return fail(fmt.Errorf("no daemon is running: request %d stays queued and starts the edit once one runs: %s", res.ID(), actDaemonFix))
	case res.Failed():
		res.printNotes(c.Stderr)
		return fail(errors.New(res.Result()))
	case f.json:
		_ = writeJSON(c.Stdout, out)
		return 0
	}
	return res.print(c.Stdout, c.Stderr)
}
