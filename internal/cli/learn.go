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

const learnUsage = "[--review [--reason <text>] | --propose | --revert <version> [--reason <text>]] [--json]"

// learnFlags are the parsed `magnum learn` flags.
type learnFlags struct {
	review, propose, json bool
	reason, revert        string
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
			"prints the proposal and asks nothing.\n\n"+
			"--revert <version> asks the daemon for a proposal that restores the skill and prompt files the version changed as "+
			"they were before it (--reason says why, in the design log's entry; no pull request, link, login or repository name), through the "+
			"format gate and the eval's gate, which holds it for --review: a revert never lands on its own. It is refused while "+
			"another learn job runs, when a waiting proposal already reverts the version, and when master changed those files "+
			"since the version landed. The daemon proposes one by itself, once per version, when the newest version's reviews "+
			"had their findings declined or argued [learn] regression_points more often than the version before.",
		func(pos []string) int { return runLearn(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.review, "review", false, "review the oldest held proposal and land (y) or reject (n) it")
	fs.StringVar(&f.reason, "reason", "", "with --review: why the proposal is rejected (the next edit reads it); with --revert: why the version is reverted")
	fs.BoolVar(&f.propose, "propose", false, "ask the daemon for a skill edit now")
	fs.StringVar(&f.revert, "revert", "", "ask the daemon for a proposal that restores the files of the version before this landed version")
	fs.BoolVar(&f.json, "json", false, "print JSON (the list, --review, --propose, --revert)")
	return cmd
}

func runLearn(c *Context, f learnFlags, pos []string) int {
	switch {
	case len(pos) > 0:
		return inspUsage(c, "learn", "it takes no arguments", learnUsage)
	case f.review && f.propose, f.revert != "" && (f.review || f.propose):
		return inspUsage(c, "learn", "--review, --propose and --revert go one at a time", learnUsage)
	case f.reason != "" && !f.review && f.revert == "":
		return inspUsage(c, "learn", "--reason goes with --review or --revert", learnUsage)
	}
	var revert int64
	if f.revert != "" {
		n, err := strconv.ParseInt(f.revert, 10, 64)
		if err != nil || n <= 0 {
			return inspUsage(c, "learn", "--revert takes the number of a landed version (`magnum learn` lists them)", learnUsage)
		}
		revert = n
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
	case revert > 0:
		return learnRevert(ctx, c, d, f, revert)
	}
	return learnList(ctx, c, d, f)
}

// learnRevert asks the daemon for a revert of landed version id
// (engine.ReqLearnRevert, with --reason), once the registry knows the
// version; the daemon refuses what it cannot do, and says why.
func learnRevert(ctx context.Context, c *Context, d *actDeps, f learnFlags, id int64) int {
	var out learnProposeJSON
	fail := func(err error) int {
		if f.json {
			out.Error = err.Error()
			_ = writeJSON(c.Stdout, out)
			return 1
		}
		return cmdFail(c, "learn", err)
	}
	vs, err := d.Store.LearnVersions(ctx)
	if err != nil {
		return fail(err)
	}
	if !slices.ContainsFunc(vs, func(v store.LearnVersion) bool { return v.ID == id }) {
		return fail(fmt.Errorf("no skill version %d landed (`magnum learn` lists the versions)", id))
	}
	res, err := d.reqs().send(ctx, engine.ReqLearnRevert, engine.LearnRevertPayload{Version: id, Reason: strings.TrimSpace(f.reason)}, d.quick())
	if err != nil {
		return fail(err)
	}
	rv := res.view()
	out.Request = &rv
	switch {
	case res.PID == 0 && res.Pending():
		res.printNotes(c.Stderr)
		return fail(fmt.Errorf("no daemon is running: request %d stays queued and starts the revert once one runs: %s", res.ID(), actDaemonFix))
	case res.Failed():
		res.printNotes(c.Stderr)
		return fail(errors.New(res.Result()))
	case f.json:
		_ = writeJSON(c.Stdout, out)
		return 0
	}
	return res.print(c.Stdout, c.Stderr)
}

// learnProposalRow is a proposal as the list shows it: with how many
// inputs it was given and noted.
type learnProposalRow struct {
	store.LearnProposal
	Inputs int `json:"inputs"`
	Noted  int `json:"noted"`
}

// learnVersionRow is a landed version as the list shows it: with what its
// reviews did in its window.
type learnVersionRow struct {
	store.LearnVersion
	Measure store.VersionMeasure `json:"measure"`
}

// learnListData is what `magnum learn` shows (and --json prints).
type learnListData struct {
	Versions    []learnVersionRow  `json:"versions"` // newest first
	Waiting     []learnProposalRow `json:"waiting"`  // gating, passed, held; oldest first
	LastFailed  *learnProposalRow  `json:"last_failed,omitempty"`
	LastInvalid *learnProposalRow  `json:"last_invalid,omitempty"`
	Editing     *engine.EditMark   `json:"editing,omitempty"` // the skill edit running now
	Next        string             `json:"next"`
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
	data := learnListData{Versions: []learnVersionRow{}, Waiting: []learnProposalRow{}}
	vs, err := d.Store.LearnVersions(ctx)
	if err != nil {
		return data, err
	}
	ms, err := d.Store.VersionMeasures(ctx, vs, 0)
	if err != nil {
		return data, err
	}
	for i, v := range vs {
		data.Versions = append(data.Versions, learnVersionRow{LearnVersion: v, Measure: ms[i]})
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
	data.Ahead = learnAhead(ctx, d, vs)
	evidence, _, _ := d.Store.GetKV(ctx, engine.KVLearnEvidence)
	editDay, _, _ := d.Store.GetKV(ctx, engine.KVLearnEditDay)
	data.Next = engine.EditNext(d.Cfg != nil && d.Cfg.Learn.Editor, evidence, editDay, notesNow(d))
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
		fmt.Fprintln(tw, "  VERSION\tLANDED\tRESTARTED\tCOMMIT\tPROPOSAL\tROUNDS\tPOSTED\tFIXED\tDECLINED\tMISSES")
		for _, v := range data.Versions {
			restarted := "-"
			if v.RestartedAt != nil {
				restarted = inspClock(now, *v.RestartedAt)
			}
			proposal := "-"
			if v.ProposalID != 0 {
				proposal = strconv.FormatInt(v.ProposalID, 10)
			}
			m := v.Measure
			declined := strconv.Itoa(m.Declined())
			if share, ok := m.DeclinedShare(); ok && m.Declined() > 0 {
				declined += fmt.Sprintf(" (%.0f%%)", share)
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%d\n", v.ID, inspClock(now, v.LandedAt), restarted, learnShort(v.LandedSHA),
				proposal, m.JudgeRuns, m.Posted, m.Outcomes[store.DeclineFixed], declined, m.MissesGeneral+m.MissesRepo)
		}
		_ = tw.Flush()
		fmt.Fprintln(w, "  from the restart on a version to the next one's: ROUNDS the reviews posted, POSTED their findings, FIXED and "+
			"DECLINED (or argued) the authors' answers, MISSES what other reviewers caught")
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
			trigger := r.Trigger
			if r.Reverts != 0 {
				trigger += fmt.Sprintf(" (reverts %d)", r.Reverts)
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%d (%d noted)\n", r.ID, r.State, inspClock(now, r.CreatedAt), trigger, r.Branch, r.Inputs, r.Noted)
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
		fmt.Fprintf(w, "%s runs (run %s, since %s)\n", editingWhat(*m), m.Run, inspClock(now, m.Started))
	}
	fmt.Fprintln(w, "next edit: "+data.Next)
	for _, r := range data.Waiting {
		if r.State == store.LearnHeld {
			fmt.Fprintf(w, "`magnum learn --review` reviews proposal %d\n", r.ID)
			break
		}
	}
}

// editingWhat names the job of the edit's slot that m marks: a skill edit,
// or the revert of a version.
func editingWhat(m engine.EditMark) string {
	if m.Revert != 0 {
		return fmt.Sprintf("the revert of skill version %d", m.Revert)
	}
	return "a skill edit"
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
		return fail(fmt.Errorf("%s is running (run %s, since %s); `magnum learn` lists it", editingWhat(*m), m.Run, inspClock(notesNow(d), m.Started)))
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
