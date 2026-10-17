package engine

// The agent CLIs' versions (DECISIONS "The daemon records each agent CLI's
// version and names a launch failure shared across PRs"): on 2026-10-16
// mise's `codex = "latest"` installed codex-cli 0.163.0 at 11:24, which died
// at its TUI bootstrap in every judge pane; four PRs parked, each naming
// its own failure, and 79 minutes passed before the operator found the new
// version. Only `magnum doctor` printed the CLIs' versions.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVKindVersion holds what the daemon last read of agent kind kind's CLI
// version (KindVersion, JSON).
func KVKindVersion(kind string) string { return "kind." + kind + ".version" }

// EventKindVersionChanged is recorded (warn, subject tool:<kind>) when an
// agent kind's CLI reports another version than the one recorded
// (KVKindVersion): the kind, the versions and the binary.
const EventKindVersionChanged = "kind.version_changed"

// EventKindVersionUnknown is recorded (warn, subject tool:<kind>) once when
// reading an agent kind's CLI version failed at cliVersionUnknownAfter
// health checks in a row (the one at the daemon's start counts): the kind,
// the number of failed checks, the binary and the last failure. The reads go
// on at every check.
const EventKindVersionUnknown = "kind.version_unknown"

// CLIChangeRecent is how long a change of a kind's CLI is news: `magnum
// status` says "(changed at 11:24)", and a launch failure of one of the
// kind's roles that parks a PR names it (attention.CLIChange).
const CLIChangeRecent = 24 * time.Hour

// cliVersionTimeout bounds `<cli> --version` and `mise which <cli>`, at the
// daemon's start and at the health checks alike; cliVersionRunes bounds the
// version kept; cliVersionUnknownAfter is how many health checks in a row
// may fail to read a kind's version before EventKindVersionUnknown.
const (
	cliVersionTimeout      = 15 * time.Second
	cliVersionRunes        = 80
	cliVersionUnknownAfter = 3
)

// kindCLIChanged counts the CLI changes in a batch summary.
var kindCLIChanged = notify.Kind{One: "agent CLI changed", Many: "agent CLIs changed"}

// KindVersion is what the daemon read of an agent kind's CLI version, at
// its start and at the health checks of every tick, through the kind's
// command on its PATH as `magnum doctor` reads it (`<kind> --version`).
type KindVersion struct {
	// Version is the first line the version command printed.
	Version string `json:"version"`
	// Path is the binary read, its links resolved: the file mise's shim
	// runs (`mise which <kind>`) for a command that is one; MTime is its
	// modification time then. The version command runs again only once
	// either differs.
	Path  string    `json:"path"`
	MTime time.Time `json:"mtime"`
	// SeenAt is when the version was read.
	SeenAt time.Time `json:"seen_at"`
	// Previous and ChangedAt are the version before the last change and
	// when the daemon read the new one (EventKindVersionChanged); empty
	// until a version changed.
	Previous  string    `json:"previous,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitzero"`
}

// ChangedWithin reports whether the version changed within d before now.
func (v KindVersion) ChangedWithin(now time.Time, d time.Duration) bool {
	return !v.ChangedAt.IsZero() && now.Sub(v.ChangedAt) < d
}

// cliStamp is the binary a kind's command runs and its modification time.
type cliStamp struct {
	path  string
	mtime time.Time
}

// cliReadFailures is what the daemon knows of a kind's failed version reads
// (Engine.cliFails): none of them leaves a stamp, so the next health check
// runs the command again.
type cliReadFailures struct {
	// run is the failed reads in a row, the one at the start included; a
	// read that works ends it. unknown says that EventKindVersionUnknown was
	// recorded for this run.
	run     int
	unknown bool
	// warned holds the failure causes logged at warn since the daemon
	// started (cliFailureCause): one cause is said once, whatever the run.
	warned map[string]bool
}

// checkCLIVersions reads the CLI version of every agent kind the
// configured roles use (a shell role's tool counts, the judge in reserve
// too), the kinds `magnum doctor` checks. Nothing is read in a dry run or
// without LookPath and a runner.
func (e *Engine) checkCLIVersions(ctx context.Context) {
	if e.d.DryRun || e.d.LookPath == nil || e.d.Runner == nil {
		return
	}
	for _, kind := range agentKinds(e.cfg.AllRoles()) {
		e.checkCLIVersion(ctx, kind)
	}
}

// checkCLIVersion reads kind's version when its binary differs from the
// one recorded (KVKindVersion), or from the one this run last read
// (cliRead): a read that failed leaves no stamp, so a binary whose version
// cannot be read is tried again at the next health check (cliReadFailed).
// The first version recorded is only logged; another one than the recorded
// is EventKindVersionChanged with one toast ("codex-cli 0.162.1 → 0.163.0
// at 11:24").
func (e *Engine) checkCLIVersion(ctx context.Context, kind string) {
	cmd, err := e.d.LookPath(kind)
	if err == nil {
		var stamp cliStamp
		if stamp, err = e.cliStamp(ctx, kind, cmd); err == nil {
			e.readCLIVersion(ctx, kind, cmd, stamp)
			return
		}
	}
	if e.logOnce("cli:"+kind, err.Error(), e.now()) {
		e.log.Info("cannot find the agent CLI's binary on the daemon's PATH; its version is not read", "kind", kind, "err", err)
	}
}

// readCLIVersion is checkCLIVersion for kind's command cmd, which runs the
// binary stamp.
func (e *Engine) readCLIVersion(ctx context.Context, kind, cmd string, stamp cliStamp) {
	if e.cliRead[kind] == stamp {
		return
	}
	key := KVKindVersion(kind)
	prev, had, err := readKVJSON[KindVersion](ctx, e, key, "kind version")
	if err != nil {
		e.log.Warn("read the recorded CLI version", "kind", kind, "err", err)
	}
	if had && prev.Path == stamp.path && prev.MTime.Equal(stamp.mtime) {
		e.cliRead[kind] = stamp
		return
	}
	version, err := e.cliVersion(ctx, kind, cmd)
	if err != nil {
		e.cliReadFailed(ctx, kind, stamp, err)
		return
	}
	e.cliRead[kind] = stamp
	failed := 0
	if f := e.cliFails[kind]; f != nil {
		failed = f.run
		f.run, f.unknown = 0, false
	}
	now := e.now()
	v := KindVersion{Version: version, Path: stamp.path, MTime: stamp.mtime, SeenAt: now, Previous: prev.Previous, ChangedAt: prev.ChangedAt}
	changed := had && prev.Version != "" && prev.Version != version
	if changed {
		v.Previous, v.ChangedAt = prev.Version, now
	}
	if err := writeKVJSON(ctx, e, key, "kind version", v); err != nil {
		e.warnUnlessStopped(ctx, err, "record the CLI version", "kind", kind)
		delete(e.cliRead, kind) // the next check writes it again
		return
	}
	if !changed {
		attrs := []any{"kind", kind, "version", version, "path", stamp.path}
		if failed > 0 {
			attrs = append(attrs, "failed_reads", failed)
		}
		e.log.Info("agent CLI version", attrs...)
		return
	}
	what := versionChange(prev.Version, version) + " at " + now.Local().Format("15:04")
	e.event(ctx, "warn", "tool:"+kind, EventKindVersionChanged, fmt.Sprintf("the %s CLI changed: %s (%s)", kind, what, stamp.path),
		map[string]any{"kind": kind, "from": prev.Version, "to": version, "path": stamp.path})
	e.info(notify.Item{Key: "cli-changed:" + kind + ":" + version, Title: "magnum: " + what,
		Body: fmt.Sprintf("The %s CLI magnum's agents start changed. If their launches fail from now on, suspect the new version first; `magnum status` shows the versions.", kind),
		Line: what, Kind: kindCLIChanged, Window: CLIChangeRecent})
}

// cliReadFailed handles a failed read of kind's version (an error, its
// timeout, a context that ended): no stamp is kept, so the command runs
// again at the next health check. ctx itself ending is the daemon stopping,
// no failure. The failure is warned about once per cause since the daemon
// started (later ones are debug lines), and the cliVersionUnknownAfter-th
// failed read in a row is one EventKindVersionUnknown; the reads go on at
// every check.
func (e *Engine) cliReadFailed(ctx context.Context, kind string, stamp cliStamp, err error) {
	if ctx.Err() != nil {
		e.log.Debug("cannot read the agent CLI's version; the daemon is stopping", "kind", kind, "path", stamp.path, "err", err)
		return
	}
	f := e.cliFails[kind]
	if f == nil {
		f = &cliReadFailures{warned: map[string]bool{}}
		e.cliFails[kind] = f
	}
	f.run++
	cause := cliFailureCause(err)
	if f.warned[cause] {
		e.log.Debug("cannot read the agent CLI's version; trying again at the next health check", "kind", kind, "path", stamp.path, "failed_reads", f.run, "err", err)
	} else {
		f.warned[cause] = true
		e.log.Warn("cannot read the agent CLI's version; trying again at the next health check", "kind", kind, "path", stamp.path, "err", err)
	}
	if f.run < cliVersionUnknownAfter || f.unknown {
		return
	}
	f.unknown = true
	e.event(ctx, "warn", "tool:"+kind, EventKindVersionUnknown,
		fmt.Sprintf("the %s CLI's version cannot be read: %d health checks in a row failed (%s); tried again at each check", kind, f.run, textx.Clip(execx.Redact(err.Error()), 200)),
		map[string]any{"kind": kind, "reads": f.run, "path": stamp.path})
}

// cliFailureCause is what a failed version read is warned about once: a
// timeout and a context that ended are one cause each, whatever command
// line the error names, any other failure its text.
func cliFailureCause(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return err.Error()
}

// cliStamp is the binary kind's command cmd (on the daemon's PATH) runs:
// cmd with its links resolved, except that a mise shim, a link to mise
// itself, runs the version mise picks when it runs, which `mise which
// <kind>` names (the shim's own file never changes). When mise cannot
// answer, the shim stands for itself.
func (e *Engine) cliStamp(ctx context.Context, kind, cmd string) (cliStamp, error) {
	path, err := filepath.EvalSymlinks(cmd)
	if err != nil {
		return cliStamp{}, err
	}
	if filepath.Base(path) == "mise" {
		res, err := e.d.Runner.Run(ctx, execx.Cmd{Name: path, Args: []string{"which", kind}, Timeout: cliVersionTimeout, Label: "mise which " + kind})
		bin := strings.TrimSpace(textx.FirstLine(string(res.Stdout)))
		switch {
		case err != nil:
			if e.logOnce("cli-mise:"+kind, err.Error(), e.now()) {
				e.log.Info("mise does not say which binary its shim runs; the shim stands for it", "kind", kind, "err", err)
			}
		case bin != "":
			if p, err := filepath.EvalSymlinks(bin); err == nil {
				path = p
			}
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return cliStamp{}, err
	}
	return cliStamp{path: path, mtime: fi.ModTime()}, nil
}

// cliVersion is the first line cmd --version prints (redacted, at most
// cliVersionRunes runes), as `magnum doctor` reads it.
func (e *Engine) cliVersion(ctx context.Context, kind, cmd string) (string, error) {
	res, err := e.d.Runner.Run(ctx, execx.Cmd{Name: cmd, Args: []string{"--version"}, Timeout: cliVersionTimeout, Label: kind + " --version"})
	if err != nil {
		return "", err
	}
	v := strings.Join(strings.Fields(textx.FirstLine(strings.TrimSpace(string(res.Stdout)))), " ")
	if v == "" {
		return "", fmt.Errorf("%s --version printed nothing", kind)
	}
	return textx.Clip(execx.Redact(v), cliVersionRunes), nil
}

// versionChange is "old → newer", newer without the words it shares with
// old at its start and its end ("codex-cli 0.162.1 → 0.163.0", "2.1.290
// (Claude Code) → 2.1.292"), or whole when nothing else would be left.
func versionChange(old, newer string) string {
	o, n := strings.Fields(old), strings.Fields(newer)
	head := 0
	for head < len(o) && head < len(n) && o[head] == n[head] {
		head++
	}
	tail := 0
	for tail < len(o)-head && tail < len(n)-head && o[len(o)-1-tail] == n[len(n)-1-tail] {
		tail++
	}
	rest := n[head : len(n)-tail]
	if len(rest) == 0 {
		rest = n
	}
	return old + " → " + strings.Join(rest, " ")
}

// cliChangeNote is what a round setup's error msg that parks its PR ends
// with (attention.CLIChange): when msg is the start of an agent role that
// failed (startRoles' "start <role>: …", attention.LaunchRole) and that
// role's kind's CLI changed within CLIChangeRecent, "; the codex CLI
// changed at 11:24"; "" otherwise.
func (e *Engine) cliChangeNote(ctx context.Context, msg string) string {
	name, ok := attention.LaunchRole(msg)
	if !ok {
		return ""
	}
	role, ok := e.cfg.RoleByNameOrAlias(nil, name)
	if !ok || !role.IsAgent() {
		return ""
	}
	kind := role.AgentKind()
	v, ok, err := readKVJSON[KindVersion](ctx, e, KVKindVersion(kind), "kind version")
	now := e.now()
	if err != nil || !ok || !v.ChangedWithin(now, CLIChangeRecent) {
		return ""
	}
	return attention.CLIChange(kind, v.ChangedAt, now)
}
