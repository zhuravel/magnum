package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// TestDoctorClonesSuggestsHTTPSAndSSHHintOnlyForSSHOrigins: a missing main
// clone is cloned over HTTPS (or gh repo clone); the reachability probe runs
// the way the daemon fetches, so a github.com SSH origin is probed over HTTPS
// through gh (a locked ssh-agent must not fail doctor), and only a non-GitHub
// SSH origin gets the ssh-agent hint.
func TestDoctorClonesSuggestsHTTPSAndSSHHintOnlyForSSHOrigins(t *testing.T) {
	home := t.TempDir()
	clone := filepath.Join(home, "talkable")
	pool := config.Pool{Repo: "talkable/talkable", MainClone: clone, Base: "master"}
	d := doctorDeps{Config: &config.Config{Pools: []config.Pool{pool}}, Run: &execx.Fake{}}

	got := doctorByName(doctorClones(context.Background(), d))["clone talkable/talkable"]
	if got.Status != doctorFail || !strings.Contains(got.Fix, "git clone https://github.com/talkable/talkable.git") ||
		!strings.Contains(got.Fix, "gh repo clone talkable/talkable") || strings.Contains(got.Fix, "git@github.com") {
		t.Fatalf("missing clone: %+v", got)
	}

	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	const viaGH = "https://github.com/talkable/talkable.git"
	for _, tc := range []struct {
		origin string
		target string // the remote ls-remote must name
		ssh    bool
	}{
		{"git@github.com:talkable/talkable.git", viaGH, false},
		{"ssh://git@github.com/talkable/talkable.git", viaGH, false},
		{"git@git.example.com:talkable/talkable.git", "origin", true},
		{"https://github.com/talkable/talkable.git", "origin", false},
		{"", "origin", false}, // origin unknown: no SSH hint
	} {
		rules := []execx.Rule{
			daemonRuleExit([]string{"git", "-C", clone}, 128, "fatal: could not read from remote repository\n"),
		}
		if tc.origin != "" {
			rules = append([]execx.Rule{daemonRuleOK([]string{"git", "-C", clone, "config", "--get", "remote.origin.url"}, tc.origin+"\n")}, rules...)
		}
		f := &execx.Fake{Rules: rules}
		d.Run = f
		c := doctorByName(doctorClones(context.Background(), d))["origin talkable/talkable"]
		if c.Status != doctorFail {
			t.Fatalf("%q: %+v", tc.origin, c)
		}
		if hasSSH := strings.Contains(c.Fix, "ssh-add -l"); hasSSH != tc.ssh {
			t.Errorf("origin %q: fix %q, want SSH hint %v", tc.origin, c.Fix, tc.ssh)
		}
		var probe []string
		for _, call := range f.Calls {
			if slices.Contains(call.Args, "ls-remote") {
				probe = call.Args
			}
		}
		i := slices.Index(probe, "ls-remote")
		if i < 0 || len(probe) < i+3 || probe[i+2] != tc.target {
			t.Errorf("origin %q: probe %q, want remote %q", tc.origin, probe, tc.target)
			continue
		}
		if usesGH := slices.Contains(probe, "credential.helper="+gitx.GHCredentialHelper); usesGH != (tc.target == viaGH) {
			t.Errorf("origin %q: probe %q, gh credential helper %v", tc.origin, probe, usesGH)
		}
	}
}

// Git ignores pushInsteadOf for a remote with an explicit pushurl, so a
// pool's main clone whose origin has one turns off the fixer App pane's
// refusal of a plain `git push`: the doctor warns about it (a read of the
// clone's config), only for a pool whose watch's fixer is a dedicated App.
func TestDoctorWarnsAboutAPushURLUnderAFixerApp(t *testing.T) {
	home := t.TempDir()
	clone := filepath.Join(home, "talkable")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}, {Name: "fixer-app", Kind: "app", Login: "fixer-app[bot]",
		AppID: 7, ClientID: "Iv-fix", InstallationID: 8, PrivateKeyEnv: "FIX_KEY", TokenRepositories: []string{"talkable"},
		TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 4242}}
	cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"talkable"}, Identity: "me", PollIdentity: "me"}}
	cfg.Pools = []config.Pool{{Repo: "talkable/talkable", MainClone: clone, Base: "master"}}
	pushURL := []string{"git", "-C", clone, "config", "--get", "remote.origin.pushurl"}
	check := func(cfg *config.Config, rules ...execx.Rule) (doctorCheck, bool, *execx.Fake) {
		t.Helper()
		f := &execx.Fake{Rules: append(rules, daemonRuleOK([]string{"git"}, ""))}
		c, ok := doctorByName(doctorFixerPushURL(context.Background(), doctorDeps{Config: cfg, Run: f}))["push url talkable/talkable"]
		return c, ok, f
	}

	// A gh fixer pushes with git push as before: nothing is read.
	if c, ok, f := check(cfg); ok || len(f.Calls) != 0 {
		t.Fatalf("gh fixer: %+v %v, calls %v", c, ok, f.Calls)
	}
	cfg.Watches[0].FixIdentity = "fixer-app"
	c, ok, _ := check(cfg, daemonRuleOK(pushURL, "git@github.com:talkable/talkable.git\n"))
	if !ok || c.Status != doctorWarn || !strings.Contains(c.Detail, "remote.origin.pushurl") || !strings.Contains(c.Detail, "pushInsteadOf") ||
		!strings.Contains(c.Fix, "git -C "+clone+" config --unset remote.origin.pushurl") {
		t.Fatalf("pushurl under a fixer App = %+v, %v", c, ok)
	}
	if c, ok, _ := check(cfg, daemonRuleExit(pushURL, 1, "")); !ok || c.Status != doctorPass {
		t.Fatalf("no pushurl under a fixer App = %+v, %v", c, ok)
	}
}

// A fixer App's tokens reach its token_repositories alone, and `magnum fix`
// refuses a PR of any other repository of its watch: the doctor names the
// watch's repositories magnum knows (include and manual_repos names, its
// pools, the repositories the registry saw under it) that the list leaves
// out, before a loop is refused. A glob names none, another watch's
// repositories and an excluded one are not the App's, and a gh fixer's
// watch is not read.
func TestDoctorNamesTheRepositoriesAFixerAppsTokensDoNotReach(t *testing.T) {
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}, {Name: "fixer-app", Kind: "app", Login: "fixer-app[bot]",
		AppID: 7, ClientID: "Iv-fix", InstallationID: 8, PrivateKeyEnv: "FIX_KEY", TokenRepositories: []string{"talkable", "Web-App"},
		TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 4242}}
	cfg.Watches = []config.Watch{
		{Owner: "talkable", Include: []string{"talkable", "talkable-esp", "web-*", "docs"}, Exclude: []string{"web-old"}, ManualRepos: []string{"docs"},
			Identity: "me", PollIdentity: "me", FixIdentity: "fixer-app"},
		{Owner: "example", Include: []string{"*"}, Identity: "me", PollIdentity: "me"},
	}
	cfg.Pools = []config.Pool{{Repo: "talkable/talkable", MainClone: "/clones/talkable", Base: "master"}, {Repo: "talkable/web-shop", MainClone: "/clones/shop"}}
	st := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	for _, r := range []store.Repo{{Owner: "talkable", Name: "web-app"}, {Owner: "talkable", Name: "web-old"}, {Owner: "talkable", Name: "web-api"},
		{Owner: "example", Name: "talkable-esp"}} {
		r.NodeID, r.Mode = "R-"+r.Owner+"-"+r.Name, store.RepoModePerPR
		if _, err := st.UpsertRepo(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	checks := doctorByName(doctorFixerRepos(context.Background(), doctorDeps{Config: cfg, Store: st}))
	c, ok := checks["fixer repos talkable"]
	if !ok || c.Status != doctorWarn {
		t.Fatalf("checks %+v: want a warning for watch talkable", checks)
	}
	for _, want := range []string{"talkable/talkable-esp", "talkable/docs", "talkable/web-shop", "talkable/web-api", "magnum fix"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not name %s", c.Detail, want)
		}
	}
	for _, not := range []string{"talkable/talkable,", "web-app", "web-old", "web-*", "example/"} {
		if strings.Contains(c.Detail+" ", not) {
			t.Errorf("detail %q names %s", c.Detail, not)
		}
	}
	if !strings.Contains(c.Fix, "token_repositories") || !strings.Contains(c.Fix, "fixer-app") || strings.Count(c.Detail, "talkable/docs") != 1 {
		t.Errorf("fix %q, detail %q", c.Fix, c.Detail)
	}
	if _, ok := checks["fixer repos example"]; ok {
		t.Error("a gh fixer's watch was checked")
	}

	cfg.Identities[1].TokenRepositories = []string{"talkable", "talkable-esp", "docs", "web-shop", "web-api", "web-app"}
	if c := doctorByName(doctorFixerRepos(context.Background(), doctorDeps{Config: cfg, Store: st}))["fixer repos talkable"]; c.Status != doctorPass {
		t.Errorf("every repository named: %+v, want a pass", c)
	}
}

// Without a fixer App on any watch the fixer repos check reads nothing and
// says nothing: a registry that cannot be listed (busy past its timeout,
// closed) gives no warning about fixer App checks that do not exist.
func TestDoctorFixerReposReadsNothingWithoutAFixerApp(t *testing.T) {
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"talkable"}, Identity: "me", PollIdentity: "me"}}
	st := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if checks := doctorFixerRepos(context.Background(), doctorDeps{Config: cfg, Store: st}); len(checks) != 0 {
		t.Errorf("checks = %+v, want none without a fixer App", checks)
	}
}
