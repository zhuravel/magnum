package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The real clients must satisfy the package's small interfaces.
var (
	_ HerdrClient = (*herdr.Client)(nil)
	_ DedupeStore = (*store.Store)(nil)
)

type shown struct{ Title, Body string }

type metaCall struct {
	Workspace, Source string
	Tokens            map[string]string
	TTL               time.Duration
}

type fakeHerdr struct {
	mu      sync.Mutex
	shows   []shown
	showRes herdr.NotificationResult
	showErr error
	queue   []herdr.NotificationResult // answered first, in order, before showRes
	metas   []metaCall
	metaErr error
	onShow  func() // runs inside NotificationShow, before it returns
}

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{showRes: herdr.NotificationResult{Shown: true, Reason: "shown"}}
}

func (f *fakeHerdr) NotificationShow(_ context.Context, title, body string) (herdr.NotificationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shows = append(f.shows, shown{title, body})
	if f.onShow != nil {
		f.onShow()
	}
	if len(f.queue) > 0 {
		res := f.queue[0]
		f.queue = f.queue[1:]
		return res, nil
	}
	return f.showRes, f.showErr
}

func (f *fakeHerdr) WorkspaceReportMetadata(_ context.Context, ws, source string, tokens map[string]string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metas = append(f.metas, metaCall{ws, source, tokens, ttl})
	return f.metaErr
}

type fakeStore struct {
	calls []string
	send  bool
	err   error

	forgot       []string
	forgetErr    error
	forgetCtxErr error // ctx.Err() observed by ForgetSend
}

func (f *fakeStore) ForgetSend(ctx context.Context, key string) error {
	f.forgot = append(f.forgot, key)
	f.forgetCtxErr = ctx.Err()
	return f.forgetErr
}

func (f *fakeStore) ShouldSend(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.calls = append(f.calls, key)
	return f.send, f.err
}

func newClock() *storetest.Clock {
	return storetest.NewClock(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
}

func realStore(t *testing.T, c *storetest.Clock) *store.Store {
	t.Helper()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	s.Clock = c.Now
	return s
}

// osascript is the absolute path Toast's fallback runs (never via $PATH).
const osascript = "/usr/bin/osascript"

func osascriptFake() *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{osascript}}}}
}

func unavailable() error {
	return fmt.Errorf("herdr: dial: %w", herdr.ErrUnavailable)
}

func TestToastSendsViaHerdr(t *testing.T) {
	h := newFakeHerdr()
	r := osascriptFake()
	n := &Notifier{Herdr: h, Store: &fakeStore{send: true}, Runner: r, Enabled: true}

	sent, err := n.Toast(context.Background(), "k", "Review posted", "talkable#12 approved", time.Hour)
	if err != nil || !sent {
		t.Fatalf("Toast = %v, %v; want sent", sent, err)
	}
	if want := []shown{{"Review posted", "talkable#12 approved"}}; !reflect.DeepEqual(h.shows, want) {
		t.Errorf("herdr shows = %v, want %v", h.shows, want)
	}
	if len(r.Calls) != 0 {
		t.Errorf("osascript must not run when herdr delivered: %v", r.Calls)
	}
}

func TestToastDisabledDoesNothing(t *testing.T) {
	h := newFakeHerdr()
	st := &fakeStore{send: true}
	r := osascriptFake()
	n := &Notifier{Herdr: h, Store: st, Runner: r, Enabled: false}

	sent, err := n.Toast(context.Background(), "k", "t", "b", time.Hour)
	if sent || err != nil {
		t.Fatalf("Toast = %v, %v; want false, nil", sent, err)
	}
	if len(h.shows) != 0 || len(r.Calls) != 0 || len(st.calls) != 0 {
		t.Errorf("disabled notifier touched herdr=%v osascript=%v store=%v", h.shows, r.Calls, st.calls)
	}
}

func TestToastDedupeWithRealStore(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Store: realStore(t, c), Runner: osascriptFake(), Enabled: true}
	ctx := context.Background()

	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); !sent || err != nil {
		t.Fatalf("first = %v, %v", sent, err)
	}
	c.Add(5 * time.Minute)
	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); sent || err != nil {
		t.Fatalf("within window = %v, %v; want suppressed", sent, err)
	}
	if sent, err := n.Toast(ctx, "other-key", "Other", "x", 10*time.Minute); !sent || err != nil {
		t.Fatalf("other key = %v, %v; want sent", sent, err)
	}
	c.Add(6 * time.Minute)
	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); !sent || err != nil {
		t.Fatalf("after window = %v, %v; want sent", sent, err)
	}
	if len(h.shows) != 3 {
		t.Errorf("herdr shows = %d, want 3: %v", len(h.shows), h.shows)
	}
}

func TestToastStoreErrorIsReturned(t *testing.T) {
	boom := errors.New("disk full")
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Store: &fakeStore{err: boom}, Enabled: true}

	sent, err := n.Toast(context.Background(), "k", "t", "b", time.Hour)
	if sent || !errors.Is(err, boom) {
		t.Fatalf("Toast = %v, %v; want false, wrapped store error", sent, err)
	}
	if len(h.shows) != 0 {
		t.Errorf("a toast must not be shown when the dedupe gate failed")
	}
}

func TestToastSkipsDedupeForEmptyKeyOrWindow(t *testing.T) {
	st := &fakeStore{send: false} // would suppress everything if consulted
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Store: st, Enabled: true}
	ctx := context.Background()

	if sent, _ := n.Toast(ctx, "", "t", "b", time.Hour); !sent {
		t.Errorf("empty key must skip dedupe")
	}
	if sent, _ := n.Toast(ctx, "k", "t", "b", 0); !sent {
		t.Errorf("zero window must skip dedupe")
	}
	if len(st.calls) != 0 {
		t.Errorf("store consulted: %v", st.calls)
	}
	n.Store = nil
	if sent, _ := n.Toast(ctx, "k", "t", "b", time.Hour); !sent {
		t.Errorf("nil store must skip dedupe")
	}
}

func TestToastFallsBackToOsascript(t *testing.T) {
	for name, herdrErr := range map[string]error{
		"unavailable": unavailable(),
		"timeout":     fmt.Errorf("show: %w", herdr.ErrTimeout),
		"server":      &herdr.Error{Method: "notification.show", Code: herdr.CodeTimeout, Message: "slow"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHerdr()
			h.showErr = herdrErr
			r := osascriptFake()
			n := &Notifier{Herdr: h, Runner: r, Enabled: true}

			sent, err := n.Toast(context.Background(), "k", "Hello", "World", 0)
			if err != nil || !sent {
				t.Fatalf("Toast = %v, %v; want sent via osascript", sent, err)
			}
			assertOsascript(t, r, `display notification "World" with title "Hello"`)
		})
	}
}

func assertOsascript(t *testing.T, r *execx.Fake, wantScript string) {
	t.Helper()
	calls := r.CallsWithPrefix(osascript)
	if len(calls) != 1 {
		t.Fatalf("osascript calls = %d, want 1: %v", len(calls), r.Calls)
	}
	c := calls[0]
	if want := []string{"-e", wantScript}; !reflect.DeepEqual(c.Args, want) {
		t.Errorf("osascript args = %q, want %q", c.Args, want)
	}
	if !c.Mutates {
		t.Errorf("osascript toast must set Mutates so --dry-run prints it")
	}
}

func TestToastFallsBackWhenNoForegroundClient(t *testing.T) {
	h := newFakeHerdr()
	h.showRes = herdr.NotificationResult{Shown: false, Reason: "no_foreground_client"}
	r := osascriptFake()
	n := &Notifier{Herdr: h, Runner: r, Enabled: true}

	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if err != nil || !sent {
		t.Fatalf("Toast = %v, %v", sent, err)
	}
	assertOsascript(t, r, `display notification "B" with title "T"`)
}

func TestToastRespectsHerdrSuppression(t *testing.T) {
	for _, reason := range []string{"disabled", "rate_limited", "busy"} {
		t.Run(reason, func(t *testing.T) {
			h := newFakeHerdr()
			h.showRes = herdr.NotificationResult{Shown: false, Reason: reason}
			r := osascriptFake()
			n := &Notifier{Herdr: h, Runner: r, Enabled: true}

			sent, err := n.Toast(context.Background(), "", "T", "B", 0)
			if sent || err != nil {
				t.Fatalf("Toast = %v, %v; want false, nil", sent, err)
			}
			if len(r.Calls) != 0 {
				t.Errorf("herdr chose not to show (%s); osascript must not run: %v", reason, r.Calls)
			}
		})
	}
}

func TestToastHerdrRequestErrorIsNotMasked(t *testing.T) {
	h := newFakeHerdr()
	h.showErr = &herdr.Error{Method: "notification.show", Code: herdr.CodeInvalidRequest, Message: "bad"}
	r := osascriptFake()
	n := &Notifier{Herdr: h, Runner: r, Enabled: true}

	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if sent || !herdr.IsCode(err, herdr.CodeInvalidRequest) {
		t.Fatalf("Toast = %v, %v; want the herdr error", sent, err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("osascript ran for a request error: %v", r.Calls)
	}
}

func TestToastNilHerdrUsesOsascript(t *testing.T) {
	r := osascriptFake()
	n := &Notifier{Runner: r, Enabled: true}
	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if err != nil || !sent {
		t.Fatalf("Toast = %v, %v", sent, err)
	}
	assertOsascript(t, r, `display notification "B" with title "T"`)
}

func TestToastBothSurfacesFail(t *testing.T) {
	osErr := errors.New("osascript exploded")
	h := newFakeHerdr()
	h.showErr = unavailable()
	r := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{osascript}, Err: osErr}}}
	n := &Notifier{Herdr: h, Runner: r, Enabled: true}

	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if sent {
		t.Fatalf("sent = true despite both surfaces failing")
	}
	if !errors.Is(err, osErr) || !errors.Is(err, herdr.ErrUnavailable) {
		t.Fatalf("err = %v; want both herdr and osascript causes", err)
	}
}

func TestToastNoSurfaceAvailable(t *testing.T) {
	h := newFakeHerdr()
	h.showErr = unavailable()
	n := &Notifier{Herdr: h, Enabled: true} // no Runner
	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if sent || !errors.Is(err, herdr.ErrUnavailable) {
		t.Fatalf("Toast = %v, %v; want unavailable error", sent, err)
	}
}

func TestOsascriptEscaping(t *testing.T) {
	cases := []struct{ name, title, body, script string }{
		{"quotes", `say "hi"`, `a "quoted" word`, `display notification "a \"quoted\" word" with title "say \"hi\""`},
		{"backslash", `C:\x`, `back\slash`, `display notification "back\\slash" with title "C:\\x"`},
		{"newlines normalised, tabs flattened", "t", "line1\nline2\r\n\tx\rend", `display notification "line1\nline2\n x\nend" with title "t"`},
		{"title stays one line", "a\nb", "body", `display notification "body" with title "a b"`},
		{"empty title gets a default", "", "body", `display notification "body" with title "magnum"`},
		{"empty body", "T", "", `display notification "" with title "T"`},
		{"control chars dropped", "t\x1b[31m", "b\x00\x07", `display notification "b" with title "t[31m"`},
		{"injection stays a literal", `x`, `" & (do shell script "touch /tmp/pwn") & "`,
			`display notification "\" & (do shell script \"touch /tmp/pwn\") & \"" with title "x"`},
		{"unicode", "Ревʼю", "готово ✓", `display notification "готово ✓" with title "Ревʼю"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := osascriptFake()
			n := &Notifier{Runner: r, Enabled: true}
			if _, err := n.Toast(context.Background(), "", tc.title, tc.body, 0); err != nil {
				t.Fatal(err)
			}
			assertOsascript(t, r, tc.script)
		})
	}
}

func TestToastRedactsSecretsAndClips(t *testing.T) {
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Enabled: true}
	secret := "ghp_" + strings.Repeat("a", 36)
	long := strings.Repeat("é", 2000)

	if _, err := n.Toast(context.Background(), "", "token "+secret, "body "+secret+" "+long, 0); err != nil {
		t.Fatal(err)
	}
	got := h.shows[0]
	if strings.Contains(got.Title, secret) || strings.Contains(got.Body, secret) {
		t.Errorf("secret leaked into toast: %+v", got)
	}
	if n := len([]rune(got.Body)); n > MaxBodyRunes {
		t.Errorf("body not clipped: %d runes", n)
	}
	if !strings.HasSuffix(got.Body, "…") {
		t.Errorf("clipped body should end with an ellipsis")
	}
}

func TestSidebar(t *testing.T) {
	h := newFakeHerdr()
	n := &Notifier{Herdr: h}
	tokens := map[string]string{"pr": "#123", "state": "judging\nround 2"}

	if err := n.Sidebar(context.Background(), "w_1", tokens); err != nil {
		t.Fatalf("Sidebar: %v", err)
	}
	if len(h.metas) != 1 {
		t.Fatalf("metadata calls = %d", len(h.metas))
	}
	got := h.metas[0]
	if got.Workspace != "w_1" || got.Source != "magnum" || got.TTL != 24*time.Hour {
		t.Errorf("call = %+v; want workspace w_1, source magnum, ttl 24h", got)
	}
	want := map[string]string{"pr": "#123", "state": "judging round 2"}
	if !reflect.DeepEqual(got.Tokens, want) {
		t.Errorf("tokens = %v, want %v", got.Tokens, want)
	}
	if tokens["state"] != "judging\nround 2" {
		t.Errorf("caller's map was mutated")
	}
}

func TestSidebarWorksWhenToastsDisabled(t *testing.T) {
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Enabled: false}
	if err := n.Sidebar(context.Background(), "w_1", map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if len(h.metas) != 1 {
		t.Errorf("sidebar tokens are display-only and not gated by the toast switch")
	}
}

func TestSidebarErrors(t *testing.T) {
	ctx := context.Background()
	h := newFakeHerdr()
	h.metaErr = &herdr.Error{Method: "workspace.report_metadata", Code: herdr.CodeWorkspaceNotFound, Message: "gone"}
	n := &Notifier{Herdr: h}
	if err := n.Sidebar(ctx, "w_x", nil); !herdr.IsCode(err, herdr.CodeWorkspaceNotFound) {
		t.Errorf("err = %v; want wrapped workspace_not_found", err)
	}
	if err := n.Sidebar(ctx, "", nil); err == nil {
		t.Errorf("empty workspace id must be rejected")
	}
	if err := (&Notifier{}).Sidebar(ctx, "w", nil); !errors.Is(err, herdr.ErrUnavailable) {
		t.Errorf("nil herdr: err = %v; want ErrUnavailable", err)
	}
}

func TestSidebarRedactsTokenValues(t *testing.T) {
	h := newFakeHerdr()
	n := &Notifier{Herdr: h}
	secret := "ghp_" + strings.Repeat("b", 36)
	if err := n.Sidebar(context.Background(), "w", map[string]string{"err": "failed: " + secret}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.metas[0].Tokens["err"], secret) {
		t.Errorf("secret leaked into sidebar token")
	}
}

// logBuf collects log lines and, beside them, their levels
// (execx.LevelLogger).
type logBuf struct {
	lines  []string
	levels []slog.Level
}

func (l *logBuf) Printf(format string, args ...any) { l.Logf(slog.LevelInfo, format, args...) }

func (l *logBuf) Logf(level slog.Level, format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.levels = append(l.levels, level)
}

func TestToastDryRunPrintsInsteadOfShowing(t *testing.T) {
	inner := &execx.Fake{} // any real execution would fail: no rules
	dry := &execx.DryRun{Inner: inner, Out: &logBuf{}}
	n := &Notifier{Runner: dry, Enabled: true} // no herdr: straight to osascript

	sent, err := n.Toast(context.Background(), "", "T", "B", 0)
	if err != nil || !sent {
		t.Fatalf("Toast = %v, %v", sent, err)
	}
	if len(inner.Calls) != 0 {
		t.Errorf("dry-run executed osascript: %v", inner.Calls)
	}
	if len(dry.Planned) != 1 || dry.Planned[0].Name != osascript {
		t.Errorf("planned = %v; want the osascript toast", dry.Planned)
	}
}

func TestToastLogsSuppressionAndFallbackRedacted(t *testing.T) {
	log := &logBuf{}
	h := newFakeHerdr()
	h.showErr = fmt.Errorf("dial with ghp_%s: %w", strings.Repeat("d", 36), herdr.ErrUnavailable)
	n := &Notifier{Herdr: h, Store: &fakeStore{send: false}, Runner: osascriptFake(), Enabled: true, Log: log}

	if sent, _ := n.Toast(context.Background(), "k", "T", "B", time.Minute); sent {
		t.Fatalf("expected suppression")
	}
	n.Store = nil
	if sent, err := n.Toast(context.Background(), "", "T", "B", 0); !sent || err != nil {
		t.Fatalf("fallback toast = %v, %v", sent, err)
	}
	all := strings.Join(log.lines, "\n")
	if !strings.Contains(all, "suppressed") || !strings.Contains(all, "falling back to osascript") {
		t.Errorf("log = %q; want suppression and fallback lines", all)
	}
	if strings.Contains(all, "ghp_") {
		t.Errorf("token leaked into the log: %q", all)
	}
}

func TestToastFailedDeliveryReleasesDedupeKey(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = unavailable()
	n := &Notifier{Herdr: h, Store: realStore(t, c), Enabled: true} // no Runner: nothing can deliver
	ctx := context.Background()

	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); sent || err == nil {
		t.Fatalf("first Toast = %v, %v; want the delivery error", sent, err)
	}
	// Herdr is back, and the retry is still inside the dedupe window.
	h.showErr = nil
	h.shows = nil
	c.Add(time.Minute)
	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); !sent || err != nil {
		t.Fatalf("retry Toast = %v, %v; want delivered (the failed send must not consume the window)", sent, err)
	}
	if len(h.shows) != 1 {
		t.Errorf("herdr shows = %v, want exactly the retry", h.shows)
	}
	// The successful delivery does arm the gate.
	c.Add(time.Minute)
	if sent, err := n.Toast(ctx, "codex-login", "Codex", "needs login", 10*time.Minute); sent || err != nil {
		t.Fatalf("repeat Toast = %v, %v; want suppressed", sent, err)
	}
}

func TestToastHerdrDecliningKeepsDedupeKey(t *testing.T) {
	for _, reason := range []string{"disabled", "rate_limited", "busy"} {
		t.Run(reason, func(t *testing.T) {
			h := newFakeHerdr()
			h.showRes = herdr.NotificationResult{Shown: false, Reason: reason}
			st := &fakeStore{send: true}
			n := &Notifier{Herdr: h, Store: st, Enabled: true}

			if sent, err := n.Toast(context.Background(), "k", "T", "B", 10*time.Minute); sent || err != nil {
				t.Fatalf("Toast = %v, %v; want false, nil", sent, err)
			}
			if len(st.forgot) != 0 {
				t.Errorf("herdr declined on purpose; the dedupe record must stay (forgot %v)", st.forgot)
			}
		})
	}
}

func TestToastReleasesKeyEvenWhenContextIsCancelled(t *testing.T) {
	h := newFakeHerdr()
	h.showErr = errors.New("canceled mid-flight")
	st := &fakeStore{send: true}
	n := &Notifier{Herdr: h, Store: st, Enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	h.onShow = cancel

	if sent, err := n.Toast(ctx, "k", "T", "B", time.Hour); sent || err == nil {
		t.Fatalf("Toast = %v, %v; want an error", sent, err)
	}
	if want := []string{"k"}; !reflect.DeepEqual(st.forgot, want) {
		t.Errorf("forgot = %v, want %v", st.forgot, want)
	}
	if st.forgetCtxErr != nil {
		t.Errorf("ForgetSend ran with a dead context: %v", st.forgetCtxErr)
	}
}

func TestToastForgetErrorKeepsDeliveryError(t *testing.T) {
	deliveryErr := errors.New("delivery boom")
	h := newFakeHerdr()
	h.showErr = &herdr.Error{Method: "notification.show", Code: herdr.CodeInvalidRequest, Message: deliveryErr.Error()}
	st := &fakeStore{send: true, forgetErr: errors.New("db locked")}
	log := &logBuf{}
	n := &Notifier{Herdr: h, Store: st, Enabled: true, Log: log}

	sent, err := n.Toast(context.Background(), "k", "T", "B", time.Hour)
	if sent || !herdr.IsCode(err, herdr.CodeInvalidRequest) {
		t.Fatalf("Toast = %v, %v; want the herdr delivery error", sent, err)
	}
	if !strings.Contains(strings.Join(log.lines, "\n"), "db locked") {
		t.Errorf("release failure not logged: %v", log.lines)
	}
}
