package cli

// `magnum factory talk`: your talk with a factory task's liaison, the agent
// that is the task's front door, through the factory daemon's requests table
// as the screen sends its requests. The daemon starts the liaison (or finds
// it) and prompts it; the command then steps into its pane as the screen's o
// steps into a task's agent: herdr focuses the liaison on the factory's
// session, and the terminal on that session comes to the front.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

const factoryTalkUsage = "talk <task>"

// factoryTalkWait is how long the command waits for the daemon's answer to a
// talk: it starts an agent and prompts it, which takes longer than the other
// requests (factoryStartWait). A talk that has not answered by then goes on
// in the daemon; the command says so and steps into nothing.
const factoryTalkWait = 3 * time.Minute

func newFactoryTalkCmd(c *Context) *cobra.Command {
	return newCommand("", factoryTalkUsage, "talk with a factory task's liaison, the agent that is its front door",
		"Start the liaison of a factory task, or find it, and step into its pane: <task> is the task's id or its "+
			"Jira key, and the command waits for the daemon's answer as `start` does, longer, since the daemon "+
			"starts an agent. The liaison is one Claude agent per task, in a herdr workspace of its own in the "+
			"factory's session, working in the task's folder, never in its copy. It reads the task's files, asks "+
			"you the task's questions and explains them, and acts on your word through `magnum factory answer`, "+
			"`decide` and `retry`. Talking again finds the same liaison and its conversation, resumed if it was "+
			"parked. When you typed into its pane a moment before, its first prompt waits and the next talk sends "+
			"it. Then herdr focuses its pane and the terminal on the factory's session comes to the front. A task "+
			"that is done, stopped or failed is refused.",
		func(pos []string) int { return runFactoryTalkCmd(c, pos) })
}

func runFactoryTalkCmd(c *Context, pos []string) int {
	if len(pos) != 1 {
		return inspUsage(c, "factory talk", "which task? its id or its Jira key", factoryTalkUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory talk: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	if !loadDaemonGroupConfig(c, "factory talk") { // the factory's herdr session and your terminal
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory talk: %v\n", err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory talk: %v\n", err)
		return 1
	}
	code, q := sendFactoryRequestFor(ctx, c, reg, "factory talk", factory.ReqTalk, factory.TalkPayload{TaskID: t.ID, Surface: "cli"},
		"it starts the task's liaison", factoryTalkWait)
	if code == 0 && q.State == registry.RequestDone {
		stepIntoLiaison(ctx, c, t.ID)
	}
	return code
}

// stepIntoLiaison brings task id's liaison to the front: herdr focuses its
// agent on the factory's session, then the terminal on that session comes
// to the front (revealSession's call). A step that fails is said on stderr
// and the next still runs: the liaison talks all the same, and you may find
// it yourself.
func stepIntoLiaison(ctx context.Context, c *Context, id int64) {
	session := c.Config.FactoryHerdr()
	target, err := liaisonTarget(ctx, c, id)
	if err == nil {
		err = daemonSys.focusAgent(ctx, session.Socket, target)
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory talk: could not focus the liaison's pane in the %s herdr session: %v\n", session.Name, err)
	}
	out, rerr := reveal.Reveal(ctx, daemonSys.runner(false, nil), c.Config.Terminal, daemonSys.Getenv("HERDR_BIN_PATH"),
		reveal.Options{Session: session.Name})
	switch {
	case rerr != nil:
		fmt.Fprintf(c.Stderr, "magnum factory talk: bringing the terminal to the front failed: %v (fix [terminal] in config.toml)\n", rerr)
	case err == nil:
		fmt.Fprintf(c.Stdout, "you're in the pane of the liaison; %s\n", out)
	}
}

// liaisonTarget is the agent to focus for task id's liaison: its live
// session in the factory's agents store (its name, else its pane). No
// session, or no store yet, is an error that says so.
func liaisonTarget(ctx context.Context, c *Context, id int64) (string, error) {
	if !fsx.Exists(c.Layout.FactoryAgentsDB()) {
		return "", errors.New("no agent of the factory ran yet")
	}
	st, err := openFactoryAgentsStore(c.Layout)
	if err != nil {
		return "", fmt.Errorf("the factory's agents cannot be read: %w", err)
	}
	defer st.Close()
	s, err := runtime.LiaisonSession(ctx, st, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("the task has no liaison live in the agents store")
	}
	if err != nil {
		return "", err
	}
	target := cmp.Or(store.Deref(s.AgentName), store.Deref(s.HerdrPaneID))
	if target == "" {
		return "", errors.New("the liaison's session names no agent and no pane")
	}
	return target, nil
}
