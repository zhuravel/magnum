package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// inspTestConfig is a complete config.toml for the inspect tests; HOME is
// replaced with the test's temp home.
const inspTestConfig = `
[daemon]
default_repo = "talkable/talkable"
min_free_disk_gb = 1
max_concurrent_reviews = 3
max_total_working_codex = 5

[herdr]
socket = "HOME/no-herdr.sock"

[codex]
skill_path = "{{repo}}/skills/magnum-review/SKILL.md"

[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"
no_findings_event = "APPROVE"

[[identity]]
name = "talkable-app"
kind = "app"
login = "talkable[bot]"
app_id = 1
client_id = "Iv-test"
installation_id = 2
private_key_env = "MAGNUM_TEST_NO_KEY"
no_findings_event = "COMMENT"
blocking_event = "REQUEST_CHANGES"

[[watch]]
owner = "talkable"
include = ["talkable"]
identity = "talkable-app"
poll_identity = "zhuravel"
clone_root = "HOME"

[[watch]]
owner = "zhuravel"
include = ["*"]
identity = "zhuravel"
poll_identity = "zhuravel"
clone_root = "HOME"

[[pool]]
repo = "talkable/talkable"
main_clone = "HOME/talkable"
slot_name = "review{n}"
slot_path = "HOME/talkable.review{n}"
base = "master"
min = 2
max = 3
min_free_disk_gb = 1
setup = ["bin/worktree-setup"]
teardown = ["bin/worktree-archive"]
databases = ["talkable_development__{slug}", "talkable_test__{slug}"]

  [pool.env]
  WT_BRANCH = "{slot}"
`

// inspFixture is a temp magnum home with a config, a scripted runner and
// captured output.
type inspFixture struct {
	t      *testing.T
	Home   string
	Ctx    *Context
	Out    *bytes.Buffer
	Err    *bytes.Buffer
	Runner *execx.Fake
}

func newInspFixture(t *testing.T) *inspFixture {
	t.Helper()
	t.Setenv("MAGNUM_CONFIG", "") // config.Load must read the fixture's config.toml
	home := t.TempDir()
	cfg := strings.ReplaceAll(inspTestConfig, "HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &inspFixture{t: t, Home: home, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Runner: &execx.Fake{}}
	f.Ctx = &Context{Version: "test", Layout: paths.Layout{Home: home}, Stdout: f.Out, Stderr: f.Err}
	prevHook, prevStdin, prevTTY, prevPoll, prevWait := inspAppHook, inspStdin, inspIsTTY, inspPoll, inspHandoffWait
	inspAppHook = func(o *app.Options) {
		o.Runner = f.Runner
		o.MySQLDSN = "root:@tcp(127.0.0.1:1)/?timeout=1s"
		o.Getenv = func(string) string { return "" }
		o.Logger = slog.New(slog.DiscardHandler)
	}
	inspStdin = strings.NewReader("")
	inspIsTTY = func() bool { return false }
	inspPoll = 5 * time.Millisecond
	inspHandoffWait = 50 * time.Millisecond
	t.Cleanup(func() {
		inspAppHook, inspStdin, inspIsTTY, inspPoll, inspHandoffWait = prevHook, prevStdin, prevTTY, prevPoll, prevWait
	})
	return f
}

// store opens the fixture's registry for seeding.
func (f *inspFixture) store() *store.Store {
	f.t.Helper()
	if err := f.Ctx.Layout.EnsureDirs(); err != nil {
		f.t.Fatal(err)
	}
	st, err := store.Open(f.Ctx.Layout.DB())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { st.Close() })
	return st
}

// tty makes confirmations read answers from input.
func (f *inspFixture) tty(input string) {
	inspIsTTY = func() bool { return true }
	inspStdin = strings.NewReader(input)
}

// run runs a command through the cobra tree and returns its exit code.
func (f *inspFixture) run(name string, args ...string) int {
	f.t.Helper()
	f.Out.Reset()
	f.Err.Reset()
	return execute(f.Ctx, append([]string{name}, args...))
}

// seedPR inserts repo owner/name (if needed) and PR number in state.
func inspSeedPR(t *testing.T, st *store.Store, fullName string, number int, state string, set func(*store.PRUpdate)) (store.Repo, store.PR) {
	t.Helper()
	ctx := context.Background()
	owner, name, _ := strings.Cut(fullName, "/")
	repo, err := st.RepoByFullName(ctx, fullName)
	if err != nil {
		mode := store.RepoModePerPR
		if fullName == "talkable/talkable" {
			mode = store.RepoModePool
		}
		repo, err = st.UpsertRepo(ctx, store.Repo{NodeID: "R_" + fullName, Owner: owner, Name: name, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
	}
	title := "PR " + strings.Repeat("x", 3)
	res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{
		RepoID: repo.ID, NodeID: "PR_" + fullName + "_" + strconv.Itoa(number), Number: number,
		URL: "https://github.com/" + fullName + "/pull/" + strconv.Itoa(number), HeadSHA: "abcdef0123456789abcdef0123456789abcdef01",
		Title: &title, AuthorLogin: store.Ptr("dev"), GHState: store.GHOpen, InitialState: store.PRBaseline, Identity: "talkable-app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionPR(ctx, res.PR.ID, nil, state, set); err != nil {
		t.Fatal(err)
	}
	pr, err := st.PRByID(ctx, res.PR.ID)
	if err != nil {
		t.Fatal(err)
	}
	return repo, pr
}

func TestInspFormatting(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	past := now.Add(-90 * time.Minute)
	future := now.Add(5 * time.Minute)
	cases := map[string]string{
		inspDur(45 * time.Second):           "45s",
		inspDur(12 * time.Minute):           "12m",
		inspDur(125 * time.Minute):          "2h5m",
		inspDur(76 * time.Hour):             "3d4h",
		inspAgo(now, &past):                 "1h30m ago",
		inspAgo(now, &future):               "in 5m",
		inspAgo(now, nil):                   "-",
		inspBytes(2048):                     "2K",
		inspBytes(5 << 20):                  "5M",
		inspBytes(3 << 29):                  "1.5G",
		inspMB(800):                         "800M",
		sha7("abcdef0123"):                  "abcdef0",
		trunc("hello world", 6):             "hello…",
		inspPRLabel("talkable/talkable", 7): "talkable#7",
		inspShellQuote("~/Projects/a b"):    "~/'Projects/a b'",
		inspShellQuote("/tmp/x"):            "/tmp/x",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestInspConfirm(t *testing.T) {
	f := newInspFixture(t)
	if inspConfirm(context.Background(), f.Ctx, "go?") {
		t.Fatal("non-tty must never confirm")
	}
	f.tty("y\nreview9\n")
	if !inspConfirm(context.Background(), f.Ctx, "go?") {
		t.Fatal("y must confirm")
	}
	if !inspConfirmTyped(context.Background(), f.Ctx, "drop?", "review9") {
		t.Fatal("typed slug must confirm")
	}
	f.tty("yes\nreview8\n")
	inspConfirm(context.Background(), f.Ctx, "go?")
	if inspConfirmTyped(context.Background(), f.Ctx, "drop?", "review9") {
		t.Fatal("wrong slug must not confirm")
	}
}

func TestInspResolve(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, nil)
	if _, err := st.CreateSlot(ctx, store.Slot{Name: "review1", Kind: store.SlotKindPool, Path: f.Home + "/talkable.review1",
		MainClone: f.Home + "/talkable", RepoFullName: "talkable/talkable", State: store.SlotHeld, PRID: &pr.ID}); err != nil {
		t.Fatal(err)
	}
	refs := app.RefParser{DefaultRepo: "talkable/talkable"}
	got, err := inspResolveIn(ctx, st, refs, "review1")
	if err != nil || got.Slot == nil || got.PR == nil || got.PR.Number != 11920 {
		t.Fatalf("slot ref: %+v %v", got, err)
	}
	got, err = inspResolveIn(ctx, st, refs, "11920")
	if err != nil || got.Slot != nil || got.PR == nil || got.Label() != "talkable/talkable#11920" {
		t.Fatalf("pr ref: %+v %v", got, err)
	}
	if _, err := inspResolveIn(ctx, st, refs, "not a ref!"); err == nil || !strings.Contains(err.Error(), "neither a slot name nor a PR reference") {
		t.Fatalf("bad ref err = %v", err)
	}
	if _, err := inspResolveIn(ctx, st, refs, "4"); err == nil || !strings.Contains(err.Error(), "not in the registry") {
		t.Fatalf("unknown PR err = %v", err)
	}
}

func TestInspSubmitWaitsForCompletion(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if req, err := st.NextPendingRequest(ctx); err == nil {
				_ = st.CompleteRequest(ctx, req.ID, store.RequestDone, "pinned review1")
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	out, err := inspHandOff(ctx, f.Ctx, st, "pin", map[string]string{"slot": "review1"}, 2*time.Second)
	<-done
	if err != nil || out.PID != 0 || out.Req.State != store.RequestDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if code := out.print(f.Ctx.Stdout, f.Ctx.Stderr); code != 0 || !strings.Contains(f.Out.String(), "pinned review1") {
		t.Fatalf("code=%d out=%q", code, f.Out.String())
	}
}
