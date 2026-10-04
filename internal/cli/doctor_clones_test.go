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
