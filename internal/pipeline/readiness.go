package pipeline

// Verification readiness (backlog 14): in 9 of 18 posted rounds the judge
// skipped or failed a check it wanted (no test database in a per-PR
// worktree, a missing table, the wrong Ruby), and four Ruby-blocked rounds
// burned about 160 agent-minutes before the judge found the cause. Before
// the reviewers start, a round now runs the repository's prepare commands
// and ready probes (config.Config.ReadinessFor) and a built-in Ruby check in
// the checkout, through the login shell the agents' tools use, and tells
// the judge what will not work. A failure never stops the round. When the
// PR changes the pool's schema, the pool's reset_db commands run first, the
// same way but within the release's reset_db budget, and ready_timeout
// starts after them (4 of 9 rounds of a pool repository skipped DB specs on
// a table or column the PR added, because the slot's databases had the base
// schema).

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReadinessPlan is a round's readiness step: the repository's commands
// (config.Readiness) and the slot's environment they run with.
type ReadinessPlan struct {
	// ResetDB are the pool's reset_db commands when the checkout changes the
	// pool's schema_paths (the slot is dirty_schema): run before everything
	// else, in order (Mutates), so the slot's databases carry the PR's
	// schema. Empty when the checkout does not change it.
	ResetDB []string
	// ResetDBTimeout is the budget of the ResetDB commands together, apart
	// from Timeout: the release's (slots.ResetDBTimeout, also when 0). A
	// command still running when it ends is stopped (timeout); the reset_db
	// commands after it are skipped, the others still run.
	ResetDBTimeout time.Duration
	Prepare        []string // run next, in order (Mutates)
	Ready          []string // probes, run after them (exit 0 = ready)
	// Timeout is the budget of the step's other commands (prepare, ready and
	// the Ruby check), which starts once the reset is done; 0 =
	// config.DefaultReadyTimeout. A command still running when it ends is
	// stopped (timeout); the ones after it are skipped.
	Timeout time.Duration
	// Env overlays the daemon's environment: the slot's pool env or the
	// per-PR worktree env, what the slot's own setup commands get.
	Env map[string]string
}

const (
	// ReadinessFile is the JSON file the readiness step writes into the
	// round's report directory, next to the judge's result file.
	ReadinessFile = "readiness.json"
	// ReadinessShell runs every readiness command as `zsh -lc <command>`:
	// the login shell Codex and Claude run their tool commands in, so the
	// commands see the Ruby, Node and database settings the agents see.
	ReadinessShell = "zsh"
	// RubyCheckCommand is the built-in Ruby check's command.
	RubyCheckCommand = "ruby -v"

	readinessLineMax = 200      // runes kept of a command's last output line
	pinFileMax       = 64 << 10 // bytes read of .ruby-version or .mise.toml
)

// readinessFile is the JSON the step writes (ReadinessFile).
type readinessFile struct {
	HeadSHA        string                  `json:"head_sha"`
	RanAt          string                  `json:"ran_at"`
	Timeout        string                  `json:"timeout"`                    // ready_timeout
	ResetDBTimeout string                  `json:"reset_db_timeout,omitempty"` // the schema reset's own budget, when it ran
	Checks         []agents.ReadinessCheck `json:"checks"`
}

// readinessBudget is the time a phase of the step has: what its checks call
// it ("ready_timeout") and how long it is.
type readinessBudget struct {
	name string
	d    time.Duration
}

func (b readinessBudget) String() string { return fmt.Sprintf("%s (%s)", b.name, b.d) }

// readinessCmd is one command of the step.
type readinessCmd struct {
	kind, script string
}

// readiness runs the step and records its outcome for the judge prompt
// (readinessData). It returns ctx's error when the round was cancelled
// meanwhile; every other failure is part of the outcome.
func (rd *round) readiness(ctx context.Context) error {
	if rd.r.Exec == nil || rd.in.Kind == KindContinue {
		return nil
	}
	plan := rd.in.Readiness
	var resets, cmds []readinessCmd
	for _, s := range plan.ResetDB {
		resets = append(resets, readinessCmd{agents.ReadinessResetDB, s})
	}
	for _, s := range plan.Prepare {
		cmds = append(cmds, readinessCmd{agents.ReadinessPrepare, s})
	}
	for _, s := range plan.Ready {
		cmds = append(cmds, readinessCmd{agents.ReadinessReady, s})
	}
	pin, pinned := rubyPin(rd.in.SlotPath)
	if pinned {
		cmds = append(cmds, readinessCmd{agents.ReadinessRuby, RubyCheckCommand})
	}
	if len(resets)+len(cmds) == 0 {
		return nil
	}
	budget := plan.Timeout
	if budget <= 0 {
		budget = config.DefaultReadyTimeout
	}
	resetBudget := plan.ResetDBTimeout
	if resetBudget <= 0 {
		resetBudget = slots.ResetDBTimeout
	}
	start := rd.r.now()
	checks := make([]agents.ReadinessCheck, 0, len(resets)+len(cmds))
	// The reset runs within its own budget; ready_timeout starts after it.
	checks = append(checks, rd.runPhase(ctx, resets, plan.Env, readinessBudget{"reset_db's budget", resetBudget}, pin)...)
	checks = append(checks, rd.runPhase(ctx, cmds, plan.Env, readinessBudget{"ready_timeout", budget}, pin)...)
	res := agents.Readiness{Checks: checks}
	for _, c := range checks {
		if !c.OK {
			res.Failed++
		}
	}
	file := filepath.Join(rd.dir, ReadinessFile)
	rf := readinessFile{HeadSHA: rd.in.TargetSHA, RanAt: start.UTC().Format(time.RFC3339), Timeout: budget.String(), Checks: checks}
	if len(resets) > 0 {
		rf.ResetDBTimeout = resetBudget.String()
	}
	b, err := json.MarshalIndent(rf, "", "  ")
	if err == nil {
		err = fsx.WriteFileAtomic(file, append(b, '\n'), 0o600)
	}
	if err != nil {
		rd.warn(ctx, "readiness: write %s: %v", ReadinessFile, err)
	} else {
		res.File = file
	}
	rd.mu.Lock()
	rd.ready = res
	rd.mu.Unlock()
	rd.readinessEvent(ctx, res, rd.r.now().Sub(start))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// runPhase runs cmds one after the other within budget, which starts now: a
// command gets what is left of it, and the ones after a command that spent
// it are skipped. The Ruby check's outcome is read against pin.
func (rd *round) runPhase(ctx context.Context, cmds []readinessCmd, env map[string]string, budget readinessBudget, pin rubyPinned) []agents.ReadinessCheck {
	deadline := rd.r.now().Add(budget.d)
	checks := make([]agents.ReadinessCheck, 0, len(cmds))
	for _, c := range cmds {
		check := agents.ReadinessCheck{Kind: c.kind, Command: c.script}
		left := deadline.Sub(rd.r.now())
		switch {
		case ctx.Err() != nil:
			check.Status, check.Detail = agents.ReadinessSkipped, "the round was cancelled"
		case left <= 0:
			check.Status, check.Detail = agents.ReadinessSkipped, budget.String()+" was spent before it ran"
		default:
			check = rd.runReadiness(ctx, c, env, left, budget)
			if c.kind == agents.ReadinessRuby && check.Status == agents.ReadinessOK {
				check = rubyVerdict(check, pin)
			} else if c.kind == agents.ReadinessRuby && check.Status == agents.ReadinessFailed {
				check.Detail = fmt.Sprintf("the checkout pins Ruby %s (%s) but `%s` failed (%s)", pin.label(), pin.source, RubyCheckCommand, check.Detail)
			}
		}
		check.OK = check.Status == agents.ReadinessOK
		checks = append(checks, check)
	}
	return checks
}

// runReadiness runs one command as `zsh -lc <script>` in the checkout with
// at most left of the budget.
func (rd *round) runReadiness(ctx context.Context, c readinessCmd, env map[string]string, left time.Duration, budget readinessBudget) agents.ReadinessCheck {
	check := agents.ReadinessCheck{Kind: c.kind, Command: c.script}
	mutates := c.kind == agents.ReadinessPrepare || c.kind == agents.ReadinessResetDB
	res, err := rd.r.Exec.Run(ctx, execx.Cmd{
		Name: ReadinessShell, Args: []string{"-lc", c.script}, Dir: rd.in.SlotPath,
		Env: maps.Clone(env), Unset: gitx.ScrubbedEnv(), Timeout: left,
		Mutates: mutates, Probe: !mutates, Label: "readiness " + c.kind,
	})
	check.Duration = res.Duration.Round(100 * time.Millisecond).String()
	check.LastLine = lastLine(res, err != nil)
	var exit *execx.ExitError
	switch {
	case err == nil:
		check.Status = agents.ReadinessOK
	case ctx.Err() != nil:
		check.Status, check.Detail = agents.ReadinessSkipped, "the round was cancelled while it ran"
	case errors.As(err, &exit):
		check.Status, check.Detail = agents.ReadinessFailed, fmt.Sprintf("exit %d", exit.Code)
	case errors.Is(err, context.DeadlineExceeded):
		check.Status, check.Detail = agents.ReadinessTimeout, "stopped when "+budget.String()+" ran out"
	default:
		check.Status, check.Detail = agents.ReadinessFailed, "could not run: "+oneLine(execx.Redact(errorCause(err)))
	}
	return check
}

// errorCause is a runner error without the command line execx puts in
// front of it (the check names its command already).
func errorCause(err error) string {
	if re, ok := errors.AsType[*execx.RunError](err); ok && re.Err != nil {
		return re.Err.Error()
	}
	return err.Error()
}

// readinessEvent records the step: round.readiness, a warning when a check
// did not pass. Commands come from the configuration; their output stays in
// the readiness file (it is the PR's code talking).
func (rd *round) readinessEvent(ctx context.Context, res agents.Readiness, took time.Duration) {
	var failed []string
	summary := make([]map[string]any, 0, len(res.Checks))
	for _, c := range res.Checks {
		summary = append(summary, map[string]any{"kind": c.Kind, "command": c.Command, "status": c.Status, "detail": c.Detail, "duration": c.Duration})
		if !c.OK {
			failed = append(failed, fmt.Sprintf("%s `%s`: %s", c.Kind, c.Command, cmp.Or(c.Detail, c.Status)))
		}
	}
	level, msg := "info", fmt.Sprintf("readiness: %d of %d checks ok in %s", len(res.Checks)-res.Failed, len(res.Checks), took.Round(time.Second))
	if len(failed) > 0 {
		level = "warn"
		msg += "; " + strings.Join(failed, "; ")
	}
	rd.event(ctx, level, "round.readiness", msg, map[string]any{"checks": summary, "failed": res.Failed, "file": res.File})
}

// readinessData is the readiness outcome for the judge prompt
// (agents.JudgeData.Readiness); zero when no step ran.
func (rd *round) readinessData() agents.Readiness {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	out := rd.ready
	out.Checks = append([]agents.ReadinessCheck(nil), rd.ready.Checks...)
	return out
}

// lastLine is the last non-empty line a command printed (stderr first when
// it failed), redacted, without control characters, shortened.
func lastLine(res execx.Result, failed bool) string {
	streams := [][]byte{res.Stdout, res.Stderr}
	if failed {
		streams = [][]byte{res.Stderr, res.Stdout}
	}
	for _, s := range streams {
		s = bytes.TrimSuffix(s, []byte(execx.TruncationMarker))
		lines := strings.Split(strings.TrimSpace(string(s)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := oneLine(execx.Redact(lines[i])); l != "" {
				return l
			}
		}
	}
	return ""
}

// oneLine drops control characters and invalid UTF-8 from s, trims it and
// cuts it to readinessLineMax runes.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	return textx.Clip(s, readinessLineMax)
}

// rubyPinned is the Ruby version a checkout pins.
type rubyPinned struct {
	source  string // the file: .mise.toml or .ruby-version
	version string // digits and dots ("3.3.4", "3.3"); "" when the pin names no version this check can compare ("latest", a path)
}

func (p rubyPinned) label() string {
	if p.version == "" {
		return "(a version this check cannot compare)"
	}
	return p.version
}

var (
	// rubyPinRe accepts what version files put first: an optional
	// "ruby-" prefix and a dotted version, possibly followed by a suffix.
	rubyPinRe = regexp.MustCompile(`^(?:ruby-)?(\d+(?:\.\d+){0,2})(?:[-.+][0-9A-Za-z.]*)?$`)
	// rubyVersionRe finds the version in `ruby -v` output ("ruby 3.3.4p94 (...)").
	rubyVersionRe = regexp.MustCompile(`^ruby (\d+\.\d+\.\d+)`)
)

// rubyPin reads the Ruby version the checkout pins: .mise.toml's [tools]
// ruby, else .ruby-version. Both files belong to the PR, so they are read
// through an os.Root confined to the checkout, size-limited, and only a
// dotted version ever leaves this function.
func rubyPin(checkout string) (rubyPinned, bool) {
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return rubyPinned{}, false
	}
	defer root.Close()
	read := func(name string) ([]byte, bool) {
		f, err := root.Open(name)
		if err != nil {
			return nil, false
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(f, pinFileMax))
		return b, err == nil
	}
	if b, ok := read(".mise.toml"); ok {
		if v, ok := miseRuby(b); ok {
			return rubyPinned{source: ".mise.toml", version: pinVersion(v)}, true
		}
	}
	if b, ok := read(".ruby-version"); ok {
		line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
		return rubyPinned{source: ".ruby-version", version: pinVersion(strings.TrimSpace(line))}, true
	}
	return rubyPinned{}, false
}

// miseRuby is the ruby entry of a .mise.toml's [tools]: a string, the first
// of a list, or a table's version.
func miseRuby(b []byte) (string, bool) {
	var doc struct {
		Tools map[string]any `toml:"tools"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return "", false
	}
	v, ok := doc.Tools["ruby"]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return s, true
			}
		}
	case map[string]any:
		if s, ok := t["version"].(string); ok {
			return s, true
		}
	}
	return "", true // pinned, in a shape this check does not compare
}

// pinVersion is the dotted version of a pin ("ruby-3.3.4" → "3.3.4"); ""
// when it names none.
func pinVersion(s string) string {
	if m := rubyPinRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	return ""
}

// rubyVerdict judges a `ruby -v` that ran: the version it printed must
// match the pin (a pin of "3.3" accepts any 3.3.x).
func rubyVerdict(check agents.ReadinessCheck, pin rubyPinned) agents.ReadinessCheck {
	m := rubyVersionRe.FindStringSubmatch(check.LastLine)
	switch {
	case m == nil:
		check.Status = agents.ReadinessFailed
		check.Detail = fmt.Sprintf("the checkout pins Ruby %s (%s) but `%s` printed no Ruby version", pin.label(), pin.source, RubyCheckCommand)
	case pin.version == "":
		check.Detail = fmt.Sprintf("runs Ruby %s; the pin in %s is not a version this check compares", m[1], pin.source)
	case m[1] == pin.version || strings.HasPrefix(m[1], pin.version+"."):
		check.Detail = "runs Ruby " + m[1]
	default:
		check.Status = agents.ReadinessFailed
		check.Detail = fmt.Sprintf("the checkout pins Ruby %s (%s) but `%s -lc '%s'` in it runs %s", pin.version, pin.source, ReadinessShell, RubyCheckCommand, m[1])
	}
	return check
}
