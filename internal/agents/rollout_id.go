package agents

// herdr learns a Codex session's id from the SessionStart hook it installs
// in the Codex home (session_hook.go). On 2026-10-22 that hook stopped
// reaching most new sessions (the hook script reports over herdr's socket
// with a short timeout and swallows a failure, and the machine was loaded),
// so judges ran without an id: they could not be resumed, and TurnError had
// no rollout to read. The id need not come from herdr alone. Codex writes
// each session's rollout as
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<start>-<id>.jsonl (the day and
// the start in local time), creating it at the session's first prompt, and
// its first line, the session_meta, carries the id, the cwd, the start
// (payload.timestamp; the line's own timestamp is when it was written, which
// may be much later), the source ("cli" for the interactive TUI an agent
// pane runs, "exec" for `codex exec`, which a shell role such as
// codex-review runs in the same checkout) and, for a helper agent,
// parent_thread_id. recoverSessionID matches a session to its rollout by
// those.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// EventSessionID is recorded (info, the PR's subject) when the observer
// records a Codex session's id from its rollout because herdr's session
// hook gave none (data: role, session, session_id, source).
const EventSessionID = "agent.session_id"

// SessionIDFromRollout is EventSessionID's source of an id read from the
// session's Codex rollout.
const SessionIDFromRollout = "rollout"

const (
	// RecoverIDAfter is how long after its start a live Codex session that
	// herdr gave no id gets it from its rollout; it is tried again at each
	// observation until it has one.
	RecoverIDAfter = time.Minute
	// RecoverIDWindow is how far a rollout's start (its session_meta's
	// timestamp) may be from the session's start to be its rollout.
	RecoverIDWindow = 2 * time.Minute
	// RecoverIDHeads bounds the rollouts one recovery reads the first line
	// of, newest first by name.
	RecoverIDHeads = 300
	// metaLineMax bounds a session_meta line (its instructions run to tens
	// of kilobytes); a longer one is skipped.
	metaLineMax = 1 << 20
	// codexSourceCLI is the session_meta source of Codex's interactive TUI,
	// which an agent pane runs.
	codexSourceCLI = "cli"
)

// rolloutHead is what a rollout's first line, its session_meta, says of the
// session that wrote it.
type rolloutHead struct {
	id      string
	started time.Time // payload.timestamp: when the session started
	cwd     string
	source  string // "cli", "exec", "vscode", ...; "" for a helper's (an object)
	parent  string // a helper agent's parent_thread_id
}

// recoverSessionID gives Codex session s (its row before this tick's
// update), whose agent herdr reports without an id and whose row has none,
// the id of its rollout once RecoverIDAfter has passed since its start: of
// the rollouts of its Codex home (TurnError's) started within
// RecoverIDWindow of the session's start (rolloutHeads), the one of
// Codex's interactive TUI (source "cli", never a helper's) whose cwd is the
// session's (both cleaned, symlinks resolved) and whose id no other session
// of the registry holds. Exactly one such rollout is recorded as the
// session's id, compare-and-set on a row that still has none, with an
// EventSessionID; none, or two (two Codex sessions started in the same
// checkout within the window), records nothing and is logged once per
// session. It returns the id recorded ("" none).
func (m *Manager) recoverSessionID(ctx context.Context, s store.Session, now time.Time) string {
	if m.sessionKind(s) != KindCodex || store.Deref(s.SessionID) != "" || now.Sub(s.StartedAt) < RecoverIDAfter {
		return ""
	}
	if store.Deref(s.Cwd) == "" || m.codexHome(s) == "" {
		return ""
	}
	ids, err := m.cliRolloutIDs(ctx, s)
	if len(ids) != 1 {
		if firstFor(m, m.recoverLogged, s.ID) {
			why := "no rollout of its Codex home matches its checkout and start yet"
			if len(ids) > 1 {
				why = fmt.Sprintf("%d rollouts of its Codex home match its checkout and start (Codex sessions started in the same checkout within %s)",
					len(ids), RecoverIDWindow)
			}
			if err != nil {
				why += fmt.Sprintf(" (%v)", err)
			}
			m.logAt(slog.LevelInfo, "agents: %s (session %d): herdr's session hook gave no Codex session id, and %s; it stays unknown",
				s.Role, s.ID, why)
		}
		return ""
	}
	id := ids[0]
	err = m.d.Store.TransitionSession(ctx, s.ID, liveStates, store.SessionLive, func(u *store.SessionUpdate) {
		u.Where("session_id", nil)
		u.Set("session_id", id)
	})
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			m.logErr(ctx, err, "agents: %s (session %d): record the session id of its Codex rollout: %v", s.Role, s.ID, err)
		}
		return ""
	}
	m.mu.Lock()
	m.recovered[s.ID] = id
	m.mu.Unlock()
	m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventSessionID,
		fmt.Sprintf("%s: session id %s from its Codex rollout; herdr's session hook gave none", s.Role, id),
		map[string]any{"role": s.Role, "session": s.ID, "session_id": id, "source": SessionIDFromRollout})
	return id
}

// cliRolloutIDs are the ids of the rollouts that may be Codex session s's
// (recoverSessionID's rule): of the rollouts of its Codex home (TurnError's)
// started within RecoverIDWindow of its start (rolloutHeads), those of
// Codex's interactive TUI (source "cli", never a helper's) whose cwd is the
// session's (both cleaned, symlinks resolved) and whose id no other session
// of the registry holds. err joins the read errors met on the way.
func (m *Manager) cliRolloutIDs(ctx context.Context, s store.Session) ([]string, error) {
	cwd, home := store.Deref(s.Cwd), m.codexHome(s)
	if cwd == "" || home == "" {
		return nil, nil
	}
	heads, err := rolloutHeads(home, s.StartedAt, RecoverIDWindow, RecoverIDHeads)
	dir := resolvedDir(cwd)
	var ids []string
	for _, h := range heads {
		if h.source != codexSourceCLI || h.parent != "" || resolvedDir(h.cwd) != dir || slices.Contains(ids, h.id) {
			continue
		}
		held, herr := m.d.Store.SessionIDHeld(ctx, h.id, s.ID)
		if herr != nil {
			err = errors.Join(err, herr)
			continue
		}
		if !held {
			ids = append(ids, h.id)
		}
	}
	return ids, err
}

// herdrSessionID notes that herdr reports id for session s: when it differs
// from the id its rollout gave the session (recoverSessionID), herdr's wins,
// which the caller records; this logs it.
func (m *Manager) herdrSessionID(s store.Session, id string) {
	m.mu.Lock()
	rec, ok := m.recovered[s.ID]
	if ok {
		delete(m.recovered, s.ID)
	}
	m.mu.Unlock()
	if ok && id != "" && id != rec {
		m.logAt(slog.LevelWarn, "agents: %s (session %d): herdr reports Codex session id %s, not %s, which its rollout gave; herdr's wins",
			s.Role, s.ID, id, rec)
	}
}

// resolvedDir is dir cleaned, with its symlinks resolved when it exists.
func resolvedDir(dir string) string {
	if dir == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// rolloutHeads reads the first lines of the rollouts under Codex home home
// whose names say they started within window of start (the name's start is
// local time, whole seconds), at most limit of them, newest first by name:
// the day directories of that span only. A rollout whose first line is no
// session_meta naming the id its file name ends with is left out, and so is
// one whose session_meta started outside the window. A missing directory is
// none; err names the first other read error.
func rolloutHeads(home string, start time.Time, window time.Duration, limit int) ([]rolloutHead, error) {
	from, to := start.Add(-window), start.Add(window)
	var files []string
	var errs []error
	first, last := from.Local(), to.Local()
	for day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.Local); !day.After(last); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if at, ok := rolloutNameStart(e.Name()); ok && !at.Before(from.Add(-time.Second)) && !at.After(to) {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	slices.SortFunc(files, func(a, b string) int { return strings.Compare(filepath.Base(b), filepath.Base(a)) })
	var out []rolloutHead
	for _, path := range files[:min(len(files), limit)] {
		h, ok, err := readRolloutHead(path)
		if err != nil {
			errs = append(errs, err)
		}
		if ok && !h.started.Before(from) && !h.started.After(to) {
			out = append(out, h)
		}
	}
	return out, errors.Join(errs...)
}

// rolloutNameStart is the start a rollout's file name carries
// (rollout-2026-10-22T18-42-01-<id>.jsonl, local time).
func rolloutNameStart(name string) (time.Time, bool) {
	const layout = "2006-01-02T15-04-05"
	rest, ok := strings.CutPrefix(name, "rollout-")
	if !ok || !strings.HasSuffix(name, ".jsonl") || len(rest) < len(layout) {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation(layout, rest[:len(layout)], time.Local)
	return at, err == nil
}

// readRolloutHead reads the session_meta of the rollout at path (its first
// line, at most metaLineMax bytes). ok is false for a first line that is no
// session_meta of the id the file name ends with.
func readRolloutHead(path string) (rolloutHead, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return rolloutHead{}, false, err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(io.LimitReader(f, metaLineMax), 64<<10).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return rolloutHead{}, false, err
	}
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string          `json:"id"`
			Timestamp      time.Time       `json:"timestamp"`
			Cwd            string          `json:"cwd"`
			Source         json.RawMessage `json:"source"`
			ParentThreadID string          `json:"parent_thread_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "session_meta" {
		return rolloutHead{}, false, nil
	}
	p := ev.Payload
	if !rolloutID(p.ID) || !strings.HasSuffix(filepath.Base(path), "-"+p.ID+".jsonl") || p.Timestamp.IsZero() {
		return rolloutHead{}, false, nil
	}
	var source string
	_ = json.Unmarshal(p.Source, &source) // a helper's source is an object: ""
	return rolloutHead{id: p.ID, started: p.Timestamp, cwd: p.Cwd, source: source, parent: p.ParentThreadID}, true, nil
}
