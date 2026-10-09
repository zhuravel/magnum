package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// Permission-prompt events, recorded on the PR subject ("pr:<owner>/<name>#<N>").
const (
	// EventPromptDenied: magnum answered No to an agent's permission prompt.
	EventPromptDenied = "agent.prompt_denied"
	// EventPromptDeniedLimit: a run reached MaxPromptDenies; magnum stopped
	// answering and the agent stays blocked for the human.
	EventPromptDeniedLimit = "agent.prompt_denied_limit"
	// EventDenyContinued: an agent that went idle after a deny was sent its
	// kind's after_deny_prompt (see continueAfterDeny).
	EventDenyContinued = "agent.deny_continued"
)

// MaxPromptDenies caps the permission prompts magnum denies, plus the
// after-deny continuations it sends, per run: an agent that keeps asking is
// misbehaving and is left blocked.
const MaxPromptDenies = 10

// denyKeyDelay is the pause before re-reading a prompt after a key press.
const denyKeyDelay = 300 * time.Millisecond

// permOption is one numbered option of a permission prompt.
type permOption struct {
	num      string // "1".."9"
	label    string // the option text, wrapped lines joined
	selected bool   // the cursor mark is on this option
}

// permPrompt is a permission (approval) prompt found on screen.
type permPrompt struct {
	header   string // the dialog's title line (Claude "Bash command"), else the question
	question string
	options  []permOption // numbered prompts; nil for an inline (y/n) question
	deny     int          // index in options of the first "No…" option
	key      string       // identity of this prompt: hash of its title, body and options
}

// cursor is the index of the option carrying the cursor (-1 = none).
func (p permPrompt) cursor() int {
	for i, o := range p.options {
		if o.selected {
			return i
		}
	}
	return -1
}

var (
	// permQuestion is the question line of an approval prompt (box borders
	// trimmed): Claude Code's "Do you want to proceed?" family, Codex's
	// "Would you like to run the following command?" family, "Allow …?".
	permQuestion = regexp.MustCompile(`(?i)^(?:do you want to (?:proceed|make this edit|create|overwrite|allow|run|apply)\b|would you like to (?:run the following command|make the following edits|allow)\b|allow\b)[^?]*\?$`)
	// permYesNo is an inline question with its (y/n) or [y/N] choice.
	permYesNo = regexp.MustCompile(`(?i)^(.*\?)\s*[(\[]\s*y\s*/\s*n\s*[)\]]$`)
	// permOptionLine is a numbered option: cursor mark (group 1), number
	// (group 2), text (group 3).
	permOptionLine = regexp.MustCompile(`^([❯›>▸▶])?\s*([1-9])[.)]\s+(\S.*)$`)
	// permHint is a key-hint line under the options ("Esc to cancel · Tab to
	// amend", "Press enter to confirm or esc to cancel").
	permHint = regexp.MustCompile(`(?i)^(?:press\s+)?(?:enter|esc|tab|ctrl\+\S+|shift\+\S+)\b`)
	permYes  = regexp.MustCompile(`(?i)^yes\b`)
	permNo   = regexp.MustCompile(`(?i)^no\b`)
	// permTrust keeps first-launch trust questions out (AnswerTrustDialog's).
	permTrust = regexp.MustCompile(`(?i)\btrust\b|\bthis folder\b`)
)

// boxChars are the border and rule characters trimmed from screen lines.
const boxChars = " \t│┃║╭╮╰╯┌┐└┘─━═"

func stripBox(line string) string { return strings.Trim(line, boxChars) }

// isRule reports whether a line is a horizontal rule (a dialog's top border).
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && stripBox(t) == "" && strings.ContainsAny(t, "─━═")
}

// leftIndent is the column (in runes) where a line's text starts, after a
// left box border.
func leftIndent(line string) int {
	t := strings.TrimLeft(line, " \t")
	n := utf8.RuneCountInString(line) - utf8.RuneCountInString(t)
	if r, size := utf8.DecodeRuneInString(t); strings.ContainsRune("│┃║", r) {
		t = t[size:]
		n++
	}
	return n + utf8.RuneCountInString(t) - utf8.RuneCountInString(strings.TrimLeft(t, " \t"))
}

// detectPermissionPrompt finds a live permission prompt at the bottom of a
// pane's visible text: the last question line matching permQuestion
// (never a trust question), then either its own (y/n) choice or numbered
// options (one carries the cursor, at least one "Yes…" and one "No…"), with
// nothing below them but blank, rule and key-hint lines. Text that merely
// quotes the question, or a quoted prompt the agent's output continues
// under, is not a prompt.
func detectPermissionPrompt(text string) (permPrompt, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	q, yesNo := -1, false
	var question string
	for i := len(lines) - 1; i >= 0 && q < 0; i-- {
		l := stripBox(lines[i])
		if m := permYesNo.FindStringSubmatch(l); m != nil && permQuestion.MatchString(strings.TrimSpace(m[1])) {
			q, yesNo, question = i, true, l
		} else if permQuestion.MatchString(l) {
			q, question = i, l
		}
	}
	if q < 0 || permTrust.MatchString(question) {
		return permPrompt{}, false
	}
	p := permPrompt{question: question, deny: -1}
	header, start := promptHeader(lines, q)
	if p.header = header; header == "" {
		p.header = question
	}
	end := q // last line of the prompt proper
	if !yesNo {
		var ok bool
		if p.options, end, ok = promptOptions(lines, q); !ok {
			return permPrompt{}, false
		}
		yes := false
		for i, o := range p.options {
			yes = yes || permYes.MatchString(o.label)
			if p.deny < 0 && permNo.MatchString(o.label) {
				p.deny = i
			}
		}
		if !yes || p.deny < 0 || p.cursor() < 0 {
			return permPrompt{}, false
		}
	}
	for _, l := range lines[end+1:] {
		if t := stripBox(l); t != "" && !permHint.MatchString(t) {
			return permPrompt{}, false
		}
	}
	p.key = promptKey(lines[start : end+1])
	return p, true
}

// promptKey identifies a prompt by its lines from title to last option,
// cursor marks left out (the cursor moves; the prompt stays the same).
func promptKey(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		t := stripBox(l)
		if m := permOptionLine.FindStringSubmatch(t); m != nil {
			t = m[2] + ". " + m[3]
		}
		h.Write([]byte(t + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// promptHeader is the title of the dialog whose question is lines[q]: the
// first non-blank line under the nearest rule above it (Claude Code draws
// one over its permission dialog, whose body is at most a dozen lines),
// with its index; "" when no rule is that close.
func promptHeader(lines []string, q int) (string, int) {
	for i := q - 1; i >= max(0, q-16); i-- {
		if !isRule(lines[i]) {
			continue
		}
		for j := i + 1; j < q; j++ {
			if t := stripBox(lines[j]); t != "" {
				return t, j
			}
		}
		return "", q
	}
	return "", q
}

// promptOptions reads the numbered options after the question at lines[q]:
// the first option line below it starts the block; a non-blank line indented
// at least to its option's text continues that option (wrapped text, at most
// two lines each); the block ends at a blank line, a key hint or anything
// else. Numbers must run 1, 2, …; ok needs two options or more.
func promptOptions(lines []string, q int) (opts []permOption, end int, ok bool) {
	i := q + 1
	for i < len(lines) && !permOptionLine.MatchString(stripBox(lines[i])) {
		i++
	}
	col, wraps := 0, 0
	for ; i < len(lines); i++ {
		t := stripBox(lines[i])
		if m := permOptionLine.FindStringSubmatch(t); m != nil {
			if m[2] != strconv.Itoa(len(opts)+1) {
				return nil, 0, false
			}
			opts = append(opts, permOption{num: m[2], label: strings.TrimSpace(m[3]), selected: m[1] != ""})
			col = leftIndent(lines[i]) + utf8.RuneCountInString(t) - utf8.RuneCountInString(m[3])
			end, wraps = i, 0
			continue
		}
		if t == "" || permHint.MatchString(t) || len(opts) == 0 || wraps == 2 || leftIndent(lines[i]) < col {
			break
		}
		opts[len(opts)-1].label += " " + t
		end, wraps = i, wraps+1
	}
	return opts, end, len(opts) >= 2
}

// denyState is the deny policy's progress for one session row (memory only:
// a daemon restart gives a run a fresh budget).
type denyState struct {
	run     string    // the run whose denies are counted
	count   int       // prompts denied plus continuations sent during run
	limited bool      // EventPromptDeniedLimit recorded for run
	key     string    // the prompt denied last
	tick    uint64    // observe tick of that deny
	at      time.Time // when that deny's keys were sent
	cont    bool      // that deny still awaits its continuation (continueAfterDeny)
}

// answerPermission applies the kind's on_permission_prompt policy to an
// agent that herdr reports blocked during run, and returns the observation
// kind: ObsPromptDenied when the visible screen shows a permission prompt
// (detectPermissionPrompt; never the first-launch trust dialog, which
// AnswerTrustDialog handles) that magnum denied this tick or denied on the
// previous tick and still sees (the screen may lag; one tick is waited
// before the same prompt is denied again), else ObsBlocked (policy "wait",
// an unknown kind, no prompt recognised, MaxPromptDenies reached for run,
// or the keys could not be sent). At most one deny per session per tick.
// Each deny records EventPromptDenied; reaching the cap records
// EventPromptDeniedLimit once per run. The answer is never Yes: see
// sendDeny.
func (m *Manager) answerPermission(ctx context.Context, s store.Session, run store.Run, ref paneRef) ObservationKind {
	kind := m.sessionKind(s)
	spec, ok := m.kindSpec(kind)
	if !ok || spec.OnPermissionPrompt != config.PermissionDeny {
		return ObsBlocked
	}
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		m.logf("agents: %s blocked: %v", ref, err)
		return ObsBlocked
	}
	if _, trust := detectTrustDialog(text); trust {
		return ObsBlocked
	}
	p, ok := detectPermissionPrompt(text)
	if !ok {
		return ObsBlocked
	}
	m.mu.Lock()
	st := m.denies[s.ID]
	if st == nil || st.run != run.ID {
		st = &denyState{run: run.ID}
		m.denies[s.ID] = st
	}
	switch {
	case st.count > 0 && (st.tick == m.tick || (st.key == p.key && st.tick+1 == m.tick)):
		m.mu.Unlock()
		return ObsPromptDenied // denied this tick, or just denied and the screen has not caught up
	case st.count >= MaxPromptDenies:
		first := !st.limited
		st.limited = true
		m.mu.Unlock()
		if first {
			m.promptDeniedLimit(ctx, s, run, ref, kind, p)
		}
		return ObsBlocked
	}
	st.count++
	st.key, st.tick, st.at = p.key, m.tick, m.now()
	n := st.count
	m.mu.Unlock()
	keys, err := m.sendDeny(ctx, ref, p)
	if err != nil {
		m.logf("agents: deny %s permission prompt of %s in %s (keys sent: %s): %v", kind, s.Role, ref, strings.Join(keys, ", "), err)
		if len(keys) == 0 { // nothing reached the agent: no deny to count or wait for
			m.mu.Lock()
			st.count--
			st.key, st.tick = "", 0
			m.mu.Unlock()
			return ObsBlocked
		}
	}
	m.mu.Lock()
	st.cont = true
	m.mu.Unlock()
	m.promptDenied(ctx, s, run, ref, kind, p, keys, n)
	return ObsPromptDenied
}

// denyPending reports whether session s still awaits the continuation of a
// deny during run (pending) and, for a claude agent (Claude Code session
// sid) whose transcript magnum reads, that its working on after the deny is
// no resumption (holds): Claude Code ends its turn on a No, so the agent
// works only on background work it started before (a forked /code-review
// keeps herdr showing it working until the fork reports) or on the turn
// the fork's notification begins. cut is true once that transcript shows
// the turn cut by the rejection (the "[Request interrupted by user for tool
// use]" line, stamped after the deny): the composer is free from then on.
// Any other kind, and a transcript it cannot read, hold nothing: an agent
// working after a deny carried on by itself, as before.
func (m *Manager) denyPending(s store.Session, sid string, run *store.Run) (pending, holds, cut bool) {
	m.mu.Lock()
	st := m.denies[s.ID]
	pending = run != nil && st != nil && st.run == run.ID && st.cont
	var at time.Time
	if st != nil {
		at = st.at
	}
	m.mu.Unlock()
	if !pending || m.sessionKind(s) != KindClaude || sid == "" {
		return pending, false, false
	}
	w := m.watch(s.ID, *run)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, sid, w) {
		return pending, false, false
	}
	return pending, true, w.cut.After(at.Add(-transcriptSkew))
}

// denyResumed notes that a session's agent works again: a deny that still
// awaited its continuation needs none (the agent carried on by itself).
func (m *Manager) denyResumed(sessionID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.denies[sessionID]; st != nil {
		st.cont = false
	}
}

// continueAfterDeny sends the kind's after_deny_prompt to an agent whose
// permission prompt magnum denied during run (the same run, still in
// flight) and whose turn ended on it: one herdr shows idle and that has not
// worked since, or a claude agent whose transcript shows the turn cut by
// the rejection while its background work keeps it working (denyPending).
// Claude Code ends its turn on a No and would otherwise end the run
// unfinished, or answer its fork's notification only to say it stopped
// (a round lost its claude-review report so). It is sent through herdr
// agent.prompt without creating a run, at most once per denied prompt, only
// when the visible screen shows no trust or permission dialog and an empty
// composer (composerEmpty), and only while the kind's on_permission_prompt
// is "deny" and its after_deny_prompt is not "". It counts against
// MaxPromptDenies (none is sent past it), resets the session's idle_ticks
// (so the run does not end before the agent picks the text up), sets
// last_prompt_at and records EventDenyContinued. It reports whether the
// message was sent; a screen it cannot judge yet is retried on the next
// idle tick, while the run is still in flight.
func (m *Manager) continueAfterDeny(ctx context.Context, s store.Session, run store.Run, ref paneRef) bool {
	m.mu.Lock()
	st := m.denies[s.ID]
	pending := st != nil && st.run == run.ID && st.cont
	m.mu.Unlock()
	if !pending {
		return false
	}
	kind := m.sessionKind(s)
	spec, ok := m.kindSpec(kind)
	if !ok || spec.OnPermissionPrompt != config.PermissionDeny || spec.AfterDenyPrompt == "" {
		m.denyResumed(s.ID)
		return false
	}
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		m.logf("agents: continue %s after a deny: %v", ref, err)
		return false
	}
	if _, trust := detectTrustDialog(text); trust {
		return false
	}
	if _, prompt := detectPermissionPrompt(text); prompt {
		return false
	}
	if !composerEmpty(text) {
		m.logf("agents: continue %s after a deny: the composer is not empty; not typing into it", ref)
		return false
	}
	m.mu.Lock()
	if !st.cont {
		m.mu.Unlock()
		return false // sent meanwhile
	}
	st.cont = false
	if st.count >= MaxPromptDenies {
		m.mu.Unlock()
		m.logf("agents: continue %s after a deny: %d denies and continuations this run; not sending more", ref, MaxPromptDenies)
		return false
	}
	st.count++
	n := st.count
	m.mu.Unlock()
	if _, err := m.d.Herdr.AgentPrompt(ctx, target(s), spec.AfterDenyPrompt, nil); err != nil {
		m.logf("agents: continue %s after a deny: %v", ref, mapHerdr(err))
		return false
	}
	now := m.now()
	if err := m.d.Store.UpdateSession(context.WithoutCancel(ctx), s.ID, func(u *store.SessionUpdate) {
		u.Set("idle_ticks", 0)
		u.Set("last_prompt_at", now)
	}); err != nil {
		m.logf("agents: continue %s after a deny: session %d: %v", ref, s.ID, err)
	}
	msg := fmt.Sprintf("sent the after-deny continuation to the %s agent of %s in %s (%d/%d this run)", kind, s.Role, ref, n, MaxPromptDenies)
	m.permissionEvent(ctx, s.PRID, "info", EventDenyContinued, msg, map[string]any{"role": s.Role, "kind": kind,
		"agent": ref.name, "pane": ref.pane, "run": run.ID, "text": detail(spec.AfterDenyPrompt), "count": n})
	return true
}

var (
	// composerLine is an agent CLI's input line: Claude Code's ">" or "❯",
	// Codex's "›", then what it holds.
	composerLine = regexp.MustCompile(`^[>❯›](?:\s+(.*))?$`)
	// composerPlaceholder is the dimmed hint an empty composer shows
	// (Claude Code's `Try "…"`, Codex's suggestions), which a screen read
	// cannot tell from typed text.
	composerPlaceholder = regexp.MustCompile(`(?i)^(?:try ".*"|ask codex to do anything|explain this codebase|summarize recent commits|` +
		`implement \{feature\}|find and fix a bug in @filename|write tests for @filename|improve documentation in @filename|` +
		`run /review on my current changes|use /skills to list available skills)$`)
)

// composerEmpty reports whether the agent's input line (the last line
// starting with a composer mark among the last 8 non-blank lines of the
// visible screen) holds nothing but a placeholder. No such line, or text a
// human may have typed, is not empty.
func composerEmpty(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 8; i-- {
		t := stripBox(lines[i])
		if t == "" {
			continue
		}
		seen++
		if m := composerLine.FindStringSubmatch(t); m != nil {
			rest := strings.TrimSpace(m[1])
			return rest == "" || composerPlaceholder.MatchString(rest)
		}
	}
	return false
}

// sendDeny answers a permission prompt No, never Yes. A (y/n) question gets
// "n" and Enter. A numbered prompt gets the number of its first "No…" option
// (Claude Code and Codex select an option by its number); when a re-read
// still shows the same prompt, Enter is pressed only with the cursor shown on
// that option. If the prompt is still there after that, Escape (both CLIs'
// cancel, which rejects) is sent once. It returns the keys sent.
func (m *Manager) sendDeny(ctx context.Context, ref paneRef, p permPrompt) ([]string, error) {
	var sent []string
	send := func(keys ...string) error {
		if err := m.sendKeys(ctx, ref, keys...); err != nil {
			return err
		}
		sent = append(sent, keys...)
		return nil
	}
	if p.options == nil {
		if err := send("n", "enter"); err != nil {
			return sent, err
		}
	} else {
		if err := send(p.options[p.deny].num); err != nil {
			return sent, err
		}
		q, still, err := m.rereadPrompt(ctx, ref, p.key)
		if err != nil || !still {
			return sent, err
		}
		if c := q.cursor(); c >= 0 && c == q.deny {
			if err := send("enter"); err != nil {
				return sent, err
			}
		}
	}
	if _, still, err := m.rereadPrompt(ctx, ref, p.key); err != nil || !still {
		return sent, err
	}
	return sent, send("esc")
}

// rereadPrompt waits denyKeyDelay and reports whether the prompt identified
// by key is still on screen (and that prompt as now shown).
func (m *Manager) rereadPrompt(ctx context.Context, ref paneRef, key string) (permPrompt, bool, error) {
	if err := m.sleep(ctx, denyKeyDelay); err != nil {
		return permPrompt{}, false, err
	}
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		return permPrompt{}, false, err
	}
	p, ok := detectPermissionPrompt(text)
	return p, ok && p.key == key, nil
}

// promptDenied logs and records a denied prompt (best effort).
func (m *Manager) promptDenied(ctx context.Context, s store.Session, run store.Run, ref paneRef, kind string, p permPrompt, keys []string, n int) {
	header := detail(p.header)
	msg := fmt.Sprintf("denied a %s permission prompt of %s in %s (%s; %d/%d this run): %s", kind, s.Role, ref,
		strings.Join(keys, ", "), n, MaxPromptDenies, header)
	m.permissionEvent(ctx, s.PRID, "info", EventPromptDenied, msg, map[string]any{"role": s.Role, "kind": kind,
		"agent": ref.name, "pane": ref.pane, "run": run.ID, "header": header, "question": detail(p.question),
		"keys": keys, "count": n})
}

// promptDeniedLimit logs and records that a run reached MaxPromptDenies.
func (m *Manager) promptDeniedLimit(ctx context.Context, s store.Session, run store.Run, ref paneRef, kind string, p permPrompt) {
	header := detail(p.header)
	msg := fmt.Sprintf("stopped denying %s permission prompts of %s in %s after %d this run; it stays blocked: %s", kind, s.Role, ref,
		MaxPromptDenies, header)
	m.permissionEvent(ctx, s.PRID, "warn", EventPromptDeniedLimit, msg, map[string]any{"role": s.Role, "kind": kind,
		"agent": ref.name, "pane": ref.pane, "run": run.ID, "header": header, "question": detail(p.question),
		"count": MaxPromptDenies})
}

func (m *Manager) permissionEvent(ctx context.Context, prID int64, level, kind, msg string, data map[string]any) {
	m.event(ctx, m.prSubject(ctx, prID), level, kind, msg, data)
}
