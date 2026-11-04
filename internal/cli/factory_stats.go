package cli

// `magnum factory stats` and `magnum factory baseline`: how much the factory
// still asks of the operator, per task, against how much the operator typed
// before it (internal/factory/stats). Stats only reads the factory registry;
// the baseline is written once, before the first task.

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/fsx"
)

const (
	factoryStatsUsage    = "stats [--since 7d] [--json|--markdown]"
	factoryBaselineUsage = "baseline --transcripts <dir> --labelled <file>"

	// baselineMinAccuracy is the share of the operator's labelled messages
	// the classifier should get right for the baseline's step-start counts
	// to be read as more than a rough guide.
	baselineMinAccuracy = 0.9
)

// factoryStatsFlags are the flags of `magnum factory stats`.
type factoryStatsFlags struct {
	since          string
	json, markdown bool
}

func newFactoryStatsCmd(c *Context) *cobra.Command {
	var f factoryStatsFlags
	cmd := newCommand("", factoryStatsUsage, "what the factory still asks of you, per task, against the baseline",
		"Count, per factory task, what it asked of you: your typed messages (the task's text, the start's "+
			"guidance, what you typed in a pane or said in one line, the note of a send-back), those of them that "+
			"only start a step (the target is zero; a retry counts as one, as you start a step by hand, and the "+
			"task's text never does: it is the one message the factory needs to start the task), approvals, "+
			"corrections and access, your answer batches (the revisions of questions you answered), your card "+
			"decisions, the local review's rounds, your time on cards (for each decision and each answered batch, "+
			"from the card shown to your answer, at most 30 minutes each, so a card left open and idle does not "+
			"count), how long the task waited on you (asleep or not) and how long it took to reach its ready card. "+
			"The numbers come from the evidence records of the factory's "+
			"registry, which this command only reads (it never creates one, and it refuses to migrate one while a "+
			"factory daemon of an older build runs on it). The default output is markdown, which sets each task "+
			"beside the frozen baseline (`magnum factory baseline`: your typed and step-starting messages per "+
			"ticket before the factory, where the first message of a ticket is typed and no step start either) and "+
			"says where its counts lie against the baseline's middle half; --json "+
			"is the same data with durations in seconds. --since takes N days (7d), a Go duration (36h, 90m) or a "+
			"date (2026-10-01, local midnight), and keeps the tasks started since; without it every task counts.",
		func(pos []string) int { return runFactoryStatsCmd(c, f, pos) })
	cmd.Flags().StringVar(&f.since, "since", "", "only the tasks started since: 7d, 36h, 90m or a date such as 2026-10-01 (default: all)")
	cmd.Flags().BoolVar(&f.json, "json", false, "print JSON")
	cmd.Flags().BoolVar(&f.markdown, "markdown", false, "print markdown (the default)")
	return cmd
}

func runFactoryStatsCmd(c *Context, f factoryStatsFlags, pos []string) int {
	fail := func(err error) int {
		fmt.Fprintf(c.Stderr, "magnum factory stats: %v\n", err)
		return 1
	}
	switch {
	case len(pos) > 0:
		return inspUsage(c, "factory stats", "no arguments expected", factoryStatsUsage)
	case f.json && f.markdown:
		return inspUsage(c, "factory stats", "--json or --markdown, not both", factoryStatsUsage)
	}
	var since time.Time
	if f.since != "" {
		var err error
		if since, err = statsParseSince(f.since, inspNow()); err != nil {
			return inspUsage(c, "factory stats", err.Error(), factoryStatsUsage)
		}
	}
	baseline, err := stats.LoadBaseline(c.Layout.FactoryBaseline())
	if err != nil {
		return fail(err)
	}
	rep := stats.Report{Since: since, Tasks: []stats.TaskStats{}}
	if fsx.Exists(c.Layout.FactoryDB()) {
		reg, err := openFactoryRegistry(c.Layout)
		if err != nil {
			return fail(err)
		}
		defer reg.Close()
		ctx, stop := daemonSys.signals()
		defer stop()
		if rep, err = stats.Compute(ctx, reg, since); err != nil {
			return fail(err)
		}
	}
	rep.Baseline = baseline
	if f.json {
		if err := writeJSON(c.Stdout, rep); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Fprint(c.Stdout, rep.Markdown())
	return 0
}

// factoryBaselineFlags are the flags of `magnum factory baseline`.
type factoryBaselineFlags struct{ transcripts, labelled string }

func newFactoryBaselineCmd(c *Context) *cobra.Command {
	var f factoryBaselineFlags
	cmd := newCommand("", factoryBaselineUsage, "freeze, once, how much you typed per ticket before the factory",
		"Count what you typed to your agents before the factory, per ticket, and write it to baseline.json in the "+
			"factory's data directory: once, before the first task. --transcripts is a directory of your old Claude "+
			"Code transcripts and Codex rollouts (every .jsonl file below it; a Codex file is named rollout-*): a "+
			"session belongs to the ticket whose key (a tracker of your config's [board] or [[repo]] trackers) its "+
			"file path or its first messages name, and the sessions of one ticket are one ticket, never counted "+
			"apart. The first message of a ticket (the oldest session's first) counts as typed and never as a step "+
			"start, as a task's text does in `magnum factory stats`, while the first message of a later session keeps "+
			"its class. A transcript with no message or no ticket key is left out and counted. --labelled is a JSON-lines "+
			"file of your messages with the class you gave each by hand ({\"text\": \"open PR\", \"class\": "+
			"\"step_start\"}; the classes are step_start, approval, correction, access and info): the classifier that "+
			"counts your step-starting messages, here and in `magnum factory stats`, is scored on it, and its "+
			"accuracy is kept with the baseline (a warning below 90%). Keep that file, and the transcripts, outside "+
			"the repository. The command refuses to write when baseline.json exists, before it reads anything, and "+
			"never rewrites it: to freeze a new one, move the old file away yourself.",
		func(pos []string) int { return runFactoryBaselineCmd(c, f, pos) })
	cmd.Flags().StringVar(&f.transcripts, "transcripts", "", "the directory of your old transcripts and rollouts")
	cmd.Flags().StringVar(&f.labelled, "labelled", "", "the JSON-lines file of your labelled messages")
	_ = cmd.MarkFlagDirname("transcripts")
	_ = cmd.MarkFlagFilename("labelled", "jsonl", "json")
	return cmd
}

func runFactoryBaselineCmd(c *Context, f factoryBaselineFlags, pos []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(c.Stderr, "magnum factory baseline: "+format+"\n", a...)
		return 1
	}
	if len(pos) > 0 || f.transcripts == "" || f.labelled == "" {
		return inspUsage(c, "factory baseline", "which transcripts, and which labelled messages? both flags, no arguments", factoryBaselineUsage)
	}
	path := c.Layout.FactoryBaseline()
	frozen := func() int {
		return fail("the baseline is frozen already: %s exists, and magnum never rewrites it\nfix: to freeze another, move that file away yourself, then re-run", path)
	}
	if _, err := os.Lstat(path); err == nil {
		return frozen()
	}
	trackers := baselineTrackers(c.Config)
	if len(trackers) == 0 {
		return fail("no issue tracker is configured, so no ticket key can be told from a word\nfix: set [board] trackers (or a [[repo]]'s) to the template of your tickets' links, such as https://example.atlassian.net/browse/PR-{num}")
	}
	labelled, err := stats.ReadLabelled(f.labelled)
	if err != nil {
		return fail("%v", err)
	}
	sessions, skipped, err := stats.ReadSessions(f.transcripts, func(text string) string {
		key, _ := config.Issue(text, trackers)
		return key
	})
	if err != nil {
		return fail("%v", err)
	}
	b, err := stats.BuildBaseline(sessions, labelled)
	if err != nil {
		return fail("%v", err)
	}
	if err := stats.FreezeBaseline(path, b); errors.Is(err, stats.ErrBaselineFrozen) {
		return frozen()
	} else if err != nil {
		return fail("%v", err)
	}
	w := c.Stdout
	fmt.Fprintf(w, "froze %s\n", path)
	fmt.Fprintf(w, "%d tickets, from %d sessions; %s skipped (no ticket key, or no message of yours)\n", b.Tickets, len(sessions),
		plural(skipped, "transcript"))
	fmt.Fprintf(w, "typed per ticket: %s\n", b.TypedPerTicket)
	fmt.Fprintf(w, "step starts per ticket: %s\n", b.StepStartsPerTicket)
	fmt.Fprintf(w, "classifier accuracy: %.0f%% of your %d labelled messages\n", b.Accuracy*100, len(labelled))
	if b.Accuracy < baselineMinAccuracy {
		fmt.Fprintf(c.Stderr, "magnum factory baseline: warning: the classifier got %.0f%% of your labelled messages right, under %.0f%%: "+
			"the step-start counts are a rough guide\nfix: tune stats.Classify, then move %s away and freeze again\n",
			b.Accuracy*100, baselineMinAccuracy*100, path)
	}
	return 0
}

// baselineTrackers are the issue trackers that tell a transcript's ticket:
// those of each factory repository, then the board's, one for each key
// prefix.
func baselineTrackers(cfg *config.Config) []config.Tracker {
	var out []config.Tracker
	add := func(ts []config.Tracker) {
		for _, t := range ts {
			if !slices.ContainsFunc(out, func(o config.Tracker) bool { return o.Prefix == t.Prefix }) {
				out = append(out, t)
			}
		}
	}
	for _, r := range cfg.Factory.Repos {
		add(cfg.TrackersFor(r.Repo))
	}
	add(cfg.TrackersFor(""))
	return out
}

// plural spells n of noun: "1 transcript", "3 transcripts".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
