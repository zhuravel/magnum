package github

import (
	"context"
	"errors"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A client WithEnv derives runs gh as its parent does with the token of
// one batch over the parent's env (GH_TOKEN beside the App's
// GH_CONFIG_DIR), counts its calls on the parent's Meter, never reauths
// (a 401 of that token is no reason to rewrite the App's hosts.yml) and
// leaves the parent's env as it was.
func TestWithEnvRunsAsTheParentOverTheBatchsToken(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"}, Result: execx.Result{
		Stdout: []byte(`{"message":"Bad credentials","status":"401"}`), Stderr: []byte("gh: Bad credentials (HTTP 401)\n"), Code: 1}}}}
	reauths, metered := 0, 0
	parent := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/fixer", "GH_TOKEN": "", "GITHUB_TOKEN": ""},
		Reauth: func(context.Context) error { reauths++; return nil }, Meter: func(Call) { metered++ }}
	c := parent.WithEnv(map[string]string{"GH_TOKEN": "ghs_batch"})
	err := c.RerunRun(context.Background(), "talkable", "talkable", 42673098082)
	if !errors.Is(err, ErrUnauthorized) || reauths != 0 || len(f.Calls) != 1 {
		t.Fatalf("err = %v, reauths = %d, calls = %d; want one 401 and no reauth", err, reauths, len(f.Calls))
	}
	if env := f.Calls[0].Env; env["GH_TOKEN"] != "ghs_batch" || env["GH_CONFIG_DIR"] != "/state/gh/fixer" {
		t.Errorf("env = %v, want the batch's token over the App's config dir", env)
	}
	if metered != 1 || parent.Env["GH_TOKEN"] != "" {
		t.Errorf("metered = %d, parent env = %v; want one call metered and the parent unchanged", metered, parent.Env)
	}
}
