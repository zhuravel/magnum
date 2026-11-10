package cli

// `magnum factory decide`: your decision on a factory task's ready card,
// through the factory daemon's requests table as the screen sends it.
// Without a choice it prints the card; with one it sends what the card you
// read showed (--card, its revision), which the daemon checks against the
// branch as it is.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryDecideUsage = "decide <task> [push|send_back --card <revision> [--reason <key> --note <text>]|drop] " +
	"[--surface cli|liaison] [--shown <time>] [--diff-read]"

// factoryDecideFlags are the flags of `magnum factory decide`.
type factoryDecideFlags struct {
	card         int
	reason, note string
	where        whereFlags
	diffRead     bool
}

func newFactoryDecideCmd(c *Context) *cobra.Command {
	var f factoryDecideFlags
	cmd := newCommand("", factoryDecideUsage, "decide on a factory task's ready card",
		"Decide on the ready card of a factory task, as the screen does: <task> is the task's id or its Jira key. "+
			"Without a choice it prints the card: its revision, the PR's title, the branch and its head, the FLAGS "+
			"line, the local review's end, the QA run's line (when visual QA ran), and the y/N sentence of a push (the branch, the copy it frees and the queued "+
			"task that then starts). push has the daemon push the head the card shows and open its PR, ready for "+
			"review, under your login; send_back returns the task to its writer with --reason, the key of your "+
			"reason, and --note, your note (the writer reads it from a file, never in its prompt), or --reason "+
			factory.ReasonKnown+" alone, which sends the defects the local review left open (the KNOWN row's, not "+
			"its simplifications or doubts) in a note the factory writes from the card; drop stops the "+
			"task, freeing its copy and keeping its branch. A push or a send_back names --card, the revision you "+
			"read: a card drawn since is refused, and the daemon refuses one whose branch, report, PR body or diff "+
			"changed after the card was drawn (\"the branch changed after you read it\") and draws the card again. "+
			"A drop is bound to no card: it needs no --card, a stale one is not refused, and the daemon drops the "+
			"task at its ready stage whatever its card shows, or when none could be drawn. The decision records as "+
			"when the card was shown the time this command read it, else --shown <time> (RFC 3339), which the factory's "+
			"liaison, the agent that asks you in a chat, passes with --surface liaison (the default, cli, is you by "+
			"hand). The decision records an unread diff unless you say with --diff-read that you read this card's "+
			"diff, as of the time it was shown.",
		func(pos []string) int { return runFactoryDecideCmd(c, f, pos) })
	cmd.Flags().IntVar(&f.card, "card", 0, "the revision of the card you read (the printed card names it)")
	cmd.Flags().StringVar(&f.reason, "reason", "", "send_back: the key of your reason (a word: lowercase letters, digits, - and _)")
	cmd.Flags().StringVar(&f.note, "note", "", "send_back: your note to the writer, one line")
	cmd.Flags().BoolVar(&f.diffRead, "diff-read", false, "you read this card's diff (as of the time it was shown); without it the decision records an unread diff")
	f.where.add(cmd, "the card")
	return cmd
}

// factoryChoices are the choices of the ready card.
var factoryChoices = []string{factory.ChoicePush, factory.ChoiceSendBack, factory.ChoiceDrop}

func runFactoryDecideCmd(c *Context, f factoryDecideFlags, pos []string) int {
	choice := ""
	if len(pos) == 2 {
		choice = pos[1]
	}
	switch {
	case len(pos) < 1 || len(pos) > 2:
		return inspUsage(c, "factory decide", "which task, and which choice? its id or its Jira key, then push, send_back or drop", factoryDecideUsage)
	case choice != "" && !slices.Contains(factoryChoices, choice):
		return inspUsage(c, "factory decide", fmt.Sprintf("unknown choice %q: push, send_back or drop", choice), factoryDecideUsage)
	case (choice == factory.ChoicePush || choice == factory.ChoiceSendBack) && f.card < 1:
		return inspUsage(c, "factory decide", "a push or a send_back names --card, the revision of the card you read", factoryDecideUsage)
	case choice == factory.ChoiceSendBack && f.reason == factory.ReasonKnown && strings.TrimSpace(f.note) != "":
		return inspUsage(c, "factory decide", "the reason "+factory.ReasonKnown+" takes no --note: the factory writes it from the card's known defects", factoryDecideUsage)
	case choice == factory.ChoiceSendBack && (f.reason == "" || (f.reason != factory.ReasonKnown && strings.TrimSpace(f.note) == "")):
		return inspUsage(c, "factory decide", "send_back takes --reason (the key of your reason) and --note (your note to the writer); "+
			"--reason "+factory.ReasonKnown+" alone sends the card's known defects", factoryDecideUsage)
	case choice != factory.ChoiceSendBack && (f.reason != "" || f.note != ""):
		return inspUsage(c, "factory decide", "--reason and --note go with send_back", factoryDecideUsage)
	}
	surface, shown, err := f.where.resolve()
	if err != nil {
		return inspUsage(c, "factory decide", err.Error(), factoryDecideUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory decide: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory decide: %v\n", err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory decide: %v\n", err)
		return 1
	}
	card, err := factory.ReadReadyCard(c.Layout, t.ID)
	// When you were shown the card: the stats time your decision from it. It
	// is when this command read the card, unless the liaison says it showed it
	// to you earlier. A drop is bound to no card, as the daemon takes it.
	read := shown
	if read.IsZero() {
		read = time.Now()
	}
	// The diff was read when the card was shown, if you say you read it.
	var diffOpened time.Time
	if f.diffRead {
		diffOpened = read
	}
	if choice == factory.ChoiceDrop {
		return sendFactoryRequest(ctx, c, reg, "factory decide", factory.ReqDecide, factory.DecidePayload{TaskID: t.ID, Choice: choice,
			HeadSHA: card.Head, ReportHash: card.ReportHash, DiffHash: card.DiffHash, Shown: read, DiffOpened: diffOpened, Surface: surface},
			"it takes your decision")
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory decide: %s has no ready card: %v\n", factory.TaskKey(t), err)
		return 1
	}
	if choice == "" {
		return printReadyCard(ctx, c, reg, t, card)
	}
	if f.card != card.Revision {
		fmt.Fprintf(c.Stderr, "magnum factory decide: card %d of %s is drawn now, not card %d: read it with `magnum factory decide %d`\n",
			card.Revision, factory.TaskKey(t), f.card, t.ID)
		return 1
	}
	p := factory.DecidePayload{TaskID: t.ID, Choice: choice, Reason: f.reason, Note: strings.TrimSpace(f.note), HeadSHA: card.Head,
		ReportHash: card.ReportHash, DiffHash: card.DiffHash, Shown: read, DiffOpened: diffOpened, Surface: surface}
	return sendFactoryRequest(ctx, c, reg, "factory decide", factory.ReqDecide, p, "it takes your decision")
}

// printReadyCard prints task t's ready card and how to decide on it: its
// flags, the local review's end, the QA run's line, the PR body's file and
// the push's sentence.
func printReadyCard(ctx context.Context, c *Context, reg *registry.Registry, t registry.Task, card factory.ReadyCardFile) int {
	tasks, err := reg.Tasks(ctx)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory decide: %v\n", err)
		return 1
	}
	w := c.Stdout
	fmt.Fprintf(w, "%s · card %d, drawn %s · %s, %s\n", factory.TaskKey(t), card.Revision, card.Drawn.Local().Format("Jan 2 15:04"), t.State,
		t.WaitsWhat)
	fmt.Fprintf(w, "title    %s\n", card.Title)
	fmt.Fprintf(w, "branch   %s at %s (its diff from %s)\n", card.Branch, shortSHA(card.Head), shortSHA(card.Base))
	if card.Remote != "" && card.Remote != card.Branch {
		fmt.Fprintf(w, "pushed   as %s (the local branch keeps its name)\n", card.Remote)
	}
	flags := card.Flags
	if len(flags) == 0 {
		flags = []string{"nothing flagged"}
	}
	for i, l := range flags {
		fmt.Fprintf(w, "%-9s%s\n", map[bool]string{true: "flags", false: ""}[i == 0], l)
	}
	r := card.Review
	fmt.Fprintf(w, "review   %s after %d round(s): %d left, %d declined, %d doubts\n", r.Exit, r.Rounds, len(r.Leftovers), len(r.Declined),
		len(r.Doubts))
	if card.QA != nil {
		if line := card.QA.CardLine(); line != "" {
			fmt.Fprintf(w, "qa       %s\n", line)
		}
	}
	fmt.Fprintf(w, "body     %s\n", filepath.Join(c.Layout.FactoryTaskDir(t.ID), "pr-body.md"))
	fmt.Fprintf(w, "\n%s\n", factory.ConfirmSentence(t, card.Copy, factory.NextQueued(tasks)))
	fmt.Fprintf(w, "decide with `magnum factory decide %d push --card %d`, `send_back --card %d --reason <key> --note <text>` or "+
		"`drop`\n", t.ID, card.Revision, card.Revision)
	return 0
}

// shortSHA is a commit's first 12 characters.
func shortSHA(sha string) string { return sha[:min(len(sha), 12)] }
