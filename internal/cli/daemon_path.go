package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
)

// shellPATHTimeout bounds the login shell's answer at the daemon's start
// ([daemon] shell_path); a variable so tests can shorten it.
var shellPATHTimeout = 20 * time.Second

// shellPATHScript prints the shell's PATH; printf, not zsh's print, so a
// bash login shell answers too.
const shellPATHScript = `printf '%s\n' "$PATH"`

// daemonShellPATH gives the daemon, with [daemon] shell_path on, the PATH
// directories of the operator's login shell that its own PATH lacks: it asks
// `$SHELL -ilc` (zsh when SHELL is unset; interactive and login, so the
// shell's .zshrc exports count too, detached from any terminal, without
// stdin, for shellPATHTimeout at most), appends them in the shell's order
// and sets the result in the daemon's own environment, which every command
// it starts inherits. launchd's PATH keeps its order and stays first, so
// mise's shims still win. It runs once, before app.New, so nothing the
// daemon runs precedes it. A shell that fails, hangs or prints no PATH
// leaves the PATH as it was. The result is what the engine records at its
// start (engine.ShellPATH); nil with the key off, when no shell runs.
func daemonShellPATH(ctx context.Context, cfg *config.Config) *engine.ShellPATH {
	if cfg == nil || !cfg.Daemon.ShellPath {
		return nil
	}
	shell := cmp.Or(daemonSys.Getenv("SHELL"), "zsh")
	s := &engine.ShellPATH{Shell: filepath.Base(shell)}
	res, err := daemonSys.runner(false, io.Discard).Run(ctx, execx.Cmd{
		Name: shell, Args: []string{"-ilc", shellPATHScript},
		Timeout: shellPATHTimeout, NoTTY: true, Label: "login shell PATH",
	})
	// The reasons are fixed words: the shell's output and stderr may name
	// anything its startup files print.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.Failed = "answered nothing in " + shellPATHTimeout.String()
	case errors.Is(err, context.Canceled):
		s.Failed = "was stopped with the daemon"
	case err != nil && res.Code < 0:
		s.Failed = "could not start"
	case err != nil:
		s.Failed = fmt.Sprintf("exited with status %d", res.Code)
	}
	if s.Failed != "" {
		return s
	}
	line, ok := lastPATHLine(string(res.Stdout))
	if !ok {
		s.Failed = "printed no PATH"
		return s
	}
	merged, added := mergePATH(daemonSys.Getenv("PATH"), line)
	if len(added) == 0 {
		return s
	}
	set := daemonSys.Setenv
	if set == nil {
		set = os.Setenv
	}
	if err := set("PATH", merged); err != nil {
		s.Failed = "gave a PATH the daemon could not set"
		return s
	}
	s.Added, s.Count = added, len(added)
	return s
}

// lastPATHLine is the last non-empty line of out that reads as a PATH, so
// banners and hook output around it are skipped.
func lastPATHLine(out string) (string, bool) {
	for _, line := range slices.Backward(strings.Split(out, "\n")) {
		if line = strings.TrimSpace(line); line != "" && looksLikePATH(line) {
			return line, true
		}
	}
	return "", false
}

// looksLikePATH: colon-separated directories, at least one absolute. An
// entry may start with `~` (a shell's PATH can carry an unexpanded one,
// such as `~/.dotnet/tools`) or be relative (`.`, `bin`): neither makes the
// line something else than a PATH, and mergePATH expands the first and
// skips the second. A line with a space, or with no absolute entry, is a
// banner, not a PATH.
func looksLikePATH(line string) bool {
	found := false
	for entry := range strings.SplitSeq(line, ":") {
		if strings.ContainsAny(entry, " \t") {
			return false
		}
		if filepath.IsAbs(entry) {
			found = true
		}
	}
	return found
}

// mergePATH appends to own the entries of shell it lacks (compared cleaned,
// so a trailing slash or a repeat counts once), in shell's order, a `~`
// expanded to HOME and a relative entry left out; own keeps its order and
// stays first. added are the appended entries.
func mergePATH(own, shell string) (merged string, added []string) {
	var out []string
	if own != "" {
		out = strings.Split(own, ":")
	}
	have := map[string]bool{}
	for _, e := range out {
		if e != "" {
			have[filepath.Clean(e)] = true
		}
	}
	home := daemonSys.Getenv("HOME")
	for e := range strings.SplitSeq(shell, ":") {
		if home != "" && (e == "~" || strings.HasPrefix(e, "~/")) {
			e = home + e[1:] // the shell left it unexpanded; the daemon's PATH holds directories
		}
		if e == "" || !filepath.IsAbs(e) || have[filepath.Clean(e)] { // a relative entry (`.`) is never appended
			continue
		}
		have[filepath.Clean(e)] = true
		added = append(added, e)
	}
	return strings.Join(append(out, added...), ":"), added
}
