// Package execx is the single choke point for every subprocess magnum runs
// (git, gh, mysql, mise exec, launchctl, osascript, codex/claude probes).
//
// Every call carries a timeout, runs in its own process group so a cancelled
// context terminates the whole tree (SIGTERM, then SIGKILL), gets an explicit
// env overlay and a working directory, captures at most MaxOutput per stream,
// and logs a redacted transcript. Tests use Fake; --dry-run wraps
// the real runner in DryRun so mutating commands are printed instead of run.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// Cmd describes one subprocess.
type Cmd struct {
	Name string   // executable; resolved through PATH unless absolute
	Args []string // argv after the executable
	Dir  string   // working directory ("" = inherit)
	// Env overlays the parent environment. An empty value blanks the variable
	// (exported as KEY= so shells and Ruby .presence treat it as unset).
	Env map[string]string
	// Unset removes variables from the inherited environment entirely (git,
	// for one, treats an empty GIT_DIR differently from an absent one). A key
	// that is also in Env keeps its Env value.
	Unset []string
	// Timeout bounds the run; zero means DefaultTimeout.
	Timeout time.Duration
	// Mutates marks commands that change state. DryRun refuses to execute them.
	Mutates bool
	// Stdin, when non-nil, is written to the process.
	Stdin []byte
	// Label is a short human name used in logs ("git fetch pr 123").
	Label string
	// Probe marks a command whose non-zero exit is an expected answer (is
	// this a git repository? does this ref exist?): a failed exit is logged
	// at Debug instead of Warn. Start failures and timeouts still warn.
	Probe bool
	// Expected, when set, picks the non-zero exits that are expected answers,
	// logged at Debug as Probe logs every one: gh api exits 1 for a 404 as
	// for a 500, and only its output tells them apart. Other exits still warn.
	Expected func(Result) bool
	// NoTTY starts the process in a new session, without a controlling
	// terminal (setsid), as launchd starts the daemon. An interactive shell
	// (`zsh -ic`) run from a CLI in a terminal otherwise shares that
	// terminal from a background process group, where shell startup that
	// touches the terminal stops it until the timeout.
	NoTTY bool
}

// Result is the outcome of a finished subprocess.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	Code     int
	Duration time.Duration
	// Truncated reports that a stream exceeded MaxOutput: the kept prefix is
	// followed by TruncationMarker and the rest was discarded.
	Truncated bool
}

// Out returns trimmed stdout.
func (r Result) Out() string { return strings.TrimSpace(string(r.Stdout)) }

// stderrKeep is how much of a failed command's stderr its ExitError keeps
// from each end.
const stderrKeep = 32 << 10

// ExitError is returned when the process ran but exited non-zero.
type ExitError struct {
	Cmd  Cmd
	Code int
	// Stderr is what the process wrote to stderr, redacted, and when longer
	// than 64 KiB only its first and last 32 KiB around a "[N bytes cut]"
	// line (Result.Stderr keeps it whole): the error is wrapped into step
	// rows, a PR's last_error and log lines.
	Stderr string
	msg    string // the text of Error, rendered once by exitError
}

// exitError is the ExitError of c exiting code with stderr: the excerpt is
// taken from the redacted stderr, so the cut never splits a token before it
// is masked, and the text is rendered once.
func exitError(c Cmd, code int, stderr []byte) *ExitError {
	e := &ExitError{Cmd: c, Code: code, Stderr: stderrExcerpt(Redact(string(stderr)))}
	e.msg = e.render(e.Stderr)
	return e
}

func (e *ExitError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	// Built by hand (a fake): its Stderr was never redacted or cut.
	return e.render(stderrExcerpt(Redact(e.Stderr)))
}

func (e *ExitError) render(stderr string) string {
	return fmt.Sprintf("%s exited %d: %s", Redact(e.Cmd.String()), e.Code, strings.TrimSpace(stderr))
}

// stderrExcerpt is s whole when it fits in two stderrKeep, else its first
// stderrKeep bytes, a "[N bytes cut]" line and its last stderrKeep bytes
// from the first line start in them, as a slot log keeps a stream's tail.
// Neither cut splits a UTF-8 sequence.
func stderrExcerpt(s string) string {
	if len(s) <= 2*stderrKeep {
		return s
	}
	head, tail := stderrKeep, len(s)-stderrKeep
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	if i := strings.IndexByte(s[tail:], '\n'); i >= 0 && tail+i+1 < len(s) {
		tail += i + 1
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	var b strings.Builder
	b.Grow(head + len(s) - tail + 32)
	b.WriteString(s[:head])
	if !strings.HasSuffix(s[:head], "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "[%d bytes cut]\n", tail-head)
	b.WriteString(s[tail:])
	return b.String()
}

// RunError is a runner failure other than a non-zero exit: a missing working
// directory, a failed start, a cancelled or timed-out context. Its message
// shows the redacted command line; Unwrap exposes the cause (context errors,
// fs.ErrNotExist, exec.ErrNotFound) for errors.Is.
type RunError struct {
	Cmd Cmd
	Err error
}

func (e *RunError) Error() string { return Redact(e.Cmd.String()) + ": " + e.Err.Error() }
func (e *RunError) Unwrap() error { return e.Err }

// String renders the command the way a shell would show it: every argument
// that is not plainly shell-safe is single-quoted (see ShellQuote). The result
// is not redacted; pass it through Redact before logging.
func (c Cmd) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, ShellQuote(c.Name))
	for _, a := range c.Args {
		parts = append(parts, ShellQuote(a))
	}
	return strings.Join(parts, " ")
}

// ShellQuote single-quotes s unless it consists only of characters a POSIX
// shell never interprets; the empty string becomes two single quotes. Newlines and carriage
// returns inside quotes are kept verbatim; callers that need one log line
// should escape them first.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	})
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DefaultTimeout applies when Cmd.Timeout is zero.
const DefaultTimeout = 2 * time.Minute

// Runner executes commands. Implementations: Real, Fake, DryRun.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Logger receives one line per finished command (already redacted).
type Logger interface {
	Printf(format string, args ...any)
}

// LevelLogger is a Logger that also takes a level per line. Real logs a
// command that succeeded at slog.LevelDebug and one that failed (a non-zero
// exit, a failed start, a timeout) at slog.LevelWarn when its Log
// implements it, except one the caller's context cancellation ended (the
// daemon stopping), which is logged at slog.LevelDebug; a plain Logger gets
// every line through Printf.
type LevelLogger interface {
	Logger
	Logf(level slog.Level, format string, args ...any)
}

// AttrLogger is a LevelLogger that also takes slog attributes with a line:
// the bridge to the daemon's slog.Logger (app.Printf) keeps an audit
// event's subject and kind as attributes of its record (LogEvent).
type AttrLogger interface {
	LevelLogger
	LogAttrs(level slog.Level, msg string, attrs ...slog.Attr)
}

// LogAt sends one line to l: at level through Logf when l is a LevelLogger,
// else through Printf (a plain Logger has no levels). A nil l drops it.
func LogAt(l Logger, level slog.Level, format string, args ...any) {
	switch ll := l.(type) {
	case nil:
	case LevelLogger:
		ll.Logf(level, format, args...)
	default:
		l.Printf(format, args...)
	}
}

// LogEvent mirrors an audit event to l as line: at the event's level
// (EventLevel), with its subject and kind as attributes when l is an
// AttrLogger, else as LogAt sends it. The caller redacts line.
func LogEvent(l Logger, level, subject, kind, line string) {
	if al, ok := l.(AttrLogger); ok {
		al.LogAttrs(EventLevel(level), line, slog.String("subject", subject), slog.String("kind", kind))
		return
	}
	LogAt(l, EventLevel(level), "%s", line)
}

// EventLevel is the slog level of an audit event's level ("debug", "info",
// "warn", "error"; anything else is info), for the line that mirrors the
// event in the log.
func EventLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// IsStop reports whether a failure is the daemon stopping rather than a
// fault: ctx ended (a shutdown cancels it, an abort cancels a round's) or
// err says the work was canceled.
func IsStop(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

// FailLevel is the level of a line that reports err, a failure the caller
// tolerates: slog.LevelWarn, or slog.LevelInfo when err is the daemon
// stopping (IsStop), which cuts short every call in flight and is no fault.
func FailLevel(ctx context.Context, err error) slog.Level {
	if IsStop(ctx, err) {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// Real runs commands with os/exec.
type Real struct {
	// Log, when set, receives a redacted one-line transcript per command:
	// at debug level for a success and at warn level for a failure when it
	// is a LevelLogger (a command the caller's cancellation ended: debug).
	Log Logger
	// BaseEnv, when non-empty, replaces os.Environ() as the parent environment.
	BaseEnv []string
}

// MaxOutput caps each captured stream (stdout and stderr separately). Output
// past it is drained and discarded so the child never blocks on a full pipe;
// the kept prefix is followed by TruncationMarker and Result.Truncated is set.
const MaxOutput = 8 << 20

// TruncationMarker is appended to a stream cut at MaxOutput.
const TruncationMarker = "\n[execx: output truncated at 8 MiB]\n"

// killGrace is how long a cancelled process group gets between SIGTERM and
// SIGKILL. git removes its index.lock and ref locks on SIGTERM but cannot on
// SIGKILL. It is also the WaitDelay after which descendants still holding the
// output pipes are killed. A variable so tests can shorten it.
var killGrace = 5 * time.Second

// Run executes the command in its own process group. When the context is
// cancelled or the timeout elapses the whole group gets SIGTERM, then SIGKILL
// after killGrace. Descendants that outlive the leader while still holding
// its output pipes are killed too. Every error renders the command redacted.
func (r *Real) Run(ctx context.Context, c Cmd) (Result, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// os/exec reports a missing Dir as "fork/exec <binary>: no such file or
	// directory", which reads like the executable is missing.
	if c.Dir != "" {
		if _, err := os.Stat(c.Dir); err != nil {
			rerr := &RunError{Cmd: c, Err: fmt.Errorf("working directory: %w", err)}
			if r.Log != nil {
				LogAt(r.Log, slog.LevelWarn, "exec %s dir=%s code=-1 dur=0s err=%v", Redact(c.String()), c.Dir, rerr.Err)
			}
			return Result{Code: -1}, rerr
		}
	}

	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = mergeEnv(r.baseEnv(), c.Env, c.Unset)
	cmd.SysProcAttr = procAttr(c)
	var killer atomic.Pointer[time.Timer]
	cmd.Cancel = func() error {
		// Signal the whole group, not just the leader: SIGTERM first so git
		// can drop its lock files, SIGKILL for whatever is left after the grace.
		pgid := cmd.Process.Pid
		killer.Store(time.AfterFunc(killGrace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }))
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = killGrace
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	stdout, stderr := &capWriter{limit: MaxOutput}, &capWriter{limit: MaxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	if cmd.Process != nil {
		reapGroup(cmd.Process.Pid, killer.Load(), errors.Is(err, exec.ErrWaitDelay))
	}
	res := Result{Stdout: stdout.bytes(), Stderr: stderr.bytes(), Duration: time.Since(start), Truncated: stdout.truncated || stderr.truncated}
	res.Code = -1 // the process never started
	if cmd.ProcessState != nil {
		res.Code = cmd.ProcessState.ExitCode()
	}
	if r.Log != nil {
		trunc := ""
		if res.Truncated {
			trunc = " truncated"
		}
		level := slog.LevelDebug
		// A command the caller's cancellation ended (the daemon stopping) is
		// routine; its own timeout or the caller's deadline still warns.
		stopped := errors.Is(caller.Err(), context.Canceled)
		answer := res.Code > 0 && ctx.Err() == nil && (c.Probe || c.Expected != nil && c.Expected(res))
		if err != nil && !stopped && !answer {
			level = slog.LevelWarn
		}
		LogAt(r.Log, level, "exec %s dir=%s code=%d dur=%s%s err=%v", Redact(c.String()), c.Dir, res.Code, res.Duration.Round(time.Millisecond), trunc, err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return res, &RunError{Cmd: c, Err: ctx.Err()}
		}
		if _, exited := errors.AsType[*exec.ExitError](err); exited {
			return res, exitError(c, res.Code, res.Stderr)
		}
		return res, &RunError{Cmd: c, Err: err}
	}
	return res, nil
}

// reapGroup finishes off the process group of a command that has returned.
// leaked means descendants outlived the leader holding its output pipes
// (os/exec gave up after WaitDelay): they are killed now. Otherwise a pending
// SIGKILL from a cancellation is left armed only while the group still has
// members, so a stopped timer never fires at a recycled group id.
func reapGroup(pgid int, killer *time.Timer, leaked bool) {
	if leaked {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if killer != nil && errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		killer.Stop()
	}
}

// capWriter keeps the first limit bytes written to it and counts the rest as
// truncated. It never fails, so the child keeps draining into the pipe.
type capWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := max(w.limit-w.buf.Len(), 0)
	if len(p) > room {
		w.truncated = true
	}
	w.buf.Write(p[:min(len(p), room)])
	return len(p), nil
}

func (w *capWriter) bytes() []byte {
	if w.truncated {
		w.buf.WriteString(TruncationMarker)
	}
	return w.buf.Bytes()
}

func (r *Real) baseEnv() []string {
	if len(r.BaseEnv) > 0 {
		return r.BaseEnv
	}
	return os.Environ()
}

// mergeEnv overlays env onto base: keys in env win (empty values are kept as
// KEY=), keys in unset that are not in env are dropped entirely.
// MergeEnv returns base with env's keys overriding and unset's keys removed;
// it is what Run gives every subprocess and what other runners (the CLI's
// TTY runner) use to honour Cmd.Env and Cmd.Unset the same way.
func MergeEnv(base []string, env map[string]string, unset []string) []string {
	return mergeEnv(base, env, unset)
}

func mergeEnv(base []string, env map[string]string, unset []string) []string {
	if len(env) == 0 && len(unset) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(env))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := env[k]; override || slices.Contains(unset, k) {
			continue
		}
		out = append(out, kv)
	}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// secretPattern matches GitHub tokens (OAuth, installation, personal, user,
// refresh, fine-grained), three-segment JWTs, Authorization header values
// (Bearer, token, Basic) and PEM private-key blocks, including a block cut
// off before its END line.
var secretPattern = regexp.MustCompile(strings.Join([]string{
	`(?:gho_|ghs_|ghp_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+`,
	`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`,
	`(?i:authorization:\s*(?:bearer|token|basic)\s+)\S+`,
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`,
}, "|"))

// Redact masks GitHub tokens, JWTs, Authorization header values and PEM
// private keys in free text. An Authorization header keeps its name.
func Redact(s string) string {
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		if i := strings.IndexByte(m, ':'); i >= 0 && strings.HasPrefix(strings.ToLower(m), "authorization") {
			return m[:i+1] + " <redacted>"
		}
		return "<redacted>"
	})
}

// DryRun executes read-only commands through Inner and only prints mutating ones.
type DryRun struct {
	Inner Runner
	Out   Logger
	mu    sync.Mutex
	// Planned records the mutating commands that would have run.
	Planned []Cmd
}

// Run implements Runner.
func (d *DryRun) Run(ctx context.Context, c Cmd) (Result, error) {
	if !c.Mutates {
		return d.Inner.Run(ctx, c)
	}
	d.mu.Lock()
	d.Planned = append(d.Planned, c)
	d.mu.Unlock()
	if d.Out != nil {
		d.Out.Printf("dry-run: would run %s (dir=%s)", Redact(c.String()), c.Dir)
	}
	return Result{}, nil
}

// Rule scripts one Fake response. Prefix matches the leading argv words
// (Name followed by Args); the first matching rule wins.
type Rule struct {
	Prefix []string
	Result Result
	Err    error
	// Fn, when set, computes the response from the actual command.
	Fn func(c Cmd) (Result, error)
}

// Fake is a scripted Runner for tests. Unmatched commands fail loudly.
type Fake struct {
	Rules []Rule
	mu    sync.Mutex
	Calls []Cmd
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	f.mu.Unlock()
	argv := append([]string{c.Name}, c.Args...)
	for _, rule := range f.Rules {
		if hasPrefix(argv, rule.Prefix) {
			if rule.Fn != nil {
				return rule.Fn(c)
			}
			if rule.Err != nil {
				return rule.Result, rule.Err
			}
			if rule.Result.Code != 0 {
				return rule.Result, exitError(c, rule.Result.Code, rule.Result.Stderr)
			}
			return rule.Result, nil
		}
	}
	return Result{}, fmt.Errorf("execx.Fake: no rule for %s", c.String())
}

// CallsWithPrefix returns the recorded calls whose argv starts with prefix.
func (f *Fake) CallsWithPrefix(prefix ...string) []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Cmd
	for _, c := range f.Calls {
		if hasPrefix(append([]string{c.Name}, c.Args...), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func hasPrefix(argv, prefix []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for i, p := range prefix {
		if argv[i] != p {
			return false
		}
	}
	return true
}

// procAttr puts the process in its own process group, which the timeout
// signals as a whole; with NoTTY the group leads a new session, which has
// no controlling terminal.
func procAttr(c Cmd) *syscall.SysProcAttr {
	if c.NoTTY {
		return &syscall.SysProcAttr{Setsid: true}
	}
	return &syscall.SysProcAttr{Setpgid: true}
}
