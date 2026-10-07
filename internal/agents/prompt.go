package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// NewRun inserts a pending run for pr's role (a configured role name or
// alias; the run gets the name): id = NewRunID, target_sha = pr.HeadSHA,
// prev_reviewed_sha = pr.ReviewedSHA, identity = pr.Identity,
// reviewer_login from the identity config, report_path =
// Layout.ReviewDir(...)/<the role's ReportFile> (OwnFindingsFile for the
// judge's own pass, kind store.RunOwnPass), session_id = the role's live
// session when there is one. Create the run first when the prompt must
// quote its id (judge templates), then Submit.
func (m *Manager) NewRun(ctx context.Context, pr store.PR, role Role, kind string, round int) (store.Run, error) {
	spec, ok := m.roleSpec(role)
	if !ok {
		return store.Run{}, fmt.Errorf("agents: new run: unknown role %q", role)
	}
	role = Role(spec.Name)
	repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return store.Run{}, fmt.Errorf("agents: new run pr %d: %w", pr.Number, err)
	}
	login := ""
	if id := m.d.Config.IdentityByName(pr.Identity); id != nil {
		login = id.Login
	}
	if round < 1 {
		round = 1
	}
	report := spec.ReportFile()
	if kind == store.RunOwnPass {
		report = OwnFindingsFile
	}
	r := store.Run{
		PRID: pr.ID, Round: round, Role: string(role), Kind: kind, TargetSHA: pr.HeadSHA,
		PrevReviewedSHA: pr.ReviewedSHA, Identity: pr.Identity, ReviewerLogin: login, State: store.RunPending,
		ReportPath: store.Ptr(filepath.Join(m.d.Layout.ReviewDir(repo.Owner, repo.Name, pr.Number, pr.HeadSHA), report)),
	}
	if s, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, string(role)); err == nil {
		r.SessionID = &s.ID
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Run{}, fmt.Errorf("agents: new run pr %d: %w", pr.Number, err)
	}
	for attempt := 1; ; attempt++ {
		r.ID = NewRunID(m.now())
		created, err := m.d.Store.CreateRun(ctx, r)
		if err == nil {
			return created, nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt == runIDAttempts {
			return store.Run{}, fmt.Errorf("agents: new run pr %d %s: %w", pr.Number, role, err)
		}
	}
}

// runIDAttempts bounds NewRun's retries on an id collision.
const runIDAttempts = 3

// NewRunID is a run id: "r-<UTC yyyymmddThhmmss>-<6 random hex digits>",
// sortable by creation second. The judge's review carries it as its
// magnum:run marker, and a review with the marker posted by another login is
// an identity leak, so the random part keeps outsiders from guessing it.
func NewRunID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return fmt.Sprintf("r-%s-%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// LatestRound is the highest run round of a PR (0 when it has no runs).
func (m *Manager) LatestRound(ctx context.Context, prID int64) (int, error) {
	runs, err := m.d.Store.RunsByPR(ctx, prID)
	if err != nil {
		return 0, fmt.Errorf("agents: rounds of pr %d: %w", prID, err)
	}
	n := 0
	for _, r := range runs {
		n = max(n, r.Round)
	}
	return n, nil
}

// Prompt creates a run of kind (store.RunInitial, RunRereview, RunContinue,
// RunNudge, RunRecovery) in the PR's latest round (1 when none) and submits
// text to the role's live agent (see Submit). It returns the run id, also
// when Submit fails after the run was created; a run refused before sending
// (ErrNoSession, ErrHumanActive, ErrBusy) is marked abandoned. Start a new
// round, or quote the run id in the text, with NewRun + Submit.
func (m *Manager) Prompt(ctx context.Context, pr store.PR, role Role, kind, text string) (string, error) {
	if m.shellRole(role) {
		return "", fmt.Errorf("agents: prompt pr %d %s: %w", pr.Number, role, ErrNotAgent)
	}
	round, err := m.LatestRound(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	run, err := m.NewRun(ctx, pr, role, kind, round)
	if err != nil {
		return "", err
	}
	err = m.Submit(ctx, run, text)
	if errors.Is(err, ErrNoSession) || errors.Is(err, ErrHumanActive) || errors.Is(err, ErrBusy) {
		// Refused before sending: the run this call created must not linger
		// as pending (it would count as in flight).
		_ = m.d.Store.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunAbandoned, func(u *store.RunUpdate) {
			u.Set("error", err.Error())
			u.Set("ended_at", m.now())
		})
	}
	return run.ID, err
}

// Submit sends text to the live agent of run's role and records the outcome.
// The run must be pending (a run once submitted is observed, never re-sent:
// store.ErrConflict). It refuses with ErrHumanActive inside
// daemon.human_cooldown after a human typed into the session, and with
// ErrNoSession unless the role's session is live, and it holds a prompt to
// a judge still finishing the turn of its last run until herdr shows the
// agent no longer working (awaitTail; ErrBusy after tailWait); each refusal
// leaves the run pending for the caller to retry or abandon. A role whose
// newest session is lost while a fresh herdr snapshot still shows its agent
// gets that session back live (rebindLost) instead of a refusal. Before
// sending, a session on a limited model switches to a fallback, and one
// magnum switched earlier switches back once its role's model is no longer
// limited (ensureModel). The prompt waits for the agent to reach working or
// blocked (PromptAckTimeout):
//
//   - working: run working (submitted_at, working_seen_at).
//   - blocked after submission: run submitted, ErrBlocked.
//   - herdr agent_blocked (rejected before sending): run failed, ErrBlocked.
//     Within TrustWindow of the session's start and before its first
//     prompt, a first-launch trust dialog on screen is answered first
//     (AnswerTrustDialog) and the prompt re-sent once.
//   - agent_prompt_stalled: run submitted with error, ErrStalled.
//   - timeout, or ctx ending while the prompt was in flight: run submitted
//     with error (the text may have arrived), ErrTimeout or ctx's error.
//   - anything else (herdr unavailable, agent gone): run failed.
//
// Once the prompt was sent, the run and session rows are recorded even when
// ctx ends meanwhile, so a delivered prompt never stays pending.
//
// The session gets last_prompt_at, idle_ticks 0, the acked status and, once
// herdr reports it, session_id. prompt_text is stored redacted. An agent
// named by a rename command (codex) acked as working is named right away
// when its title lacks Title (nameAgent). A shell role refuses with
// ErrNotAgent (see RunShell).
func (m *Manager) Submit(ctx context.Context, run store.Run, text string) error {
	role := Role(run.Role)
	if m.shellRole(role) {
		return fmt.Errorf("agents: submit %s: %w", run.ID, ErrNotAgent)
	}
	if run.State != store.RunPending {
		return fmt.Errorf("agents: submit %s: state %s: %w", run.ID, run.State, store.ErrConflict)
	}
	sess, err := m.d.Store.LiveSessionByPRRole(ctx, run.PRID, run.Role)
	if errors.Is(err, store.ErrNotFound) {
		if s, ok := m.rebindLost(ctx, run.PRID, run.Role); ok {
			sess, err = s, nil
		}
	}
	if errors.Is(err, store.ErrNotFound) || (err == nil && sess.State != store.SessionLive) {
		return fmt.Errorf("agents: submit %s: pr %d %s: %w", run.ID, run.PRID, role, ErrNoSession)
	}
	if err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err)
	}
	if !m.isAgentSession(sess) {
		return fmt.Errorf("agents: submit %s: %w", run.ID, ErrNotAgent)
	}
	pr, err := m.d.Store.PRByID(ctx, run.PRID)
	if err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err)
	}
	now := m.now()
	if cd := m.d.Config.Daemon.HumanCooldown.Duration; pr.HumanActiveAt != nil && cd > 0 && now.Before(pr.HumanActiveAt.Add(cd)) {
		return fmt.Errorf("agents: submit %s: until %s: %w", run.ID, pr.HumanActiveAt.Add(cd).Format(time.RFC3339), ErrHumanActive)
	}
	if err := m.awaitTail(ctx, sess); err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err) // nothing sent: the run stays pending
	}
	m.ensureModel(ctx, sess)
	// Attach the run to the session before prompting, so Observe never takes
	// the agent's reaction for a human.
	if err := m.d.Store.TransitionRun(ctx, run.ID, []string{store.RunPending}, "", func(u *store.RunUpdate) {
		u.Set("session_id", sess.ID)
		u.Set("prompt_text", execx.Redact(text))
	}); err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err) // nothing sent: the run stays pending
	}

	// A Codex started with its checkout untrusted opens on its restricted
	// "Folder access", and one resumed after its hooks changed shows its
	// hooks review; either would eat the prompt: answer them first
	// (openRestricted, answerHooks).
	if kind := m.sessionKind(sess); kind == KindCodex {
		ref := paneRef{name: store.Deref(sess.AgentName), pane: store.Deref(sess.HerdrPaneID)}
		if _, err := m.openRestricted(ctx, run.PRID, role, ref); err != nil {
			m.logf("agents: submit %s: restricted folder: %v", run.ID, err)
		}
		if _, err := m.answerHooks(ctx, run.PRID, role, kind, ref, store.Deref(sess.Cwd)); err != nil {
			m.logf("agents: submit %s: hooks review: %v", run.ID, err)
		}
	}
	wait := &herdr.PromptWait{Until: []herdr.Status{herdr.StatusWorking, herdr.StatusBlocked}, Timeout: PromptAckTimeout}
	info, perr := m.d.Herdr.AgentPrompt(ctx, target(sess), text, wait)
	if perr != nil && ctx.Err() == nil && herdr.IsCode(perr, herdr.CodeAgentBlocked) && m.trustRetry(ctx, run.ID, sess) {
		// Rejected before sending: answering a first-launch trust dialog
		// makes the one re-send safe.
		info, perr = m.d.Herdr.AgentPrompt(ctx, target(sess), text, wait)
	}
	if perr != nil && ctx.Err() == nil && herdr.IsCode(perr, codeAgentNotReady) && m.waitRegistered(ctx, sess) {
		// Rejected before sending: herdr had not registered the agent yet (a
		// resumed Claude Code takes a moment to show up as a named agent).
		info, perr = m.d.Herdr.AgentPrompt(ctx, target(sess), text, wait)
	}
	// The prompt may have been delivered: record it whatever ctx does now.
	cut := ctx.Err() != nil // ctx ended while the prompt was in flight
	bctx := context.WithoutCancel(ctx)
	now = m.now()
	if perr != nil {
		mapped := mapHerdr(perr)
		state := store.RunFailed
		if errors.Is(mapped, ErrStalled) || errors.Is(mapped, ErrTimeout) || cut {
			state = store.RunSubmitted // the text may be in the agent: observe it, never re-send
		}
		if err := m.d.Store.TransitionRun(bctx, run.ID, []string{store.RunPending}, state, func(u *store.RunUpdate) {
			u.Set("error", execx.Redact(perr.Error()))
			if state == store.RunSubmitted {
				u.Set("submitted_at", now)
			} else {
				u.Set("ended_at", now)
			}
		}); err != nil {
			return errors.Join(fmt.Errorf("agents: prompt %s: %w", target(sess), mapped), err)
		}
		if state == store.RunSubmitted {
			m.promptedSession(bctx, sess, herdr.AgentInfo{}, now)
		}
		return fmt.Errorf("agents: prompt %s: %w", target(sess), mapped)
	}

	m.promptedSession(bctx, sess, info, now)
	state := store.RunSubmitted
	if info.AgentStatus == herdr.StatusWorking {
		state = store.RunWorking
	}
	if err := m.d.Store.TransitionRun(bctx, run.ID, []string{store.RunPending}, state, func(u *store.RunUpdate) {
		u.Set("submitted_at", now)
		if state == store.RunWorking {
			u.Set("working_seen_at", now)
		}
	}); err != nil {
		return fmt.Errorf("agents: submit %s: %w", run.ID, err)
	}
	if state == store.RunWorking {
		pane := info.PaneID
		if pane == "" {
			pane = store.Deref(sess.HerdrPaneID)
		}
		m.nameAgent(ctx, sess, pane, terminalTitle(info, herdr.Pane{}))
	}
	if info.AgentStatus == herdr.StatusBlocked {
		return fmt.Errorf("agents: prompt %s: acked as blocked: %w", target(sess), ErrBlocked)
	}
	return nil
}

// awaitTail holds a prompt to a judge whose agent is still finishing the
// turn of its last run (turnTail): it asks herdr (PaneGet) every tailPoll
// until the agent no longer works, so nothing is typed into that turn, and
// refuses with ErrBusy after tailWait. An agent idle, blocked or gone goes
// on to the prompt, which finds out the rest.
func (m *Manager) awaitTail(ctx context.Context, s store.Session) error {
	if !m.turnTail(ctx, s) {
		return nil
	}
	pane := store.Deref(s.HerdrPaneID)
	for poll := 0; ; poll++ {
		p, err := m.d.Herdr.PaneGet(ctx, pane)
		if err != nil || p.AgentStatus != herdr.StatusWorking {
			return nil
		}
		if time.Duration(poll)*tailPoll >= tailWait {
			return fmt.Errorf("%s still finishes its last turn after %s: %w", s.Role, tailWait, ErrBusy)
		}
		if err := m.sleep(ctx, tailPoll); err != nil {
			return err
		}
	}
}

// promptedSession records a prompt on the session row (best effort: the run
// row is the authority).
func (m *Manager) promptedSession(ctx context.Context, sess store.Session, info herdr.AgentInfo, now time.Time) {
	_ = m.d.Store.UpdateSession(ctx, sess.ID, func(u *store.SessionUpdate) {
		u.Set("last_prompt_at", now)
		u.Set("idle_ticks", 0)
		if info.AgentStatus != "" {
			u.Set("agent_status", string(info.AgentStatus))
			u.Set("agent_status_at", now)
		}
		if info.AgentSession != nil && info.AgentSession.Value != "" && info.AgentSession.Value != store.Deref(sess.SessionID) {
			u.Set("session_id", info.AgentSession.Value)
		}
	})
}

// ShellStatusUnknown is RunShell's status when the done marker carried no
// exit status (a full-line template that echoes the bare marker).
const ShellStatusUnknown = -1

// RunShell types line (see ShellLine) into the pane of a shell role and
// blocks until the done marker (DoneMarker(runID)) appears on a line of its
// own, optionally followed by the command's exit status, or timeout
// (ErrTimeout). It returns that status (ShellStatusUnknown without one).
// The pane must be an idle shell (ErrBusy otherwise); like Submit it refuses
// with ErrHumanActive inside daemon.human_cooldown after a human typed into
// the PR's panes. The shell's line editor is cleared (ctrl+u) first, so text
// a human left there never joins the command. The match is a line-anchored
// regex on unwrapped output, so the echoed command line itself never
// matches. The role's session (agent_kind "shell") becomes live with
// last_prompt_at; the run row is the caller's. An agent role is refused with
// an error.
func (m *Manager) RunShell(ctx context.Context, pr store.PR, role config.Role, paneID, line, marker string, timeout time.Duration) (int, error) {
	fail := func(err error) (int, error) {
		return ShellStatusUnknown, fmt.Errorf("agents: run %s pr %d: %w", role.Name, pr.Number, err)
	}
	if !role.IsShell() {
		return fail(fmt.Errorf("not a shell role (kind %q)", role.Kind))
	}
	if marker == "" {
		return fail(errors.New("empty marker"))
	}
	if cur, err := m.d.Store.PRByID(ctx, pr.ID); err == nil {
		if cd := m.d.Config.Daemon.HumanCooldown.Duration; cur.HumanActiveAt != nil && cd > 0 && m.now().Before(cur.HumanActiveAt.Add(cd)) {
			return fail(fmt.Errorf("until %s: %w", cur.HumanActiveAt.Add(cd).Format(time.RFC3339), ErrHumanActive))
		}
	}
	if _, err := m.d.Herdr.WaitIdleShell(ctx, paneID, IdleShellTimeout); err != nil {
		if herdr.IsTimeout(err) {
			return fail(fmt.Errorf("pane %s: %w: %w", paneID, ErrBusy, err))
		}
		return fail(fmt.Errorf("pane %s: %w", paneID, err))
	}
	if err := m.d.Herdr.PaneSendKeys(ctx, paneID, "ctrl+u"); err != nil {
		return fail(fmt.Errorf("clear the line of pane %s: %w", paneID, err))
	}
	if err := m.d.Herdr.PaneRun(ctx, paneID, line); err != nil {
		return fail(err)
	}
	if s, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, role.Name); err == nil && store.Deref(s.HerdrPaneID) == paneID {
		_ = m.d.Store.TransitionSession(ctx, s.ID, liveStates, store.SessionLive, func(u *store.SessionUpdate) {
			u.Set("last_prompt_at", m.now())
		})
	}
	match, err := m.d.Herdr.PaneWaitOutput(ctx, paneID, herdr.WaitOutputOptions{
		Match:   `(?m)^` + regexp.QuoteMeta(marker) + `(?:[ \t]+\d+)?[ \t]*$`,
		Regex:   true,
		Source:  herdr.SourceRecentUnwrapped,
		Timeout: timeout,
	})
	if err != nil {
		return fail(fmt.Errorf("wait for %s: %w", marker, mapHerdr(err)))
	}
	return doneStatus(match.MatchedLine, marker), nil
}

// doneStatus reads the exit status from a done-marker line ("<marker> 1").
func doneStatus(line, marker string) int {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), marker)
	if !ok {
		return ShellStatusUnknown
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return ShellStatusUnknown
	}
	return n
}

// rebindLost makes the role's newest session live again when it is lost but
// a fresh herdr snapshot still shows its agent (an observe tick judged it
// from a snapshot that predated the agent). It reports the session row
// after the rebind; false when there is nothing to rebind.
func (m *Manager) rebindLost(ctx context.Context, prID int64, role string) (store.Session, bool) {
	sessions, err := m.d.Store.SessionsByPR(ctx, prID)
	if err != nil {
		return store.Session{}, false
	}
	s, ok := latest(sessions, Role(role), func(store.Session) bool { return true })
	if !ok || s.State != store.SessionLost || !m.isAgentSession(s) {
		return store.Session{}, false
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return store.Session{}, false
	}
	a, found := findAgent(snap, s)
	if !found {
		return store.Session{}, false
	}
	now := m.now()
	err = m.d.Store.TransitionSession(ctx, s.ID, []string{store.SessionLost}, store.SessionLive, func(u *store.SessionUpdate) {
		if a.PaneID != "" {
			u.Set("herdr_pane_id", a.PaneID)
		}
		if a.AgentStatus != "" {
			u.Set("agent_status", string(a.AgentStatus))
			u.Set("agent_status_at", now)
		}
		u.Set("idle_ticks", 0)
	})
	if err != nil {
		m.logf("agents: rebind %s session %d: %v", role, s.ID, err)
		return store.Session{}, false
	}
	m.markedLive(s.ID, now)
	m.event(ctx, m.prSubject(ctx, prID), "info", EventRebound,
		fmt.Sprintf("%s session %d was lost but its agent is present: live again", role, s.ID),
		map[string]any{"session": s.ID, "role": role, "agent": a.Name, "pane": a.PaneID})
	fresh, err := m.d.Store.SessionByID(ctx, s.ID)
	if err != nil {
		return store.Session{}, false
	}
	return fresh, true
}
