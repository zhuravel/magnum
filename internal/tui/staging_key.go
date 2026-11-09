package tui

// E, a staging deploy of the row's PR on the board and the dashboard: a
// chooser of the PR's staging envs in the footer, each with its state (1,
// 2, 3 toggle one, a all, enter asks, any other key cancels); enter asks
// the daemon's plan of the envs chosen (a dry run) and puts it as one y/N,
// wrapped over the footer's lines so that all it names shows, whose y
// sends the deploy bound to the plan's head and to what it named.

import (
	"context"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// StagingAsked is what a staging deploy's y/N binds its deploy to: the
// envs that take the merge, the head the plan merges, and what the
// question named of each env (a deploy that now adds more is refused).
type StagingAsked struct {
	Envs []string
	Head string
	Seen []StagingSeen
}

// StagingSeen is what a deploy's question named of one env's plan: the
// migrations Jenkins then runs, and whether the merge changes
// .github/workflows.
type StagingSeen struct {
	Env        string
	Migrations []string
	Workflows  bool
}

// stagingChoice is E's chooser of the row's staging envs: on says which
// are chosen; empty says enter came with none.
type stagingChoice struct {
	ref, label string
	envs       []StagingInfo
	on         []bool
	empty      bool
}

// newStagingChoice is the chooser of r's envs, none chosen.
func newStagingChoice(r actRow) *stagingChoice {
	return &stagingChoice{ref: r.target(), label: r.label, envs: r.stagings, on: make([]bool, len(r.stagings))}
}

// key is the chooser's state in a frame's fingerprint; "" for none.
func (c *stagingChoice) key() string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(c.ref)
	for _, on := range c.on {
		b.WriteString(map[bool]string{true: "1", false: "0"}[on])
	}
	if c.empty {
		b.WriteString("!")
	}
	return b.String()
}

// chosen is the names of the envs chosen, in the chooser's order.
func (c *stagingChoice) chosen() []string {
	var out []string
	for i, on := range c.on {
		if on {
			out = append(out, c.envs[i].Env)
		}
	}
	return out
}

// line is the chooser in width cells: the PR, each env's number, mark,
// name and state, then the keys (or that enter needs an env first). The
// envs come first: the keys shorten, then go, before an env is cut, since
// a digit toggles an env the operator must see.
func (c *stagingChoice) line(st styles, width int) string {
	items := make([]string, len(c.envs))
	for i, s := range c.envs {
		mark := "[ ]"
		if c.on[i] {
			mark = "[x]"
		}
		items[i] = strconv.Itoa(i+1) + " " + mark + " " + s.Env + " " + stagingStateWords(s)
	}
	digits := "1-" + strconv.Itoa(len(c.envs))
	keys := []string{digits + " toggle, a all, enter asks, other keys cancel", digits + ", a all, enter asks", digits + ", a, enter"}
	if c.empty {
		for i := range keys {
			keys[i] = "choose an env first: " + keys[i]
		}
		keys = append(keys, "choose an env first")
	}
	line := "Deploy " + c.label + " to: " + strings.Join(items, " · ")
	if width <= 0 {
		return st.Warn.Render(line) + "  " + st.Dim.Render(keys[0])
	}
	for _, k := range keys {
		if ansi.StringWidth(line)+2+ansi.StringWidth(k) <= width {
			return st.Warn.Render(line) + "  " + st.Dim.Render(k)
		}
	}
	return st.Warn.Render(truncate(line, width))
}

// press is a key while the chooser is up, on the board's bar and the
// factory screen's alike: a digit toggles its env, a toggles them all,
// enter with none chosen says to choose one first, and up says the
// chooser stays; enter with envs chosen gives them (up false: plan them),
// any other key none (up false: cancelled).
func (c *stagingChoice) press(k string) (envs []string, up bool) {
	c.empty = false
	switch {
	case len(k) == 1 && k[0] >= '1' && k[0] <= '9':
		if i := int(k[0] - '1'); i < len(c.on) {
			c.on[i] = !c.on[i]
		}
		return nil, true
	case k == "a":
		all := !slices.Contains(c.on, false)
		for i := range c.on {
			c.on[i] = !all
		}
		return nil, true
	case k == "enter":
		envs = c.chosen()
		c.empty = len(envs) == 0
		return envs, c.empty
	}
	return nil, false
}

// chooseKey handles a key while the chooser is up (press): enter asks the
// daemon's plan of the envs chosen, any other key but a digit and a
// cancels.
func (b *actionBar) chooseKey(k string) (cmd tea.Cmd, started bool) {
	c := b.choose
	envs, up := c.press(k)
	switch {
	case up:
		return nil, false
	case envs == nil:
		b.choose = nil
		return b.note(rowActDefs[actStaging].what + " cancelled"), false
	}
	b.choose = nil
	what, fn := stagingRun(c.ref, envs)
	return b.start(what+" plan", c.ref, fn)
}

// stagingRun is what enter runs on ref's chosen envs: the daemon's plan,
// whose question then goes up as the y/N, and y the deploy bound to it.
func stagingRun(ref string, envs []string) (string, actionFunc) {
	what := rowActDefs[actStaging].what + " " + ref
	return what, func(ctx context.Context, act DashboardActions) (ActionResult, error) {
		res, err := act.StagingPlan(ctx, ref, envs)
		if err == nil && res.Ask != "" && res.Staging != nil {
			asked := *res.Staging
			res.next = &pendingAction{question: res.Ask, what: what, target: ref, wrap: true,
				fn: func(ctx context.Context, act DashboardActions) (ActionResult, error) {
					return act.StagingDeploy(ctx, ref, asked)
				}}
		}
		return res, err
	}
}
