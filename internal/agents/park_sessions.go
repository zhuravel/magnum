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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
// something). Each selected agent herdr lists is quit by earlyQuit (one
// ctrl+c and its kind's exit keys, a kind without them two ctrl+c, then the
// wait for an idle shell; never the hard mode: one that does not quit stays
// live with ErrBusy and waits for the PR's park). Once
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
	note, err := m.parkOne(ctx, snap, s, a, listed)
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

// parkOne quits the agent of session s when herdr listed it (listed, as
// a), until its pane is an idle shell (earlyQuit), then closes s's own pane
// and parks s, in that order, as the PR's park closes its workspace before
// it retires the sessions: a parked row whose pane herdr still showed with
// the quit conversation would be restored as live by the next start's
// Recover. The close and the retire run on a context a stop cannot cut
// (parkEndTimeout), so a stop never lands between them; a crash after the
// close leaves a live row without a pane, which the observer turns into an
// ordinary lost session, still resumable. A close the check refuses
// (something runs there) or herdr fails parks s all the same, its pane left
// for a later CloseRetiredPanes, as after the PR's park; note says so for
// the ok event, and how the agent quit when its exit keys quit it.
func (m *Manager) parkOne(ctx context.Context, snap herdr.Snapshot, s store.Session, a herdr.AgentInfo, listed bool) (note string, err error) {
	if listed {
		exitKeys, waited, err := m.earlyQuit(ctx, &s, a)
		switch {
		case err != nil:
			return "", err
		case waited != nil:
			return "", quitBusy(s, waited) // it did not quit: live, as after Quit
		case exitKeys:
			note = "; quit with its exit keys"
		}
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkEndTimeout)
	defer cancel()
	switch closed, err := m.closeOwnPane(ectx, snap, s); {
	case err != nil:
		m.logErr(ctx, err, "agents: early park of session %d: its pane stays for a later close: %v", s.ID, err)
		note += fmt.Sprintf("; its pane %s stays for a later close (%v)", store.Deref(s.HerdrPaneID), err)
	case closed:
		note += fmt.Sprintf("; its pane %s closed", store.Deref(s.HerdrPaneID))
	}
	return note, m.retireAs(ectx, s, store.SessionParked)
}

// earlyQuit is the early park's quit of session s's agent, which the
// park's snapshot listed as a: one ctrl+c (the session id recorded first,
// s.SessionID too), then, after 1 s, the kind's exit keys (Kind.ExitKeys,
// Claude's /exit) and a wait of up to QuitIdleTimeout for the pane to be an
// idle shell (sendExitKeys, the hard quit's): two ctrl+c did not quit
// Claude 2.1.301 nor 2.1.303 at its idle prompt, one ctrl+c and its /exit
// did. The same look (idlePrompt) runs before the ctrl+c and again before
// the exit keys: the conversation the park found, taken before the ctrl+c
// (the snapshot's agent_session, else the id s recorded: the ctrl+c's id
// refresh records whatever herdr shows then), idle at a bare composer with
// no dialog on screen. A screen that is not bare before the ctrl+c gets no
// key at all; one that changes during the pause gets the ctrl+c alone; an
// agent gone before the ctrl+c gets none, and one the ctrl+c quit no exit
// keys. Nothing is forced: a pane still no idle shell after them comes back
// as waited, as interrupt's. A kind without exit keys, or a session that
// records no pane, is quit as Quit does (interrupt). exitKeys: the exit
// keys quit it.
func (m *Manager) earlyQuit(ctx context.Context, s *store.Session, a herdr.AgentInfo) (exitKeys bool, waited, err error) {
	k, ok := m.kindSpec(m.sessionKind(*s))
	pane := store.Deref(s.HerdrPaneID)
	if !ok || len(k.ExitKeys) == 0 || pane == "" {
		waited, err := m.interrupt(ctx, s)
		return false, waited, err
	}
	conv := store.Deref(s.SessionID)
	if a.AgentSession != nil && a.AgentSession.Value != "" {
		conv = a.AgentSession.Value
	}
	if gone, err := m.idlePrompt(ctx, *s, pane, a, conv, "before its ctrl+c, no key sent"); err != nil || gone {
		return false, nil, err
	}
	if gone, err := m.ctrlC(ctx, s, 1); err != nil || gone {
		return false, nil, err
	}
	if err := m.sleep(ctx, time.Second); err != nil {
		return false, nil, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
	}
	if quit, err := m.idlePrompt(ctx, *s, pane, a, conv, "after its ctrl+c, no exit keys sent"); err != nil || quit {
		return false, nil, err
	}
	sent, waited, err := m.sendExitKeys(ctx, pane, k.ExitKeys)
	switch {
	case err != nil && !sent:
		return false, nil, fmt.Errorf("agents: quit session %d: exit keys: %w", s.ID, err)
	case err != nil:
		return false, nil, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
	}
	return waited == nil, waited, nil
}

// idlePrompt is earlyQuit's look at pane before each of its keys: quit
// when the pane is an idle shell or gone (the agent has quit, so no key is
// sent). Otherwise it fails with ErrBusy, sending nothing (when says when,
// for the error), unless herdr still has the conversation the park found
// in the pane (a's kind, conv as its agent_session value, and a's terminal
// when herdr gave one) idle or done, and then, read last, right before the
// key, the pane's visible screen shows no dialog and a bare composer
// (bareComposer). herdr's send-keys delivers unconditionally, so a change
// between this look and the key stays possible (DECISIONS "An early park
// quits an agent with its kind's exit keys").
func (m *Manager) idlePrompt(ctx context.Context, s store.Session, pane string, a herdr.AgentInfo, conv, when string) (quit bool, err error) {
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
	case herdr.IdleShell(pi):
		return true, nil
	}
	p, err := m.d.Herdr.PaneGet(ctx, pane)
	switch {
	case isGone(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("agents: quit session %d: %w", s.ID, err)
	}
	label := Role(s.Role).Label()
	switch {
	case a.Agent == "" || p.Agent != a.Agent:
		return false, fmt.Errorf("agents: quit session %d: pane %s runs no %s agent %s: %w", s.ID, pane, a.Agent, when, ErrBusy)
	case conv == "" || p.AgentSession == nil || p.AgentSession.Value != conv || (a.TerminalID != "" && p.TerminalID != a.TerminalID):
		return false, fmt.Errorf("agents: quit session %d: pane %s no longer shows %s's conversation %s: %w", s.ID, pane, label, when, ErrBusy)
	case !idleStatus(p.AgentStatus):
		return false, fmt.Errorf("agents: quit session %d: %s is %s %s: %w", s.ID, label, p.AgentStatus, when, ErrBusy)
	}
	text, err := m.readVisible(ctx, paneRef{pane: pane})
	switch {
	case err != nil: // its text alone: a read's timeout is no agent that did not quit (the PR's park stays plain)
		return false, fmt.Errorf("agents: quit session %d: %s's screen is unreadable (%s) %s: %w", s.ID, label, err.Error(), when, ErrBusy)
	case dialogShown(text):
		return false, fmt.Errorf("agents: quit session %d: %s shows a dialog %s: %w", s.ID, label, when, ErrBusy)
	case !bareComposer(text):
		return false, fmt.Errorf("agents: quit session %d: %s's composer is not bare %s: %w", s.ID, label, when, ErrBusy)
	}
	return false, nil
}

// The early park's composer (bareComposer): composerRuleLine is a rule of
// Claude Code's composer, a run of rule characters from column 0, maybe
// labelled with the session's name and closed by another run ("──── PR #12
// claude-review - talkable ─", the top rule), plainRuleLine one without a
// label (the bottom rule), each at least composerRuleMin runes long, the
// label included (a 53-column pane's top rule starts with 16 rule
// characters), so a short typed rule is content; barePrompt is its prompt
// line with nothing typed, a prompt mark alone (composerLine's marks), maybe
// between spaces, and promptStart a line that starts with one;
// composerStatusMax bounds the status lines under the bottom rule.
var (
	composerRuleLine = regexp.MustCompile(`^[─━═]{3,}(?: .* [─━═]+)?$`)
	plainRuleLine    = regexp.MustCompile(`^[─━═]+$`)
	barePrompt       = regexp.MustCompile(`^[ \t]*[>❯›]$`)
	promptStart      = regexp.MustCompile(`^\s*[>❯›]`)
)

const (
	composerRuleMin   = 20
	composerStatusMax = 6
)

// bareComposer is the early park's strict test of an empty composer before
// its keys, stricter than composerEmpty and read from the screen's
// structure alone, its lines split at LF with only their trailing
// whitespace trimmed (a CRLF's CR goes with it, any other CR stays): the
// bottom rule is the last unlabelled composer rule (plainRuleLine), under
// which at most composerStatusMax non-blank status lines are skipped, none
// of them starting with a prompt mark; the top rule is the composer rule
// (composerRuleLine) next above it; and the region between them must hold
// exactly one line, a prompt mark alone (barePrompt), as a session magnum
// prompted shows after its turn. A placeholder (which a screen read cannot
// tell from typed text), a draft, a pasted block, a continuation line of
// any kind, a typed box or rule character, an empty region or no two rules
// is not bare.
func bareComposer(text string) bool {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	long := func(l string) bool { return utf8.RuneCountInString(l) >= composerRuleMin }
	bottom, status := -1, 0
	for i := len(lines) - 1; i >= 0 && bottom < 0; i-- {
		switch l := lines[i]; {
		case long(l) && plainRuleLine.MatchString(l):
			bottom = i
		case l == "":
		case promptStart.MatchString(l):
			return false
		default:
			if status++; status > composerStatusMax {
				return false
			}
		}
	}
	top := -1
	for i := bottom - 1; i >= 0 && top < 0; i-- {
		if long(lines[i]) && composerRuleLine.MatchString(lines[i]) {
			top = i
		}
	}
	return bottom > 0 && top >= 0 && bottom-top == 2 && barePrompt.MatchString(lines[top+1])
}

// dialogShown reports whether a pane's visible text shows a dialog magnum
// knows (a trust dialog, a permission prompt, the hooks review, the model
// switch's confirmation), where an Enter would answer it.
func dialogShown(text string) bool {
	_, trust := detectTrustDialog(text)
	_, prompt := detectPermissionPrompt(text)
	_, hooks := detectHooksDialog(text)
	_, sw := detectSwitchDialog(text)
	return trust || prompt || hooks || sw
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
