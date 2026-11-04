package factory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/slots"
)

// The factory never runs a remote console, `kubectl exec` or gh-ost (spec
// 8.4 and 12): Denied refuses them however a command wraps them (mise exec,
// env, bundle exec, a shell's -c script), and every command the factory
// runs goes through it (Guard), so a refused one never starts and its error
// says why.
func TestTheFactoryNeverRunsARemoteConsoleOrGhost(t *testing.T) {
	repo := config.FactoryRepo{Repo: "example/shop", Consoles: []string{"bin/remote-console", "heroku run", "kubectl exec"}}
	shell := func(script string) []string {
		return append([]string{"mise"}, slots.MiseExecArgs("/copies/shop1", map[string]string{"PORT": "3001"}, script)...)
	}
	denied := []struct {
		name string
		argv []string
		why  string
	}{
		{"kubectl exec", []string{"kubectl", "exec", "-it", "web-1", "--", "bin/rails", "console"}, "kubectl exec"},
		{"kubectl exec in a script", shell("kubectl --context prod exec web-1 -- bin/rails runner 'User.count'"), "kubectl exec"},
		{"gh-ost --execute", []string{"gh-ost", "--execute", "--table=users", "--alter=ADD COLUMN x INT"}, "gh-ost"},
		{"gh-ost with any argument", []string{"/usr/local/bin/gh-ost", "--help"}, "gh-ost"},
		{"gh-ost after a lint", shell("bundle exec rubocop app && gh-ost --execute --table=users"), "gh-ost"},
		{"gh-ost under env", []string{"env", "DB=x", "gh-ost", "--table=users"}, "gh-ost"},
		{"a console", []string{"bin/remote-console", "production"}, "bin/remote-console"},
		{"a console in a script", shell("./bin/remote-console production"), "bin/remote-console"},
		{"a console under bundle exec", shell("cd app; bundle exec bin/remote-console bastion"), "bin/remote-console"},
		{"a two-word console", shell("heroku run rails c --app shop"), "heroku run"},
		{"a two-word console in a nested shell", shell(`sh -c "heroku run rails c"`), "heroku run"},
		{"gh-ost in a substitution", shell(`echo "$(gh-ost --execute --table=users)"`), "gh-ost"},
		{"gh-ost named in quotes", shell(`echo 'gh-ost and kubectl exec are words here'`), "gh-ost"},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			why, ok := Denied(tc.argv, repo)
			if !ok || !strings.Contains(why, tc.why) {
				t.Fatalf("Denied(%q) = %q, %v; want denied naming %q", tc.argv, why, ok, tc.why)
			}
		})
	}
	for _, entry := range repo.Consoles {
		if why, ok := Denied(strings.Fields(entry+" production"), repo); !ok || !strings.Contains(why, entry) {
			t.Errorf("the console %q is not denied (%q, %v)", entry, why, ok)
		}
	}

	allowed := [][]string{
		{"git", "-C", "/copies/shop1", "status", "--porcelain"},
		shell("bundle exec rubocop bin/remote-console app/models/credit.rb"), // the console's own file, linted
		shell("bin/impact-specs spec/models/credit_spec.rb"),
		shell("echo kubectl; bundle exec rspec spec/models/credit_spec.rb"),
		{"kubectl", "get", "pods"},
	}
	for _, argv := range allowed {
		if why, ok := Denied(argv, repo); ok {
			t.Errorf("Denied(%q) = %q, want allowed", argv, why)
		}
	}
	// Without a repository the consoles are none, the rest stays denied.
	if _, ok := Denied([]string{"bin/remote-console"}, config.FactoryRepo{}); ok {
		t.Error("a console of no repository was denied")
	}
	if _, ok := Denied([]string{"gh-ost", "--execute"}, config.FactoryRepo{}); !ok {
		t.Error("gh-ost without a repository was allowed")
	}

	// Every command the factory runs takes Guard: a denied one never starts,
	// and its error names the reason.
	cfg := config.Defaults()
	cfg.Factory.Repos = []config.FactoryRepo{repo}
	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{}}}}
	run := Guard(fake, cfg)
	_, err := run.Run(context.Background(), execx.Cmd{Name: "mise", Args: shell("heroku run rails c")[1:], Label: "lint"})
	if !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "heroku run") {
		t.Fatalf("Guard ran a console: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("a denied command reached the runner: %+v", fake.Calls)
	}
	if _, err := run.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"status"}}); err != nil {
		t.Fatalf("Guard refused git status: %v", err)
	}
	if Guard(run, cfg) != run {
		t.Fatal("Guard wraps a guarded runner twice")
	}
}
