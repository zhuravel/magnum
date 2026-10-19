package agents

// The idle park's early park (DECISIONS "An idle agent parks once keeping
// it live stops paying"): the engine parks the agents of a PR that idled
// past their threshold while another agent of the PR stays live, one
// session at a time, with Park's guards, and ends it as the PR's park ends:
// once its quit reached an idle shell, the session's own pane is closed
// (herdr drops an agent with its pane) and then the session is parked, as
// the PR's park closes the workspace before it retires the sessions. No
// start can come in between: it needs the PR's reservation.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The events of one session's early park (ParkSessions), info each, with
// the role, the session, its pane and agent: its begin, its end as parked
// (ok) and its failure (fail, with the cause). The engine's pr.parked sums
// the park up, and its pr.park_failed warns once per cause.
const (
	EventSessionParkBegin  = "pr.session_park_begin"
	EventSessionParked     = "pr.session_parked"
	EventSessionParkFailed = "pr.session_park_failed"
)

// ParkSessions parks the live sessions of pr that ids names, and only
// them: the idle park's early park of the agents past their threshold while
// another agent of the PR stays live (the engine's parkEarly, which runs it
// under the PR's reservation). It keeps Park's
// guards over a fresh snapshot (ErrBusy while a run of the PR is pending,
// submitted or working, or a selected agent works or is blocked; the
// session ids herdr shows recorded first) and refuses, before it quits any,
// a selected session with no conversation to resume (resumable): only a
// conversation the next round resumes is worth an early park. A selected
// session whose agent herdr no longer lists is parked without a key when
// its pane is an idle shell (ErrBusy, before any key, when the pane runs
// something). Each selected agent herdr lists is quit as Quit does (ctrl+c,
// 1 s, ctrl+c, the wait for an idle shell; never the hard mode: one that
// does not quit stays live with ErrBusy and waits for the PR's park). Once
// its pane is an idle shell, that pane is closed and then the session is
// parked (parkOne); a pane the close refuses or herdr fails to close is
// left for a later CloseRetiredPanes, the session parked all the same. The
// workspace stays while other panes live. Ids of sessions no longer live
// are skipped. Each session's park
// records its begin, ok and fail (EventSessionParkBegin ...), and a failure
// does not stop the other sessions: parked are the sessions it parked, and
// the failures come back joined beside them.
func (m *Manager) ParkSessions(ctx context.Context, pr store.PR, ids []int64) (parked []store.Session, err error) {
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, fmt.Errorf("agents: park sessions of pr %d: %w", pr.Number, err)
	}
	selected := slices.DeleteFunc(sessions, func(s store.Session) bool { return !isLive(s) || !slices.Contains(ids, s.ID) })
	if len(selected) == 0 {
		return nil, nil
	}
	if err := m.parkCheckRuns(ctx, pr); err != nil {
		return nil, err
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("agents: park sessions of pr %d: %w", pr.Number, err)
	}
	if err := m.parkCheckLive(ctx, pr, snap, selected); err != nil {
		return nil, err
	}
	panes := paneIndex(snap)
	for _, s := range selected {
		if !m.resumable(s) {
			return nil, fmt.Errorf("agents: park sessions of pr %d: %s session %d has no conversation to resume", pr.Number, Role(s.Role).Label(), s.ID)
		}
		if _, listed := findAgent(snap, s); listed {
			continue
		}
		if p, ok := panes[store.Deref(s.HerdrPaneID)]; ok {
			// No agent herdr lists for it: its pane must be an idle shell, not
			// a command or an agent herdr does not count as the session's.
			if err := m.parkCheckPane(ctx, pr, snap, p); err != nil {
				return nil, fmt.Errorf("%w (%s pane)", err, Role(s.Role).Label())
			}
		}
	}
	subject := m.prSubject(ctx, pr.ID)
	var errs []error
	for _, s := range selected {
		if err := m.parkSession(ctx, subject, snap, s); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		parked = append(parked, s)
	}
	if len(errs) > 0 {
		return parked, fmt.Errorf("agents: park sessions of pr %d: %w", pr.Number, errors.Join(errs...))
	}
	return parked, nil
}

// parkSession parks session s alone (ParkSessions) between its begin and
// its ok or fail event: the agent snap lists for it is quit until its pane
// is an idle shell, then s is parked.
func (m *Manager) parkSession(ctx context.Context, subject string, snap herdr.Snapshot, s store.Session) error {
	label := Role(s.Role).Label()
	a, listed := findAgent(snap, s)
	agent := store.Deref(s.AgentName)
	if a.Name != "" {
		agent = a.Name
	}
	data := map[string]any{"role": s.Role, "session": s.ID, "pane": store.Deref(s.HerdrPaneID), "agent": agent}
	m.event(ctx, subject, "info", EventSessionParkBegin, fmt.Sprintf("parking %s (session %d) alone", label, s.ID), data)
	note, err := m.parkOne(ctx, snap, s, listed)
	if err != nil {
		m.event(ctx, subject, "info", EventSessionParkFailed, fmt.Sprintf("%s (session %d) was not parked: %v", label, s.ID, err), data)
		return err
	}
	m.event(ctx, subject, "info", EventSessionParked, fmt.Sprintf("parked %s (session %d)%s; the next round resumes its conversation", label, s.ID, note), data)
	return nil
}

// parkEndTimeout bounds the end of one session's early park, its pane's
// close and its retire, which run even when the park's context ended.
const parkEndTimeout = 10 * time.Second

// parkOne quits the agent of session s when herdr listed it (listed),
// until its pane is an idle shell, then closes s's own pane and parks s,
// in that order, as the PR's park closes its workspace before it retires
// the sessions: a parked row whose pane herdr still showed with the quit
// conversation would be restored as live by the next start's Recover. The
// close and the retire run on a context a stop cannot cut (parkEndTimeout),
// so a stop never lands between them; a crash after the close leaves a live
// row without a pane, which the observer turns into an ordinary lost
// session, still resumable. A close the check refuses (something runs
// there) or herdr fails parks s all the same, its pane left for a later
// CloseRetiredPanes, as after the PR's park; note says so for the ok event.
func (m *Manager) parkOne(ctx context.Context, snap herdr.Snapshot, s store.Session, listed bool) (note string, err error) {
	if listed {
		waited, err := m.interrupt(ctx, &s)
		switch {
		case err != nil:
			return "", err
		case waited != nil:
			return "", quitBusy(s, waited) // it did not quit: live, as after Quit
		}
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkEndTimeout)
	defer cancel()
	switch closed, err := m.closeOwnPane(ectx, snap, s); {
	case err != nil:
		m.logErr(ctx, err, "agents: early park of session %d: its pane stays for a later close: %v", s.ID, err)
		note = fmt.Sprintf("; its pane %s stays for a later close (%v)", store.Deref(s.HerdrPaneID), err)
	case closed:
		note = fmt.Sprintf("; its pane %s closed", store.Deref(s.HerdrPaneID))
	}
	return note, m.retireAs(ectx, s, store.SessionParked)
}

// closeOwnPane closes session s's own pane, never another, once a fresh
// look finds only the shell in its foreground (CloseRetiredPanes' check),
// and a workspace the close leaves without a pane (closeEmptied over snap,
// the park's snapshot). closed is false when s records no pane or herdr no
// longer has it; err says why a pane was left (something runs there, or
// herdr failed).
func (m *Manager) closeOwnPane(ctx context.Context, snap herdr.Snapshot, s store.Session) (closed bool, err error) {
	pane := store.Deref(s.HerdrPaneID)
	if pane == "" {
		return false, nil
	}
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err):
		return false, nil
	case err != nil:
		return false, err
	case !herdr.IdleShell(pi):
		return false, fmt.Errorf("pane %s runs a command", pane)
	}
	if err := m.d.Herdr.PaneClose(ctx, pane); err != nil {
		if isGone(err) {
			return false, nil
		}
		return false, err
	}
	m.closeEmptied(ctx, snap, []string{pane})
	return true, nil
}
