package agents

// A Codex session shows its startup dialogs (the hooks review when hooks it
// has not trusted are new or changed, the folder trust) in place of its
// composer, and a prompt typed into one of them is lost: on 2026-10-22,
// after herdr's integration update and then the operator rewrote
// ~/.codex/hooks.json, six new judges stopped at the hooks review, magnum
// answered it right before their first prompt, waited only for herdr's idle
// (which herdr reported with the dialog on screen too) and typed the
// prompt; Codex never started a turn (no rollout of the session's TUI
// exists), herdr kept the agent idle, and two idle ticks ended the runs, so
// three own passes failed "finished without writing judge-own.md" and
// talkable#12071 parked in needs_attention. So the first prompt of a Codex
// session waits until its screen shows the composer and no startup dialog
// for CodexPromptSettle, answering the dialogs it finds first
// (codexPromptReady), and after a first prompt herdr did not see Codex take
// magnum checks that it did (codexPromptTaken): a prompt a startup dialog
// ate is sent again once, after the dialog is answered, and a second loss
// fails the run with ErrNotTaken.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// CodexPromptSettle is how long a Codex session's screen must show its
	// composer with no startup dialog before its first prompt goes out:
	// Codex may draw its composer and show its hooks review a moment later
	// (the tests' fake shows it 2 s after the composer), and a dialog that
	// comes while the wait runs is answered and the wait starts again.
	CodexPromptSettle = 3 * time.Second
	// CodexReadyTimeout bounds that wait: a screen magnum cannot read, or
	// one that shows neither the composer nor a dialog it knows, gets the
	// prompt after it as before (logged), and the check after the prompt
	// (codexPromptTaken) still looks for a dialog that ate it.
	CodexReadyTimeout = 30 * time.Second
	// PromptTakenWait bounds the check that Codex took a first prompt herdr
	// did not see it take (agent_prompt_stalled).
	PromptTakenWait = 10 * time.Second

	codexReadyPoll = time.Second // between screen reads of the wait and of the check
	// codexReadyAnswers bounds the startup dialogs one wait answers (a
	// dialog that comes back after its answer is not answered forever).
	codexReadyAnswers = 3
	// composerScan is how many non-blank lines from the bottom of a screen
	// are searched for the composer (composerEmpty's).
	composerScan = 8
)

// EventPromptRetried is recorded (warn, the PR's subject) each time magnum
// sends a Codex session's first prompt again because a startup dialog ate
// it (data: role, run, agent, pane, dialog).
const EventPromptRetried = "agents.prompt_retried"

// optionText is the text of a dialog's option line after its cursor mark
// ("1. Review hooks"), which composerLine's mark also starts.
var optionText = regexp.MustCompile(`^\d+[.)]\s`)

// codexComposer reports whether a Codex screen shows its composer: the
// last line marked like an input line (composerLine) among the last
// composerScan non-blank lines, which is no dialog's option line (a
// dialog's cursor carries the same mark). What it holds does not matter.
func codexComposer(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < composerScan; i-- {
		t := stripBox(lines[i])
		if t == "" {
			continue
		}
		seen++
		if m := composerLine.FindStringSubmatch(t); m != nil {
			return !optionText.MatchString(strings.TrimSpace(m[1]))
		}
	}
	return false
}

// startupDialog names the Codex startup dialog on a screen: the hooks review
// or a folder-access dialog (the first-launch trust or the restricted one);
// "" when there is none.
func startupDialog(text string) string {
	if _, ok := detectHooksDialog(text); ok {
		return "the hooks review"
	}
	if d, ok := detectTrustDialog(text); ok && d.kind == KindCodex {
		return "the folder access dialog"
	}
	return ""
}

// codexPromptReady holds the first prompt of Codex session sess (its row
// before the prompt) until the screen at ref has shown the composer and
// no dialog (codexComposer, dialogShown) on reads spanning
// CodexPromptSettle, one every codexReadyPoll. A startup dialog found on
// the way is answered by the rules that answer it elsewhere (answerTrust
// with the session's gate, which also opens a restricted folder at any
// time, then answerHooks), at most codexReadyAnswers times, and the wait
// starts again. After CodexReadyTimeout, or when ctx ends, it gives up and
// the prompt goes out as before; that is logged.
func (m *Manager) codexPromptReady(ctx context.Context, run store.Run, sess store.Session, ref paneRef) {
	settle := int(CodexPromptSettle / codexReadyPoll)
	ready, answers := 0, 0 // ready: consecutive reads that showed the composer and no dialog
	for range int(CodexReadyTimeout / codexReadyPoll) {
		text, err := m.readVisible(ctx, ref)
		switch {
		case err != nil:
			ready = 0
		case startupDialog(text) != "":
			ready = 0
			if answers < codexReadyAnswers {
				answers++
				if m.answerStartup(ctx, run, sess, ref) {
					continue // the answer waited for the agent; read again at once
				}
			}
		case !dialogShown(text) && codexComposer(text):
			if ready++; ready > settle {
				return
			}
		default:
			ready = 0
		}
		if m.sleep(ctx, codexReadyPoll) != nil {
			return
		}
	}
	m.logAt(slog.LevelWarn, "agents: submit %s: %s's screen did not show its composer with no startup dialog for %s within %s; "+
		"sending the first prompt anyway", run.ID, sess.Role, CodexPromptSettle, CodexReadyTimeout)
}

// answerStartup answers the Codex startup dialog on ref's screen, as
// AnswerTrustDialog and answerHooks do, and reports whether it answered one.
func (m *Manager) answerStartup(ctx context.Context, run store.Run, sess store.Session, ref paneRef) bool {
	role := Role(sess.Role)
	ok, err := m.answerTrust(ctx, sess.PRID, role, KindCodex, ref, sessionGate(sess))
	if err != nil {
		m.logErr(ctx, err, "agents: submit %s: folder access: %v", run.ID, err)
	}
	if ok {
		if _, ready := m.waitTrustReady(ctx, ref); !ready {
			m.logf("agents: %s is not idle %s after its folder access was answered", ref, TrustReadyTimeout)
		}
		return true
	}
	ok, err = m.answerHooks(ctx, sess.PRID, role, KindCodex, ref, store.Deref(sess.Cwd))
	if err != nil {
		m.logErr(ctx, err, "agents: submit %s: hooks review: %v", run.ID, err)
	}
	return ok
}

// codexPromptTaken checks, for up to PromptTakenWait, that Codex session
// sess took the first prompt of run, which herdr did not see it take:
// herdr shows the agent working or blocked (with no startup dialog on its
// screen), its rollout holds a user message since the run's creation
// (codexPromptLogged), or its screen shows the composer free of the
// prompt (composerEmpty) and no dialog. A startup dialog on the screen
// ends the check at once: not taken, and dialog names it. Not taken with
// no dialog ("" after the bound) is no evidence either way.
func (m *Manager) codexPromptTaken(ctx context.Context, run store.Run, sess store.Session, ref paneRef) (taken bool, dialog string) {
	for poll := 0; ; poll++ {
		text, err := m.readVisible(ctx, ref)
		if err == nil {
			if d := startupDialog(text); d != "" {
				return false, d
			}
		}
		sid := store.Deref(sess.SessionID)
		if snap, serr := m.d.Herdr.Snapshot(ctx); serr == nil {
			if a, ok := findAgent(snap, sess); ok {
				if a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked {
					return true, ""
				}
				if a.AgentSession != nil && a.AgentSession.Value != "" {
					sid = a.AgentSession.Value
				}
			}
		}
		if m.codexPromptLogged(ctx, sess, sid, run.CreatedAt.Add(-transcriptSkew)) {
			return true, ""
		}
		if err == nil && !dialogShown(text) && codexComposer(text) && composerEmpty(text) {
			return true, ""
		}
		if time.Duration(poll+1)*codexReadyPoll > PromptTakenWait || m.sleep(ctx, codexReadyPoll) != nil {
			return false, ""
		}
	}
}

// codexPromptLogged reports whether the rollout of Codex session s holds a
// user message stamped at or after since: the rollout of sid (herdr's id or
// the row's), else, for a session herdr gave no id, the one rollout of its
// checkout and start (cliRolloutIDs, recoverSessionID's rule). A rollout it
// cannot find or read is no.
func (m *Manager) codexPromptLogged(ctx context.Context, s store.Session, sid string, since time.Time) bool {
	if sid == "" {
		ids, _ := m.cliRolloutIDs(ctx, s)
		if len(ids) != 1 {
			return false
		}
		sid = ids[0]
	}
	path, err := m.rolloutPath(s, sid)
	if err != nil {
		return false
	}
	ok, err := userMessageSince(path, since)
	if err != nil {
		m.logErr(ctx, err, "agents: %s: read the Codex rollout for its first prompt: %v", s.Role, err)
	}
	return ok
}

// userMessageSince reports whether the end of the rollout at path (its last
// rolloutTail bytes) holds a user_message event stamped at or after since.
func userMessageSince(path string, since time.Time) (bool, error) {
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
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"user_message"`)) {
			continue // most lines: no JSON decoding
		}
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) == nil && ev.Payload.Type == "user_message" && !ev.Timestamp.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

// codexRetry sends the first prompt of run again, once, when Codex session
// sess did not take it because a startup dialog ate it: perr is the stall
// herdr reported. Without a dialog on screen (codexPromptTaken) the prompt
// stays as herdr left it (perr, the run observed as before). With one, it
// records EventPromptRetried, waits for the composer again
// (codexPromptReady, which answers the dialog) and sends the prompt once
// more; a second stall that the check does not find taken fails the run
// with ErrNotTaken.
func (m *Manager) codexRetry(ctx context.Context, run store.Run, sess store.Session, ref paneRef, text string, wait *herdr.PromptWait,
	perr error) (herdr.AgentInfo, error) {
	taken, dialog := m.codexPromptTaken(ctx, run, sess, ref)
	if taken || dialog == "" || ctx.Err() != nil {
		return herdr.AgentInfo{}, perr
	}
	m.event(ctx, m.prSubject(ctx, run.PRID), "warn", EventPromptRetried,
		fmt.Sprintf("%s in %s did not take the first prompt of run %s: %s was on screen; answering it and sending the prompt again",
			sess.Role, ref, run.ID, dialog),
		map[string]any{"role": sess.Role, "run": run.ID, "agent": ref.name, "pane": ref.pane, "dialog": dialog})
	m.codexPromptReady(ctx, run, sess, ref)
	if ctx.Err() != nil {
		return herdr.AgentInfo{}, perr // nothing more is sent
	}
	info, err := m.d.Herdr.AgentPrompt(ctx, target(sess), text, wait)
	if err == nil || ctx.Err() != nil || !herdr.IsCode(err, herdr.CodeAgentPromptStalled) {
		return info, err
	}
	if taken, _ := m.codexPromptTaken(ctx, run, sess, ref); taken {
		return info, err
	}
	return info, fmt.Errorf("%s did not take the prompt (%w)", KindName(KindCodex), ErrNotTaken)
}
