// Package slots manages the checkouts magnum reviews in: the Talkable pool
// slots (~/Projects/talkable.reviewN, provisioned once with bin/worktree-setup
// and reused) and per-PR worktrees for small repositories, next to the
// repository's main clone whatever it is named
// (~/Projects/<owner>-<name>__worktrees/pr-N).
//
// Every operation is a sequence of steps.Step side effects, each idempotent
// or prechecked, so a crash in the middle is repaired by calling the same
// operation again. Slot state moves through compare-and-set store
// transitions; the slot row is written before any side effect so the
// registry always knows about a directory or database before it exists.
//
// Heavy commands (setup, dependency installs, schema reloads, teardown) run
// through `mise -C <slot> exec -- env <slot env> /bin/sh -c <cmd>` and are
// serialized by the Manager, so at most one runs at a time.
package slots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// Timeouts of the heavy commands.
const (
	SetupTimeout    = 45 * time.Minute
	DepsTimeout     = 30 * time.Minute
	ResetDBTimeout  = 30 * time.Minute
	TeardownTimeout = 30 * time.Minute
)

// AgentPrefix starts the name of every agent magnum starts; the guard treats
// any other agent as a human's.
const AgentPrefix = "mg-"

// LockFiles are hashed into slots.lock_sha; a change reruns pool.PostCheckout.
var LockFiles = []string{"Gemfile.lock", "pnpm-lock.yaml"}

// SchemaFile is read for the `define(version: …)` the slot's dev database
// must match after a release.
const SchemaFile = "db/schema.rb"

// MarkerFile is written by bin/worktree-setup with the slot's database slug.
const MarkerFile = "tmp/.worktree-db-slug"

var (
	// ErrNoFreeSlot: the pool has no free, unpinned, unheld slot.
	ErrNoFreeSlot = errors.New("slots: no free slot")
	// ErrBroken: the slot was moved to broken (needs Repair).
	ErrBroken = errors.New("slots: slot is broken")
	// ErrLowDisk: provisioning refused below pool.min_free_disk_gb.
	ErrLowDisk = errors.New("slots: not enough free disk space")
	// ErrOriginMismatch: an existing clone's origin is not the expected repository.
	ErrOriginMismatch = errors.New("slots: clone origin does not match the repository")
	// ErrVerify: a post-condition (HEAD, marker file, databases) did not hold.
	ErrVerify = errors.New("slots: verification failed")
)

// Hold reasons returned in ErrHold. HoldHeadDrift, HoldUnpushed and
// HoldDirtyWorktree are also persisted as slots.hold_reason (the slot stays
// held until Unpin); the others are transient and only refuse the current
// operation.
const (
	HoldPinned            = "pinned"
	HoldForeignAgent      = "foreign_agent"
	HoldForegroundProcess = "foreground_process"
	HoldHeadDrift         = "head_drift"
	HoldUnpushed          = "unpushed_commits"
	HoldDirtyWorktree     = "dirty_worktree"
)

// ErrHold is returned when a guard refuses to touch a slot because a human is
// (or may be) using it. Match it with errors.As(err, &slots.ErrHold{}) or
// AsHold. A slot.hold_reason set by hand is reported with that reason.
type ErrHold struct {
	Reason string // one of the Hold* constants, or a manual hold_reason
	Detail string // what was seen (pane, process, shas)
}

// holdErrorPrefix starts every ErrHold's text.
const holdErrorPrefix = "slots: held: "

func (e ErrHold) Error() string {
	if e.Detail == "" {
		return holdErrorPrefix + e.Reason
	}
	return holdErrorPrefix + e.Reason + ": " + e.Detail
}

// AsHold reports whether err carries an ErrHold.
func AsHold(err error) (ErrHold, bool) {
	var h ErrHold
	ok := errors.As(err, &h)
	return h, ok
}

// MySQL is the part of *mysqlx.Client the slots package uses.
type MySQL interface {
	DBLister
	SchemaMigrationsMax(ctx context.Context, dbName string) (string, error)
	DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult
}

// Deps are the Manager's collaborators.
type Deps struct {
	Store *store.Store
	Run   execx.Runner
	// Git defaults to gitx.New(Run). Share one client with other packages so
	// fetches into the same clone stay serialized.
	Git   *gitx.Client
	MySQL MySQL // required for pool slots
	// Snapshot feeds the guard (e.g. (*herdr.Client).Snapshot). Nil skips the
	// herdr checks.
	Snapshot func(ctx context.Context) (herdr.Snapshot, error)
	// ProcessInfo (e.g. (*herdr.Client).PaneProcessInfo) lets the guard see a
	// non-shell foreground process in panes without an agent. Optional.
	ProcessInfo func(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	Layout      paths.Layout
	// Mise is the mise executable; "" means "mise" on PATH.
	Mise string
	// LookPath finds Mise for per-PR worktree hooks (nil: exec.LookPath);
	// without it they run through /bin/sh. Pool scripts always use mise.
	LookPath func(file string) (string, error)
	// Repos are the [[repo]] blocks (config.Config.Repos): setup, teardown,
	// copy_files, strip_env and env of per-PR worktrees.
	Repos []config.Repo
	// FreeDiskBytes returns the free bytes of the filesystem holding path;
	// nil uses statfs.
	FreeDiskBytes func(path string) (uint64, error)
	// Now defaults to time.Now.
	Now func() time.Time
	// Log receives one line per decision and dry-run plan (optional).
	Log execx.Logger
	// DryRun makes every mutating method log what it would do and return
	// without touching git, MySQL, files or the store. Claim returns the slot
	// it would claim.
	DryRun bool
}

// Manager runs slot operations. Safe for concurrent use; heavy commands are
// serialized.
type Manager struct {
	d     Deps
	git   *gitx.Client
	mise  string
	heavy chan struct{} // 1-slot semaphore for heavy commands
}

// New returns a Manager.
func New(d Deps) *Manager {
	m := &Manager{d: d, git: d.Git, mise: d.Mise, heavy: make(chan struct{}, 1)}
	if m.git == nil {
		m.git = gitx.New(d.Run)
	}
	if m.mise == "" {
		m.mise = "mise"
	}
	if m.d.Now == nil {
		m.d.Now = time.Now
	}
	if m.d.FreeDiskBytes == nil {
		m.d.FreeDiskBytes = freeDiskBytes
	}
	return m
}

func (m *Manager) now() time.Time { return m.d.Now() }

func (m *Manager) logf(format string, args ...any) {
	if m.d.Log != nil {
		m.d.Log.Printf("%s", execx.Redact(fmt.Sprintf(format, args...)))
	}
}

func (m *Manager) dryRun(format string, args ...any) {
	m.logf("dry-run: would "+format, args...)
}

// step runs fn as steps.Step(subject, name).
func (m *Manager) step(ctx context.Context, subject, name string, fn func(ctx context.Context) error) error {
	return steps.Step(ctx, m.d.Store, subject, name, fn)
}

// event appends an audit row (message redacted).
func (m *Manager) event(ctx context.Context, subject, level, kind, msg string) {
	_, err := m.d.Store.AppendEvent(context.WithoutCancel(ctx), store.Event{
		Level: level, Subject: &subject, Kind: kind, Message: execx.Redact(msg),
	})
	if err != nil {
		m.logf("slots: event %s %s: %v", subject, kind, err)
	}
}

// reload returns the current row of sl (by id).
func (m *Manager) reload(ctx context.Context, sl store.Slot) (store.Slot, error) {
	cur, err := m.d.Store.SlotByID(ctx, sl.ID)
	if err != nil {
		return store.Slot{}, fmt.Errorf("slots: reload %s: %w", sl.Name, err)
	}
	return cur, nil
}

// setLastError records err on the slot (redacted); nil clears it.
func (m *Manager) setLastError(ctx context.Context, id int64, err error) {
	var v any
	if err != nil {
		v = execx.Redact(err.Error())
	}
	if uerr := m.d.Store.UpdateSlotFields(context.WithoutCancel(ctx), id, func(u *store.SlotUpdate) { u.Set("last_error", v) }); uerr != nil {
		m.logf("slots: record last_error on slot %d: %v", id, uerr)
	}
}

// Slot states each operation accepts; cleanup plans with the same lists.
// Callers that extend one must slices.Clone it first.
var (
	// ReleaseStates can be released: claimed and held start a release,
	// releasing and dirty_schema resume one.
	ReleaseStates = []string{store.SlotClaimed, store.SlotHeld, store.SlotReleasing, store.SlotDirtySchema}
	// RemoveStates are the pool slot states Remove starts from without force
	// (removing resumes; removed is a no-op).
	RemoveStates = []string{store.SlotFree, store.SlotBroken, store.SlotLost, store.SlotProvisioning}
	// ForceRemoveStates are the extra pool slot states Remove starts from
	// with force. Busy never is.
	ForceRemoveStates = []string{store.SlotClaimed, store.SlotHeld, store.SlotReleasing, store.SlotDirtySchema}
	// RemovePRStates are the per-PR worktree states RemovePRWorktree starts
	// from (removing resumes; busy never is).
	RemovePRStates = []string{store.SlotProvisioning, store.SlotClaimed, store.SlotHeld, store.SlotReleasing,
		store.SlotFree, store.SlotBroken, store.SlotLost}
)

// placeholder is the pool slot's placeholder branch (its name by default).
func placeholder(sl store.Slot) string {
	if sl.PlaceholderBranch != nil && *sl.PlaceholderBranch != "" {
		return *sl.PlaceholderBranch
	}
	return sl.Name
}

// acquireHeavy takes the heavy-command lock, honoring ctx while waiting.
func (m *Manager) acquireHeavy(ctx context.Context) (release func(), err error) {
	select {
	case m.heavy <- struct{}{}:
		return func() { <-m.heavy }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("slots: waiting for the heavy-command lock: %w", ctx.Err())
	}
}

// MiseExecArgs builds `-C dir exec -- env K=V… /bin/sh -c script` (keys
// sorted; blank values blank the variable): the arguments of the mise
// executable that run a pool script as the release, the provisioning and
// `magnum open` do, with the real Ruby first on PATH (no shim that would
// re-apply the checkout's .mise.local.toml [env] over env).
func MiseExecArgs(dir string, env map[string]string, script string) []string {
	args := []string{"-C", dir, "exec", "--", "env"}
	for _, k := range sortedKeys(env) {
		args = append(args, k+"="+env[k])
	}
	return append(args, "/bin/sh", "-c", script)
}

// runHeavy runs one heavy script in the slot through mise exec, serialized
// with every other heavy command, and appends a redacted transcript to
// layout.Logs()/<logName>.
func (m *Manager) runHeavy(ctx context.Context, dir string, env map[string]string, script, label, logName string, timeout time.Duration) error {
	return m.runLogged(ctx, execx.Cmd{
		Name: m.mise, Args: MiseExecArgs(dir, env, script), Dir: dir,
		Timeout: timeout, Mutates: true, Label: label,
	}, logName)
}

// runLogged runs a heavy command c, serialized with every other heavy
// command, and appends a redacted transcript to layout.Logs()/<logName>: one
// block written after the command (begin, output, end), so a rotation moves
// whole commands, with each stream cut to its last logStreamTail bytes.
func (m *Manager) runLogged(ctx context.Context, c execx.Cmd, logName string) error {
	release, err := m.acquireHeavy(ctx)
	if err != nil {
		return err
	}
	defer release()
	label := c.Label
	begin := fmt.Sprintf("== %s begin %s: %s\n", store.FormatTime(m.now()), label, c.String())
	res, err := m.d.Run.Run(ctx, c)
	var b strings.Builder
	b.WriteString(begin)
	writeTail(&b, res.Stdout)
	if len(res.Stderr) > 0 {
		b.WriteString("--- stderr ---\n")
		writeTail(&b, res.Stderr)
	}
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	status := fmt.Sprintf("exit %d (%s)", res.Code, res.Duration.Round(time.Millisecond))
	if err != nil {
		status += ": " + err.Error()
	}
	fmt.Fprintf(&b, "== %s end %s: %s\n", store.FormatTime(m.now()), label, status)
	m.transcript(logName, b.String())
	if err != nil {
		return fmt.Errorf("slots: %s: %w", label, err)
	}
	return nil
}

// writeTail writes out to b whole when it fits in logStreamTail bytes, else
// a "[N bytes cut]" line and the last logStreamTail bytes from the first line
// start in them: a failure shows at the end of a command's output, and a seed
// that prints its SQL would otherwise fill the log with one command.
func writeTail(b *strings.Builder, out []byte) {
	if len(out) <= logStreamTail {
		b.Write(out)
		return
	}
	cut := len(out) - logStreamTail
	if i := bytes.IndexByte(out[cut:], '\n'); i >= 0 && i < len(out)-cut-1 {
		cut += i + 1
	}
	fmt.Fprintf(b, "[%d bytes cut]\n", cut)
	b.Write(out[cut:])
}

// SlotLogMax caps each log transcript writes (slot-<name>.log,
// provision-<name>.log, perpr-<slug>.log): the write that would push one past
// it first moves it to <name>.1, replacing the previous one, as daemon.log
// rotates. A seed that prints its SQL fills several MB on every reset.
const SlotLogMax = 2 << 20

// logStreamTail is how much of each output stream a command's block keeps
// (its end): a block is at most two of them plus its begin and end lines,
// far under SlotLogMax, so the cap rotates whole commands.
const logStreamTail = 256 << 10

// transcript appends redacted text to a log file under layout.Logs(),
// rotated at SlotLogMax; best effort.
func (m *Manager) transcript(logName, text string) {
	if !m.d.Layout.Valid() || logName == "" {
		return
	}
	dir := m.d.Layout.Logs()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.logf("slots: log dir: %v", err)
		return
	}
	text = execx.Redact(text)
	path := filepath.Join(dir, logName)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 && fi.Size()+int64(len(text)) > SlotLogMax {
		if err := os.Rename(path, path+".1"); err != nil {
			m.logf("slots: rotate log: %v", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		m.logf("slots: open log: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		m.logf("slots: write log: %v", err)
	}
}

// runScripts runs scripts in order through runHeavy, stopping at the first failure.
func (m *Manager) runScripts(ctx context.Context, sl store.Slot, env map[string]string, scripts []string, what, logName string, timeout time.Duration) error {
	for _, s := range scripts {
		if err := m.runHeavy(ctx, sl.Path, env, s, fmt.Sprintf("%s %s: %s", what, sl.Name, s), logName, timeout); err != nil {
			return err
		}
	}
	return nil
}

// slotPR is the PR the slot holds (by pr_id, else its open assignment); ok
// is false when it holds none. A store failure is an error, never "none":
// callers delete refs and run teardown by the PR's number.
func (m *Manager) slotPR(ctx context.Context, sl store.Slot) (store.PR, bool, error) {
	prID := sl.PRID
	if prID == nil {
		a, err := m.d.Store.OpenAssignmentBySlot(ctx, sl.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return store.PR{}, false, nil
		case err != nil:
			return store.PR{}, false, fmt.Errorf("slots: PR of %s: %w", sl.Name, err)
		}
		prID = &a.PRID
	}
	pr, err := m.d.Store.PRByID(ctx, *prID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.PR{}, false, nil
	case err != nil:
		return store.PR{}, false, fmt.Errorf("slots: PR of %s: %w", sl.Name, err)
	}
	return pr, true, nil
}
