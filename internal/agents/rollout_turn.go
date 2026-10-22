package agents

// herdr learns a Codex agent's status from the same hook script that gives
// it the session's id, and on 2026-10-22, when that script's reports did
// not reach herdr on a loaded machine, herdr showed judges idle while Codex
// still worked: their rollouts kept growing, the observer ended their turns
// on two idle ticks, and the own passes of three PRs failed "finished
// without writing judge-own.md". Codex ends every turn in its rollout with
// a task_complete event, or turn_aborted when the turn is interrupted, so
// before an idle reading ends a Codex turn the observer reads the rollout
// (codexTurnGoing): a turn with neither since its run's prompt, whose
// rollout Codex wrote within RolloutGrowth, still works.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// RolloutGrowth is how recently a Codex session's rollout must have been
// written for a turn without a task_complete since its prompt to count as
// still working while herdr reports the agent idle or done.
const RolloutGrowth = 90 * time.Second

// codexTurnGoing reports whether the turn of run, in flight on Codex
// session s (sid its Codex session id) whose agent herdr reports with
// status (idle or done), still works as the session's rollout shows: no
// task_complete or turn_aborted event stamped at or after the run's
// creation (less transcriptSkew), and the rollout written within
// RolloutGrowth of now. Then the observer keeps the run open, as it does
// for a claude agent's background work, and logs it once per run. A turn
// that ended in the rollout ends on herdr's idle as before, with no added
// wait, and so does one whose rollout stopped growing (the role's timeout
// bounds a rollout that never does); another kind, a session without an id
// and a rollout it cannot find or read keep herdr's word too.
func (m *Manager) codexTurnGoing(ctx context.Context, s store.Session, sid string, run store.Run, status herdr.Status, now time.Time) bool {
	if m.sessionKind(s) != KindCodex || sid == "" {
		return false
	}
	path, err := m.rolloutPath(s, sid)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	age := now.Sub(st.ModTime())
	if age >= RolloutGrowth {
		return false
	}
	ended, err := turnEndedSince(path, run.CreatedAt.Add(-transcriptSkew))
	if err != nil {
		m.logErr(ctx, err, "agents: %s: read the Codex rollout for the end of run %s: %v", s.Role, run.ID, err)
		return false
	}
	if ended {
		return false
	}
	if firstFor(m, m.rolloutHeld, run.ID) {
		m.logAt(slog.LevelWarn, "agents: %s (session %d): herdr reports the agent %s, but its Codex rollout has no task_complete since run %s "+
			"and grew %s ago: the turn goes on", s.Role, s.ID, status, run.ID, max(age, 0).Round(time.Second))
	}
	return true
}

// turnEndedSince reports whether the end of the rollout at path (its last
// rolloutTail bytes) holds a task_complete or turn_aborted event stamped at
// or after since: the last such event decides.
func turnEndedSince(path string, since time.Time) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	off := max(st.Size()-rolloutTail, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return false, err
	}
	for _, line := range slices.Backward(strings.Split(string(b), "\n")) {
		if !strings.Contains(line, `"task_complete"`) && !strings.Contains(line, `"turn_aborted"`) {
			continue // most lines: no JSON decoding
		}
		var ev rolloutEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Payload.Type != "task_complete" && ev.Payload.Type != "turn_aborted" {
			continue
		}
		return !ev.Timestamp.Before(since), nil
	}
	return false, nil
}
