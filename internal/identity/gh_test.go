package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

func ghIdentity() config.Identity {
	return config.Identity{Name: "zhuravel", Kind: "gh", Login: "zhuravel", NoFindingsEvent: "APPROVE"}
}

var tokenRule = execx.Rule{
	Prefix: []string{"gh", "auth", "token", "--hostname", "github.com"},
	Result: execx.Result{Stdout: []byte("gho_secret123\n")},
}

func TestGHEnvAndAccessors(t *testing.T) {
	g := NewGH(ghIdentity(), &execx.Fake{})
	env, err := g.Env(context.Background())
	if err != nil || len(env) != 1 || env["GH_HOST"] != "github.com" {
		t.Fatalf("env %v %v", env, err)
	}
	if g.Name() != "zhuravel" || g.Login() != "zhuravel" || g.Kind() != "gh" {
		t.Fatal(g.Name(), g.Login(), g.Kind())
	}
}

func TestGHCheck(t *testing.T) {
	userRule := func(login string) execx.Rule {
		return execx.Rule{
			Prefix: []string{"gh", "api", "user", "--jq", ".login"},
			Result: execx.Result{Stdout: []byte(login + "\n")},
		}
	}
	cases := []struct {
		name  string
		rules []execx.Rule
		pass  bool
		lines []string
	}{
		{
			name: "pass", rules: []execx.Rule{tokenRule, userRule("zhuravel")}, pass: true,
			lines: []string{"PASS gh token for github.com", "PASS gh api user: zhuravel"},
		},
		{
			name: "login is case-insensitive", rules: []execx.Rule{tokenRule, userRule("Zhuravel")}, pass: true,
			lines: []string{"PASS gh api user: Zhuravel"},
		},
		{
			name: "wrong account", rules: []execx.Rule{tokenRule, userRule("someone")},
			lines: []string{
				"PASS gh token for github.com",
				"FAIL gh api user is someone, identity zhuravel expects zhuravel",
				"     fix: gh auth switch --hostname github.com --user zhuravel (or gh auth login --hostname github.com), or fix login for identity zhuravel in config.toml",
			},
		},
		{
			name: "not logged in",
			rules: []execx.Rule{{
				Prefix: []string{"gh", "auth", "token"},
				Result: execx.Result{Code: 1, Stderr: []byte("no oauth token found for github.com")},
			}},
			lines: []string{
				"FAIL gh token for github.com: gh auth token --hostname github.com exited 1: no oauth token found for github.com",
				"     fix: gh auth login --hostname github.com (as zhuravel)",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &execx.Fake{Rules: tc.rules}
			g := NewGH(ghIdentity(), run)
			r, err := g.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if r.Pass != tc.pass {
				t.Fatalf("pass=%v:\n%s", r.Pass, r)
			}
			assertLines(t, r, tc.lines...)
			assertNoSecrets(t, r)
			for _, c := range run.Calls {
				if len(c.Env) != 0 || c.Mutates || c.Timeout == 0 {
					t.Fatalf("gh identity must not set env or mutate, and needs a timeout: %+v", c)
				}
			}
		})
	}
}

func TestGHCheckAsksGhEveryTime(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{tokenRule, {
		Prefix: []string{"gh", "api", "user"}, Result: execx.Result{Stdout: []byte("zhuravel\n")},
	}}}
	g := NewGH(ghIdentity(), run)
	for range 2 {
		if r, err := g.Check(context.Background()); err != nil || !r.Pass {
			t.Fatalf("%v\n%s", err, r)
		}
	}
	if n := len(run.CallsWithPrefix("gh", "auth", "token")); n != 2 {
		t.Fatalf("a check must not trust an earlier answer, calls=%d", n)
	}
}

func TestGHCheckPinsHostname(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{tokenRule, {
		Prefix: []string{"gh", "api", "user"}, Result: execx.Result{Stdout: []byte("zhuravel\n")},
	}}}
	if _, err := NewGH(ghIdentity(), run).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.Calls {
		if !strings.Contains(strings.Join(c.Args, " "), "--hostname github.com") {
			t.Errorf("%s does not pin the host; an inherited GH_HOST would redirect it", c.String())
		}
	}
}

func TestGHCheckEmptyToken(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "auth", "token"}, Result: execx.Result{Stdout: []byte("\n")}}}}
	r, err := NewGH(ghIdentity(), run).Check(context.Background())
	if err != nil || r.Pass {
		t.Fatalf("an empty token must fail the check: %v\n%s", err, r)
	}
	assertLines(t, r, "FAIL gh token for github.com: gh auth token printed no token")
}
