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
