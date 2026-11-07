package identity

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// fixerIdentity is the fake server's App, narrowed as a fixer App is: one
// repository, contents and pull_requests write, its bot's user id.
func fixerIdentity() config.Identity {
	id := appIdentity()
	id.TokenRepositories = []string{"talkable"}
	id.TokenPermissions = map[string]string{"contents": "write", "pull_requests": "write"}
	id.UserID = 4242
	return id
}

// Every token magnum mints for a narrowed App reaches the named
// repositories with the named permissions only: the mint sends them as
// GitHub's access_tokens body, so the daemon's refresh and the check, which
// share the mint, never write a wider token to the App's gh config.
func TestATokenIsMintedForNamedRepositoriesAndTwoPermissions(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id = fixerIdentity()
	app, _ := newTestApp(t, f, clock)
	if _, err := app.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, ctype := f.lastMint()
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}
	var got struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("mint body %q: %v", body, err)
	}
	if !slices.Equal(got.Repositories, []string{"talkable"}) ||
		!maps.Equal(got.Permissions, map[string]string{"contents": "write", "pull_requests": "write"}) {
		t.Errorf("mint body = %s, want the repository and the two permissions", body)
	}
}

// An App without token_repositories and token_permissions mints as it
// always did: no body, so the token has the installation's whole access.
func TestAnUnnarrowedIdentityMintsAsBefore(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	if _, err := app.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body, ctype := f.lastMint(); len(body) != 0 || ctype != "" {
		t.Errorf("mint body %q (Content-Type %q), want none", body, ctype)
	}
}

// probeLists is the HTTPS probe's answer when git lists HEAD.
var probeLists = &execx.Result{Stdout: []byte("0123456789abcdef0123456789abcdef01234567\tHEAD\n")}

// fixerCheck runs the check of the fake server's App as a fixer of repos,
// with the gh probe and the HTTPS probe answered (probe answers the
// latter: a non-zero Code fails it; nil = probeLists).
func fixerCheck(t *testing.T, f *fakeGitHub, probe *execx.Result, repos ...string) (Report, []execx.Cmd, string) {
	t.Helper()
	clock := f.clock
	run := &execx.Fake{}
	app, layout := newTestApp(t, f, clock, WithRunner(run), WithFixer(repos...))
	dir := layout.GhConfigDir(f.id.Name)
	if probe == nil {
		probe = probeLists
	}
	run.Rules = []execx.Rule{
		ghRepoRule(t, repos[0], dir),
		{Prefix: []string{"git"}, Result: *probe},
	}
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, r)
	return r, run.CallsWithPrefix("git"), dir
}

// The fixer pushes with the App's token: its installation needs contents
// write as well as pull_requests write, where a reviewing App needs read.
// Workflows write is not needed: magnum push refuses a workflow change.
func TestAppCheckOfAFixIdentityRequiresContentsWrite(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id = fixerIdentity()
	f.users = map[string]int64{"talkable[bot]": 4242}
	r, _, _ := fixerCheck(t, f, nil, "talkable/talkable")
	if r.Pass {
		t.Fatalf("want fail with contents read:\n%s", r)
	}
	assertLines(t, r,
		"FAIL permission contents: read (need write: the fixer pushes as the App)",
		"INFO permission workflows: none (a push that changes .github/workflows needs you: magnum push refuses it)",
	)

	f.instPerms["contents"] = "write"
	r, _, _ = fixerCheck(t, f, nil, "talkable/talkable")
	if !r.Pass {
		t.Fatalf("want pass with contents write:\n%s", r)
	}
	assertLines(t, r, "PASS permission contents: write (the fixer pushes as the App)")
}

// The fixer's commits carry <user_id>+<login>@users.noreply.github.com, so
// GitHub links them to the App's bot only when user_id is that bot's id:
// the check asks GitHub and prints the line to set on a mismatch.
func TestAppCheckOfAFixIdentityChecksTheBotsUserID(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id = fixerIdentity()
	f.instPerms["contents"] = "write"
	f.users = map[string]int64{"talkable[bot]": 999}
	r, _, _ := fixerCheck(t, f, nil, "talkable/talkable")
	if r.Pass {
		t.Fatalf("want fail on a wrong user_id:\n%s", r)
	}
	assertLines(t, r,
		"FAIL user_id = 4242, but GitHub says talkable[bot] is user 999",
		"     fix: set user_id = 999 for identity talkable-app in config.toml",
	)

	f.users["talkable[bot]"] = 4242
	r, _, _ = fixerCheck(t, f, nil, "talkable/talkable")
	if !r.Pass {
		t.Fatalf("want pass:\n%s", r)
	}
	assertLines(t, r, "PASS talkable[bot] is user 4242 (user_id): its commits' email is 4242+talkable[bot]@users.noreply.github.com")
}

// The check proves the App can reach the fixer's first repository over
// HTTPS with gh as git's only credential helper and the App's gh config,
// the way magnum push reaches GitHub: git's first line is the failure.
func TestAppCheckOfAFixIdentityProbesHTTPS(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id = fixerIdentity()
	f.instPerms["contents"] = "write"
	f.users = map[string]int64{"talkable[bot]": 4242}
	r, calls, dir := fixerCheck(t, f, nil, "talkable/talkable")
	if !r.Pass {
		t.Fatalf("want pass:\n%s", r)
	}
	if len(calls) != 1 {
		t.Fatalf("git calls = %v, want the one probe", calls)
	}
	c := calls[0]
	want := []string{"-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "ls-remote", "https://github.com/talkable/talkable.git", "HEAD"}
	if !slices.Equal(c.Args, want) {
		t.Errorf("probe args = %q, want %q", c.Args, want)
	}
	if c.Env["GH_CONFIG_DIR"] != dir || c.Env["GH_TOKEN"] != "" || c.Env["GITHUB_TOKEN"] != "" || c.Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("probe env = %v, want the App's gh config, no token and no prompt", c.Env)
	}
	assertLines(t, r, "PASS git ls-remote https://github.com/talkable/talkable.git over HTTPS as the App")

	r, _, _ = fixerCheck(t, f, &execx.Result{Code: 128, Stderr: []byte("remote: Invalid username or token ghs_abcdefghijklmnop.\nfatal: Authentication failed\n")},
		"talkable/talkable")
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	var line string
	for _, l := range r.Lines {
		if strings.HasPrefix(l, "FAIL git ls-remote") {
			line = l
		}
	}
	if !strings.Contains(line, "remote: Invalid username or token") || strings.Contains(line, "Authentication failed") {
		t.Errorf("FAIL line = %q, want git's first line alone, redacted", line)
	}
}

// A fixer App's token reaches only its installation's account: a watch of
// another owner whose fix_identity names it would mint tokens that cannot
// push to its repositories.
func TestAppCheckOfAFixIdentityRefusesAnInstallationOfAnotherOwner(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id = fixerIdentity()
	f.instPerms["contents"] = "write"
	f.users = map[string]int64{"talkable[bot]": 4242}
	f.account = "example"
	f.repos = []string{"talkable/talkable"}
	r, _, _ := fixerCheck(t, f, nil, "talkable/talkable")
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r,
		"FAIL installation 105365229 is on example, but the fixer pushes to talkable/talkable as it",
		"     fix: give the watch of talkable a fix_identity of the App's installation on talkable (one [[identity]] block per installation)",
	)
}

// FixRepos lists the repositories a fixer App pushes to, "owner/name": for
// each watch whose fixer acts as it (its fix_identity, or the fix role's
// identity), the App's token_repositories under the watch's owner.
func TestFixReposListsTheFixersRepositoriesPerWatch(t *testing.T) {
	cfg := config.Defaults()
	app := func(name string, repos ...string) config.Identity {
		return config.Identity{Name: name, Kind: "app", Login: "fixer[bot]", AppID: 1, InstallationID: 2, ClientID: "Iv1",
			PrivateKeyEnv: "K", TokenRepositories: repos, TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 3}
	}
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}, app("fix-talkable", "talkable", "other"), app("fix-example", "repo")}
	cfg.Watches = []config.Watch{
		{Owner: "talkable", Include: []string{"*"}, Identity: "me", PollIdentity: "me", FixIdentity: "fix-talkable"},
		{Owner: "example", Include: []string{"*"}, Identity: "me", PollIdentity: "me", FixIdentity: "fix-example"},
		{Owner: "zhuravel", Include: []string{"*"}, Identity: "me", PollIdentity: "me"},
	}
	cfg.Normalize()
	if got := FixRepos(cfg, "fix-talkable"); !slices.Equal(got, []string{"talkable/talkable", "talkable/other"}) {
		t.Errorf("FixRepos(fix-talkable) = %v", got)
	}
	if got := FixRepos(cfg, "fix-example"); !slices.Equal(got, []string{"example/repo"}) {
		t.Errorf("FixRepos(fix-example) = %v", got)
	}
	if got := FixRepos(cfg, "me"); len(got) != 0 {
		t.Errorf("FixRepos(me) = %v, want none: a gh identity mints no token", got)
	}
}
