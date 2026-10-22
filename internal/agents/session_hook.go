package agents

// herdr learns a Codex session's id (the pane's agent_session, which
// resumable and TurnError need) from a SessionStart hook it installs in
// $CODEX_HOME/hooks.json ("bash ~/.codex/herdr-agent-state.sh session",
// timeout 10 s). Codex keeps each hook's trust in config.toml's
// [hooks.state] table, one entry per hook keyed by its place,
// "<hooks.json path>:session_start:<group>:<index>", with trusted_hash and
// enabled: a hook moved to another place runs only once the operator
// trusts it there, and a hooks dialog the operator answers can set enabled =
// false. On 2026-10-16 that turned herdr's hook off; on 2026-10-22 a rewrite
// of hooks.json moved it to the third group, behind two hooks without a
// timeout, and most Codex sessions got no id. magnum never changes either
// file (the hooks and their trust are the operator's): `magnum doctor`
// reads them (ReadCodexSessionHook, CodexSessionHook.Problems) and the
// registry's session ids (RecentSessionIDs, MissingSessionIDs), the daemon
// warns when hooks.json changes into a failing state and when Codex sessions
// go without ids (internal/engine/codex_session_hook.go), and the observer
// says once per kind and daemon start when a session's first turn ends
// without an id (sessionIDMissing).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// EventSessionIDMissing is recorded (warn, subject tool:<kind>) once per kind
// and Manager when a session's first turn ends without a session id from
// herdr (see sessionIDMissing).
const EventSessionIDMissing = "agents.session_id_missing"

// SessionIDBlind says what goes blind without a Codex session id: resumable
// never resumes the judge and TurnError cannot read the rollout, which the
// pipeline's check for a helper's refusal reads.
const SessionIDBlind = "no session id: judges are never resumed, and a helper's Codex refusal is not seen"

// HerdrSessionHook is the script herdr's Codex SessionStart hook runs; a
// hook whose command names it is herdr's.
const HerdrSessionHook = "herdr-agent-state.sh"

// CodexHookMaxTimeout is the longest timeout, in seconds, a SessionStart
// hook that runs before herdr's may have for herdr's hook's place to pass:
// herdr's own hook has 10 s, and Codex gives a hook without a timeout up to
// 600 s.
const CodexHookMaxTimeout = 10

// codexSessionStart is the SessionStart event as hooks.json names it, and
// as the keys of config.toml's [hooks.state] entries do.
const (
	codexSessionStart      = "SessionStart"
	codexSessionStartState = "session_start"
)

// The checks herdr's Codex session hook can fail (CodexHookProblem.Check).
const (
	CodexHookMissing  = "missing"  // no SessionStart hook names HerdrSessionHook
	CodexHookDisabled = "disabled" // every such hook's [hooks.state] entry has enabled = false
	CodexHookPlace    = "place"    // a hook before it has no timeout, or one over CodexHookMaxTimeout
	CodexHookTrust    = "trust"    // its place has no [hooks.state] entry with a trusted_hash
)

// CodexHook is one hook of a Codex hooks.json, as messages name it.
type CodexHook struct {
	// Name is the last path element of its program (of the script an
	// interpreter such as bash runs), cleaned for the terminal; its type for
	// a hook that runs no command.
	Name string
	// Timeout is its timeout in seconds; 0 = none.
	Timeout int
}

// CodexSessionHook is what a Codex home says of herdr's SessionStart hook
// (ReadCodexSessionHook).
type CodexSessionHook struct {
	HooksFile  string // <home>/hooks.json
	ConfigFile string // <home>/config.toml
	// Found: a SessionStart hook of HooksFile names HerdrSessionHook.
	Found bool
	// Disabled are the keys of the [hooks.state] entries that set enabled =
	// false on each such hook, when every one of them has such an entry;
	// nil when one of them runs (its entry enables it, or it has none).
	Disabled []string
	// Group and Index are the place of herdr's hook that runs, when one
	// does (the first whose place has a trust entry, else the first not
	// disabled): its group among HooksFile's SessionStart groups and its
	// index in that group. Place is its [hooks.state] key without the
	// hooks.json path ("session_start:<group>:<index>").
	Group, Index int
	Place        string
	// Before are the SessionStart hooks before it, in Codex's order, but
	// those their entries disable.
	Before []CodexHook
	// Trusted: config.toml has a [hooks.state] entry with a trusted_hash for
	// its place. Whether that hash is of this command magnum cannot tell
	// (Codex hashes its own normal form of the hook).
	Trusted bool
	// Version is the HERDR_INTEGRATION_VERSION its script's header names
	// ("" = unknown: no such line, or no script where its command says).
	Version string
}

// CodexHookProblem is one check herdr's Codex session hook fails
// (CodexSessionHook.Problems).
type CodexHookProblem struct {
	Check string // CodexHookMissing, CodexHookDisabled, CodexHookPlace or CodexHookTrust
	// Text says what is wrong in one sentence without its final period.
	Text string
}

// Problems are the checks herdr's hook fails, naming the files as
// hooksFile and configFile say (doctor shows their paths; a daemon message
// CodexFileName's, since a Codex home may be a pane env value): the hook is
// missing, or disabled, or else its place (a hook before it can hold it up:
// no timeout, or one over CodexHookMaxTimeout) and its trust (its place has
// no trust entry, so Codex asks the operator to review the hooks before it
// runs it). None when it passes.
func (h CodexSessionHook) Problems(hooksFile, configFile string) []CodexHookProblem {
	switch {
	case !h.Found:
		return []CodexHookProblem{{CodexHookMissing, fmt.Sprintf("herdr's Codex session hook is missing: no SessionStart hook in %s names %s",
			hooksFile, HerdrSessionHook)}}
	case len(h.Disabled) > 0:
		entries := make([]string, len(h.Disabled))
		for i, key := range h.Disabled {
			entries[i] = hookEntryName(key)
		}
		return []CodexHookProblem{{CodexHookDisabled, fmt.Sprintf("herdr's Codex session hook is disabled: its [hooks.state] entry %s in %s has enabled = false",
			strings.Join(entries, ", "), configFile)}}
	}
	var out []CodexHookProblem
	if text := h.placeProblem(hooksFile); text != "" {
		out = append(out, CodexHookProblem{CodexHookPlace, text})
	}
	if !h.Trusted {
		out = append(out, CodexHookProblem{CodexHookTrust, fmt.Sprintf("herdr's Codex session hook has no trust entry for its place %s in %s",
			h.Place, configFile)})
	}
	return out
}

// codexHookSubject starts the text of every CodexHookProblem.
const codexHookSubject = "herdr's Codex session hook "

// JoinCodexHookProblems says problems in one sentence without its final
// period: the first in full, each other after "; it" ("herdr's Codex
// session hook runs after ...; it has no trust entry ...").
func JoinCodexHookProblems(problems []CodexHookProblem) string {
	texts := make([]string, len(problems))
	for i, p := range problems {
		texts[i] = p.Text
		if rest, ok := strings.CutPrefix(p.Text, codexHookSubject); ok && i > 0 {
			texts[i] = "it " + rest
		}
	}
	return strings.Join(texts, "; ")
}

// placeProblem names the hooks of Before that can hold herdr's hook up, by
// what holds: "runs after 2 SessionStart hooks without a timeout (a, b) and
// 1 with a timeout over 10 s (c, 30 s)"; "" when none can.
func (h CodexSessionHook) placeProblem(hooksFile string) string {
	var none, over []string
	for _, b := range h.Before {
		switch {
		case b.Timeout <= 0:
			none = append(none, b.Name)
		case b.Timeout > CodexHookMaxTimeout:
			over = append(over, fmt.Sprintf("%s, %d s", b.Name, b.Timeout))
		}
	}
	var parts []string
	if len(none) > 0 {
		parts = append(parts, fmt.Sprintf("%s without a timeout (%s)",
			textx.Count(len(none), "SessionStart hook", "SessionStart hooks"), strings.Join(none, ", ")))
	}
	if len(over) > 0 {
		count := textx.Count(len(over), "SessionStart hook", "SessionStart hooks")
		if len(none) > 0 {
			count = strconv.Itoa(len(over))
		}
		parts = append(parts, fmt.Sprintf("%s with a timeout over %d s (%s)", count, CodexHookMaxTimeout, strings.Join(over, "; ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("herdr's Codex session hook runs after %s in %s", strings.Join(parts, " and "), hooksFile)
}

// CodexFileName names file of the Codex home home for a message that must
// not name a pane env value: "~/.codex/<file>" for the default home, else
// "$CODEX_HOME/<file>".
func CodexFileName(home, file string) string {
	if u, err := os.UserHomeDir(); err == nil && u != "" && home == filepath.Join(u, ".codex") {
		return "~/.codex/" + file
	}
	return "$CODEX_HOME/" + file
}

// CodexHookHomes are the Codex homes herdr's session hook is checked in, in
// the roles' order, with the session roles of the codex kind that read each
// (home: the Manager's CodexHome; "" for one unknown). None unless the codex
// kind's session ids come from herdr (session_source "herdr"); a shell
// role's `codex review` needs no id.
func CodexHookHomes(cfg *config.Config, home func(config.Role) string) (homes []string, users map[string][]string) {
	if k, ok := cfg.KindSpec(KindCodex); !ok || k.SessionSource != config.SessionHerdr {
		return nil, nil
	}
	users = map[string][]string{}
	for _, r := range cfg.AllRoles() {
		if !r.IsAgent() || r.Kind != KindCodex {
			continue
		}
		h := home(r)
		if _, seen := users[h]; !seen {
			homes = append(homes, h)
		}
		users[h] = append(users[h], r.Name)
	}
	return homes, users
}

// hookEntryName is a [hooks.state] key without its hooks.json path
// ("session_start:0:0"), for a message that must not name a pane env value
// (the path is under CODEX_HOME).
func hookEntryName(key string) string {
	if i := strings.LastIndex(key, ":"+codexSessionStartState+":"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// codexHooks is what ReadCodexSessionHook reads of a hooks.json: the type,
// command and timeout (seconds) of each hook, by event and group.
type codexHooks struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout any    `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// codexHookEntry is one [hooks.state] entry of a config.toml.
type codexHookEntry struct {
	Enabled     *bool  `toml:"enabled"`
	TrustedHash string `toml:"trusted_hash"`
}

// codexHookState is what ReadCodexSessionHook reads of a config.toml: its
// [hooks.state] entries.
type codexHookState struct {
	Hooks struct {
		State map[string]codexHookEntry `toml:"state"`
	} `toml:"hooks"`
}

// sessionStartHook is one SessionStart hook of a hooks.json at its place.
type sessionStartHook struct {
	group, index int
	hook         CodexHook
	command      string
	herdr        bool // its command names HerdrSessionHook
}

// ReadCodexSessionHook reads herdr's SessionStart hook in the Codex home
// home: whether a SessionStart hook of its hooks.json names
// HerdrSessionHook, whether config.toml's [hooks.state] entry of each such
// hook (keyed by the hooks.json path, as written or with its symlinks
// resolved) sets enabled = false, and, of the one that runs, its place, the
// hooks before it, its trust entry and its script's integration version.
// ok is false when there is nothing to judge: home unknown or no hooks.json
// there (herdr not installed is another check). A missing config.toml,
// [hooks.state] table or entry disables nothing and trusts nothing. err
// names a file that cannot be read or decoded, without the decoder's
// message, which may quote the file. It only reads.
func ReadCodexSessionHook(home string) (h CodexSessionHook, ok bool, err error) {
	if home == "" {
		return h, false, nil
	}
	h.HooksFile, h.ConfigFile = filepath.Join(home, "hooks.json"), filepath.Join(home, "config.toml")
	raw, err := os.ReadFile(h.HooksFile)
	if errors.Is(err, fs.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("cannot read %s: %w", h.HooksFile, unwrapPath(err))
	}
	var hooks codexHooks
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return h, false, fmt.Errorf("%s is not valid JSON", h.HooksFile)
	}
	var all []sessionStartHook
	for g, group := range hooks.Hooks[codexSessionStart] {
		for i, hook := range group.Hooks {
			herdr := strings.Contains(hook.Command, HerdrSessionHook)
			all = append(all, sessionStartHook{group: g, index: i, command: hook.Command, herdr: herdr,
				hook: CodexHook{Name: codexHookName(hook.Type, hook.Command), Timeout: codexHookTimeout(hook.Timeout)}})
			h.Found = h.Found || herdr
		}
	}
	if !h.Found {
		return h, true, nil
	}
	state, err := readCodexHookState(h.ConfigFile)
	if err != nil {
		return h, false, err
	}
	prefixes := []string{h.HooksFile}
	if resolved, err := filepath.EvalSymlinks(h.HooksFile); err == nil && resolved != h.HooksFile {
		prefixes = append(prefixes, resolved)
	}
	entry := func(s sessionStartHook) (string, codexHookEntry, bool) {
		for _, p := range prefixes {
			key := p + ":" + codexSessionStartState + ":" + strconv.Itoa(s.group) + ":" + strconv.Itoa(s.index)
			if e, ok := state[key]; ok {
				return key, e, true
			}
		}
		return "", codexHookEntry{}, false
	}
	off := func(s sessionStartHook) (string, bool) {
		key, e, ok := entry(s)
		return key, ok && e.Enabled != nil && !*e.Enabled
	}
	trusted := func(s sessionStartHook) bool {
		_, e, ok := entry(s)
		return ok && e.TrustedHash != ""
	}
	runs := -1 // herdr's hook that runs: the first trusted, else the first not disabled
	var disabled []string
	for n, s := range all {
		if !s.herdr {
			continue
		}
		if key, isOff := off(s); isOff {
			disabled = append(disabled, key)
			continue
		}
		if runs < 0 {
			runs = n
		}
		if trusted(s) {
			runs = n
			break
		}
	}
	if runs < 0 {
		h.Disabled = disabled
		return h, true, nil
	}
	s := all[runs]
	h.Group, h.Index = s.group, s.index
	h.Place = codexSessionStartState + ":" + strconv.Itoa(s.group) + ":" + strconv.Itoa(s.index)
	h.Trusted = trusted(s)
	for _, b := range all[:runs] {
		if _, isOff := off(b); !isOff {
			h.Before = append(h.Before, b.hook)
		}
	}
	h.Version = herdrScriptVersion(s.command)
	return h, true, nil
}

// readCodexHookState reads the [hooks.state] entries of the config.toml at
// path; none when it does not exist.
func readCodexHookState(path string) (map[string]codexHookEntry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, unwrapPath(err))
	}
	var state codexHookState
	if _, err := toml.Decode(string(raw), &state); err != nil {
		return nil, fmt.Errorf("%s is not valid TOML", path)
	}
	return state.Hooks.State, nil
}

// codexHookTimeout is a hooks.json timeout in seconds (0 = none): Codex
// reads a whole number of seconds and runs a hook of 0 for 1 s; anything
// else is none.
func codexHookTimeout(v any) int {
	f, ok := v.(float64)
	if !ok || f < 0 || f > 1<<31 || f != float64(int(f)) {
		return 0
	}
	return max(int(f), 1)
}

// codexHookMaxName bounds a hook's name in a message.
const codexHookMaxName = 40

// shellAssignment is a shell word that sets a variable for the command
// after it ("FOO=1").
var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// hookWrappers run the command after them; hookInterpreters run the script
// their first argument names, which names the hook ("bash
// ~/.codex/herdr-agent-state.sh session").
var (
	hookWrappers     = map[string]bool{"env": true, "exec": true, "nohup": true, "command": true}
	hookInterpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
		"python": true, "python3": true, "node": true, "ruby": true, "perl": true}
)

// codexHookName names a hook by the last path element of its program: the
// first word of its command after variable assignments and env, exec,
// nohup or command, or the script an interpreter's first argument names;
// cleaned for the terminal (printable runes only, at most codexHookMaxName).
// A hook that runs no command is named by its type.
func codexHookName(typ, command string) string {
	words := hookWords(command)
	for len(words) > 0 && (shellAssignment.MatchString(words[0]) || hookWrappers[words[0]]) {
		words = words[1:]
	}
	name := ""
	if len(words) > 0 {
		prog := words[0]
		if hookInterpreters[filepath.Base(prog)] && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
			prog = words[1]
		}
		name = printable(filepath.Base(strings.TrimRight(prog, "/")))
	}
	if name == "" || name == "." || name == "/" {
		name = cmp.Or(printable(typ), "hook")
	}
	return textx.Clip(name, codexHookMaxName)
}

// printable is s with only its printable runes, trimmed.
func printable(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s))
}

// hookWords splits a hook's command into shell words, honouring single and
// double quotes and backslashes, leniently: an open quote runs to the end.
func hookWords(command string) []string {
	var (
		words            []string
		word             strings.Builder
		quote            rune
		started, escaped bool
	)
	for _, r := range command {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, started = true, true
		case (r == '\'' || r == '"') && (quote == 0 || quote == r):
			if quote == 0 {
				quote = r
			} else {
				quote = 0
			}
			started = true
		case quote == 0 && unicode.IsSpace(r):
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if started {
		words = append(words, word.String())
	}
	return words
}

// herdrVersion is the line of herdr's hook script that names its
// integration version ("HERDR_INTEGRATION_VERSION=10").
var herdrVersion = regexp.MustCompile(`HERDR_INTEGRATION_VERSION=["']?([0-9A-Za-z._-]{1,32})`)

// herdrScriptMax bounds how much of herdr's hook script is read for its
// header.
const herdrScriptMax = 8 << 10

// herdrScriptVersion is the integration version the header of the script
// herdr's hook command runs names ("" = unknown): the command's word whose
// last path element is HerdrSessionHook, a leading ~/ or $HOME/ read as the
// user's home as the shell does (never in a test binary); only an absolute
// path is read.
func herdrScriptVersion(command string) string {
	for _, w := range hookWords(command) {
		if filepath.Base(w) != HerdrSessionHook {
			continue
		}
		for _, prefix := range []string{"~/", "$HOME/", "${HOME}/"} {
			if rest, ok := strings.CutPrefix(w, prefix); ok {
				u, err := os.UserHomeDir()
				if err != nil || u == "" || testing.Testing() { // a test never reads the user's home
					return ""
				}
				w = filepath.Join(u, rest)
				break
			}
		}
		if !filepath.IsAbs(w) {
			return ""
		}
		return scriptVersion(w)
	}
	return ""
}

// scriptVersion is the integration version the first herdrScriptMax bytes
// of the script at path name ("" = none, or the script cannot be read).
func scriptVersion(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head, _ := io.ReadAll(io.LimitReader(f, herdrScriptMax))
	if m := herdrVersion.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

// SessionIDWindow is how far back doctor and the daemon read the session
// ids of a kind's sessions (RecentSessionIDs).
const SessionIDWindow = 24 * time.Hour

// RecentSessionIDs are the sessions of agent kind kind started within
// SessionIDWindow before now whose session id tells whether herdr's hook
// gives ids, newest first: those that ran an agent (magnum prompted them),
// were not resumed (a resumed session takes the id it resumes) and started
// at least wait before now (herdr's hook reports the id at the start of a
// session's first turn). It only reads.
func RecentSessionIDs(ctx context.Context, st *store.Store, kind string, now time.Time, wait time.Duration) ([]store.Session, error) {
	all, err := st.SessionsStartedSince(ctx, kind, now.Add(-SessionIDWindow))
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, s := range all {
		if s.LastPromptAt == nil || store.Deref(s.ResumedFrom) != "" || now.Sub(s.StartedAt) < wait {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// HasSessionID reports whether session s has its CLI's session id.
func HasSessionID(s store.Session) bool { return store.Deref(s.SessionID) != "" }

// SessionIDGap is what sessions say of their ids (MissingSessionIDs).
type SessionIDGap struct {
	// Streak is how many of the newest sessions in a row have no id.
	Streak int
	// Sessions counts the sessions from the newest back to the oldest
	// without an id since two in a row last had one, Missing those of them
	// without one, and Since is when that oldest one started (zero when no
	// session lacks an id).
	Sessions, Missing int
	Since             time.Time
}

// MissingSessionIDs reads the gap in the session ids of sessions
// (RecentSessionIDs, newest first): the newest without an id in a row, and
// those without one since two sessions in a row last had one.
func MissingSessionIDs(sessions []store.Session) SessionIDGap {
	var g SessionIDGap
	for i, s := range sessions {
		if HasSessionID(s) {
			if i+1 < len(sessions) && HasSessionID(sessions[i+1]) {
				break
			}
			continue
		}
		if g.Missing == i {
			g.Streak++
		}
		g.Missing++
		g.Sessions, g.Since = i+1, s.StartedAt
	}
	return g
}

// unwrapPath drops the path of a *fs.PathError, which the caller names.
func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// CodexHome is the Codex home a session of role reads (codexHomeFor of its
// pane env: its CODEX_HOME, else the daemon's $CODEX_HOME, else ~/.codex;
// "" in a test binary without Deps.CodexConfig).
func (m *Manager) CodexHome(role config.Role) string {
	if m.d.Config == nil {
		return ""
	}
	home, _ := m.codexHomeFor(m.d.Config.RoleEnv(role))
	return home
}

// promptedTurnEnded reports whether this tick ends a turn of session s
// (its row) that magnum prompted: the agent is idle now and was working
// before (prev, the stored status, set at prevAt, no earlier than its last
// prompt). The observer asks it of a session that has no session id yet,
// none stored and none from herdr this tick.
func promptedTurnEnded(s store.Session, prev herdr.Status, prevAt *time.Time, idle bool) bool {
	return idle && prev == herdr.StatusWorking && s.LastPromptAt != nil && prevAt != nil && !prevAt.Before(*s.LastPromptAt)
}

// sessionIDMissing records EventSessionIDMissing for session s, whose turn
// ended without a session id from herdr (promptedTurnEnded), once per kind
// and Manager (a daemon start), for a kind whose session ids come from
// herdr (session_source "herdr"). The message names herdr's hook and what
// goes blind; for codex it says what the Codex home shows of the hook
// (ReadCodexSessionHook: missing, disabled, its place or its trust), never
// its path, a pane env value.
func (m *Manager) sessionIDMissing(ctx context.Context, s store.Session) {
	kind := m.sessionKind(s)
	if k, ok := m.kindSpec(kind); !ok || k.SessionSource != config.SessionHerdr {
		return
	}
	if !firstFor(m, m.sidMissing, kind) {
		return
	}
	data := map[string]any{"kind": kind, "role": s.Role, "session": s.ID}
	msg := fmt.Sprintf("%s: the first turn of %s (session %d) ended without a session id from herdr", kind, s.Role, s.ID)
	if kind != KindCodex {
		msg += fmt.Sprintf(", which learns it from its %s integration's session hook; no session id: its sessions are never resumed", kind)
		m.event(ctx, "tool:"+kind, "warn", EventSessionIDMissing, msg, data)
		return
	}
	data["hook"] = HerdrSessionHook
	hook := "herdr's SessionStart hook (" + HerdrSessionHook + " in $CODEX_HOME/hooks.json)"
	home, _ := m.codexHomeFor(s.Env)
	h, ok, err := ReadCodexSessionHook(home)
	var problems []CodexHookProblem
	if err == nil && ok {
		problems = h.Problems(CodexFileName(home, "hooks.json"), CodexFileName(home, "config.toml"))
	}
	switch {
	case err == nil && ok && !h.Found:
		data["state"] = "missing"
		msg += fmt.Sprintf(": %s is missing: no SessionStart hook there names %s; %s. Reinstall herdr's Codex integration",
			hook, HerdrSessionHook, SessionIDBlind)
	case err == nil && ok && len(h.Disabled) > 0:
		entries := make([]string, len(h.Disabled))
		for i, key := range h.Disabled {
			entries[i] = hookEntryName(key)
		}
		data["state"], data["entries"] = "disabled", entries
		msg += fmt.Sprintf(": %s is disabled: its [hooks.state] entry %s in $CODEX_HOME/config.toml has enabled = false; %s. "+
			"Set enabled = true there (`magnum doctor` names the entry); magnum never changes Codex's hook trust",
			hook, strings.Join(entries, ", "), SessionIDBlind)
	case len(problems) > 0:
		checks := make([]string, len(problems))
		for i, p := range problems {
			checks[i] = p.Check
		}
		data["state"], data["checks"], data["place"] = "failing", checks, h.Place
		msg += fmt.Sprintf(": %s; %s. `magnum doctor` says what to change; magnum never changes Codex's hooks or their trust",
			JoinCodexHookProblems(problems), SessionIDBlind)
	default:
		msg += fmt.Sprintf(", which it learns from %s; %s. `magnum doctor` checks the hook", hook, SessionIDBlind)
	}
	m.event(ctx, "tool:"+kind, "warn", EventSessionIDMissing, msg, data)
}

// firstFor marks key in set (one of the Manager's, under m.mu) and reports
// whether it was not marked yet.
func firstFor[K comparable](m *Manager, set map[K]bool, key K) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if set[key] {
		return false
	}
	set[key] = true
	return true
}
