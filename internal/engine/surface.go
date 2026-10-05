package engine

// What the user sees of the daemon besides the registry: toasts (urgent ones
// in their own goroutines, informational ones batched) and the tab-bar file
// herdr shows.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// newRepoWindow: a new repository is announced at most once a day.
	newRepoWindow = 24 * time.Hour
	// toastDrain bounds how long shutdown waits for urgent toasts still
	// retrying through herdr's rate limit before it cancels them, and
	// toastCancelWait how long it then waits for them to return.
	toastDrain      = 10 * time.Second
	toastCancelWait = 2 * time.Second
	// tabBarStaleTicks is how many poll intervals the tab-bar file may age
	// before the herdr tab-bar command shows "magnum down" instead of it.
	tabBarStaleTicks = 3
)

// urgent shows an alert that must reach the user (notify.ToastUrgent: retried
// through herdr's rate limit, osascript after) in its own goroutine, so a
// retrying toast never holds up the tick or a round. The goroutines run on
// the engine's toast context, which shutdown cancels after a bounded wait
// (toastDrain). Dry runs and a missing notifier only log.
func (e *Engine) urgent(key, title, body string, window time.Duration) {
	if e.d.DryRun || e.d.Notifier == nil {
		e.log.Info("toast", "title", title, "body", body)
		return
	}
	e.toastMu.Lock()
	if e.toastsClosed {
		e.toastMu.Unlock()
		e.log.Warn("toast after shutdown began; not shown", "key", key, "title", title)
		return
	}
	e.toastWG.Add(1)
	e.toastMu.Unlock()
	go func() {
		defer e.toastWG.Done()
		if _, err := e.d.Notifier.ToastUrgent(e.toastCtx, key, title, body, window); err != nil {
			e.log.Warn("urgent toast failed", "key", key, "err", err)
		}
	}()
}

// info queues an informational toast (a posted review, a new repository) on
// the batcher, which coalesces a burst into one summary per minute. Without
// a batcher (dry run, no notifier) it only logs.
func (e *Engine) info(it notify.Item) {
	if e.batch == nil {
		e.log.Info("toast", "title", it.Title, "body", it.Body)
		return
	}
	e.batch.Add(it)
}

// stopToasts waits up to toastDrain for urgent toasts in flight, then
// cancels them (their dedupe keys are released) and waits briefly for them
// to return. Toasts asked for afterwards are only logged.
func (e *Engine) stopToasts() {
	e.toastMu.Lock()
	e.toastsClosed = true
	e.toastMu.Unlock()
	done := make(chan struct{})
	go func() { e.toastWG.Wait(); close(done) }()
	wait := e.toastWait
	if wait <= 0 {
		wait = toastDrain
	}
	select {
	case <-done:
	case <-time.After(wait):
		e.log.Warn("urgent toasts still retrying; cancelling them", "after", wait)
		e.toastStop()
		select {
		case <-done:
		case <-time.After(toastCancelWait):
		}
	}
	e.toastStop()
}

// surface flushes coalesced toasts and rewrites the tab-bar line.
func (e *Engine) surface(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	if e.batch != nil {
		if _, err := e.batch.FlushDue(ctx); err != nil {
			e.log.Warn("flush toasts", "err", err)
		}
	}
	e.lastTabBar = e.tabBar(ctx)
	e.writeTabBar()
}

// heartbeat rewrites the tab-bar file with the last line and a fresh
// timestamp at the start of a tick, so a slow poll does not read as a dead
// daemon.
func (e *Engine) heartbeat() {
	if e.d.DryRun || e.lastTabBar == "" {
		return
	}
	e.writeTabBar()
}

// writeTabBar writes the tab-bar file (WriteTabBarFile) with the last line
// tabBar built.
func (e *Engine) writeTabBar() {
	if !e.d.Layout.Valid() {
		return
	}
	interval := e.cfg.Daemon.PollInterval.Duration // Config.Validate keeps it positive
	if err := WriteTabBarFile(e.d.Layout.TabBar(), e.now(), tabBarStaleTicks*interval, e.lastTabBar); err != nil {
		e.log.Warn("tab bar", "err", err)
	}
}

// WriteTabBarFile atomically replaces the tab-bar file with two lines: when
// it was written and how long it stays fresh (unix seconds and seconds,
// "1791710538 90"), then text flattened to one plain line (secrets
// redacted). herdr renders only the last line a tab-bar command prints, so
// `cat` of the file still shows the status; the command magnum suggests
// (TabBarCommand in the cli) prints "magnum down" once the first line is
// older than its own max age.
func WriteTabBarFile(path string, at time.Time, maxAge time.Duration, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("tab bar: %w", err)
	}
	line := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, execx.Redact(text)))
	data := fmt.Sprintf("%d %d\n%s\n", at.Unix(), int64(maxAge/time.Second), line)
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("tab bar: %w", err)
	}
	tmp := f.Name()
	_, werr := f.WriteString(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("tab bar: %w", werr)
	}
	return nil
}

// readTabBarFile parses a file WriteTabBarFile wrote: the time it was
// written, its max age and the status line. ok is false for a file without
// the timestamp line (an older daemon's single line).
func readTabBarFile(path string) (at time.Time, maxAge time.Duration, line string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, 0, "", false
	}
	head, rest, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(head)
	if len(f) != 2 {
		return time.Time{}, 0, "", false
	}
	sec, err1 := strconv.ParseInt(f[0], 10, 64)
	age, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, "", false
	}
	return time.Unix(sec, 0), time.Duration(age) * time.Second, strings.TrimSpace(rest), true
}

// tabBar is the short status line herdr shows in its tab bar.
func (e *Engine) tabBar(ctx context.Context) string {
	parts := []string{"magnum"}
	if v, _ := e.getKV(ctx, KVDaemonDraining); v != "" {
		parts = append(parts, "draining")
	}
	if n := e.reviewRounds(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d reviewing", n))
	}
	if prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}}); err == nil && len(prs) > 0 {
		parts = append(parts, fmt.Sprintf("%d attention", len(prs)))
	}
	if v, _ := e.getKV(ctx, KVDaemonPaused); v == "1" {
		parts = append(parts, e.pauseTabBar(ctx)...) // "paused 19h · 6 requests held"
	}
	if _, ok := e.infraPause(ctx); ok {
		parts = append(parts, "infra paused")
	}
	now := e.now()
	for _, tool := range e.kinds() {
		if p, ok := e.toolPause(ctx, tool); ok && now.Before(p.Until) {
			parts = append(parts, tool+" paused")
		}
	}
	if len(parts) == 1 {
		parts = append(parts, "idle")
	}
	if g := e.codexGauge(); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}
