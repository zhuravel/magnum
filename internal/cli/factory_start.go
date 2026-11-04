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

const factoryStartUsage = "start <key, link or prompt> [--kind bug|feature|chore] [--repo owner/name]"

// The wait for the factory daemon's answer to a start: it answers in its
// next tick, which a kick starts at once; reading Jira takes a few seconds.
const (
	factoryStartWait = 30 * time.Second
	factoryStartPoll = 250 * time.Millisecond
)

func newFactoryStartCmd(c *Context) *cobra.Command {
	var kind, repo string
	cmd := newCommand("", factoryStartUsage, "start a factory task from a Jira key, a link or your words",
		"Hand the factory daemon a task. A Jira key (PR-12345) or a Jira link at the start makes a task of that "+
			"ticket, which the daemon reads itself, and the words after it are your guidance for every agent: "+
			"\"PR-12345 only the refund path\". Anything else is the task in your words; its first question asks "+
			"which Jira issue it is. The daemon picks the kind of task from the Jira type or your words (--kind "+
			"picks it), names the branch <login>-<KEY>-<words>, and starts the task at once, or queues it while "+
			"memory is full or max_active tasks are at work. A key that has a task gets that task back, and no "+
			"second one. A GitHub PR link (adoption) and an Actions run link (a CI task) come in later milestones. "+
			"While Jira does not answer, the task waits for it and tries again after 1, 2, 4, 8, 16 and then every "+
			"30 minutes, and needs you after 6 hours; an unknown key or a missing token file makes no task. "+
			"--repo names the [[factory.repo]] when the config has more than one.",
		func(pos []string) int { return runFactoryStartCmd(c, kind, repo, pos) })
	cmd.Flags().StringVar(&kind, "kind", "", "the kind of task (a [factory.kinds] name: bug, feature, chore); default: picked")
	cmd.Flags().StringVar(&repo, "repo", "", "the repository (owner/name of a [[factory.repo]]); default: the only one")
	return cmd
}

func runFactoryStartCmd(c *Context, kind, repo string, pos []string) int {
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
	return sendFactoryRequest(ctx, c, reg, "factory start", factory.ReqStart, factory.StartPayload{Text: text, Kind: kind, Repo: repo},
		"it starts the task")
}

// waitFactoryRequest re-reads request id until the factory daemon answers
// it, ctx ends or factoryStartWait passes, and returns its last read.
func waitFactoryRequest(ctx context.Context, reg *registry.Registry, id int64) (registry.Request, error) {
	deadline := time.Now().Add(factoryStartWait)
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
