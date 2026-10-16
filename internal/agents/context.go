package agents

// How full a session's context is, and its compaction. The fix job resumes
// one fixer conversation across a PR's cycles; Claude Code compacts it by
// itself once it is full, in the middle of a turn, and can lose the finding
// it was fixing. The engine reads the figure the pane's status lines show
// (ContextUsed, the kind's context_pattern) before a cycle that resumes the
// fixer and, when [pipeline] fix_compact_at says so, has it compact first
// (Compact), between two turns.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// CompactCommand is what Compact types into the agent's pane: Claude Code's
// /compact, which replaces the conversation so far with its summary.
const CompactCommand = "/compact"

// Compaction timing.
const (
	// CompactTimeout bounds Compact's wait for the compaction turn to end.
	CompactTimeout = 10 * time.Minute
	compactPoll    = 2 * time.Second
	// compactPolls bounds the polls by count, not by the clock alone.
	compactPolls = int(CompactTimeout / compactPoll)
	// compactQuietPolls: an agent never seen working since the command
	// (a compaction that ended between two polls, or one that never
	// started) counts as done once it stayed idle this many polls; the
	// figure read afterwards tells which.
	compactQuietPolls = 5
)

// contextStatusLines is how many of the visible screen's last non-empty
// lines ContextUsed reads: its status lines, as for a model switch
// (switchStatusLines), so a percent the agent wrote in its output above the
// composer never counts.
const contextStatusLines = switchStatusLines

// ContextUsed reads how full session s's context is, in percent, from the
// last status lines of its pane's visible screen (contextStatusLines), with
// its kind's context_pattern (config.Kind.ContextRegexp): the newest match,
// the bottom line's last first, gives the figure, its "used" group as
// written, its "left" group as 100 minus it; a figure above 100 is no
// figure. ok is false when the kind has no pattern, the pane cannot be
// read, or no line shows a figure.
func (m *Manager) ContextUsed(ctx context.Context, s store.Session) (percent int, ok bool) {
	k, found := m.kindSpec(m.sessionKind(s))
	if !found {
		return 0, false
	}
	re, err := k.ContextRegexp()
	if err != nil || re == nil {
		return 0, false
	}
	text, err := m.readVisible(ctx, paneRef{name: store.Deref(s.AgentName), pane: store.Deref(s.HerdrPaneID)})
	if err != nil {
		m.logErr(ctx, err, "agents: %s: read the context figure: %v", s.Role, err)
		return 0, false
	}
	return contextFigure(re, lastLines(text, contextStatusLines))
}

// lastLines is text's last n non-empty lines, trimmed, oldest first.
func lastLines(text string, n int) []string {
	var out []string
	for l := range strings.SplitSeq(strings.ReplaceAll(text, "\r", ""), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out[max(0, len(out)-n):]
}

// contextFigure is the newest figure re finds in lines (see ContextUsed).
func contextFigure(re *regexp.Regexp, lines []string) (int, bool) {
	names := re.SubexpNames()
	for _, line := range slices.Backward(lines) {
		for _, match := range slices.Backward(re.FindAllStringSubmatch(line, -1)) {
			for g, name := range names {
				v := match[g]
				if v == "" || (name != config.ContextUsedGroup && name != config.ContextLeftGroup) {
					continue
				}
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 || n > 100 {
					continue
				}
				if name == config.ContextLeftGroup {
					n = 100 - n
				}
				return n, true
			}
		}
	}
	return 0, false
}

// Compact compacts the conversation of session s: it types CompactCommand
// into the idle agent's pane, as SwitchModel types /model, and waits for
// the compaction turn to end: the agent seen working, then idle on
// CompletionIdleTicks polls in a row (compactQuietPolls when it was never
// seen working). The turn is magnum's: the session's last_prompt_at and
// idle_ticks are set as a prompt sets them, and the observer never takes the
// agent's work for a human's while Compact waits (isCompacting). An agent
// that is not idle is refused with ErrBusy before anything is typed; a
// turn that has not ended within CompactTimeout is ErrTimeout (the agent
// may still be at it).
func (m *Manager) Compact(ctx context.Context, s store.Session) error {
	pane := store.Deref(s.HerdrPaneID)
	if pane == "" {
		return fmt.Errorf("agents: compact %s: no pane: %w", s.Role, ErrNoSession)
	}
	p, err := m.d.Herdr.PaneGet(ctx, pane)
	if err != nil {
		return fmt.Errorf("agents: compact %s: %w", s.Role, mapHerdr(err))
	}
	if !idleStatus(p.AgentStatus) {
		return fmt.Errorf("agents: compact %s: agent is %q: %w", s.Role, p.AgentStatus, ErrBusy)
	}
	m.setCompacting(s.ID, true)
	defer m.setCompacting(s.ID, false)
	if err := m.d.Herdr.PaneRun(ctx, pane, CompactCommand); err != nil {
		return fmt.Errorf("agents: compact %s: %w", s.Role, mapHerdr(err))
	}
	now := m.now()
	if err := m.d.Store.UpdateSession(context.WithoutCancel(ctx), s.ID, func(u *store.SessionUpdate) {
		u.Set("last_prompt_at", now)
		u.Set("idle_ticks", 0)
	}); err != nil {
		m.logErr(ctx, err, "agents: compact %s: record the prompt: %v", s.Role, err)
	}
	seen, idle := false, 0
	for poll := 1; ; poll++ {
		if err := m.sleep(ctx, compactPoll); err != nil {
			return fmt.Errorf("agents: compact %s: %w", s.Role, err)
		}
		p, err := m.d.Herdr.PaneGet(ctx, pane)
		if err != nil {
			return fmt.Errorf("agents: compact %s: %w", s.Role, mapHerdr(err))
		}
		if idleStatus(p.AgentStatus) {
			idle++
		} else {
			seen, idle = true, 0
		}
		if seen && idle >= CompletionIdleTicks || idle >= compactQuietPolls {
			return nil
		}
		if poll >= compactPolls {
			return fmt.Errorf("agents: compact %s: not idle again after %s (agent %q): %w", s.Role, CompactTimeout, p.AgentStatus, ErrTimeout)
		}
	}
}

// setCompacting marks session id as compacting (Compact), during which the
// observer never takes its agent's work for a human's.
func (m *Manager) setCompacting(id int64, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.compacting[id] = true
	} else {
		delete(m.compacting, id)
	}
}

func (m *Manager) isCompacting(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.compacting[id]
}
