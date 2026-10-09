package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Trust-dialog fallback timing.
const (
	// TrustWindow: the fallback answers a trust dialog only this soon after
	// the agent started (it is a first-launch dialog), and only before the
	// session was ever prompted.
	TrustWindow = 90 * time.Second
	// TrustReadyTimeout bounds the wait for the agent to be idle after the
	// dialog was answered.
	TrustReadyTimeout = 60 * time.Second

	trustPoll       = 2 * time.Second        // readiness poll interval
	trustKeyDelay   = 300 * time.Millisecond // before re-reading the dialog after a cursor move
	trustKeyRereads = 5
)

// EventTrustDialogAnswered is the event kind recorded (subject
// "pr:<owner>/<name>#<N>") each time a trust dialog is answered.
const EventTrustDialogAnswered = "agents.trust_dialog_answered"

// herdr's agent.start error for an agent blocked during startup (the name
// stays usable for agent.read / agent.send_keys).
const codeAgentNotReady = "agent_not_ready"

// herdr's agent.start error when the pane is not an available shell yet; the
// idle-shell check can pass a moment before herdr agrees, so StartAgent
// retries the same start (resume included) startBusyRetries times.
const (
	codeAgentPaneBusy = "agent_pane_busy"
	startBusyRetries  = 5
	startBusyDelay    = 2 * time.Second
)

// trustSpec is one of the folder dialogs magnum answers, as Codex 0.160 and
// Claude Code render them: the title on a line of its own, then (codex's
// first-launch dialog) the question on a line of its own, then the two
// options on adjacent lines, with nothing below them but blank lines and the
// key hints.
type trustSpec struct {
	kind           string
	title          string
	question       string // "" = none
	accept, reject string // the option texts
	// restricted: Codex's "Folder access" for a folder the session treats
	// as untrusted (the -c projects table magnum passes when the PR changes
	// .codex/, checkoutProject): "Open restricted" loads none of the folder's
	// .codex/ and saves no trust (tui/src/onboarding/trust_directory.rs,
	// onboarding_screen.rs), so magnum takes it at any time. "Open existing
	// task" (a resumed task on Codex's shared daemon, which may keep what it
	// loaded while trusted) is not this dialog and is left for the human.
	restricted bool
}

var trustSpecs = []trustSpec{
	{kind: KindCodex, title: "Folder access", question: "Trust this folder?", accept: "Trust and continue", reject: "Quit"},
	{kind: KindCodex, title: "Folder access", accept: "Open restricted", reject: "Quit", restricted: true},
	{kind: KindCodex, title: "Folder access", accept: "Open restricted", reject: "Back to Agent Command Center", restricted: true},
	{kind: KindClaude, title: "Accessing workspace:", accept: "Yes, I trust this folder", reject: "No, exit"},
}

// trustOptionLine splits an option line: indentation, an optional cursor
// mark (group 1), an optional "N." number, the option's text (group 2).
var trustOptionLine = regexp.MustCompile(`^[ \t]*([❯›>▸▶])?[ \t]*(?:\d+[.)][ \t]*)?(.*?)[ \t]*$`)

// trustHints matches the key-hint line under the options ("enter continue ·
// esc quit", "Enter to confirm · Esc to cancel").
var trustHints = regexp.MustCompile(`(?i)^[ \t]*enter\b.*\besc\b`)

// trustDialog is a trust dialog found on screen.
type trustDialog struct {
	kind           string // KindCodex | KindClaude
	acceptSelected bool   // the cursor is on the trust option
	rejectSelected bool   // the cursor is on the quit/exit option
	acceptBelow    bool   // the trust option is listed after the other one
	restricted     bool   // Codex's "Open restricted" dialog (trustSpec.restricted)
}

// answerable: exactly one of the dialog's options carries the cursor, so
// the keys to press are known.
func (d trustDialog) answerable() bool { return d.acceptSelected != d.rejectSelected }

// detectTrustDialog finds a Codex or Claude first-launch trust dialog in a
// pane's visible text: the whole dialog shape (see trustSpec), never text
// that merely quotes it, and nothing when the screen also shows an approval
// dialog (blockedRules).
func detectTrustDialog(text string) (trustDialog, bool) {
	for _, r := range blockedRules {
		if r.re.MatchString(text) {
			return trustDialog{}, false
		}
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for _, sp := range trustSpecs {
		if d, ok := sp.match(lines); ok {
			return d, true
		}
	}
	return trustDialog{}, false
}

// match finds sp's dialog in screen lines (the last title on screen).
func (sp trustSpec) match(lines []string) (trustDialog, bool) {
	i := -1
	for j, l := range lines {
		if strings.TrimSpace(l) == sp.title {
			i = j
		}
	}
	if i < 0 {
		return trustDialog{}, false
	}
	i++
	if sp.question != "" {
		for i < len(lines) && strings.TrimSpace(lines[i]) != sp.question {
			i++
		}
		if i == len(lines) {
			return trustDialog{}, false
		}
		i++
	}
	d := trustDialog{kind: sp.kind, restricted: sp.restricted}
	accept, reject := -1, -1
	for ; i < len(lines); i++ {
		m := trustOptionLine.FindStringSubmatch(lines[i])
		switch {
		case m == nil:
		case m[2] == sp.accept && accept < 0:
			accept, d.acceptSelected = i, m[1] != ""
		case m[2] == sp.reject && reject < 0:
			reject, d.rejectSelected = i, m[1] != ""
		}
	}
	if accept < 0 || reject < 0 || max(accept, reject)-min(accept, reject) != 1 {
		return trustDialog{}, false
	}
	for _, l := range lines[max(accept, reject)+1:] {
		if strings.TrimSpace(l) != "" && !trustHints.MatchString(l) {
			return trustDialog{}, false
		}
	}
	d.acceptBelow = accept > reject
	return d, true
}

// paneRef addresses an agent's pane: by agent name, else by pane id.
type paneRef struct{ name, pane string }

func (r paneRef) String() string {
	if r.name != "" {
		return r.name
	}
	return r.pane
}

// readVisible returns the pane's rendered viewport (where a dialog is).
func (m *Manager) readVisible(ctx context.Context, ref paneRef) (string, error) {
	opts := herdr.ReadOptions{Source: herdr.SourceVisible}
	var agentErr error
	if ref.name != "" {
		r, err := m.d.Herdr.AgentRead(ctx, ref.name, opts)
		if err == nil {
			return r.Text, nil
		}
		agentErr = err
	}
	if ref.pane == "" {
		return "", fmt.Errorf("agents: read %s: %w", ref, agentErr)
	}
	r, err := m.d.Herdr.PaneRead(ctx, ref.pane, opts)
	if err != nil {
		return "", fmt.Errorf("agents: read pane %s: %w", ref.pane, err)
	}
	return r.Text, nil
}

// sendKeys sends key presses to an agent: by name, else to its pane. The
// pane is the fallback only when herdr does not know the name
// (agent_not_found): after a timeout or any other error the keys may have
// arrived already, and sending them again would press them twice.
func (m *Manager) sendKeys(ctx context.Context, ref paneRef, keys ...string) error {
	if ref.name != "" {
		err := m.d.Herdr.AgentSendKeys(ctx, ref.name, keys...)
		if err == nil || ref.pane == "" || !herdr.IsCode(err, herdr.CodeAgentNotFound) {
			return err
		}
	}
	return m.d.Herdr.PaneSendKeys(ctx, ref.pane, keys...)
}

// trustGate says whether the trust-dialog fallback may act for an agent: it
// started at since (zero = unknown) and was prompted or not.
type trustGate struct {
	since    time.Time
	prompted bool
}

// sessionGate is the gate of a session row: its start (StartAgent resets it
// when it launches the agent) and whether it was ever prompted.
func sessionGate(s store.Session) trustGate {
	return trustGate{since: s.StartedAt, prompted: s.LastPromptAt != nil}
}

// open reports whether the gate lets the fallback act now: within
// TrustWindow of the agent's start and before any prompt.
func (g trustGate) open(now time.Time) bool {
	return !g.prompted && !g.since.IsZero() && now.Sub(g.since) < TrustWindow
}

// answerTrust answers the first-launch trust dialog of the agent kind
// running role when the gate is open and the pane shows the whole dialog
// (detectTrustDialog) with the cursor on one of its two options: Enter when
// the cursor is on the trust option, else one Up/Down toward it, a re-read
// confirming the cursor got there, then Enter. Codex's restricted "Folder
// access" (trustSpec.restricted) is answered "Open restricted" the same
// way whatever the gate: it grants nothing. Only the codex and claude
// dialogs are known; for any other kind (droid, omp, ...) nothing is
// answered. Anything else on screen is left alone (false, nil). It records
// EventTrustDialogAnswered.
func (m *Manager) answerTrust(ctx context.Context, prID int64, role Role, kind string, ref paneRef, gate trustGate) (bool, error) {
	if kind != KindCodex && kind != KindClaude {
		return false, nil
	}
	open := gate.open(m.now())
	if !open && kind != KindCodex {
		return false, nil
	}
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		return false, err
	}
	d, ok := detectTrustDialog(text)
	if !ok || d.kind != kind || !d.answerable() || !open && !d.restricted {
		return false, nil
	}
	var keys []string
	if d.rejectSelected {
		move := "up"
		if d.acceptBelow {
			move = "down"
		}
		if err := m.sendKeys(ctx, ref, move); err != nil {
			return false, fmt.Errorf("agents: %s trust dialog in %s: %w", d.kind, ref, err)
		}
		keys = append(keys, move)
		if !m.trustCursorOnAccept(ctx, ref, d) {
			return false, fmt.Errorf("agents: %s trust dialog in %s: the cursor did not reach the trust option; not confirming", d.kind, ref)
		}
	}
	if err := m.sendKeys(ctx, ref, "enter"); err != nil {
		return false, fmt.Errorf("agents: %s trust dialog in %s: %w", d.kind, ref, err)
	}
	keys = append(keys, "enter")
	m.trustAnswered(ctx, prID, role, ref, d, keys)
	return true, nil
}

// trustCursorOnAccept re-reads the dialog until the cursor shows on the trust
// option of the same dialog (a restricted one stays restricted).
func (m *Manager) trustCursorOnAccept(ctx context.Context, ref paneRef, was trustDialog) bool {
	for range trustKeyRereads {
		if m.sleep(ctx, trustKeyDelay) != nil {
			return false
		}
		text, err := m.readVisible(ctx, ref)
		if err != nil {
			continue
		}
		if d, ok := detectTrustDialog(text); ok && d.kind == was.kind && d.restricted == was.restricted && d.answerable() && d.acceptSelected {
			return true
		}
	}
	return false
}

// trustAnswered logs and records the answered dialog (best effort).
func (m *Manager) trustAnswered(ctx context.Context, prID int64, role Role, ref paneRef, d trustDialog, keys []string) {
	kind := d.kind
	msg := fmt.Sprintf("answered the %s folder-trust dialog of %s in %s (%s)", kind, role, ref, strings.Join(keys, ", "))
	if d.restricted {
		msg = fmt.Sprintf("answered the %s \"Folder access\" of %s in %s with \"Open restricted\": the session treats the checkout as "+
			"untrusted and loads none of its .codex/ (%s)", kind, role, ref, strings.Join(keys, ", "))
	}
	m.event(ctx, m.prSubject(ctx, prID), "info", EventTrustDialogAnswered, msg,
		map[string]any{"role": string(role), "kind": kind, "agent": ref.name, "pane": ref.pane, "keys": keys, "restricted": d.restricted})
}

// prSubject is the audit subject of a PR: "pr:<owner>/<name>#<N>", or
// "pr:<id>" when it cannot be looked up.
func (m *Manager) prSubject(ctx context.Context, prID int64) string {
	ctx = context.WithoutCancel(ctx)
	pr, err := m.d.Store.PRByID(ctx, prID)
	if err != nil {
		return fmt.Sprintf("pr:%d", prID)
	}
	repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return fmt.Sprintf("pr:%d", prID)
	}
	return fmt.Sprintf("pr:%s/%s#%d", repo.Owner, repo.Name, pr.Number)
}

// waitTrustReady polls herdr (up to TrustReadyTimeout) until the agent at
// ref is idle or done.
func (m *Manager) waitTrustReady(ctx context.Context, ref paneRef) (herdr.AgentInfo, bool) {
	probe := store.Session{AgentName: nonEmpty(ref.name), HerdrPaneID: nonEmpty(ref.pane)}
	for range int(TrustReadyTimeout / trustPoll) {
		if m.sleep(ctx, trustPoll) != nil {
			return herdr.AgentInfo{}, false
		}
		snap, err := m.d.Herdr.Snapshot(ctx)
		if err != nil {
			continue
		}
		if a, ok := findAgent(snap, probe); ok && (a.AgentStatus == herdr.StatusIdle || a.AgentStatus == herdr.StatusDone) {
			return a, true
		}
	}
	return herdr.AgentInfo{}, false
}

// waitRegistered waits (at most TrustReadyTimeout) until herdr lists sess's
// agent by its name, idle: a prompt herdr rejected as agent_not_ready may
// then be sent again.
func (m *Manager) waitRegistered(ctx context.Context, sess store.Session) bool {
	name := store.Deref(sess.AgentName)
	if name == "" {
		return false
	}
	for range int(TrustReadyTimeout / trustPoll) {
		if m.sleep(ctx, trustPoll) != nil {
			return false
		}
		snap, err := m.d.Herdr.Snapshot(ctx)
		if err != nil {
			continue
		}
		if a, ok := snap.AgentByName(name); ok && (a.AgentStatus == herdr.StatusIdle || a.AgentStatus == herdr.StatusDone) {
			return true
		}
	}
	return false
}

// AnswerTrustDialog is the fallback for an agent stopped at its CLI's
// first-launch folder-trust dialog (EnsureTrust normally prevents it). It
// acts only within TrustWindow of the session's start (StartedAt) and before
// the session was ever prompted (LastPromptAt), and only when the pane's
// visible screen shows the whole dialog of the session's agent kind (see
// detectTrustDialog) with the cursor on one of its options: Codex ("Folder
// access", "Trust this folder?", "Trust and continue" / "Quit"): Enter;
// Claude ("Accessing workspace:", "No, exit" / "Yes, I trust this folder"):
// Down (or Up) to the trust option, confirmed by a re-read, then Enter. It
// records an EventTrustDialogAnswered event and waits up to
// TrustReadyTimeout for the agent to be idle. It reports whether it
// answered; any other dialog or screen, a shell role and any kind other than
// codex and claude (whose trust handling is unknown) are left untouched
// (false, nil).
func (m *Manager) AnswerTrustDialog(ctx context.Context, s store.Session) (bool, error) {
	if !m.isAgentSession(s) {
		return false, nil
	}
	ref := paneRef{name: store.Deref(s.AgentName), pane: store.Deref(s.HerdrPaneID)}
	ok, err := m.answerTrust(ctx, s.PRID, Role(s.Role), m.sessionKind(s), ref, sessionGate(s))
	if !ok {
		return false, err
	}
	if _, ready := m.waitTrustReady(ctx, ref); !ready {
		m.logf("agents: %s is not idle %s after its trust dialog was answered", ref, TrustReadyTimeout)
	}
	return true, nil
}

// openRestricted answers Codex's restricted "Folder access" on ref's screen
// with "Open restricted" (answerTrust with a closed gate: no other dialog)
// and waits for the agent to be idle.
func (m *Manager) openRestricted(ctx context.Context, prID int64, role Role, ref paneRef) (bool, error) {
	ok, err := m.answerTrust(ctx, prID, role, KindCodex, ref, trustGate{})
	if !ok {
		return false, err
	}
	if _, ready := m.waitTrustReady(ctx, ref); !ready {
		m.logf("agents: %s is not idle %s after it opened restricted", ref, TrustReadyTimeout)
	}
	return true, nil
}

// trustRetry answers a trust dialog in run's session (AnswerTrustDialog),
// or a Codex hooks review (answerHooks), and reports whether the run's
// rejected prompt may be sent again.
func (m *Manager) trustRetry(ctx context.Context, runID string, sess store.Session) bool {
	ok, err := m.AnswerTrustDialog(ctx, sess)
	if err != nil {
		m.logErr(ctx, err, "agents: run %s: trust dialog fallback: %v", runID, err)
	}
	if kind := m.sessionKind(sess); !ok && kind == KindCodex {
		ref := paneRef{name: store.Deref(sess.AgentName), pane: store.Deref(sess.HerdrPaneID)}
		if ok, err = m.answerHooks(ctx, sess.PRID, Role(sess.Role), kind, ref, store.Deref(sess.Cwd)); err != nil {
			m.logErr(ctx, err, "agents: run %s: hooks review: %v", runID, err)
		}
	}
	return ok
}

// startAfterTrustDialog answers a trust dialog (or a Codex hooks review)
// that stopped a starting agent in checkout dir and returns the agent once
// it is idle.
func (m *Manager) startAfterTrustDialog(ctx context.Context, prID int64, role Role, kind string, ref paneRef, dir string, gate trustGate) (herdr.AgentInfo, bool) {
	ok, err := m.answerTrust(ctx, prID, role, kind, ref, gate)
	if err != nil {
		m.logErr(ctx, err, "agents: start %s: trust dialog fallback: %v", ref, err)
	}
	if kind == KindCodex { // a resumed or fresh Codex may stop at its hooks review (after the trust dialog, too)
		answered, err := m.answerHooks(ctx, prID, role, kind, ref, dir)
		if err != nil {
			m.logErr(ctx, err, "agents: start %s: hooks review: %v", ref, err)
		}
		ok = ok || answered
	}
	if !ok {
		return herdr.AgentInfo{}, false
	}
	a, ready := m.waitTrustReady(ctx, ref)
	if !ready {
		m.logf("agents: start %s: not idle %s after its trust dialog was answered", ref, TrustReadyTimeout)
	}
	return a, ready
}
