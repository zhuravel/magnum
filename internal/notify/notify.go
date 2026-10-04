// Package notify is magnum's user-facing status surface: toasts, herdr sidebar
// tokens and the tab-bar file.
//
//   - Toast shows a deduplicated message through herdr's notification.show and
//     falls back to `osascript display notification` when herdr cannot show it
//     (socket unreachable, timeout, or no terminal client attached).
//   - ToastUrgent is Toast for alerts that must not be lost (identity leak,
//     login required, usage paused, needs attention): it retries through
//     herdr's rate limit with backoff and falls back to osascript after.
//   - Batcher coalesces informational toasts (reviews posted, new
//     repositories) into one summary per minute.
//   - Sidebar publishes display-only tokens for a workspace under the source
//     "magnum" with a 24 h TTL, so stale tokens disappear on their own.
//   - WriteTabBar atomically rewrites state/tabbar.txt, which herdr's
//     ui.tab_bar_right command segment reads with `cat`.
//
// Everything is best effort and never decides pipeline behavior: callers log
// the returned errors and carry on. All subprocesses go through execx.Runner;
// toast text and token values are passed through execx.Redact first.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
)

const (
	// SidebarSource is the metadata source id magnum reports under.
	SidebarSource = "magnum"
	// SidebarTTL is how long herdr keeps reported tokens without a refresh.
	SidebarTTL = 24 * time.Hour
	// MaxTitleRunes and MaxBodyRunes bound toast text (clipped with an
	// ellipsis) so a runaway error message cannot flood the screen.
	MaxTitleRunes = 80
	MaxBodyRunes  = 400

	defaultTitle     = "magnum"
	osascriptTimeout = 10 * time.Second
	releaseTimeout   = 5 * time.Second
	osascriptPath    = "/usr/bin/osascript"   // absolute: never resolved through $PATH
	reasonNoClient   = "no_foreground_client" // herdr.NotificationResult.Reason
	reasonRateLimit  = "rate_limited"
	reasonBusy       = "busy"
	reasonDisabled   = "disabled"
)

// Urgent toast retries: herdr's rate_limited (or busy) answer is retried after
// UrgentRetryFirst, doubling each time, until UrgentRetryBudget of waiting is
// spent; then the toast goes to osascript.
const (
	UrgentRetryFirst  = time.Second
	UrgentRetryBudget = 60 * time.Second
)

// HerdrClient is the part of *herdr.Client this package uses.
type HerdrClient interface {
	NotificationShow(ctx context.Context, title, body string) (herdr.NotificationResult, error)
	WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, tokens map[string]string, ttl time.Duration) error
}

// DedupeStore is the part of *store.Store this package uses. ShouldSend
// reserves key for the window; ForgetSend releases a reservation whose toast
// was never delivered.
type DedupeStore interface {
	ShouldSend(ctx context.Context, key string, window time.Duration) (bool, error)
	ForgetSend(ctx context.Context, key string) error
}

// errNoSurface is returned when a toast has neither herdr nor a runner.
var errNoSurface = errors.New("notify: no notification surface configured")

// Notifier delivers toasts and status tokens. Its methods are safe for
// concurrent use as long as the injected clients are.
type Notifier struct {
	// Herdr shows toasts and reports sidebar tokens. Nil means herdr is not
	// available (Toast goes straight to osascript, Sidebar fails).
	Herdr HerdrClient
	// Store dedupes toasts by key. Nil disables deduplication.
	Store DedupeStore
	// Runner runs the osascript fallback. Nil disables the fallback.
	Runner execx.Runner
	// Layout locates the tab-bar file.
	Layout paths.Layout
	// Enabled is the [herdr] notify switch. When false Toast (and every
	// Batcher flush) is a silent no-op. Sidebar and WriteTabBar are display
	// surfaces, not notifications, and ignore it.
	Enabled bool
	// Log, when set, receives one redacted line per fallback or suppression.
	Log execx.Logger
	// Sleep waits between urgent retries; nil means a timer that stops early
	// when ctx is done (returning ctx.Err()). Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// priority selects how hard a toast tries to reach the user.
type priority int

const (
	normal priority = iota // one herdr attempt; herdr declining is final
	urgent                 // retried through rate limits, osascript after
)

// Toast shows title/body to the user and reports whether a toast was actually
// delivered.
//
// With a non-empty key, a positive window and a Store, Toast first asks
// Store.ShouldSend(key, window): a repeat inside the window is counted and
// dropped (false, nil). Otherwise it calls herdr's notification.show. If herdr
// is unreachable or times out, or has no terminal client attached
// ("no_foreground_client"), it falls back to osascript. If herdr itself chose
// not to show the toast (disabled, rate_limited, busy) that is respected:
// (false, nil). Any other herdr error is returned and nothing falls back.
//
// ShouldSend records the send before delivery, so it works as a reservation:
// when delivery fails with an error (every surface failed), Toast releases it
// with Store.ForgetSend and a retry of the same key inside the window is
// delivered. A herdr decision not to show the toast is not an error and keeps
// the reservation, so the window stays consumed.
func (n *Notifier) Toast(ctx context.Context, key, title, body string, window time.Duration) (bool, error) {
	return n.toast(ctx, normal, key, title, body, window)
}

// ToastUrgent is Toast for alerts that must reach the user: an identity leak,
// a login that needs the user, a paused agent kind, a PR that needs attention.
// Dedupe works as in Toast (the key is reserved once for all attempts,
// released when nothing was delivered, consumed when something was). Delivery
// differs: when herdr answers rate_limited or busy, the toast is retried after
// 1s, 2s, 4s… until UrgentRetryBudget of waiting is spent and then shown with
// osascript; when herdr is unreachable, has no client or fails the request,
// osascript is used at once. Only herdr's "disabled" (the user turned toasts
// off in herdr) is respected as final.
//
// ToastUrgent can block for up to UrgentRetryBudget plus the osascript
// timeout; a caller on a latency-sensitive loop runs it in a goroutine. A
// cancelled ctx stops the waiting, releases the key and returns ctx's error.
func (n *Notifier) ToastUrgent(ctx context.Context, key, title, body string, window time.Duration) (bool, error) {
	return n.toast(ctx, urgent, key, title, body, window)
}

func (n *Notifier) toast(ctx context.Context, p priority, key, title, body string, window time.Duration) (bool, error) {
	if !n.Enabled {
		return false, nil
	}
	title = cleanText(title, false, MaxTitleRunes)
	if title == "" {
		title = defaultTitle
	}
	body = cleanText(body, true, MaxBodyRunes)

	reserved := false
	if key != "" && window > 0 && n.Store != nil {
		ok, err := n.Store.ShouldSend(ctx, key, window)
		if err != nil {
			return false, fmt.Errorf("toast %q: %w", key, err)
		}
		if !ok {
			n.logf("notify: toast %q suppressed (sent within %s)", key, window)
			return false, nil
		}
		reserved = true
	}
	deliver := n.deliver
	if p == urgent {
		deliver = n.deliverUrgent
	}
	sent, err := deliver(ctx, title, body)
	if err != nil && reserved {
		n.release(ctx, key)
	}
	return sent, err
}

// release gives back the dedupe reservation of a toast that was not delivered.
// It runs on a context detached from ctx (which may be the very reason the
// delivery failed) and only logs a failure: the delivery error is what the
// caller needs.
func (n *Notifier) release(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := n.Store.ForgetSend(ctx, key); err != nil {
		n.logf("notify: toast %q: releasing the dedupe key after a failed delivery: %v", key, err)
	}
}

func (n *Notifier) deliver(ctx context.Context, title, body string) (bool, error) {
	var herdrErr error
	reason := ""
	if n.Herdr != nil {
		res, err := n.Herdr.NotificationShow(ctx, title, body)
		switch {
		case err == nil && res.Shown:
			return true, nil
		case err == nil && res.Reason == reasonNoClient:
			reason = "herdr has no foreground client"
		case err == nil:
			n.logf("notify: herdr did not show %q: %s", title, res.Reason)
			return false, nil
		case errors.Is(err, herdr.ErrUnavailable) || herdr.IsTimeout(err):
			herdrErr = err
			reason = "herdr unavailable: " + err.Error()
		default:
			return false, fmt.Errorf("herdr notification: %w", err)
		}
	}
	if n.Runner == nil {
		switch {
		case herdrErr != nil:
			return false, fmt.Errorf("toast not delivered, no osascript runner: %w", herdrErr)
		case n.Herdr == nil:
			return false, errNoSurface
		}
		return false, nil // herdr is up but nobody is watching, and there is no fallback
	}
	if reason != "" {
		n.logf("notify: %s; falling back to osascript", reason)
	}
	if err := n.osascript(ctx, title, body); err != nil {
		if herdrErr != nil {
			return false, fmt.Errorf("toast not delivered: herdr: %w; osascript: %w", herdrErr, err)
		}
		return false, fmt.Errorf("toast not delivered: osascript: %w", err)
	}
	return true, nil
}

// deliverUrgent shows an urgent toast: herdr first, retried through its rate
// limit, osascript when herdr cannot or will not show it in time. It never
// returns (false, nil) except for herdr's "disabled".
func (n *Notifier) deliverUrgent(ctx context.Context, title, body string) (bool, error) {
	var herdrErr error
	if n.Herdr != nil {
		wait, waited := UrgentRetryFirst, time.Duration(0)
		for {
			res, err := n.Herdr.NotificationShow(ctx, title, body)
			if err == nil && res.Shown {
				return true, nil
			}
			if err == nil && res.Reason == reasonDisabled {
				n.logf("notify: herdr did not show urgent %q: toasts are disabled in herdr", title)
				return false, nil
			}
			if err == nil && (res.Reason == reasonRateLimit || res.Reason == reasonBusy) && waited < UrgentRetryBudget {
				d := min(wait, UrgentRetryBudget-waited)
				n.logf("notify: herdr answered %s for urgent %q; retrying in %s", res.Reason, title, d)
				if err := n.sleep(ctx, d); err != nil {
					return false, fmt.Errorf("urgent toast not delivered: %w", err)
				}
				waited += d
				wait *= 2
				continue
			}
			switch {
			case err != nil:
				herdrErr = fmt.Errorf("herdr notification: %w", err)
			case res.Reason == reasonRateLimit || res.Reason == reasonBusy:
				herdrErr = fmt.Errorf("herdr answered %s for %s", res.Reason, UrgentRetryBudget)
			default:
				herdrErr = fmt.Errorf("herdr did not show it: %s", res.Reason)
			}
			break
		}
	}
	if n.Runner == nil {
		if herdrErr == nil {
			return false, errNoSurface
		}
		return false, fmt.Errorf("urgent toast not delivered, no osascript runner: %w", herdrErr)
	}
	if herdrErr != nil {
		n.logf("notify: urgent %q: %v; falling back to osascript", title, herdrErr)
	}
	if err := n.osascript(ctx, title, body); err != nil {
		if herdrErr != nil {
			return false, fmt.Errorf("urgent toast not delivered: %w; osascript: %w", herdrErr, err)
		}
		return false, fmt.Errorf("urgent toast not delivered: osascript: %w", err)
	}
	return true, nil
}

func (n *Notifier) sleep(ctx context.Context, d time.Duration) error {
	if n.Sleep != nil {
		return n.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// osascript shows a macOS notification. Title and body are placed in
// AppleScript string literals with backslash, quote and newline escaped, so
// their content can never become script code.
func (n *Notifier) osascript(ctx context.Context, title, body string) error {
	script := "display notification " + appleScriptString(body) + " with title " + appleScriptString(title)
	_, err := n.Runner.Run(ctx, execx.Cmd{
		Name:    osascriptPath,
		Args:    []string{"-e", script},
		Mutates: true, // user-visible side effect: --dry-run prints it instead
		Timeout: osascriptTimeout,
		Label:   "osascript toast",
	})
	return err
}

// appleScriptString renders s (already cleaned: no control characters except
// newline) as a double-quoted AppleScript string literal.
func appleScriptString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Sidebar publishes tokens for the herdr workspace under source "magnum" with
// a 24 h TTL (see SidebarSource and SidebarTTL). Values are redacted and
// flattened to one line; the caller's map is not modified. Errors from herdr
// (for example workspace_not_found) are wrapped, so match them with
// herdr.IsCode.
func (n *Notifier) Sidebar(ctx context.Context, workspaceID string, tokens map[string]string) error {
	if workspaceID == "" {
		return errors.New("notify: sidebar needs a workspace id")
	}
	if n.Herdr == nil {
		return fmt.Errorf("notify: sidebar: %w", herdr.ErrUnavailable)
	}
	clean := make(map[string]string, len(tokens))
	for k, v := range tokens {
		clean[k] = cleanText(v, false, 0)
	}
	if err := n.Herdr.WorkspaceReportMetadata(ctx, workspaceID, SidebarSource, clean, SidebarTTL); err != nil {
		return fmt.Errorf("notify: sidebar %s: %w", workspaceID, err)
	}
	return nil
}

func (n *Notifier) logf(format string, args ...any) {
	if n.Log != nil {
		n.Log.Printf("%s", execx.Redact(fmt.Sprintf(format, args...)))
	}
}

// cleanText makes s safe to show: secrets redacted, CRLF/CR normalised to LF,
// tabs to spaces, other control characters (ESC included) dropped, surrounding
// space trimmed. Without multiline every newline becomes a space. max > 0
// clips to that many runes with a trailing ellipsis.
func cleanText(s string, multiline bool, max int) string {
	s = execx.Redact(s)
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			if multiline {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return clip(strings.TrimSpace(b.String()), max)
}

func clip(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRightFunc(string(r[:max-1]), unicode.IsSpace) + "…"
}
