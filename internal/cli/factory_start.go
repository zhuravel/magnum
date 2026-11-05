package cli

// `magnum factory start`: hand the factory daemon a task, a Jira key or a
// Jira link with your guidance after it, or the task in your words, through
// its requests table, as magnum's CLI hands work to the review daemon.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/paths"
)

const factoryStartUsage = "start <key, link or prompt> [--kind bug|feature|chore] [--repo owner/name] [--words-only]"

// The wait for the factory daemon's answer to a start: it answers in its
// next tick, which a kick starts at once; reading Jira takes a few seconds.
const (
	factoryStartWait = 30 * time.Second
	factoryStartPoll = 250 * time.Millisecond
)

func newFactoryStartCmd(c *Context) *cobra.Command {
	var kind, repo string
	var wordsOnly bool
	cmd := newCommand("", factoryStartUsage, "start a factory task from a Jira key, a link or your words",
		"Hand the factory daemon a task. A Jira key (PR-12345) or a Jira link at the start makes a task of that "+
			"ticket, which the daemon reads itself, and the words after it are your guidance for every agent: "+
			"\"PR-12345 only the refund path\". Anything else is the task in your words; its first question asks "+
			"which Jira issue it is. A key or a Jira link elsewhere in your words counts too when Jira knows the "+
			"key (\"fix the expiry date in PR-12345, only the refund path\"): the task follows that ticket and the "+
			"whole text is its guidance; a word such as UTF-8 stays a word (when the trackers name Jira projects, a "+
			"bare key of another project is never read from Jira), and several known keys make the first question ask which of "+
			"them it is. The daemon picks the kind of task from the Jira type or your words (--kind "+
			"picks it), names the branch <login>-<KEY>-<words>, and starts the task at once, or queues it while "+
			"memory is full or max_active tasks are at work. A key at the start that has a task gets that task back, "+
			"and no second one; a key in the middle of your words that a task holds stays a word, and the answer "+
			"names its task. A GitHub PR link (adoption) and an Actions run link (a CI task) come in later milestones. "+
			"While Jira does not answer (a timeout, no connection, a 5xx or a 429), the task waits for it and tries "+
			"again after 1, 2, 4, 8, 16 and then every 30 minutes, and needs you after 6 hours; an unknown key or a "+
			"missing token file makes no task (not for a key found in the middle of your words: whatever Jira refuses "+
			"it for, it stays a word and the task starts from your words). --words-only starts the task from your "+
			"words only, as the screen's \"Start from your words only?\": a key at the start (one Jira does not know) "+
			"is words, and no key of them is read from Jira. --repo names the [[factory.repo]] when the config has "+
			"more than one; without it the daemon takes the one your text tells: a GitHub link of one, else the first "+
			"key whose prefix is a tracker of one of them only (its [[repo]]'s or its [[watch]]'s, and no other's sends "+
			"the prefix to another tracker), and asks for --repo when it tells none. A key of a tracker that is not "+
			"Jira (one whose link is no atlassian.net /browse/ link and not on the [jira] site, such as a Linear issue) "+
			"is never read from Jira: at the start, bare or as its tracker's link, it is the task's key and the task "+
			"starts from the words after it; alone, it is refused with what to do.",
		func(pos []string) int { return runFactoryStartCmd(c, kind, repo, wordsOnly, pos) })
	cmd.Flags().StringVar(&kind, "kind", "", "the kind of task (a [factory.kinds] name: bug, feature, chore); default: picked")
	cmd.Flags().StringVar(&repo, "repo", "", "the repository (owner/name of a [[factory.repo]]); default: the one your text tells, or the only one")
	cmd.Flags().BoolVar(&wordsOnly, "words-only", false, "start from your words only: no key of them is read from Jira")
	return cmd
}

func runFactoryStartCmd(c *Context, kind, repo string, wordsOnly bool, pos []string) int {
	text := strings.TrimSpace(strings.Join(pos, " "))
	if text == "" {
		return inspUsage(c, "factory start", "what to start? a Jira key, a link or the task in your words", factoryStartUsage)
	}
	if !loadDaemonGroupConfig(c, "factory start") {
		return 1
	}
	f := c.Config.Factory
	if _, ok := f.Kinds[kind]; kind != "" && !ok {
		return inspUsage(c, "factory start", fmt.Sprintf("no kind of task %q: one of %s", kind,
			strings.Join(slices.Sorted(maps.Keys(f.Kinds)), ", ")), factoryStartUsage)
	}
	if repo != "" {
		if _, ok := c.Config.FactoryRepo(repo); !ok {
			var names []string
			for _, r := range f.Repos {
				names = append(names, r.Repo)
			}
			return inspUsage(c, "factory start", fmt.Sprintf("no [[factory.repo]] is %s (configured: %s)", repo,
				cmp.Or(strings.Join(names, ", "), "none")), factoryStartUsage)
		}
	}
	ctx, stop := daemonSys.signals()
	defer stop()
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory start: %v\n", err)
		return 1
	}
	defer reg.Close()
	return sendFactoryRequest(ctx, c, reg, "factory start", factory.ReqStart, factory.StartPayload{Text: text, Kind: kind, Repo: repo, WordsOnly: wordsOnly},
		"it starts the task")
}

// waitFactoryRequest re-reads request id until the factory daemon answers
// it, ctx ends or factoryStartWait passes, and returns its last read.
func waitFactoryRequest(ctx context.Context, reg *registry.Registry, id int64) (registry.Request, error) {
	return waitFactoryRequestFor(ctx, reg, id, factoryStartWait)
}

// waitFactoryRequestFor is waitFactoryRequest, giving up after wait.
func waitFactoryRequestFor(ctx context.Context, reg *registry.Registry, id int64, wait time.Duration) (registry.Request, error) {
	deadline := time.Now().Add(wait)
	for {
		q, err := reg.RequestByID(ctx, id)
		done := err != nil || q.State != registry.RequestPending || !time.Now().Before(deadline)
		if !done {
			done = daemonSys.sleep(ctx, factoryStartPoll) != nil // interrupted: the request stays pending
		}
		if done {
			return q, err
		}
	}
}

// factoryKick wakes the factory daemon (FactoryKick, else factory.Kick):
// its pid, 0 when none runs.
func (s daemonGroupSys) factoryKick(l paths.Layout) (int, error) {
	if s.FactoryKick != nil {
		return s.FactoryKick(l)
	}
	return factory.Kick(l)
}

// sleep waits d (Sleep, else actSleep).
func (s daemonGroupSys) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	return actSleep(ctx, d)
}
