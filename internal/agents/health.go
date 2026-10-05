package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// HealthKind classifies an agent pane's recent output.
type HealthKind string

// Health kinds.
const (
	HealthOK            HealthKind = "ok"
	HealthLoginRequired HealthKind = "login_required" // pause the kind; e.g. `codex login` / `claude auth login`
	HealthModelLimit    HealthKind = "model_limit"    // one model's cap: switch the session to a fallback model (Model, ResetAt)
	HealthUsageLimit    HealthKind = "usage_limit"    // pause until ResetAt (fallback 1h, doubling)
	HealthOverloaded    HealthKind = "overloaded"     // 5xx/429/reconnecting: retry with backoff
	HealthStalled       HealthKind = "stalled"        // turn interrupted / context full
	HealthBlocked       HealthKind = "blocked"        // approval (or other trust) dialog: needs the human
	HealthTrustDialog   HealthKind = "trust_dialog"   // Codex/Claude first-launch folder trust: AnswerTrustDialog
)

// Health is the classifier's verdict.
type Health struct {
	Kind    HealthKind
	ResetAt *time.Time // usage_limit and model_limit, when the text names a reset time
	// Model is the limited model a model_limit pattern captured (its "model"
	// group, lower case); "" = the session's current model.
	Model  string
	Detail string // the matching line, redacted, at most 300 bytes
}

type healthRule struct {
	kind HealthKind
	re   *regexp.Regexp
}

func compileRules(kind HealthKind, exprs ...string) []healthRule {
	out := make([]healthRule, 0, len(exprs))
	for _, e := range exprs {
		out = append(out, healthRule{kind, regexp.MustCompile(`(?i)` + e)})
	}
	return out
}

// The built-in classifier lists every kind shares: the CLIs' first-launch
// trust dialogs, approval dialogs and stalled turns (a kind's
// health_patterns cover login, usage limits and overloads).
var (
	trustDialogRules = compileRules(HealthTrustDialog,
		`trust this folder\?`,          // Codex "Folder access"
		`\btrust and continue\b`,       // Codex option 1
		`\byes, i trust this folder\b`, // Claude "Accessing workspace"
	)
	blockedRules = compileRules(HealthBlocked,
		`allow\?\s*\(y/n\)`,
		`would you like to (?:run the following command|make the following edits|allow)`,
		`do you want to (?:proceed|make this edit|create|allow|run|overwrite|apply)\b`,
		`do you trust the (?:files|contents|authors)`,
		`allow codex to work in this folder`,
		`\byes, proceed\b`,
		`press enter to confirm`,
		`waiting for (?:your )?approval`,
		`\bapproval required\b`,
	)
	stalledRules = compileRules(HealthStalled,
		`conversation interrupted`,
		`interrupted by user`,
		`\binterrupted\b.{0,5}what should claude do`,
		`ran out of room in the model'?s context window`,
		`context (?:window|length) (?:exceeded|is full)`,
		`prompt is too long`,
	)
)

// defaultHealth is config.DefaultHealthPatterns compiled.
var defaultHealth = func() config.HealthRegexps {
	rx, err := config.DefaultHealthPatterns().Compile()
	if err != nil {
		panic(err)
	}
	return rx
}()

// rulesFor orders a kind's patterns and the built-in lists by tie-break
// priority (same line: earlier rule wins): login_required > model_limit >
// usage_limit > trust_dialog > blocked > overloaded > stalled.
func rulesFor(rx config.HealthRegexps) []healthRule {
	wrap := func(kind HealthKind, res []*regexp.Regexp) []healthRule {
		out := make([]healthRule, 0, len(res))
		for _, re := range res {
			out = append(out, healthRule{kind, re})
		}
		return out
	}
	var rules []healthRule
	rules = append(rules, wrap(HealthLoginRequired, rx.LoginRequired)...)
	rules = append(rules, wrap(HealthModelLimit, rx.ModelLimit)...)
	rules = append(rules, wrap(HealthUsageLimit, rx.UsageLimit)...)
	rules = append(rules, trustDialogRules...)
	rules = append(rules, blockedRules...)
	rules = append(rules, wrap(HealthOverloaded, rx.Overloaded)...)
	rules = append(rules, stalledRules...)
	return rules
}

var defaultRules = rulesFor(defaultHealth)

// healthRules returns (and caches) the classifier rules of a kind: its
// [kinds.<kind>] health_patterns; the defaults for "" and undeclared kinds,
// and when the patterns do not compile (logged; Config validation normally
// refuses them).
func (m *Manager) healthRules(kind string) []healthRule {
	m.mu.Lock()
	rules, ok := m.health[kind]
	m.mu.Unlock()
	if ok {
		return rules
	}
	rules = defaultRules
	if k, ok := m.kindSpec(kind); ok {
		rx, err := k.HealthPatterns.Compile()
		if err != nil {
			m.logf("agents: kinds.%s health_patterns: %v (using the defaults)", kind, err)
		} else {
			rules = rulesFor(rx)
		}
	}
	m.mu.Lock()
	m.health[kind] = rules
	m.mu.Unlock()
	return rules
}

// ClassifyAt is ClassifyWith with config.DefaultHealthPatterns.
func ClassifyAt(text string, now time.Time) Health { return classify(defaultRules, text, now) }

// ClassifyWith classifies recent pane output of an agent session with a
// kind's compiled health patterns (config.HealthPatterns.Compile) plus the
// built-in trust-dialog, approval and stalled patterns. The match on the
// latest line wins (pane text keeps old errors above newer output); on one
// line login_required > model_limit > usage_limit > trust_dialog > blocked >
// overloaded > stalled. A model_limit match sets Model from the pattern's
// "model" group. For usage_limit and model_limit, ResetAt comes from "try
// again at 3:45 PM", "try again at Oct 5th, 2026 9:05 AM", "try again in 1
// day 4 hours", "resets 5pm (Europe/Kyiv)", "resets Oct 7, 9am" or "limit
// reached|<epoch>", read
// relative to now (bare clock times in now's zone unless the text names one;
// a time more than 12h past means tomorrow) and capped at MaxReset ahead.
// No match is HealthOK.
func ClassifyWith(rx config.HealthRegexps, text string, now time.Time) Health {
	return classify(rulesFor(rx), text, now)
}

func classify(rules []healthRule, text string, now time.Time) Health {
	bestLine, bestRule := -1, -1
	var bestLoc []int
	for i, r := range rules {
		locs := r.re.FindAllStringIndex(text, -1)
		if len(locs) == 0 {
			continue
		}
		loc := locs[len(locs)-1]
		line := strings.Count(text[:loc[0]], "\n")
		if line > bestLine {
			bestLine, bestRule, bestLoc = line, i, loc
		}
	}
	if bestRule < 0 {
		return Health{Kind: HealthOK}
	}
	start := strings.LastIndexByte(text[:bestLoc[0]], '\n') + 1
	end := len(text)
	if i := strings.IndexByte(text[bestLoc[0]:], '\n'); i >= 0 {
		end = bestLoc[0] + i
	}
	h := Health{Kind: rules[bestRule].kind, Detail: detail(text[start:end])}
	if h.Kind == HealthModelLimit {
		re := rules[bestRule].re
		if i := re.SubexpIndex("model"); i > 0 {
			if m := re.FindStringSubmatch(text[bestLoc[0]:bestLoc[1]]); m != nil {
				h.Model = strings.ToLower(m[i])
			}
		}
	}
	if h.Kind == HealthUsageLimit || h.Kind == HealthModelLimit {
		// The reset may sit on the next line when the TUI wrapped it.
		windowEnd := end
		if windowEnd < len(text) {
			if i := strings.IndexByte(text[windowEnd+1:], '\n'); i >= 0 {
				windowEnd = windowEnd + 1 + i
			} else {
				windowEnd = len(text)
			}
		}
		h.ResetAt = parseReset(text[start:windowEnd], now)
	}
	return h
}

func detail(line string) string {
	s := execx.Redact(strings.TrimSpace(line))
	const maxDetail = 300
	if len(s) <= maxDetail {
		return s
	}
	s = s[:maxDetail]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

var (
	reEpoch    = regexp.MustCompile(`(?i)limit reached\|(\d{9,11})`)
	reAtClock  = regexp.MustCompile(`(?i)try again at\s+(\d{1,2}):(\d{2})\s*([ap]\.?m\.?)`)
	reAtDate   = regexp.MustCompile(`(?i)try again at\s+([a-z]{3})[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4}),?\s+(?:at\s+)?(\d{1,2}):(\d{2})\s*([ap]\.?m\.?)`)
	reIn       = regexp.MustCompile(`(?i)(?:try again|resets?|available again)\s+in\s+(\d[0-9a-z ,]*)`)
	reResets   = regexp.MustCompile(`(?i)\bresets?(?:\s+at)?\s+(?:([a-z]{3})[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\b\.?(?:\s*\(([^)]+)\))?`)
	reDuration = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
)

var months = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12}

// MaxReset caps a usage-limit reset read from pane text: the text may quote
// or fake a far-off date, so a pause never lasts longer than this from one
// reading (a longer limit is simply hit again).
const MaxReset = 24 * time.Hour

// parseReset finds a usage-limit reset time in s, at most MaxReset after
// now.
func parseReset(s string, now time.Time) *time.Time {
	t := parseResetTime(s, now)
	if t != nil && t.After(now.Add(MaxReset)) {
		capped := now.Add(MaxReset)
		return &capped
	}
	return t
}

func parseResetTime(s string, now time.Time) *time.Time {
	if m := reEpoch.FindStringSubmatch(s); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		t := time.Unix(sec, 0)
		return &t
	}
	if m := reAtDate.FindStringSubmatch(s); m != nil {
		mon, ok := months[strings.ToLower(m[1])]
		if ok {
			day, _ := strconv.Atoi(m[2])
			year, _ := strconv.Atoi(m[3])
			h, mi := clock12(m[4], m[5], m[6])
			t := time.Date(year, mon, day, h, mi, 0, 0, now.Location())
			return &t
		}
	}
	if m := reAtClock.FindStringSubmatch(s); m != nil {
		h, mi := clock12(m[1], m[2], m[3])
		return todayOrNext(now, now.Location(), h, mi)
	}
	if m := reIn.FindStringSubmatch(s); m != nil {
		if d := parseDuration(m[1]); d > 0 {
			t := now.Add(d)
			return &t
		}
	}
	if m := reResets.FindStringSubmatch(s); m != nil {
		loc := now.Location()
		if m[6] != "" {
			if l, err := time.LoadLocation(strings.TrimSpace(m[6])); err == nil {
				loc = l
			}
		}
		mins := m[4]
		if mins == "" {
			mins = "0"
		}
		h, mi := clock12(m[3], mins, m[5]+"m")
		if mon, ok := months[strings.ToLower(m[1])]; ok && m[2] != "" {
			day, _ := strconv.Atoi(m[2])
			nl := now.In(loc)
			t := time.Date(nl.Year(), mon, day, h, mi, 0, 0, loc)
			if now.Sub(t) > 180*24*time.Hour {
				t = t.AddDate(1, 0, 0)
			}
			return &t
		}
		return todayOrNext(now, loc, h, mi)
	}
	return nil
}

// clock12 converts "3", "45", "PM" to 24h hour and minute.
func clock12(hs, ms, ampm string) (int, int) {
	h, _ := strconv.Atoi(hs)
	mi, _ := strconv.Atoi(ms)
	pm := strings.HasPrefix(strings.ToLower(ampm), "p")
	h %= 12
	if pm {
		h += 12
	}
	return h, mi
}

// todayOrNext is h:mi on now's date in loc; more than 12h in the past means
// the text meant tomorrow (a reset after midnight).
func todayOrNext(now time.Time, loc *time.Location, h, mi int) *time.Time {
	nl := now.In(loc)
	t := time.Date(nl.Year(), nl.Month(), nl.Day(), h, mi, 0, 0, loc)
	if now.Sub(t) > 12*time.Hour {
		t = t.AddDate(0, 0, 1)
	}
	return &t
}

func parseDuration(s string) time.Duration {
	var d time.Duration
	for _, m := range reDuration.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		switch u := strings.ToLower(m[2]); {
		case strings.HasPrefix(u, "d"):
			d += time.Duration(n) * 24 * time.Hour
		case strings.HasPrefix(u, "h"):
			d += time.Duration(n) * time.Hour
		case strings.HasPrefix(u, "m"):
			d += time.Duration(n) * time.Minute
		default:
			d += time.Duration(n) * time.Second
		}
	}
	return d
}

// Preflight checks that the agent CLI of kind is logged in before a start
// or prompt, with the kind's read-only login_check command (split on spaces,
// run without a shell, with the kind's env laid over the daemon's) judged by
// its login_ok rule (config.Kind.LoggedIn): codex `codex login status` must
// print "Logged in" (on either stream; its exit code is unreliable), claude
// `claude auth status` JSON must have "loggedIn": true. A kind without
// login_check (droid, omp) and the shell kind are not checked. A logged-out
// CLI wraps ErrLoginRequired; a CLI that could not run (missing binary,
// timeout) returns that error instead; output the rule cannot read (a format
// change) is logged and taken as logged in, so it never blocks the reviews.
// An undeclared kind is an error. PreflightRole checks with a role's own
// environment.
func (m *Manager) Preflight(ctx context.Context, kind string) error {
	var env map[string]string
	if k, ok := m.kindSpec(kind); ok {
		env = k.Env
	}
	return m.preflight(ctx, kind, env)
}

// PreflightRole is Preflight for the agent kind of role (config.Role.AgentKind:
// a shell role's tool) with the environment the role's pane gets
// (config.Config.RoleEnv: the kind's env, then the role's), so a role that
// points its CLI at another account (CODEX_HOME, CLAUDE_CONFIG_DIR) is
// checked in that account.
func (m *Manager) PreflightRole(ctx context.Context, role config.Role) error {
	var env map[string]string
	if m.d.Config != nil {
		env = m.d.Config.RoleEnv(role)
	}
	return m.preflight(ctx, role.AgentKind(), env)
}

func (m *Manager) preflight(ctx context.Context, kind string, env map[string]string) error {
	if kind == "" || kind == KindShell {
		return nil
	}
	k, ok := m.kindSpec(kind)
	if !ok {
		return fmt.Errorf("agents: preflight: unknown agent kind %q", kind)
	}
	argv := k.LoginArgv()
	if len(argv) == 0 {
		return nil
	}
	check := strings.Join(argv, " ")
	res, err := m.d.Runner.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Env: env, Timeout: PreflightTimeout, Label: check})
	var ee *execx.ExitError
	if err != nil && !errors.As(err, &ee) {
		return fmt.Errorf("agents: %s: %w", check, err)
	}
	stdout, stderr := string(res.Stdout), string(res.Stderr)
	loggedIn, readable := k.LoggedIn(stdout, stderr, err == nil)
	switch {
	case !readable:
		m.logf("agents: %s: cannot read %q with login_ok %q; assuming logged in", check, detail(firstLine(stdout+"\n"+stderr)), k.LoginOK)
		return nil
	case !loggedIn:
		return fmt.Errorf("agents: %s: %s: %q: %w", kind, check, detail(firstLine(stdout+"\n"+stderr)), ErrLoginRequired)
	}
	return nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
