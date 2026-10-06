package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

const testConfig = `
[daemon]
default_repo = "talkable/talkable"

[herdr]
socket = "/nonexistent/herdr.sock"

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
private_key_env = "MAGNUM_TEST_KEY"
no_findings_event = "COMMENT"
blocking_event = "REQUEST_CHANGES"

[[watch]]
owner = "talkable"
include = ["talkable"]
identity = "talkable-app"
poll_identity = "zhuravel"

[[watch]]
owner = "zhuravel"
include = ["*"]
identity = "zhuravel"
`

func testApp(t *testing.T, opts Options) (*App, paths.Layout) {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home}
	cfg, err := config.Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Runner == nil {
		opts.Runner = &execx.Fake{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	a, err := New(cfg, layout, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, layout
}

func TestNewWiresEverything(t *testing.T) {
	a, layout := testApp(t, Options{})
	if a.Store == nil || a.Herdr == nil || a.Git == nil || a.Slots == nil || a.Inventory == nil ||
		a.Agents == nil || a.Cleanup == nil || a.Notify == nil || a.MySQL == nil {
		t.Fatalf("missing component: %+v", a)
	}
	if a.storePath != layout.DB() {
		t.Fatalf("store path = %s, want %s", a.storePath, layout.DB())
	}
	if a.Herdr.Socket != "/nonexistent/herdr.sock" {
		t.Fatalf("herdr socket = %q", a.Herdr.Socket)
	}
	if len(a.Pipeline) != 2 || a.Pipeline["talkable-app"] == nil || a.Pipeline["zhuravel"] == nil {
		t.Fatalf("pipeline runners: %v", a.Pipeline)
	}
	if got := a.Pipeline["talkable-app"].SelfLogin; got != "zhuravel" {
		t.Fatalf("self login = %q", got)
	}
	app := a.GitHub("talkable-app")
	if app == nil || app.Env["GH_CONFIG_DIR"] != layout.GhConfigDir("talkable-app") ||
		app.Env["GH_TOKEN"] != "" || len(app.Env) != 3 {
		t.Fatalf("app gh client env = %+v", app)
	}
	if gh := a.GitHub("zhuravel"); gh == nil || len(gh.Env) != 0 {
		t.Fatalf("gh client = %+v", gh)
	}
	if a.GitHub("nobody") != nil {
		t.Fatal("unknown identity must give nil")
	}
	if a.Identities["talkable-app"].Kind() != "app" || a.Identities["zhuravel"].Kind() != "gh" {
		t.Fatalf("identities: %v", a.Identities)
	}
	if !a.Notify.Enabled {
		t.Fatal("notify should follow [herdr] notify")
	}
	if a.DryRunner != nil || a.DryRun {
		t.Fatal("not a dry run")
	}
	for _, d := range []string{layout.State(), layout.Logs(), layout.Reviews(), layout.GhRoot()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("state dir %s: %v", d, err)
		}
	}
}

// The mise executable of Options.Mise reaches every round runner, which runs
// the readiness step's reset_db commands through it as the slots manager runs
// the release's; without one they use "mise" on PATH.
func TestNewGivesEveryRoundRunnerTheMiseExecutable(t *testing.T) {
	for _, mise := range []string{"", "/opt/example/bin/mise"} {
		a, _ := testApp(t, Options{Mise: mise})
		if a.Mise != mise || len(a.Pipeline) != 2 {
			t.Fatalf("Mise = %q, %d runners", a.Mise, len(a.Pipeline))
		}
		for name, r := range a.Pipeline {
			if r.Mise != mise {
				t.Errorf("runner %s: Mise = %q, want %q", name, r.Mise, mise)
			}
		}
	}
}

func TestDryRunUsesStoreCopy(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	real, err := store.Open(layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := real.UpsertRepo(ctx, store.Repo{NodeID: "R1", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool}); err != nil {
		t.Fatal(err)
	}
	if err := real.SetKV(ctx, "probe", "real"); err != nil {
		t.Fatal(err)
	}
	if err := real.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a, err := New(cfg, layout, Options{DryRun: true, Runner: &execx.Fake{}, Stderr: &logs})
	if err != nil {
		t.Fatal(err)
	}
	if a.storePath == layout.DB() {
		t.Fatal("dry run must not open the real registry")
	}
	if a.DryRunner == nil || a.Runner != a.DryRunner {
		t.Fatal("dry run must wrap the runner")
	}
	if a.Notify.Enabled {
		t.Fatal("dry run must disable toasts")
	}
	if v, _, _ := a.Store.GetKV(ctx, "probe"); v != "real" {
		t.Fatalf("copy lacks the real rows: %q", v)
	}
	if err := a.Store.SetKV(ctx, "probe", "dry"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Runner.Run(ctx, execx.Cmd{Name: "git", Args: []string{"push"}, Mutates: true}); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Dir(a.storePath)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run copy not removed: %v", err)
	}
	if _, err := os.Stat(layout.DaemonLog()); !os.IsNotExist(err) {
		t.Fatalf("dry run must not write daemon.log: %v", err)
	}
	real, err = store.Open(layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	if v, _, _ := real.GetKV(ctx, "probe"); v != "real" {
		t.Fatalf("real registry changed: %q", v)
	}
	if !strings.Contains(logs.String(), "git push") {
		t.Fatalf("planned command not logged: %s", logs.String())
	}
}

func TestDryRunWithoutRegistry(t *testing.T) {
	a, layout := testApp(t, Options{DryRun: true})
	if _, err := os.Stat(layout.DB()); !os.IsNotExist(err) {
		t.Fatalf("dry run created the real registry: %v", err)
	}
	if err := a.Store.SetKV(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePR(t *testing.T) {
	a, _ := testApp(t, Options{})
	ctx := context.Background()
	cases := map[string]string{
		"11932":           "talkable/talkable#11932",
		"#7":              "talkable/talkable#7",
		"other#3":         "talkable/other#3",
		"zhuravel/app#12": "zhuravel/app#12",
		"https://github.com/zhuravel/x/pull/5/files": "zhuravel/x#5",
	}
	for ref, want := range cases {
		o, r, n, err := a.Refs().ResolvePR(ctx, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if got := o + "/" + r + "#" + itoa(n); got != want {
			t.Fatalf("%s: got %s want %s", ref, got, want)
		}
	}
	if _, _, _, err := a.Refs().ResolvePR(ctx, "not a ref"); err == nil {
		t.Fatal("bad ref must fail")
	}
}

func TestLookupPR(t *testing.T) {
	a, _ := testApp(t, Options{})
	ctx := context.Background()
	if _, _, err := LookupPR(ctx, a.Store, a.Refs(), "5"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown repo: %v", err)
	}
	repo, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupPR(ctx, a.Store, a.Refs(), "5"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown pr: %v", err)
	}
	if _, err := a.Store.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "P", Number: 5,
		URL: "u", HeadSHA: "abc", InitialState: store.PRBaseline, Identity: "talkable-app"}); err != nil {
		t.Fatal(err)
	}
	r, pr, err := LookupPR(ctx, a.Store, a.Refs(), "talkable#5")
	if err != nil || r.ID != repo.ID || pr.Number != 5 {
		t.Fatalf("lookup: %v %+v %+v", err, r, pr)
	}

	// repo#N under another owner than the default repository's: the one
	// registered repository of that name is meant; two are ambiguous.
	other, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R2", Owner: "example", Name: "widgets", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: other.ID, NodeID: "P2", Number: 7,
		URL: "u7", HeadSHA: "def", InitialState: store.PRBaseline, Identity: "talkable-app"}); err != nil {
		t.Fatal(err)
	}
	if r, pr, err := LookupPR(ctx, a.Store, a.Refs(), "widgets#7"); err != nil || r.ID != other.ID || pr.Number != 7 {
		t.Fatalf("repo#N under another owner: %v %+v %+v", err, r, pr)
	}
	if _, _, err := LookupPR(ctx, a.Store, a.Refs(), "example/widgets#7"); err != nil {
		t.Fatalf("owner/repo#N: %v", err)
	}
	if _, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R3", Owner: "another", Name: "widgets", Mode: store.RepoModePerPR}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupPR(ctx, a.Store, a.Refs(), "widgets#7"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two repos named widgets must be ambiguous: %v", err)
	}
}

// The repository-name fallback is for repo#N only: a bare number or #N names
// the default repository and must not be redirected to a same-named
// repository under another owner.
func TestLookupPRNameFallbackIsForRepoRefsOnly(t *testing.T) {
	a, _ := testApp(t, Options{}) // default repository talkable/talkable, not in the registry
	ctx := context.Background()
	other, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R9", Owner: "example", Name: "talkable", Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: other.ID, NodeID: "P9", Number: 7,
		URL: "u9", HeadSHA: "abc", InitialState: store.PRBaseline, Identity: "talkable-app"}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"7", "#7", " 7 "} {
		if r, _, err := LookupPR(ctx, a.Store, a.Refs(), ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("LookupPR(%q) = %s, %v; want not found (the default repository is not registered)", ref, r.FullName(), err)
		}
	}
	r, pr, err := LookupPR(ctx, a.Store, a.Refs(), "talkable#7")
	if err != nil || r.ID != other.ID || pr.Number != 7 {
		t.Fatalf("talkable#7: %v %+v %+v", err, r, pr)
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	rf, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := rf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Size() > 100 {
			t.Fatalf("%s grew to %d bytes", p, st.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("kept more than 2 rotations: %v", err)
	}
	// Reopening appends to the current file.
	rf, err = OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if rf.size == 0 {
		t.Fatal("reopen lost the current size")
	}
}

// logAppIn builds an App (default logger, no dry run) whose daemon.log already
// holds LogMaxBytes bytes, so the first write would rotate a rotating writer.
func logAppIn(t *testing.T, daemon bool) (*App, string) {
	t.Helper()
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	if err := os.MkdirAll(layout.Logs(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := layout.DaemonLog()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, LogMaxBytes); err != nil { // sparse: cheap
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, layout, Options{Runner: &execx.Fake{}, Stderr: io.Discard, Daemon: daemon})
	if err != nil {
		t.Fatal(err)
	}
	return a, path
}

func TestOnlyTheDaemonRotatesDaemonLog(t *testing.T) {
	t.Run("cli process appends without rotating", func(t *testing.T) {
		a, path := logAppIn(t, false)
		a.Logger.Info("from a cli command")
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
			t.Fatalf("a non-daemon process rotated daemon.log: %v", err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() <= LogMaxBytes {
			t.Fatalf("daemon.log size = %d; the line should have been appended past %d", st.Size(), LogMaxBytes)
		}
	})
	t.Run("daemon rotates", func(t *testing.T) {
		a, path := logAppIn(t, true)
		a.Logger.Info("from the daemon")
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".1"); err != nil {
			t.Fatalf("the daemon should have rotated an oversize daemon.log: %v", err)
		}
		if st, err := os.Stat(path); err != nil || st.Size() == 0 || st.Size() >= LogMaxBytes {
			t.Fatalf("fresh daemon.log: %v %v", st, err)
		}
	})
}

func TestOpenDaemonLogWithoutRotationAppendsPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	w, err := openDaemonLog(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.(*RotatingFile); ok {
		t.Fatal("non-daemon log writer must not be a RotatingFile")
	}
	if _, err := w.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = openDaemonLog(path, false) // reopening appends
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("log = %q, %v", data, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", st.Mode(), err)
	}
	d, err := openDaemonLog(filepath.Join(t.TempDir(), "d.log"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, ok := d.(*RotatingFile); !ok {
		t.Fatalf("daemon log writer = %T, want *RotatingFile", d)
	}
}

func TestLoggerRedacts(t *testing.T) {
	var file, term bytes.Buffer
	l := NewLogger(&file, &term, nil)
	token := "ghs_" + strings.Repeat("A", 36)
	l.Info("minted "+token, "token", token, "err", errors.New("bad "+token), slog.Group("g", "t", token))
	l.With("ctx", token).Warn("again")
	for name, out := range map[string]string{"file": file.String(), "term": term.String()} {
		if strings.Contains(out, token) {
			t.Fatalf("%s leaked the token: %s", name, out)
		}
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(file.String(), "\n", 2)[0]), &rec); err != nil {
		t.Fatalf("file is not JSON lines: %v", err)
	}
	if rec["msg"] == nil {
		t.Fatalf("record: %v", rec)
	}
	var p Printf
	p.Printf("no logger: %d", 1) // must not panic
	Printf{Logger: l, Src: "exec"}.Printf("ran %s", token)
	if strings.Contains(file.String(), token) {
		t.Fatal("Printf leaked the token")
	}
}

func TestLoggerRedactsStructuredValues(t *testing.T) {
	var file, term bytes.Buffer
	l := NewLogger(&file, &term, nil)
	token := "ghs_" + strings.Repeat("B", 36)
	type payload struct{ Auth string }
	l.Info("structured",
		"list", []string{"fine", "Bearer " + token},
		"map", map[string]string{"k": token},
		"obj", payload{Auth: token},
		"clean", []string{"a", "b"},
		"num", []int{1, 2})
	for name, out := range map[string]string{"file": file.String(), "term": term.String()} {
		if strings.Contains(out, token) {
			t.Fatalf("%s leaked the token: %s", name, out)
		}
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(file.String(), "\n", 2)[0]), &rec); err != nil {
		t.Fatalf("file is not JSON lines: %v", err)
	}
	for _, k := range []string{"list", "map", "obj"} {
		if _, ok := rec[k].(string); !ok {
			t.Errorf("%s = %#v; a redacted structured value becomes a redacted string", k, rec[k])
		}
	}
	// A value with nothing to redact keeps its structure.
	if got, ok := rec["clean"].([]any); !ok || len(got) != 2 {
		t.Errorf("clean = %#v; untouched values must stay structured", rec["clean"])
	}
	if got, ok := rec["num"].([]any); !ok || len(got) != 2 {
		t.Errorf("num = %#v", rec["num"])
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestGitHubHTTPClient(t *testing.T) {
	run := &execx.Fake{}
	c := githubHTTPClient("gh", run, nil)
	tr, ok := c.Transport.(*identity.GhTransport)
	if !ok || tr.Run != run || c.Timeout <= 0 {
		t.Fatalf("gh transport client = %+v", c)
	}
	if c := githubHTTPClient("direct", run, nil); c != nil {
		t.Fatalf("direct must leave identity.NewApp's net/http client, got %+v", c)
	}
	override := &http.Client{}
	if c := githubHTTPClient("gh", run, override); c != override {
		t.Fatal("Options.HTTPClient must win")
	}
}

func TestAppIdentityMintsThroughGhByDefault(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "--method", "POST", "--include", "--hostname", "github.com", "app/installations/2/access_tokens"},
		Result: execx.Result{Stdout: []byte("HTTP/2.0 201 Created\nContent-Type: application/json\r\n\r\n" +
			`{"token":"ghs_fromgh","expires_at":"` + expires + `"}`)},
	}}}
	a, _ := testApp(t, Options{Runner: run, Getenv: func(k string) string {
		if k == "MAGNUM_TEST_KEY" {
			return keyPEM
		}
		return ""
	}})
	if a.Config.GitHub.Transport != "gh" {
		t.Fatalf("default transport = %q", a.Config.GitHub.Transport)
	}
	src, ok := a.Identities["talkable-app"].(*identity.App)
	if !ok {
		t.Fatalf("identity = %T", a.Identities["talkable-app"])
	}
	tok, err := src.Token(context.Background())
	if err != nil || tok != "ghs_fromgh" {
		t.Fatalf("token %q err %v (calls %v)", tok, err, run.Calls)
	}
	calls := run.CallsWithPrefix("gh", "api", "--method", "POST")
	if len(calls) != 1 || !slices.Contains(calls[0].Args, "--include") {
		t.Fatalf("mint calls %v", run.Calls)
	}
}

// An App identity's GitHub client re-mints the installation token after a
// 401 (identity.App.Reauth) and retries the call once.
func TestAppIdentityClientReauthsOn401(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var mu sync.Mutex
	mints, queries := 0, 0
	run := &execx.Fake{Rules: []execx.Rule{
		{
			Prefix: []string{"gh", "api", "--method", "POST", "--include", "--hostname", "github.com", "app/installations/2/access_tokens"},
			Fn: func(execx.Cmd) (execx.Result, error) {
				mu.Lock()
				mints++
				mu.Unlock()
				return execx.Result{Stdout: []byte("HTTP/2.0 201 Created\nContent-Type: application/json\r\n\r\n" +
					`{"token":"ghs_fresh","expires_at":"` + expires + `"}`)}, nil
			},
		},
		{
			Prefix: []string{"gh", "api", "graphql"},
			Fn: func(c execx.Cmd) (execx.Result, error) {
				mu.Lock()
				queries++
				n := queries
				mu.Unlock()
				if n == 1 { // the revoked token
					stderr := "gh: Bad credentials (HTTP 401)"
					res := execx.Result{Stdout: []byte(`{"message":"Bad credentials","status":"401"}`), Stderr: []byte(stderr), Code: 1}
					return res, &execx.ExitError{Cmd: c, Code: 1, Stderr: stderr}
				}
				return execx.Result{Stdout: []byte(`{"data":{"rateLimit":{"limit":5000,"cost":1,"remaining":4999,"used":1,"resetAt":"2030-01-01T00:00:00Z"},` +
					`"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`)}, nil
			},
		},
	}}
	a, _ := testApp(t, Options{Runner: run, Getenv: func(k string) string {
		if k == "MAGNUM_TEST_KEY" {
			return keyPEM
		}
		return ""
	}})
	gh := a.GitHub("talkable-app")
	if gh == nil || gh.Reauth == nil {
		t.Fatalf("the App identity's client has no Reauth: %+v", gh)
	}
	if _, err := gh.ReviewsWithMarker(context.Background(), "talkable", "talkable", 1, ""); err != nil {
		t.Fatalf("call after a 401: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if mints != 1 || queries != 2 {
		t.Fatalf("mints %d queries %d, want one re-mint and one retry", mints, queries)
	}
	if c := a.GitHub("zhuravel"); c == nil || c.Reauth != nil {
		t.Fatal("a gh identity has no token to re-mint")
	}
}

// The exec transcript reaches the log at the level execx picks: a
// successful command at debug (hidden at the daemon's info level), a
// failure at warn.
func TestPrintfLogsAtExecxLevels(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := &execx.Real{Log: Printf{Logger: l, Level: slog.LevelInfo, Src: "exec"}}
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "sh", Args: []string{"-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "sh", Args: []string{"-c", "exit 3"}}); err == nil {
		t.Fatal("exit 3 must fail")
	}
	out := buf.String()
	if strings.Contains(out, "sh -c true") || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "exit 3") || !strings.Contains(out, "src=exec") {
		t.Fatalf("log:\n%s", out)
	}
	var p Printf
	p.Logf(slog.LevelWarn, "no logger: %d", 1) // must not panic
}
