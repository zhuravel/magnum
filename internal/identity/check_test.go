package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	appSettingsURL = "https://github.com/organizations/talkable/settings/apps/talkable/permissions"
	installURL     = "https://github.com/organizations/talkable/settings/installations/105365229"
)

// ghRepoRule answers `gh api repos/<repo> --jq .full_name` and asserts the
// identity's config dir is in the command env and populated.
func ghRepoRule(t *testing.T, repo, wantDir string) execx.Rule {
	return execx.Rule{
		Prefix: []string{"gh", "api", "repos/" + repo},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			if c.Env["GH_CONFIG_DIR"] != wantDir {
				t.Errorf("GH_CONFIG_DIR = %q, want %q", c.Env["GH_CONFIG_DIR"], wantDir)
			}
			if v, ok := c.Env["GH_TOKEN"]; !ok || v != "" {
				t.Errorf("GH_TOKEN must be blanked, got %q %v", v, ok)
			}
			if _, err := os.Stat(filepath.Join(wantDir, "hosts.yml")); err != nil {
				t.Errorf("hosts.yml missing during gh check: %v", err)
			}
			if strings.Join(c.Args, " ") != "api repos/"+repo+" --hostname github.com --jq .full_name" {
				t.Errorf("args %v", c.Args)
			}
			return execx.Result{Stdout: []byte(repo + "\n")}, nil
		},
	}
}

func TestAppCheckPass(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	run := &execx.Fake{}
	app, layout := newTestApp(t, f, clock, WithRunner(run), WithRepos("talkable/talkable"))
	dir := layout.GhConfigDir("talkable-app")
	run.Rules = []execx.Rule{ghRepoRule(t, "talkable/talkable", dir)}

	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Pass {
		t.Fatalf("want pass:\n%s", r)
	}
	assertLines(t, r,
		"PASS private key from $MAGNUM_TEST_APP_KEY (RSA 2048 bits)",
		"PASS app talkable (id 2700610, owner talkable)",
		"PASS installation 105365229 on talkable (repository selection: selected)",
		"PASS permission pull_requests: write",
		"INFO permission contents: read",
		"PASS permission actions: read",
		"PASS permission checks: read",
		"PASS permission statuses: read",
		"PASS installation token minted, expires 2026-10-03T13:00:00Z (in 1h0m0s)",
		"PASS gh config dir "+dir,
		"PASS installation repositories (2): talkable/talkable, talkable/other",
		"PASS gh api repos/talkable/talkable with GH_CONFIG_DIR="+dir,
	)
	for _, l := range r.Lines {
		if strings.HasPrefix(l, "WARN ") {
			t.Errorf("unexpected warning %q", l)
		}
	}
	assertNoSecrets(t, r)
	if len(run.CallsWithPrefix("gh", "api")) != 1 {
		t.Fatalf("calls %v", run.Calls)
	}
}

// The judge reads a PR's CI with the posting App's token: a failed job's log
// (Actions), its check runs (Checks) and its commit statuses (Commit
// statuses). Without read access GitHub answers 403 and the review cannot say
// why CI failed, yet posting works, so a missing one warns and the check
// still passes (`magnum identities check` exits 0).
func TestAppCheckWarnsWithoutCIReadPermissions(t *testing.T) {
	const warnActions = "WARN permission actions: none; without read the judge cannot read why a CI job failed (gh run view --log-failed)"
	cases := []struct {
		name      string
		appPerm   string // what the App requests for actions ("" = absent)
		instPerm  string // what the installation granted for actions ("" = absent)
		wantLines []string
	}{
		{
			name: "app does not request it", appPerm: "", instPerm: "",
			wantLines: []string{
				warnActions,
				`     fix: open ` + appSettingsURL + ` -> Repository permissions -> Actions: "Read-only" -> Save changes`,
				"     fix: then an owner of talkable accepts the new permissions at " + installURL,
			},
		},
		{
			name: "installation has not accepted", appPerm: "read", instPerm: "",
			wantLines: []string{
				warnActions,
				"     fix: the App already requests read; an owner of talkable accepts the new permissions at " + installURL,
			},
		},
		{
			name: "write is more than read", appPerm: "write", instPerm: "write",
			wantLines: []string{"PASS permission actions: write"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			f := newFakeGitHub(t, clock)
			setPerm := func(m map[string]string, v string) {
				if v == "" {
					delete(m, "actions")
				} else {
					m["actions"] = v
				}
			}
			setPerm(f.appPerms, tc.appPerm)
			setPerm(f.instPerms, tc.instPerm)
			run := &execx.Fake{}
			app, layout := newTestApp(t, f, clock, WithRunner(run), WithRepos("talkable/talkable"))
			run.Rules = []execx.Rule{ghRepoRule(t, "talkable/talkable", layout.GhConfigDir("talkable-app"))}

			r, err := app.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !r.Pass {
				t.Fatalf("a missing CI read permission must not fail the check:\n%s", r)
			}
			assertLines(t, r, tc.wantLines...)
			assertLines(t, r, "PASS permission checks: read", "PASS permission statuses: read")
			assertNoSecrets(t, r)
		})
	}
}

func TestAppCheckMissingPermission(t *testing.T) {
	cases := []struct {
		name      string
		appPerm   string // what the App requests
		instPerm  string // what the installation granted ("" = absent)
		wantLines []string
	}{
		{
			name: "app requests read", appPerm: "read", instPerm: "read",
			wantLines: []string{
				"FAIL permission pull_requests: read (need write)",
				`     fix: open ` + appSettingsURL + ` -> Repository permissions -> Pull requests: "Read and write" -> Save changes`,
				"     fix: then an owner of talkable accepts the new permissions at " + installURL,
			},
		},
		{
			name: "permission absent", appPerm: "", instPerm: "",
			wantLines: []string{
				"FAIL permission pull_requests: none (need write)",
				`     fix: open ` + appSettingsURL + ` -> Repository permissions -> Pull requests: "Read and write" -> Save changes`,
				"     fix: then an owner of talkable accepts the new permissions at " + installURL,
			},
		},
		{
			name: "installation has not accepted", appPerm: "write", instPerm: "read",
			wantLines: []string{
				"FAIL permission pull_requests: read (need write)",
				"     fix: the App already requests write; an owner of talkable accepts the new permissions at " + installURL,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			f := newFakeGitHub(t, clock)
			setPerm := func(m map[string]string, v string) {
				if v == "" {
					delete(m, "pull_requests")
				} else {
					m["pull_requests"] = v
				}
			}
			setPerm(f.appPerms, tc.appPerm)
			setPerm(f.instPerms, tc.instPerm)
			f.instPerms["contents"] = "write"
			run := &execx.Fake{}
			app, layout := newTestApp(t, f, clock, WithRunner(run), WithRepos("talkable/talkable"))
			run.Rules = []execx.Rule{ghRepoRule(t, "talkable/talkable", layout.GhConfigDir("talkable-app"))}

			r, err := app.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if r.Pass {
				t.Fatalf("want fail:\n%s", r)
			}
			assertLines(t, r, tc.wantLines...)
			// The remaining checks still run so one pass shows every problem.
			assertLines(t, r,
				"INFO permission contents: write (read is enough)",
				"PASS installation repositories (2): talkable/talkable, talkable/other",
			)
			assertNoSecrets(t, r)
		})
	}
}

func TestAppCheckRepoNotInstalled(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos = []string{"talkable/other"}
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"gh", "api", "repos/talkable/talkable"},
		Result: execx.Result{Code: 1, Stderr: []byte("gh: Not Found (HTTP 404)")},
	}}}
	app, layout := newTestApp(t, f, clock, WithRunner(run), WithRepos("talkable/talkable"))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	dir := layout.GhConfigDir("talkable-app")
	assertLines(t, r,
		"FAIL talkable/talkable is not in installation 105365229's repository access",
		"     fix: open "+installURL+" -> Repository access -> add talkable/talkable -> Save",
		"FAIL gh api repos/talkable/talkable with GH_CONFIG_DIR="+dir+": gh api repos/talkable/talkable --hostname github.com --jq .full_name exited 1: gh: Not Found (HTTP 404)",
		"     fix: run `GH_CONFIG_DIR="+dir+" gh api repos/talkable/talkable` to see the full error",
	)
}

func TestAppCheckDefaultsToFirstInstalledRepo(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	run := &execx.Fake{}
	app, layout := newTestApp(t, f, clock, WithRunner(run))
	run.Rules = []execx.Rule{ghRepoRule(t, "talkable/talkable", layout.GhConfigDir("talkable-app"))}
	r, err := app.Check(context.Background())
	if err != nil || !r.Pass {
		t.Fatalf("%v\n%s", err, r)
	}
}

func TestAppCheckWithoutRunnerSkipsGhCall(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/talkable"))
	r, err := app.Check(context.Background())
	if err != nil || !r.Pass {
		t.Fatalf("%v\n%s", err, r)
	}
	assertLines(t, r, "INFO gh api check skipped: no command runner configured")
}

func TestAppCheckWrongKey(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	other := pkcs8PEM(t, mustOtherKey(t))
	app := NewApp(f.id, newLayout(t), f.server.Client(), func(string) string { return other }, clock.Now, WithBaseURL(f.server.URL))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r,
		"FAIL GitHub rejected the App JWT: GET /app: 401 A JSON web token could not be decoded",
		"     fix: check client_id Iv23licS9bkgs7IGVSoS for identity talkable-app in config.toml and that $MAGNUM_TEST_APP_KEY holds a current private key of that App",
	)
	assertNoSecrets(t, r)
}

// The fix for a rejected JWT names where the key comes from (here
// private_key_file, with private_key_env unset) and the id the JWT was
// issued for under its own key (app_id when there is no client_id).
func TestAppCheckWrongKeyFileNamesTheKeySource(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, []byte(pkcs8PEM(t, mustOtherKey(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	id := f.id
	id.ClientID, id.PrivateKeyEnv, id.PrivateKeyFile = "", "", keyFile
	app := NewApp(id, newLayout(t), f.server.Client(), func(string) string { return "" }, clock.Now, WithBaseURL(f.server.URL))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r,
		"FAIL GitHub rejected the App JWT: GET /app: 401 A JSON web token could not be decoded",
		"     fix: check app_id 2700610 for identity talkable-app in config.toml and that private_key_file "+keyFile+
			" holds a current private key of that App",
	)
	assertNoSecrets(t, r)
}

func TestAppCheckMissingKey(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	layout := newLayout(t)
	app := NewApp(f.id, layout, f.server.Client(), func(string) string { return "" }, clock.Now, WithBaseURL(f.server.URL))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass || len(r.Lines) != 2 {
		t.Fatalf("want 2-line fail:\n%s", r)
	}
	assertLines(t, r,
		"FAIL private key: $MAGNUM_TEST_APP_KEY is empty",
		"     fix: set MAGNUM_TEST_APP_KEY (PEM text or a file path) in magnum's environment, or save the PEM as ~/.config/magnum/keys/talkable-app.pem and set private_key_file = \"~/.config/magnum/keys/talkable-app.pem\"",
	)
}

func TestAppCheckTransportError(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	url := f.server.URL
	f.server.Close()
	app, _ := newTestApp(t, f, clock, WithBaseURL(url))
	r, err := app.Check(context.Background())
	if err == nil {
		t.Fatalf("want transport error, got report:\n%s", r)
	}
	if r.Pass {
		t.Fatal("transport failure must not pass")
	}
}

func TestAppCheckMintRejected(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.mintStatus = 403
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/talkable"))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r,
		"PASS permission pull_requests: write",
		"FAIL identity talkable-app: mint installation token: POST /app/installations/105365229/access_tokens: 403 boom",
		"     fix: check that installation 105365229 belongs to app talkable and is not suspended",
	)
}

func TestAppCheckWrongLogin(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id.Login = "talkable" // a bot login must be "<slug>[bot]"
	run := &execx.Fake{}
	app, layout := newTestApp(t, f, clock, WithRunner(run), WithRepos("talkable/talkable"))
	run.Rules = []execx.Rule{ghRepoRule(t, "talkable/talkable", layout.GhConfigDir("talkable-app"))}

	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r,
		`FAIL identity talkable-app has login "talkable" but the App posts as "talkable[bot]"`,
		`     fix: set login = "talkable[bot]" for identity talkable-app in config.toml`,
	)
}

func TestAppCheckFailedMintKeepsDaemonToken(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test1" {
		t.Fatalf("%q %v", tok, err)
	}
	expiry := app.Expiry()
	f.failMints(503)

	r, err := app.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pass {
		t.Fatalf("want fail:\n%s", r)
	}
	assertLines(t, r, "FAIL identity talkable-app: mint installation token: POST /app/installations/105365229/access_tokens: 503 boom")
	// A transient failure during a re-check must not discard the valid token
	// the daemon is using.
	if !app.Expiry().Equal(expiry) {
		t.Fatalf("expiry changed from %v to %v", expiry, app.Expiry())
	}
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test1" {
		t.Fatalf("Token after a failed check: %q %v", tok, err)
	}
}

func TestAppCheckMintsEvenWithCachedToken(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()
	if _, err := app.Token(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := app.Check(ctx)
	if err != nil || !r.Pass {
		t.Fatalf("%v\n%s", err, r)
	}
	if f.mintCount() != 2 {
		t.Fatalf("mints = %d: a check has to prove the JWT -> token exchange works", f.mintCount())
	}
	if tok, _ := app.Token(ctx); tok != "ghs_test2" {
		t.Fatalf("the newest token is kept, got %q", tok)
	}
}

func TestAppCheckRetriesRevokedInstallationToken(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos401 = 1 // the first listing answers 401: the token was revoked
	app, _ := newTestApp(t, f, clock)
	r, err := app.Check(context.Background())
	if err != nil || !r.Pass {
		t.Fatalf("%v\n%s", err, r)
	}
	assertLines(t, r, "PASS installation repositories (2): talkable/talkable, talkable/other")
	if f.mintCount() != 2 || f.repoPageCount() != 2 {
		t.Fatalf("mints = %d, listing requests = %d, want a re-mint and one retry", f.mintCount(), f.repoPageCount())
	}

	// A second 401 is final: one retry only.
	f2 := newFakeGitHub(t, clock)
	f2.repos401 = 5
	app2, _ := newTestApp(t, f2, clock)
	r, err = app2.Check(context.Background())
	if err != nil || r.Pass {
		t.Fatalf("want a FAIL line, got %v\n%s", err, r)
	}
	if f2.mintCount() != 2 || f2.repoPageCount() != 2 {
		t.Fatalf("mints = %d, listing requests = %d, want exactly one retry", f2.mintCount(), f2.repoPageCount())
	}
	assertLines(t, r, "FAIL list installation repositories: GET /installation/repositories?per_page=100&page=1: 401 Bad credentials")
}

func TestAppCheckKeyFileMode(t *testing.T) {
	keyPEM := pkcs1PEM(testKey(t))
	cases := []struct {
		name     string
		mode     os.FileMode
		wantWarn bool
	}{
		{"world readable", 0o644, true},
		{"group readable", 0o640, true},
		{"private", 0o600, false},
		{"read only owner", 0o400, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			f := newFakeGitHub(t, clock)
			keyFile := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(keyFile, []byte(keyPEM), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(keyFile, tc.mode); err != nil {
				t.Fatal(err)
			}
			getenv := func(k string) string {
				if k == "MAGNUM_TEST_APP_KEY" {
					return keyFile
				}
				return ""
			}
			app := NewApp(f.id, newLayout(t), f.server.Client(), getenv, clock.Now, WithBaseURL(f.server.URL))
			r, err := app.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !r.Pass {
				t.Fatalf("a loose key mode is a warning, not a failure:\n%s", r)
			}
			warn := fmt.Sprintf("WARN private key file of $MAGNUM_TEST_APP_KEY is readable by other users (mode %04o)", tc.mode)
			if tc.wantWarn {
				assertLines(t, r, warn, "     fix: chmod 600 "+keyFile)
			} else if strings.Contains(strings.Join(r.Lines, "\n"), "WARN") {
				t.Fatalf("unexpected warning:\n%s", r)
			}
			assertNoSecrets(t, r)
		})
	}
}

func TestAppCheckPEMTextHasNoModeWarning(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	r, err := app.Check(context.Background())
	if err != nil || strings.Contains(strings.Join(r.Lines, "\n"), "WARN") {
		t.Fatalf("%v\n%s", err, r)
	}
}

// manyRepos is n repositories talkable/r000, talkable/r001, ...
func manyRepos(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("talkable/r%03d", i)
	}
	return out
}

func TestAppCheckStopsListingOnceReposAreFound(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos = manyRepos(950)
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/r003", "talkable/r250"))
	r, err := app.Check(context.Background())
	if err != nil || !r.Pass {
		t.Fatalf("%v\n%s", err, r)
	}
	if got := f.repoPageCount(); got != 3 {
		t.Fatalf("listing requests = %d, want 3 (page 3 holds the last wanted repo)", got)
	}
	if !strings.Contains(strings.Join(r.Lines, "\n"), "installation repositories (950): talkable/r000, ") ||
		!strings.Contains(strings.Join(r.Lines, "\n"), "... (+930 more)") {
		t.Fatalf("summary should count the whole installation:\n%s", r)
	}
}

func TestAppCheckMissingRepoOnLaterPageFailsOnlyWhenListIsComplete(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos = manyRepos(250)
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/missing"))
	r, err := app.Check(context.Background())
	if err != nil || r.Pass {
		t.Fatalf("want fail: %v\n%s", err, r)
	}
	assertLines(t, r, "FAIL talkable/missing is not in installation 105365229's repository access")
	if got := f.repoPageCount(); got != 3 {
		t.Fatalf("a complete listing reads every page, got %d requests", got)
	}
}

func TestAppCheckCappedListingIsNotAuthoritative(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos = manyRepos(5100)
	// r5050 sits on page 51, past the 5000 repositories Check reads.
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/r5050"))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.Lines, "\n")
	if !r.Pass || strings.Contains(all, "FAIL") {
		t.Fatalf("a capped listing must not report the repository as inaccessible:\n%s", r)
	}
	assertLines(t, r,
		"WARN cannot confirm that talkable/r5050 is in installation 105365229's repository access: it has 5100 repositories and only the first 5000 were listed",
		"     fix: check "+installURL+" -> Repository access by hand",
	)
	if got := f.repoPageCount(); got != maxRepoPages {
		t.Fatalf("listing requests = %d, want the cap of %d", got, maxRepoPages)
	}
}

func TestAppCheckSelectionAllCoversAccountRepos(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.repos = manyRepos(5100)
	f.selection = "all"
	app, _ := newTestApp(t, f, clock, WithRepos("talkable/r5050", "other/thing"))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.Lines, "\n")
	if strings.Contains(all, "talkable/r5050") && (strings.Contains(all, "WARN cannot confirm that talkable/r5050") || strings.Contains(all, "FAIL talkable/r5050")) {
		t.Fatalf(`repository_selection "all" covers every repository of the account:`+"\n%s", r)
	}
	// A repository of another owner is not covered by this installation.
	assertLines(t, r, "WARN cannot confirm that other/thing is in installation 105365229's repository access: it has 5100 repositories and only the first 5000 were listed")
}

func TestAppCheckPlaceholderKey(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	layout := newLayout(t)
	getenv := func(string) string { return "you-must-configure-this-in-your-.mise.local.toml" }
	app := NewApp(f.id, layout, f.server.Client(), getenv, clock.Now, WithBaseURL(f.server.URL))
	r, err := app.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, r,
		"FAIL the private key in $MAGNUM_TEST_APP_KEY is the placeholder from .mise.toml; put the PEM in .mise.local.toml",
		"     fix: set MAGNUM_TEST_APP_KEY (PEM text or a file path) in magnum's environment, or save the PEM as ~/.config/magnum/keys/talkable-app.pem and set private_key_file = \"~/.config/magnum/keys/talkable-app.pem\"",
	)
}

func TestAppTokenMintingNamesThePlaceholder(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	getenv := func(string) string { return "you-must-configure-this-in-your-.mise.local.toml" }
	app := NewApp(f.id, newLayout(t), f.server.Client(), getenv, clock.Now, WithBaseURL(f.server.URL))
	_, err := app.Token(context.Background())
	if !errors.Is(err, ErrPlaceholderKey) || !strings.Contains(err.Error(), "is the placeholder from .mise.toml; put the PEM in .mise.local.toml") {
		t.Fatalf("Token err = %v; want the placeholder named", err)
	}
	if f.mintCount() != 0 {
		t.Errorf("minted with a placeholder key")
	}
}

// private_key_file names the PEM directly, with no environment (what an
// installed magnum uses: launchd runs it without mise); it wins over
// private_key_env and gets the same mode warning.
func TestAppCheckPrivateKeyFile(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, []byte(pkcs1PEM(testKey(t))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	id := f.id
	id.PrivateKeyFile = keyFile // private_key_env stays set, unset in the environment
	app := NewApp(id, newLayout(t), f.server.Client(), func(string) string { return "" }, clock.Now, WithBaseURL(f.server.URL))
	r, err := app.Check(context.Background())
	if err != nil || !r.Pass {
		t.Fatalf("check: %v\n%s", err, r)
	}
	assertLines(t, r, "PASS private key from private_key_file "+keyFile+" (RSA 2048 bits)",
		"WARN private key file of private_key_file "+keyFile+" is readable by other users (mode 0644)", "     fix: chmod 600 "+keyFile)
	assertNoSecrets(t, r)

	id.PrivateKeyFile = filepath.Join(t.TempDir(), "missing.pem")
	app = NewApp(id, newLayout(t), f.server.Client(), func(string) string { return "" }, clock.Now, WithBaseURL(f.server.URL))
	if r, _ = app.Check(context.Background()); r.Pass || !strings.Contains(strings.Join(r.Lines, "\n"), "read private_key_file "+id.PrivateKeyFile) {
		t.Fatalf("a missing key file:\n%s", r)
	}
}
