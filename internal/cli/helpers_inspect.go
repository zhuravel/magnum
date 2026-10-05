package cli

// Shared helpers of the inspect group (status, where, slots, cleanup, doctor,
// identities, logs). Every name carries the insp prefix so the other command
// groups can add their own helpers without collisions.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// Test hooks.
var (
	// inspAppHook adjusts the app.Options of every App the inspect commands
	// build (tests inject a fake runner, socket and DSN).
	inspAppHook func(*app.Options)
	// inspStdin is where confirmations are read from.
	inspStdin io.Reader = os.Stdin
	// inspIsTTY reports whether confirmations can be asked.
	inspIsTTY = func() bool { return app.Interactive(os.Stdin) }
	// inspNow is the CLI clock.
	inspNow = time.Now
	// inspPoll is the interval for request polling and log following.
	inspPoll = 300 * time.Millisecond
)

// inspOpenApp loads config.toml and builds the App. Read-only commands log
// warnings to stderr only; mutating ones (verbose) use the App's default
// logger: JSON to state/logs/daemon.log plus progress text on stderr.
func inspOpenApp(c *Context, verbose bool) (*app.App, error) {
	if err := c.LoadConfig(); err != nil {
		return nil, fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err)
	}
	opts := app.Options{Stderr: c.Stderr, BeforeMigrate: migrateGuard(c.Layout)}
	if !verbose {
		opts.Logger = app.NewLogger(nil, c.Stderr, slog.LevelWarn)
	}
	if inspAppHook != nil {
		inspAppHook(&opts)
	}
	a, err := app.New(c.Config, c.Layout, opts)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// inspUsage prints a usage error and returns exit code 2.
func inspUsage(c *Context, cmd, msg, usage string) int {
	fmt.Fprintf(c.Stderr, "magnum %s: %s\nusage: magnum %s %s\n", cmd, msg, cmd, usage)
	return 2
}

// inspTable is the tabwriter every table uses; call Flush when done.
func inspTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// inspTilde shortens a path under $HOME to ~/...
func inspTilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return p
}

// inspAgo renders t relative to now: "3m ago", "in 5m", "-" for nil/zero.
func inspAgo(now time.Time, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	if t.After(now) {
		return "in " + tui.HumanDuration(t.Sub(now))
	}
	return tui.HumanDuration(now.Sub(*t)) + " ago"
}

// inspClock renders t as a local wall-clock time (date added when not today).
func inspClock(now, t time.Time) string {
	lt, ln := t.Local(), now.Local()
	if lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay() {
		return lt.Format("15:04")
	}
	return lt.Format("Jan 2 15:04")
}

// inspMB renders a MySQL size in MB.
func inspMB(mb float64) string { return tui.HumanBytes(int64(mb * 1024 * 1024)) }

// inspPRLabel is the short PR label used in tables: "talkable#11920".
func inspPRLabel(repoFullName string, number int) string {
	name := repoFullName
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return fmt.Sprintf("%s#%d", name, number)
}

// inspTarget is what a user-typed reference names: a managed slot (by name)
// or a pull request (any github.ParseRef form). A slot that holds a PR also
// carries that PR.
type inspTarget struct {
	Slot *store.Slot
	Repo *store.Repo
	PR   *store.PR
}

// Label names the target for messages.
func (t inspTarget) Label() string {
	switch {
	case t.PR != nil && t.Repo != nil:
		return fmt.Sprintf("%s#%d", t.Repo.FullName(), t.PR.Number)
	case t.Slot != nil:
		return "slot " + t.Slot.Name
	}
	return "?"
}

// inspResolveIn resolves ref as a slot name first, then as a PR reference
// (refs carries daemon.default_repo).
func inspResolveIn(ctx context.Context, st *store.Store, refs app.RefParser, ref string) (inspTarget, error) {
	var t inspTarget
	sl, err := st.SlotByName(ctx, ref)
	if err == nil && sl.Kind == store.SlotKindPerPR && sl.State == store.SlotRemoved {
		err = store.ErrNotFound // a removed per-PR slot's name is its PR's reference
	}
	switch {
	case err == nil:
		t.Slot = &sl
		if sl.PRID != nil {
			if pr, err := st.PRByID(ctx, *sl.PRID); err == nil {
				t.PR = &pr
				if repo, err := st.RepoByID(ctx, pr.RepoID); err == nil {
					t.Repo = &repo
				}
			}
		}
		return t, nil
	case !errors.Is(err, store.ErrNotFound):
		return t, err
	}
	repo, pr, err := app.LookupPR(ctx, st, refs, ref)
	if err != nil {
		if _, _, _, perr := refs.ResolvePR(ctx, ref); perr != nil {
			return t, fmt.Errorf("%q is neither a slot name nor a PR reference (use a URL, owner/repo#N, repo#N or N)", ref)
		}
		if errors.Is(err, store.ErrNotFound) {
			return t, fmt.Errorf("%w (the daemon registers open PRs of watched repositories on its next GitHub poll; "+
				"`magnum status` shows when it last polled)", err)
		}
		return t, err
	}
	t.Repo, t.PR = &repo, &pr
	return t, nil
}

// inspIn is the prompt reader over inspStdin (rebuilt when a test swaps it).
var inspIn *promptIn

func inspPrompt() *promptIn {
	if inspIn == nil || inspIn.src != inspStdin {
		inspIn = newPromptIn(inspStdin)
	}
	return inspIn
}

// inspConfirm asks a y/N question on a terminal; false when not a terminal
// (and on ctrl+c: callers check ctx.Err()).
func inspConfirm(ctx context.Context, c *Context, question string) bool {
	return inspIsTTY() && inspPrompt().confirm(ctx, c.Stdout, question)
}

// inspConfirmTyped asks the user to type want exactly (terminal only).
func inspConfirmTyped(ctx context.Context, c *Context, question, want string) bool {
	return inspIsTTY() && inspPrompt().confirmTyped(ctx, c.Stdout, question, want)
}

// inspRequests is the request client (request_client.go) of the inspect
// commands, which hand work to the daemon when it holds its lock.
func inspRequests(c *Context, st *store.Store) reqClient {
	return reqClient{st: st, kick: func() (int, error) { return engine.KickDaemon(c.Layout) }, now: inspNow, version: c.Version,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}}
}

// inspHandOff hands work to the daemon, which holds its lock, and waits up
// to wait for its answer (the outcome says pending when the wait ran out).
func inspHandOff(ctx context.Context, c *Context, st *store.Store, kind string, payload any, wait time.Duration) (reqOutcome, error) {
	return inspRequests(c, st).send(ctx, kind, payload, reqSend{Wait: wait, Poll: inspPoll, Held: true})
}

// inspDiskFree returns the free bytes of the filesystem holding path.
func inspDiskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// inspHome is the user's home directory ("" when unknown).
func inspHome() string {
	h, _ := os.UserHomeDir()
	return h
}
