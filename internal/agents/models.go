package agents

// Per-model limits: one model of an agent kind hit its own cap (health
// kind model_limit, "You've reached your Fable limit") while the account
// still has usage left. The pipeline then switches the session to the
// kind's next fallback model (SwitchModel) and continues the run. The limit
// is remembered in kv, so a new session of a role on the limited model
// starts on a fallback (StartAgent), a live one switches before its next
// prompt (Submit), and both return to the role's model once the limit is
// over.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// KVModelLimited is the kv key holding when kind's limit on model ends
// (store.FormatTime): "kind.<kind>.model_limited.<model>".
func KVModelLimited(kind, model string) string { return "kind." + kind + ".model_limited." + model }

// KVModelLimits is the kv key listing (comma-separated) the models of kind
// that have a KVModelLimited row, so status can find them.
func KVModelLimits(kind string) string { return "kind." + kind + ".model_limits" }

// KVKindCLIModel is the kv key holding the CLI's own default model of kind
// (what a session runs when neither its role's model nor the kind's
// default_model is set), as a limit message of such a session named it.
func KVKindCLIModel(kind string) string { return "kind." + kind + ".cli_model" }

// KVSessionModel is the kv key holding the model magnum switched a session
// row to (absent while the session runs its role's model).
func KVSessionModel(sessionID int64) string { return fmt.Sprintf("session.%d.model", sessionID) }

// UnknownModel names the model of a session whose model magnum does not
// know (a role without model whose CLI default never named itself in a
// limit message).
const UnknownModel = "default"

// Model switching timing.
const (
	// DefaultModelLimitCooldown is used when daemon.model_limit_cooldown is
	// unset (zero).
	DefaultModelLimitCooldown = 5 * time.Hour
	// SwitchModelTimeout bounds the wait for an agent to be idle again after
	// the switch command.
	SwitchModelTimeout = 30 * time.Second
	switchPoll         = time.Second
	// switchPolls bounds the polls by count, not by the clock alone.
	switchPolls = int(SwitchModelTimeout / switchPoll)
)

// Event kinds of per-model limits, and of a lost session made live again.
const (
	EventModelSwitched = "agent.model_switched" // data: session, role, from, to, reason
	EventModelLimited  = "kind.model_limited"   // data: kind, model, until
	EventRebound       = "agent.rebound"        // data: session, role, agent, pane (Submit, see rebindLost)
)

// SwitchModel reasons (EventModelSwitched's reason).
const (
	SwitchLimitHit = "model_limit"   // the model hit its limit during a run
	SwitchLimited  = "model_limited" // the model was already limited (before a prompt, or at start)
	SwitchExpired  = "limit_expired" // the role's model is no longer limited: back to it
)

// ErrNoModelSwitch: the session's kind has no switch_model command.
var ErrNoModelSwitch = errors.New("agent kind cannot switch models")

// ModelLimit is one model of a kind that hit its own limit.
type ModelLimit struct {
	Kind  string
	Model string
	Until time.Time
	// Using is the first of the kind's fallback_models that is not limited
	// itself ("" = none).
	Using string
}

// FallbackPromptName is the prompt file the continuation after a model
// switch renders (with FallbackData).
const FallbackPromptName = "model-fallback.md"

// FallbackData feeds the model-fallback prompt.
type FallbackData struct {
	Model      string // the model the session runs now
	Previous   string // the model that hit its limit
	Role       string
	URL        string
	HeadSHA    string
	ReportPath string // the role's report or result file; "" = the prompt names none
}

// FallbackPrompt renders FallbackPromptName from pipeline.prompts_dir or
// the embedded defaults.
func (m *Manager) FallbackPrompt(d FallbackData) (string, error) {
	p, err := m.d.Config.ResolvePrompt(FallbackPromptName)
	if err != nil {
		return "", fmt.Errorf("agents: %w", err)
	}
	return RenderPrompt(p, d)
}

// SameModel reports whether two model names mean the same model as limit
// messages and configs spell them: equal ignoring case, or one is a word of
// the other ("opus" and "claude-opus-4-5"). "" means UnknownModel.
func SameModel(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			return UnknownModel
		}
		return s
	}
	a, b = norm(a), norm(b)
	words := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	}
	return a == b || slices.Contains(words(b), a) || slices.Contains(words(a), b)
}

// getKV reads one registry key.
func getKV(ctx context.Context, st *store.Store, key string) (string, bool, error) {
	return st.GetKV(ctx, key)
}

// readKV is how activeLimits reads the registry (a test makes it fail).
var readKV = getKV

// activeLimits reads kind's limits that end after now: model -> until. A
// failed read is the error, with the limits the other reads found: a limit
// that cannot be read is not one that ended.
func activeLimits(ctx context.Context, st *store.Store, kind string, now time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	idx, _, err := readKV(ctx, st, KVModelLimits(kind))
	if err != nil {
		return out, fmt.Errorf("agents: read the model limits of %s: %w", kind, err)
	}
	var errs []error
	for _, name := range splitList(idx) {
		v, ok, err := readKV(ctx, st, KVModelLimited(kind, name))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("agents: read the %s limit of %s: %w", name, kind, err))
			continue
		case !ok:
			continue
		}
		if t, err := store.ParseTime(v); err == nil && t.After(now) {
			out[name] = t
		}
	}
	return out, errors.Join(errs...)
}

// limitOf finds model among limits (SameModel).
func limitOf(limits map[string]time.Time, model string) (time.Time, bool) {
	for name, until := range limits {
		if SameModel(name, model) {
			return until, true
		}
	}
	return time.Time{}, false
}

// firstFallback is the first of fallbacks that is neither limited nor one of
// skip.
func firstFallback(fallbacks []string, limits map[string]time.Time, skip ...string) (string, bool) {
	for _, f := range fallbacks {
		if _, lim := limitOf(limits, f); lim || slices.ContainsFunc(skip, func(s string) bool { return SameModel(s, f) }) {
			continue
		}
		return f, true
	}
	return "", false
}

// ModelLimits lists the per-model limits of kind recorded in st that end
// after now, by model name, each with the fallback a session uses meanwhile.
// cfg may be nil (Using then stays empty). It is for display: a limit whose
// row cannot be read is left out.
func ModelLimits(ctx context.Context, st *store.Store, cfg *config.Config, kind string, now time.Time) []ModelLimit {
	limits, _ := activeLimits(ctx, st, kind, now)
	var fallbacks []string
	if cfg != nil {
		if k, ok := cfg.KindSpec(kind); ok && k.SwitchModel != "" {
			fallbacks = k.FallbackModels
		}
	}
	out := make([]ModelLimit, 0, len(limits))
	for _, name := range slices.Sorted(maps.Keys(limits)) {
		l := ModelLimit{Kind: kind, Model: name, Until: limits[name]}
		l.Using, _ = firstFallback(fallbacks, limits, name)
		out = append(out, l)
	}
	return out
}

// configuredModel is the model a role's sessions are configured to run
// (config.Config.RoleModel: the role's model, else its kind's
// default_model; "" = the CLI's own default).
func (m *Manager) configuredModel(role config.Role) string {
	if m.d.Config == nil {
		return role.Model
	}
	return m.d.Config.RoleModel(role)
}

// roleModel is the model a role's sessions run unless magnum switched them:
// the configured one, else the CLI's default as a limit message named it,
// else "" (unknown).
func (m *Manager) roleModel(ctx context.Context, role config.Role) string {
	if mdl := m.configuredModel(role); mdl != "" {
		return mdl
	}
	v, _, _ := m.d.Store.GetKV(ctx, KVKindCLIModel(role.AgentKind()))
	return v
}

// sessionModel is the model a session runs: the one magnum switched it to
// (switched true), else its role's (roleModel).
func (m *Manager) sessionModel(ctx context.Context, s store.Session, role config.Role) (model string, switched bool) {
	if v, ok, _ := m.d.Store.GetKV(ctx, KVSessionModel(s.ID)); ok && v != "" {
		return v, true
	}
	return m.roleModel(ctx, role), false
}

// NoteModelLimit records that the model of session s hit its own limit (h,
// a HealthModelLimit verdict on its pane): h.Model, else the session's
// current model, is limited until h.ResetAt, else for
// daemon.model_limit_cooldown, never shortening a later recorded end. A
// session on its CLI's default model also teaches magnum which model that
// default is (KVKindCLIModel). It appends a kind.model_limited event. A
// recorded limit that cannot be read is the error, before anything is
// written: the index rewritten from a partial view would drop limits still
// active.
func (m *Manager) NoteModelLimit(ctx context.Context, s store.Session, h Health) (ModelLimit, error) {
	ctx = context.WithoutCancel(ctx)
	kind := m.sessionKind(s)
	role, _ := m.roleSpec(Role(s.Role))
	cur, switched := m.sessionModel(ctx, s, role)
	model := strings.ToLower(strings.TrimSpace(h.Model))
	if model == "" {
		model = strings.ToLower(cur)
	}
	if model == "" {
		model = UnknownModel
	}
	now := m.now()
	limits, err := activeLimits(ctx, m.d.Store, kind, now)
	if err != nil {
		return ModelLimit{}, fmt.Errorf("agents: record %s limit of %s: %w", model, kind, err)
	}
	if h.Model != "" && m.configuredModel(role) == "" && !switched {
		if err := m.d.Store.SetKV(ctx, KVKindCLIModel(kind), model); err != nil {
			m.logErr(ctx, err, "agents: %v", err)
		}
	}
	cooldown := DefaultModelLimitCooldown
	if m.d.Config != nil && m.d.Config.Daemon.ModelLimitCooldown.Duration > 0 {
		cooldown = m.d.Config.Daemon.ModelLimitCooldown.Duration
	}
	until := now.Add(cooldown)
	if h.ResetAt != nil && h.ResetAt.After(now) {
		until = *h.ResetAt
	}
	if prev, ok := limits[model]; ok && prev.After(until) {
		until = prev
	}
	limits[model] = until
	if err := m.d.Store.SetKV(ctx, KVModelLimited(kind, model), store.FormatTime(until)); err != nil {
		return ModelLimit{}, fmt.Errorf("agents: record %s limit of %s: %w", model, kind, err)
	}
	// The index keeps the active limits only; rows of expired ones go too.
	idx, _, err := m.d.Store.GetKV(ctx, KVModelLimits(kind))
	if err != nil {
		m.logErr(ctx, err, "agents: expired limits of %s kept: %v", kind, err)
	}
	for _, name := range splitList(idx) {
		if _, active := limits[name]; !active {
			if err := m.d.Store.DeleteKV(ctx, KVModelLimited(kind, name)); err != nil {
				m.logErr(ctx, err, "agents: expired %s limit of %s kept: %v", name, kind, err)
			}
		}
	}
	if err := m.d.Store.SetKV(ctx, KVModelLimits(kind), strings.Join(slices.Sorted(maps.Keys(limits)), ",")); err != nil {
		return ModelLimit{}, fmt.Errorf("agents: record %s limit of %s: %w", model, kind, err)
	}
	l := ModelLimit{Kind: kind, Model: model, Until: until}
	if k, ok := m.kindSpec(kind); ok && k.SwitchModel != "" {
		l.Using, _ = firstFallback(k.FallbackModels, limits, model)
	}
	m.event(ctx, "tool:"+kind, "warn", EventModelLimited,
		fmt.Sprintf("%s: %s limited until %s: %s", kind, model, until.Local().Format("15:04"), h.Detail),
		map[string]any{"kind": kind, "model": model, "until": store.FormatTime(until)})
	return l, nil
}

// FallbackModel is the model session s switches to next: the first of its
// kind's fallback_models that is not limited, not the session's current
// model and not among tried. ok is false when the kind cannot switch
// (switch_model unset), every fallback is used up, or the limits cannot be
// read (logged: the session keeps its model).
func (m *Manager) FallbackModel(ctx context.Context, s store.Session, tried []string) (string, bool) {
	kind := m.sessionKind(s)
	k, ok := m.kindSpec(kind)
	if !ok || k.SwitchModel == "" {
		return "", false
	}
	limits, err := activeLimits(ctx, m.d.Store, kind, m.now())
	if err != nil {
		m.logErr(ctx, err, "agents: %s: no fallback model: %v", s.Role, err)
		return "", false
	}
	role, _ := m.roleSpec(Role(s.Role))
	cur, _ := m.sessionModel(ctx, s, role)
	return firstFallback(k.FallbackModels, limits, append(slices.Clone(tried), cur)...)
}

// SwitchModel switches the idle agent of session s to model with its
// kind's switch_model command (claude: `/model <model>`) typed into its
// pane. In a session with history Claude Code asks first ("Switch model?",
// "❯ 1. Yes, switch to Opus 5.5", "2. No, go back"): magnum caused that
// dialog, so it presses Enter, once and only with the cursor on the "Yes,
// switch to" option (never confused with a permission prompt, which the
// observer still always denies; the observer leaves the session alone
// while it switches). The switch counts once the dialog is gone, the agent
// is idle and the visible screen's status line names the model ("Opus 5.5
// | <cwd> | …"; for the kind's reset_model, the CLI's default model as a
// limit message named it, unverified when that is unknown). Not confirmed
// within SwitchModelTimeout is ErrTimeout (a dialog still open is backed
// out of with Esc). A switch whose ctx ends after the command was typed
// reads the screen once more: it backs out of a dialog still open, or
// records the switch when the status line names the model already. A
// confirmed switch records the session's model
// (KVSessionModel; removed when model is the role's configured one, or the
// kind's reset_model for a role without one) and appends an
// agent.model_switched event with reason (SwitchLimitHit, SwitchLimited,
// SwitchExpired). A kind without switch_model returns ErrNoModelSwitch, an
// agent that is not idle ErrBusy.
//
// Claude Code also saves a /model choice as the default for new sessions:
// for a claude session SwitchModel snapshots the settings file's model
// before it types the command and restores exactly that afterwards
// (holdDefaultModel; EventDefaultModelRestored when it had changed). Claude
// switches run one at a time for that.
func (m *Manager) SwitchModel(ctx context.Context, s store.Session, model, reason string) error {
	kind := m.sessionKind(s)
	k, ok := m.kindSpec(kind)
	cmd := k.SwitchModelCommand(model)
	if !ok || cmd == "" {
		return fmt.Errorf("agents: switch %s to %s: kind %q: %w", s.Role, model, kind, ErrNoModelSwitch)
	}
	pane := store.Deref(s.HerdrPaneID)
	if pane == "" {
		return fmt.Errorf("agents: switch %s to %s: no pane: %w", s.Role, model, ErrNoSession)
	}
	// The command may also save the model as the CLI's default for new
	// sessions; put that back once the switch ended, whatever its outcome.
	// Taken before the idle check: it may wait for another session's switch.
	restore, err := m.holdDefaultModel(ctx, s, kind)
	if err != nil {
		return fmt.Errorf("agents: switch %s to %s: %w", s.Role, model, err)
	}
	defer restore()
	p, err := m.d.Herdr.PaneGet(ctx, pane)
	if err != nil {
		return fmt.Errorf("agents: switch %s to %s: %w", s.Role, model, mapHerdr(err))
	}
	if !idleStatus(p.AgentStatus) {
		return fmt.Errorf("agents: switch %s to %s: agent is %q: %w", s.Role, model, p.AgentStatus, ErrBusy)
	}
	role, _ := m.roleSpec(Role(s.Role))
	from, _ := m.sessionModel(ctx, s, role)
	names, verify := m.switchNames(ctx, role, k, model)
	ref := paneRef{name: store.Deref(s.AgentName), pane: pane}
	m.setSwitching(s.ID, true) // the observer leaves magnum's own dialog alone
	defer m.setSwitching(s.ID, false)
	if err := m.d.Herdr.PaneRun(ctx, pane, cmd); err != nil {
		return fmt.Errorf("agents: switch %s to %s: %w", s.Role, model, mapHerdr(err))
	}
	// A switch cut short must not leave magnum's own dialog open: the
	// session's next prompt would be refused as blocked.
	settled := false
	defer func() {
		if !settled && ctx.Err() != nil {
			m.settleSwitch(context.WithoutCancel(ctx), ref, cmd, names, verify, func(bctx context.Context) {
				m.switched(bctx, s, role, k, from, model, reason)
			})
		}
	}()
	answered := false
	for poll := 1; ; poll++ {
		if err := m.sleep(ctx, switchPoll); err != nil {
			return fmt.Errorf("agents: switch %s to %s: %w", s.Role, model, err)
		}
		p, err := m.d.Herdr.PaneGet(ctx, pane)
		if err != nil {
			return fmt.Errorf("agents: switch %s to %s: %w", s.Role, model, mapHerdr(err))
		}
		text, err := m.readVisible(ctx, ref)
		if err != nil {
			text = ""
		}
		onYes, dialog := detectSwitchDialog(text)
		switch {
		case dialog && onYes && !answered:
			// magnum's own command asked; the answer is Yes, and only with the
			// cursor on that option.
			if err := m.sendKeys(ctx, ref, "enter"); err != nil {
				return fmt.Errorf("agents: switch %s to %s: confirm: %w", s.Role, model, mapHerdr(err))
			}
			answered = true
		case !dialog && idleStatus(p.AgentStatus) && (!verify || modelShown(text, cmd, names)):
			if !verify {
				m.logf("agents: %s: switched to %s (the CLI's default model is unknown, so unverified)", s.Role, model)
			}
			settled = true
			m.switched(ctx, s, role, k, from, model, reason)
			return nil
		}
		if poll >= switchPolls {
			settled = true
			if dialog {
				_ = m.sendKeys(context.WithoutCancel(ctx), ref, "esc") // back out of magnum's own dialog
			}
			return fmt.Errorf("agents: switch %s to %s: not confirmed after %s (agent %q, dialog %v): %w",
				s.Role, model, SwitchModelTimeout, p.AgentStatus, dialog, ErrTimeout)
		}
	}
}

// settleSwitch ends a switch whose ctx ended after its command was typed
// (ctx here no longer ends): one read of ref's screen, then Esc out of the
// "Switch model?" dialog when it is open, or switched when the status line
// names the model already (verify: names are known). Failures are logged.
func (m *Manager) settleSwitch(ctx context.Context, ref paneRef, cmd string, names []string, verify bool, switched func(context.Context)) {
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		m.logErr(ctx, err, "agents: switch cut short: read %s: %v", ref, err)
		return
	}
	switch _, dialog := detectSwitchDialog(text); {
	case dialog:
		if err := m.sendKeys(ctx, ref, "esc"); err != nil {
			m.logErr(ctx, err, "agents: switch cut short: back out of the dialog in %s: %v", ref, err)
		}
	case verify && modelShown(text, cmd, names):
		switched(ctx)
	}
}

// switched records a confirmed switch of session s to model: its model
// (KVSessionModel, removed when model is the role's configured one, or the
// kind's reset_model for a role without one) and an agent.model_switched
// event.
func (m *Manager) switched(ctx context.Context, s store.Session, role config.Role, k config.Kind, from, model, reason string) {
	bctx := context.WithoutCancel(ctx)
	configured := m.configuredModel(role)
	back := (configured != "" && SameModel(model, configured)) || (configured == "" && model == k.ResetModel)
	var err error
	if back {
		err = m.d.Store.DeleteKV(bctx, KVSessionModel(s.ID))
	} else {
		err = m.d.Store.SetKV(bctx, KVSessionModel(s.ID), model)
	}
	if err != nil {
		m.logErr(ctx, err, "agents: %v", err)
	}
	m.event(bctx, m.prSubject(bctx, s.PRID), "info", EventModelSwitched,
		fmt.Sprintf("%s switched from %s to %s (%s)", s.Role, orUnknown(from), model, reason),
		map[string]any{"session": s.ID, "role": s.Role, "from": orUnknown(from), "to": model, "reason": reason})
}

// switchNames are the names a status line shows for model after a switch
// to it: model and its model family ("opus" for "claude-opus-4-5"); for the
// kind's reset_model, the CLI's default model as a limit message named it.
// verify is false when that default is unknown.
func (m *Manager) switchNames(ctx context.Context, role config.Role, k config.Kind, model string) (names []string, verify bool) {
	if model == k.ResetModel {
		cli, _, _ := m.d.Store.GetKV(ctx, KVKindCLIModel(role.AgentKind()))
		if cli == "" {
			return nil, false
		}
		model = cli
	}
	names = []string{strings.ToLower(model)}
	for _, f := range modelFamilies {
		if f != names[0] && slices.Contains(modelWords(model), f) {
			names = append(names, f)
		}
	}
	return names, true
}

// modelFamilies are the model names Claude Code's status line and limit
// messages use ("Opus 5.5", "You've reached your Fable limit").
var modelFamilies = []string{"fable", "mythos", "opus", "sonnet", "haiku"}

// modelWords splits a model name into its lower-case words.
func modelWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// switchStatusLines is how many of the visible screen's last non-empty
// lines count as its status line.
const switchStatusLines = 3

// modelShown reports whether the visible screen confirms a switch to one of
// names: its status line (the last switchStatusLines non-empty lines, the
// echoed command left out) names it ("Opus 5.5 | <cwd> | …"), or a
// "Set model to …" line does.
func modelShown(text, cmd string, names []string) bool {
	var lines []string
	for l := range strings.SplitSeq(strings.ReplaceAll(text, "\r", ""), "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.Contains(l, cmd) {
			lines = append(lines, t)
		}
	}
	for i, l := range lines {
		if (i >= len(lines)-switchStatusLines || setModelLine.MatchString(l)) && mentions(l, names) {
			return true
		}
	}
	return false
}

// mentions reports whether line names one of names: as one of its words,
// or, for a name of several words ("gpt-6.1-sol"), as written.
func mentions(line string, names []string) bool {
	words, lower := modelWords(line), strings.ToLower(line)
	for _, n := range names {
		if slices.Contains(words, n) || (len(modelWords(n)) > 1 && strings.Contains(lower, n)) {
			return true
		}
	}
	return false
}

// setModelLine is Claude Code's confirmation of /model in the transcript.
var setModelLine = regexp.MustCompile(`(?i)\bset model to\b`)

// The model-switch confirmation Claude Code shows in a session with history
// (Claude Code 2.1):
//
//	Switch model?
//	Your next response will be slower and use more
//	…
//	❯ 1. Yes, switch to Opus 5.5
//	  2. No, go back
var (
	switchTitle = regexp.MustCompile(`(?i)^switch model\?$`)
	switchYes   = regexp.MustCompile(`(?i)^yes,? switch to\b`)
)

// detectSwitchDialog finds a live model-switch confirmation at the bottom of
// a pane's visible screen, as a hooks review is found (detectHooksDialog):
// its title line (the last one on screen) with a numbered "Yes, switch to …"
// option below, the options on adjacent lines and nothing below them but
// blank and key-hint lines. Dialog text that output, the composer or a
// permission prompt follows is not the dialog, and a screen showing a
// permission prompt has none: the Enter magnum sends must never land on an
// approval. onYes reports whether the cursor is on the Yes option. It is
// never a permission prompt (detectPermissionPrompt does not match it), so
// the deny policy never answers it.
func detectSwitchDialog(text string) (onYes, ok bool) {
	if _, ok := detectPermissionPrompt(text); ok {
		return false, false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	title := -1
	for i := len(lines) - 1; i >= 0 && title < 0; i-- {
		if switchTitle.MatchString(stripBox(lines[i])) {
			title = i
		}
	}
	if title < 0 {
		return false, false
	}
	yes := -1
	for i := title + 1; i < len(lines) && yes < 0; i++ {
		if mt := permOptionLine.FindStringSubmatch(stripBox(lines[i])); mt != nil && switchYes.MatchString(strings.TrimSpace(mt[3])) {
			yes, onYes = i, mt[1] != ""
		}
	}
	if yes < 0 {
		return false, false
	}
	last := yes
	for last+1 < len(lines) && permOptionLine.MatchString(stripBox(lines[last+1])) {
		last++
	}
	for _, l := range lines[last+1:] {
		if t := stripBox(l); t != "" && !permHint.MatchString(t) {
			return false, false
		}
	}
	return onYes, true
}

// setSwitching marks session id as switching models (SwitchModel), during
// which the observer neither reports its agent blocked nor answers a
// dialog.
func (m *Manager) setSwitching(id int64, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.switching[id] = true
	} else {
		delete(m.switching, id)
	}
}

func (m *Manager) isSwitching(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.switching[id]
}

// startModel is the model a fresh launch of role passes through Kind.Argv:
// role.Model, or the first fallback that is not limited when the role's
// model is limited and the kind can select a model at launch (a kind
// without model args starts on the role's model and switches before the
// first prompt, see ensureModel). fallback reports the substitution. Limits
// that cannot be read start the role's model (logged).
func (m *Manager) startModel(ctx context.Context, role config.Role, k config.Kind) (model string, fallback bool) {
	if len(k.Model) == 0 || k.SwitchModel == "" || len(k.FallbackModels) == 0 {
		return role.Model, false
	}
	limits, err := activeLimits(ctx, m.d.Store, role.AgentKind(), m.now())
	if err != nil {
		m.logErr(ctx, err, "agents: %s starts on its own model: %v", role.Name, err)
		return role.Model, false
	}
	want := m.roleModel(ctx, role)
	if _, lim := limitOf(limits, want); !lim {
		return role.Model, false
	}
	if f, ok := firstFallback(k.FallbackModels, limits, want); ok {
		return f, true
	}
	return role.Model, false
}

// startedOnFallback records that a session launched on fallback model
// instead of its role's limited one.
func (m *Manager) startedOnFallback(ctx context.Context, s store.Session, role config.Role, model string) {
	ctx = context.WithoutCancel(ctx)
	if err := m.d.Store.SetKV(ctx, KVSessionModel(s.ID), model); err != nil {
		m.logErr(ctx, err, "agents: %v", err)
	}
	from := orUnknown(m.roleModel(ctx, role))
	m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventModelSwitched,
		fmt.Sprintf("%s started on %s: %s is limited", s.Role, model, from),
		map[string]any{"session": s.ID, "role": s.Role, "from": from, "to": model, "reason": SwitchLimited})
}

// ensureModel runs before a prompt to session s: a session whose model is
// limited switches to the first fallback that is not; a session magnum
// switched earlier switches back to its role's model once that one is no
// longer limited (to the kind's reset_model for a role without a configured
// model).
// Failures are logged: the prompt goes out either way, and a limit hit
// again is handled like the first one. Limits that cannot be read switch
// nothing: the session keeps its model.
func (m *Manager) ensureModel(ctx context.Context, s store.Session) {
	kind := m.sessionKind(s)
	k, ok := m.kindSpec(kind)
	if !ok || k.SwitchModel == "" {
		return
	}
	role, ok := m.roleSpec(Role(s.Role))
	if !ok {
		return
	}
	limits, err := activeLimits(ctx, m.d.Store, kind, m.now())
	if err != nil {
		m.logErr(ctx, err, "agents: %s keeps its model before a prompt: %v", s.Role, err)
		return
	}
	cur, switched := m.sessionModel(ctx, s, role)
	if !switched && len(limits) == 0 {
		return
	}
	var target, reason string
	if _, lim := limitOf(limits, cur); lim {
		f, ok := firstFallback(k.FallbackModels, limits, cur)
		if !ok {
			return
		}
		target, reason = f, SwitchLimited
	} else if want := m.roleModel(ctx, role); switched && !SameModel(cur, want) {
		if _, lim := limitOf(limits, want); lim {
			return
		}
		target, reason = cmp.Or(m.configuredModel(role), k.ResetModel), SwitchExpired
	}
	if target == "" {
		return
	}
	if err := m.SwitchModel(ctx, s, target, reason); err != nil {
		m.logErr(ctx, err, "agents: %s before a prompt: %v", s.Role, err)
	}
}

func idleStatus(s herdr.Status) bool { return s == herdr.StatusIdle || s == herdr.StatusDone }

func orUnknown(model string) string {
	if model == "" {
		return UnknownModel
	}
	return model
}

func splitList(s string) []string {
	var out []string
	for x := range strings.SplitSeq(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// event appends an audit row (best effort: failures are logged).
func (m *Manager) event(ctx context.Context, subject, level, kind, msg string, data map[string]any) {
	msg = execx.Redact(msg)
	if m.d.Log != nil {
		execx.LogEvent(m.d.Log, level, subject, kind, execx.Redact("agents: "+msg))
	}
	raw, _ := json.Marshal(data)
	if _, err := m.d.Store.AppendEvent(context.WithoutCancel(ctx), store.Event{Level: level, Subject: &subject,
		Kind: kind, Message: msg, Data: raw}); err != nil {
		m.logErr(ctx, err, "agents: event %s: %v", kind, err)
	}
}
