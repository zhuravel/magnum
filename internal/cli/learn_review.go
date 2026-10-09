package cli

// `magnum learn --review`: the oldest held skill proposal (one the gate
// could not decide or left to the operator), else the oldest gating one,
// with its evidence (the inputs it answers, as the retro recorded them, and
// what it did with each), the diff of every file its commit changes against
// the master it started from (read from magnum's checkout, which holds its
// learn/<run> branch), its design-log entry and the gate's result, then y/N
// on a terminal. The daemon decides: on a held proposal y sends
// engine.ReqLearnLand, n engine.ReqLearnReject with --reason; on a gating
// one y sends engine.ReqLearnReject (the daemon stops its gate first).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// learnReviewInput is an input of a proposal as the review shows it.
type learnReviewInput struct {
	ID       string `json:"id"`                 // m<id> or d<id>
	Severity string `json:"severity,omitempty"` // a miss's severity, a decline's posted priority
	Verdict  string `json:"verdict,omitempty"`  // a decline's
	Title    string `json:"title,omitempty"`
	Lesson   string `json:"lesson,omitempty"`
	Outcome  string `json:"outcome"` // noted | skipped
	Where    string `json:"where,omitempty"`
	Reason   string `json:"reason,omitempty"`
	State    string `json:"state,omitempty"` // the input's state now
}

// learnFileDiff is one file the proposal's commit changes.
type learnFileDiff struct {
	File string `json:"file"`
	Diff string `json:"diff"`
}

// learnReviewData is a held proposal as --review shows it (and --json
// prints).
type learnReviewData struct {
	Proposal store.LearnProposal `json:"proposal"`
	Inputs   []learnReviewInput  `json:"inputs"`
	Diffs    []learnFileDiff     `json:"diffs"`
	Decision string              `json:"decision"`
	// DiffError says why the diffs are missing (no checkout, the branch is
	// gone).
	DiffError string `json:"diff_error,omitempty"`
	// Unlisted are files the commit changes that the proposal does not
	// list (magnum checked only those it lists).
	Unlisted []string `json:"unlisted,omitempty"`
}

// learnReview reviews the oldest held proposal, else the oldest gating one
// (which only a rejection decides here).
func learnReview(ctx context.Context, c *Context, d *actDeps, f learnFlags) int {
	waiting, err := d.Store.LearnProposals(ctx, []string{store.LearnHeld, store.LearnGating})
	if err != nil {
		return cmdFail(c, "learn", err)
	}
	if len(waiting) == 0 {
		if f.json {
			_ = writeJSON(c.Stdout, map[string]any{"proposal": nil})
			return 0
		}
		fmt.Fprintln(c.Stderr, "no skill proposal waits for review (`magnum learn` lists the proposals)")
		return 0
	}
	// Newest first: the last of a state is its oldest.
	p := waiting[len(waiting)-1]
	if held := slices.DeleteFunc(slices.Clone(waiting), func(w store.LearnProposal) bool { return w.State != store.LearnHeld }); len(held) > 0 {
		p = held[len(held)-1]
	}
	data, err := learnReviewOf(ctx, d, p)
	if err != nil {
		return cmdFail(c, "learn", err)
	}
	if f.json {
		if err := writeJSON(c.Stdout, data); err != nil {
			return cmdFail(c, "learn", err)
		}
		return 0
	}
	if !d.StdinTTY {
		return inspUsage(c, "learn", "--review asks y/N on a terminal; --json prints the proposal for scripts", learnUsage)
	}
	learnPrintReview(c.Stdout, d.StdoutTTY, data)
	if p.State == store.LearnGating {
		fmt.Fprintf(c.Stdout, "Reject proposal %d? The gate has not decided it; y asks the daemon to reject it (a gate that runs for it "+
			"stops first; --reason says why, and the next edit reads it), anything else leaves it to the gate [y/N] ", p.ID)
	} else {
		fmt.Fprintf(c.Stdout, "Land proposal %d on master? y asks the daemon to land it, n rejects it (--reason says why; the next edit reads it), "+
			"anything else leaves it for later [y/N] ", p.ID)
	}
	answer, err := d.readLine(ctx)
	if err != nil {
		fmt.Fprintln(c.Stdout)
		if ctx.Err() != nil {
			return 130
		}
		answer = ""
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	if p.State == store.LearnGating {
		if a == "y" || a == "yes" {
			return learnDecide(ctx, c, d, engine.ReqLearnReject, engine.LearnDecisionPayload{ID: p.ID, Reason: strings.TrimSpace(f.reason)})
		}
		fmt.Fprintln(c.Stdout, "left to the gate: `magnum learn --review` again rejects it")
		return 0
	}
	switch a {
	case "y", "yes":
		return learnDecide(ctx, c, d, engine.ReqLearnLand, engine.LearnDecisionPayload{ID: p.ID})
	case "n", "no":
		return learnDecide(ctx, c, d, engine.ReqLearnReject, engine.LearnDecisionPayload{ID: p.ID, Reason: strings.TrimSpace(f.reason)})
	}
	fmt.Fprintln(c.Stdout, "left for later: `magnum learn --review`")
	return 0
}

// learnDecide sends the operator's answer to the daemon and prints its
// reply; without a daemon the request stays queued and the command fails.
func learnDecide(ctx context.Context, c *Context, d *actDeps, kind string, p engine.LearnDecisionPayload) int {
	res, err := d.reqs().send(ctx, kind, p, d.quick())
	if err != nil {
		return cmdFail(c, "learn", err)
	}
	if res.PID == 0 && res.Pending() {
		res.printNotes(c.Stderr)
		return cmdFail(c, "learn", fmt.Errorf("no daemon is running: request %d stays queued and the daemon decides proposal %d once one runs: %s",
			res.ID(), p.ID, actDaemonFix))
	}
	return res.print(c.Stdout, c.Stderr)
}

// learnReviewOf gathers a proposal's review: its inputs with the misses'
// and declines' text, the diffs of its files from its base to its
// candidate commit in magnum's checkout, and the decision from the copy of
// its changes.json in the run directory.
func learnReviewOf(ctx context.Context, d *actDeps, p store.LearnProposal) (learnReviewData, error) {
	data := learnReviewData{Proposal: p, Inputs: []learnReviewInput{}, Diffs: []learnFileDiff{}}
	ins, err := d.Store.LearnProposalInputs(ctx, p.ID)
	if err != nil {
		return data, err
	}
	misses, err := d.Store.Misses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeGeneral}})
	if err != nil {
		return data, err
	}
	declines, err := d.Store.Declines(ctx, store.DeclineFilter{Scopes: []string{store.MissScopeGeneral}})
	if err != nil {
		return data, err
	}
	for _, in := range ins {
		r := learnReviewInput{Outcome: in.Outcome, Where: in.Where, Reason: in.Reason, State: in.State}
		switch in.Kind {
		case store.LearnInputMiss:
			r.ID = learn.MissInputID(in.InputID)
			for _, m := range misses {
				if m.ID == in.InputID {
					r.Severity, r.Title, r.Lesson = m.Severity, m.Title, m.Lesson
				}
			}
		default:
			r.ID = learn.DeclineInputID(in.InputID)
			for _, dc := range declines {
				if dc.ID == in.InputID {
					r.Severity, r.Verdict, r.Title, r.Lesson = dc.Priority, dc.Verdict, dc.Title, dc.Lesson
				}
			}
		}
		data.Inputs = append(data.Inputs, r)
	}
	if p.RunDir != "" {
		if b, err := fsx.ReadRegular(filepath.Join(p.RunDir, learn.ChangesFile), learn.ChangesMaxBytes); err == nil {
			if ch, err := learn.ParseChanges(b); err == nil {
				data.Decision = ch.Decision
			}
		}
	}
	checkout := d.Layout.Home
	switch {
	case checkout == "" || !gitx.IsRepo(checkout):
		data.DiffError = "no checkout of magnum to read the branch from"
	case p.CandidateSHA == "":
		data.DiffError = "the proposal has no commit"
	default:
		// The branch's own word on what it changes: a file the proposal
		// does not list is shown, and named.
		git := gitx.New(d.Run)
		files, err := git.ChangedPaths(ctx, checkout, p.BaseSHA, p.CandidateSHA)
		if err != nil {
			data.DiffError = fmt.Sprintf("the branch's files: %v", err)
			break
		}
		for _, file := range files {
			if !slices.Contains(p.Files, file) {
				data.Unlisted = append(data.Unlisted, file)
			}
			diff, err := git.FileDiff(ctx, checkout, p.BaseSHA, p.CandidateSHA, file)
			if err != nil {
				data.DiffError = fmt.Sprintf("the diff of %s: %v", file, err)
				break
			}
			data.Diffs = append(data.Diffs, learnFileDiff{File: file, Diff: diff})
		}
	}
	return data, nil
}

// learnPrintReview writes a held proposal's review: what it is, its
// inputs, the diffs (colored on a terminal), the decision and the gate.
func learnPrintReview(w io.Writer, color bool, data learnReviewData) {
	p := data.Proposal
	by := "by " + p.Model
	if p.Reverts != 0 {
		by = fmt.Sprintf("a revert of skill version %d", p.Reverts)
	}
	fmt.Fprintf(w, "skill proposal %d (%s, %s), %s, on %s from %s\n", p.ID, p.Trigger, by, p.CreatedAt.Local().Format("2006-01-02 15:04"),
		p.Branch, learnShort(p.BaseSHA))
	if len(data.Inputs) > 0 {
		fmt.Fprintln(w, "\ninputs:")
		for _, in := range data.Inputs {
			line := "  " + in.ID
			for _, s := range []string{in.Severity, in.Verdict, in.Title} {
				if s = notes.Printable(s); s != "" {
					line += " " + s
				}
			}
			switch in.Outcome {
			case store.MissNoted:
				line += " → noted in " + notes.Printable(in.Where)
			default:
				line += " → skipped: " + notes.Printable(in.Reason)
			}
			fmt.Fprintln(w, line)
			if in.Lesson != "" {
				fmt.Fprintln(w, "    lesson: "+notes.Printable(in.Lesson))
			}
		}
	}
	fmt.Fprintln(w)
	if data.DiffError != "" {
		fmt.Fprintf(w, "no diff: %s (branch %s)\n", data.DiffError, p.Branch)
	}
	if len(data.Unlisted) > 0 {
		fmt.Fprintf(w, "WARNING: the branch also changes %s, which the proposal does not list and magnum did not check\n",
			notes.Printable(strings.Join(data.Unlisted, ", ")))
	}
	for _, df := range data.Diffs {
		learnPrintDiff(w, color, df.Diff)
	}
	if data.Decision != "" {
		fmt.Fprintln(w, "\ndecision:")
		fmt.Fprintln(w, "  "+notes.Printable(oneLineText(data.Decision)))
	}
	fmt.Fprintln(w, "\ngate:")
	var g engine.GateResult
	var gate bytes.Buffer
	switch {
	case len(p.Gate) == 0 && p.State == store.LearnGating:
		fmt.Fprintf(w, "  not decided yet: the proposal is gating since %s (a gate that stops short tries again; one gating 3 days is held)\n",
			p.CreatedAt.Local().Format("2006-01-02 15:04"))
	case len(p.Gate) == 0:
		fmt.Fprintln(w, "  no result")
	case json.Unmarshal(p.Gate, &g) == nil && (g.Rule != "" || len(g.Cases) > 0):
		learnPrintGate(w, g)
	case json.Indent(&gate, p.Gate, "  ", "  ") != nil:
		fmt.Fprintln(w, "  no result")
	default:
		fmt.Fprintln(w, "  "+notes.Printable(gate.String()))
	}
	fmt.Fprintln(w)
}

// learnPrintGate writes the gate's result as a table: the verdict, the mode
// and the rule, then each case (a target starred) with what the baseline
// and the candidate found of its defects, the declined findings each
// reported, and why a case was not compared.
func learnPrintGate(w io.Writer, g engine.GateResult) {
	head := strings.TrimSpace(strings.Join([]string{g.State, map[string]string{"": "", learn.GateJudgeOnly: "(the judge alone)",
		learn.GateFull: "(every role)"}[g.Mode]}, " "))
	if head != "" {
		head += ": "
	}
	fmt.Fprintln(w, "  "+notes.Printable(head+g.Rule))
	if len(g.Cases) == 0 {
		return
	}
	score := func(s *eval.Score) string {
		if s == nil {
			return "-"
		}
		out := fmt.Sprintf("%d/%d", s.Found, s.Total)
		if n := len(s.AbsentHits); n > 0 {
			out += fmt.Sprintf(", %d declined reported", n)
		}
		return out
	}
	tw := inspTable(w)
	fmt.Fprintln(tw, "  CASE\tBASELINE\tCANDIDATE\tNOTE")
	for _, c := range g.Cases {
		name, note := c.Case, c.Skip
		if c.Target {
			name += "*"
		}
		if c.Retried && note == "" {
			note = "replayed twice"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", notes.Printable(name), score(c.Before), score(c.After), notes.Printable(note))
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "  * a target: the learned case of an input the proposal noted")
}

// learnPrintDiff writes a git diff, colored on a terminal, its text made
// printable (a skill or prompt line could hold a control character).
func learnPrintDiff(w io.Writer, color bool, diff string) {
	for line := range strings.Lines(notes.Printable(diff)) {
		code := ""
		switch {
		case !color:
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "diff "):
			code = notesBold
		case strings.HasPrefix(line, "@@"):
			code = notesCyan
		case strings.HasPrefix(line, "+"):
			code = notesGreen
		case strings.HasPrefix(line, "-"):
			code = notesRed
		}
		if code == "" {
			_, _ = io.WriteString(w, line)
			continue
		}
		fmt.Fprintf(w, "%s%s%s\n", code, strings.TrimSuffix(line, "\n"), notesReset)
	}
}
