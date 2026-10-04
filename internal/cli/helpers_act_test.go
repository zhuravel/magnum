package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

// actFakeHerdr records the herdr calls of the act commands.
type actFakeHerdr struct {
	snap     herdr.Snapshot
	snapErr  error
	focused  []string
	focusErr map[string]error
	reads    []herdr.ReadResult // returned in order; the last one repeats
	readErr  error
	readOpts []herdr.ReadOptions
	opened   []herdr.PluginPaneOptions
	openErr  error
}

func (f *actFakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) { return f.snap, f.snapErr }

func (f *actFakeHerdr) AgentFocus(_ context.Context, target string) error {
	if err := f.focusErr[target]; err != nil {
		return err
	}
	f.focused = append(f.focused, target)
	return nil
}

func (f *actFakeHerdr) PaneRead(_ context.Context, _ string, o herdr.ReadOptions) (herdr.ReadResult, error) {
	f.readOpts = append(f.readOpts, o)
	if f.readErr != nil {
		return herdr.ReadResult{}, f.readErr
	}
	if len(f.reads) == 0 {
		return herdr.ReadResult{}, nil
	}
	r := f.reads[0]
	if len(f.reads) > 1 {
		f.reads = f.reads[1:]
	}
	return r, nil
}

func (f *actFakeHerdr) PluginPaneOpen(_ context.Context, o herdr.PluginPaneOptions) (herdr.PluginPane, error) {
	f.opened = append(f.opened, o)
	return herdr.PluginPane{PluginID: o.PluginID, Entrypoint: o.Entrypoint, Pane: herdr.Pane{ID: "p_popup"}}, f.openErr
}

type actFakeGitHub struct {
	details map[int]github.PRDetails
	calls   [][]int
	err     error
}

func (f *actFakeGitHub) Details(_ context.Context, _, _ string, numbers []int) (map[int]github.PRDetails, []int, error) {
	f.calls = append(f.calls, numbers)
	if f.err != nil {
		return nil, nil, f.err
	}
	out := map[int]github.PRDetails{}
	var missing []int
	for _, n := range numbers {
		if d, ok := f.details[n]; ok {
			out[n] = d
		} else {
			missing = append(missing, n)
		}
	}
	return out, missing, nil
}

type actFakeCleaner struct {
	opts    []cleanup.Options
	plan    cleanup.Plan
	applied int
	report  cleanup.Report
	err     error
}

func (f *actFakeCleaner) Plan(_ context.Context, o cleanup.Options) (cleanup.Plan, error) {
	f.opts = append(f.opts, o)
	p := f.plan
	p.Options = o
	return p, nil
}

func (f *actFakeCleaner) Apply(context.Context, cleanup.Plan, bool) (cleanup.Report, error) {
	f.applied++
	return f.report, f.err
}

// actHarness is a Context plus fake dependencies for one act command test.
type actHarness struct {
	t       *testing.T
	ctx     context.Context
	c       *Context
	d       *actDeps
	st      *store.Store
	hd      *actFakeHerdr
	gh      *actFakeGitHub
	run     *execx.Fake
	tty     *execx.Fake
	cleaner *actFakeCleaner
	out     bytes.Buffer
	errb    bytes.Buffer
	home    string
	now     time.Time
	pid     int // daemon pid Kick reports (0 = none)
	kicks   int
	held    bool // the daemon holds the lock
	busy    bool // another magnum command holds ops.lock
	locked  int
	reveals []reveal.Options
	env     map[string]string
	sleeps  int
	// onSleep plays the daemon between polls.
	onSleep func(h *actHarness)
}

func newActHarness(t *testing.T) *actHarness {
	t.Helper()
	t.Setenv("MAGNUM_CONFIG", "")
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	cfg := config.Defaults()
	cfg.Layout = layout
	cfg.Daemon.DefaultRepo = "talkable/talkable"
	cfg.Herdr.Socket = filepath.Join(home, "herdr.sock")
	cfg.Identities = []config.Identity{
		{Name: "zhuravel", Kind: "gh", Login: "zhuravel", NoFindingsEvent: "APPROVE"},
		{Name: "talkable-app", Kind: "app", Login: "talkable[bot]", NoFindingsEvent: "COMMENT"},
	}
	cfg.Watches = []config.Watch{
		{Owner: "talkable", Include: []string{"talkable"}, Identity: "talkable-app", PollIdentity: "zhuravel", CloneRoot: home},
		{Owner: "zhuravel", Include: []string{"*"}, Identity: "zhuravel", PollIdentity: "zhuravel", CloneRoot: home},
	}
	cfg.Pools = []config.Pool{{Repo: "talkable/talkable", MainClone: filepath.Join(home, "talkable"), SlotName: "review{n}",
		SlotPath: filepath.Join(home, "talkable.review{n}"), Base: "master"}}
	st, err := store.Open(filepath.Join(home, "state", "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := &actHarness{t: t, ctx: context.Background(), st: st, home: home, hd: &actFakeHerdr{},
		gh: &actFakeGitHub{details: map[int]github.PRDetails{}}, run: &execx.Fake{}, tty: &execx.Fake{},
		cleaner: &actFakeCleaner{}, env: map[string]string{},
		now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	st.Clock = func() time.Time { return h.now }
	h.c = &Context{Version: "test", Layout: layout, Config: cfg, Stdout: &h.out, Stderr: &h.errb}
	h.d = &actDeps{
		Cfg: cfg, Layout: layout, Store: st, Herdr: h.hd,
		GitHub: func(id string) actGitHub {
			if id == "zhuravel" {
				return h.gh
			}
			return nil
		},
		GHEnv: func(string) map[string]string { return map[string]string{} },
		Run:   h.run, TTY: h.tty, Cleanup: h.cleaner,
		Reveal: func(_ context.Context, o reveal.Options) (reveal.Outcome, error) {
			h.reveals = append(h.reveals, o)
			return reveal.Outcome{Action: reveal.ActionLaunched, Kind: reveal.KindITerm, Session: "default"}, nil
		},
		Kick: func() (int, error) { h.kicks++; return h.pid, nil },
		Lock: func() (func(), opsHolder, error) {
			switch {
			case h.held:
				return nil, opsDaemon, nil
			case h.busy:
				return nil, opsBusy, nil
			}
			h.locked++
			return func() { h.locked-- }, opsMine, nil
		},
		Now: func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.sleeps++
			if h.sleeps > 500 {
				return errors.New("test: too many sleeps")
			}
			h.now = h.now.Add(d)
			if h.onSleep != nil {
				h.onSleep(h)
			}
			return ctx.Err()
		},
		Stdin:  strings.NewReader(""),
		Getenv: func(k string) string { return h.env[k] },
		Poll:   2 * time.Second, Quick: 10 * time.Second,
	}
	old := actNewDeps
	actNewDeps = func(*Context, actMode) (*actDeps, error) { return h.d, nil }
	t.Cleanup(func() {
		actNewDeps = old
		_ = st.Close()
	})
	return h
}

// cmd runs a command through the cobra tree and returns its exit code.
func (h *actHarness) cmd(name string, args ...string) int {
	h.t.Helper()
	return execute(h.c, append([]string{name}, args...))
}

func (h *actHarness) stdin(s string) { h.d.Stdin, h.d.in = strings.NewReader(s), nil }

// seedPR records talkable/talkable#n (or another repo) in the registry.
func (h *actHarness) seedPR(full string, n int, state string) store.PR {
	h.t.Helper()
	owner, name, _ := strings.Cut(full, "/")
	mode := store.RepoModePerPR
	if full == "talkable/talkable" {
		mode = store.RepoModePool
	}
	repo, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_" + full, Owner: owner, Name: name, Mode: mode})
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{RepoID: repo.ID, NodeID: fmt.Sprintf("PR_%s_%d", full, n), Number: n,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", full, n), HeadSHA: "abc1234def5678", Title: store.Ptr("Fix coupon export"),
		AuthorLogin: store.Ptr("alice"), GHState: store.GHOpen, InitialState: state, Identity: "talkable-app"})
	if err != nil {
		h.t.Fatal(err)
	}
	return res.PR
}

func (h *actHarness) session(prID int64, role, state, agent, pane, workspace string) store.Session {
	h.t.Helper()
	s := store.Session{PRID: prID, Role: role, State: state}
	if agent != "" {
		s.AgentName = store.Ptr(agent)
	}
	if pane != "" {
		s.HerdrPaneID = store.Ptr(pane)
	}
	if workspace != "" {
		s.HerdrWorkspaceID = store.Ptr(workspace)
	}
	out, err := h.st.CreateSession(h.ctx, s)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *actHarness) setPR(id int64, to string, set func(u *store.PRUpdate)) {
	h.t.Helper()
	if err := h.st.TransitionPR(h.ctx, id, nil, to, set); err != nil {
		h.t.Fatal(err)
	}
}

// requests lists every request, oldest first.
func (h *actHarness) requests() []store.Request {
	h.t.Helper()
	var out []store.Request
	for id := int64(1); ; id++ {
		r, err := h.st.RequestByID(h.ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return out
		}
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
}

// completePending plays the daemon: it completes every pending request.
func (h *actHarness) completePending(state, result string) {
	for _, r := range h.requests() {
		if r.State == store.RequestPending {
			if err := h.st.CompleteRequest(h.ctx, r.ID, state, result); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

func actDecode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func actContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output lacks %q:\n%s", w, got)
		}
	}
}

func TestActLabelAndClean(t *testing.T) {
	h := newActHarness(t)
	if got := h.d.actLabel("talkable/talkable", 5); got != "talkable#5" {
		t.Errorf("default owner label = %q", got)
	}
	if got := h.d.actLabel("zhuravel/widgets", 7); got != "zhuravel/widgets#7" {
		t.Errorf("other owner label = %q", got)
	}
	if got := actClean("evil\x1b[2Jtitle\n"); strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("control characters kept: %q", got)
	}
	if got := actShellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Errorf("quote = %s", got)
	}
}

func TestActTTYRunnerCapturesStdoutAndExitCode(t *testing.T) {
	var errb bytes.Buffer
	r := actTTYRunner{Stderr: &errb}
	res, err := r.Run(context.Background(), execx.Cmd{Name: "/bin/sh", Args: []string{"-c", `cat; echo "$MAGNUM_X" >&2; exit 3`},
		Stdin: []byte("lines\n"), Env: map[string]string{"MAGNUM_X": "env-ok"}})
	var ee *execx.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || string(res.Stdout) != "lines\n" || strings.TrimSpace(errb.String()) != "env-ok" {
		t.Fatalf("res %q stderr %q err %v", res.Stdout, errb.String(), err)
	}
}

func TestActTerminalIgnoresDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if actTerminal(f) {
		t.Error("/dev/null counted as a terminal")
	}
	if actIsTTY(&bytes.Buffer{}) {
		t.Error("a buffer counted as a terminal")
	}
}
