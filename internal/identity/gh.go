package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// GH is the user's own gh login (kind = "gh"). magnum never reads or caches
// its token: gh itself picks it up from the keyring.
type GH struct {
	cfg config.Identity
	run execx.Runner
}

var _ Source = (*GH)(nil)

// NewGH returns the gh identity described by cfg; every gh call goes through run.
func NewGH(cfg config.Identity, run execx.Runner) *GH {
	return &GH{cfg: cfg, run: run}
}

// Name implements Source.
func (g *GH) Name() string { return g.cfg.Name }

// Login implements Source.
func (g *GH) Login() string { return g.cfg.Login }

// Kind implements Source.
func (g *GH) Kind() string { return "gh" }

// Env implements Source: gh already uses the user's keyring login; only
// GH_HOST is pinned to github.com (see pinnedHost).
func (g *GH) Env(context.Context) (map[string]string, error) {
	return map[string]string{"GH_HOST": pinnedHost}, nil
}

// Check verifies that gh holds a token for github.com and that `gh api user`
// is the configured login.
func (g *GH) Check(ctx context.Context) (Report, error) {
	var r reporter
	if err := g.hasToken(ctx); err != nil {
		if ctx.Err() != nil {
			return r.report(), fmt.Errorf("identity %s check: %w", g.cfg.Name, ctx.Err())
		}
		r.fail("gh token for github.com: "+err.Error(),
			fmt.Sprintf("gh auth login --hostname github.com (as %s)", g.cfg.Login))
		return r.report(), nil
	}
	r.pass("gh token for github.com")

	res, err := g.run.Run(ctx, execx.Cmd{
		Name:    "gh",
		Args:    []string{"api", "user", "--jq", ".login", "--hostname", "github.com"},
		Timeout: callTimeout,
		Label:   "gh api user (identity check)",
	})
	if err != nil {
		if ctx.Err() != nil {
			return r.report(), fmt.Errorf("identity %s check: %w", g.cfg.Name, ctx.Err())
		}
		r.fail("gh api user: "+err.Error(),
			fmt.Sprintf("gh auth login --hostname github.com (as %s)", g.cfg.Login))
		return r.report(), nil
	}
	login := res.Out()
	if !strings.EqualFold(login, g.cfg.Login) {
		r.fail(fmt.Sprintf("gh api user is %s, identity %s expects %s", login, g.cfg.Name, g.cfg.Login),
			fmt.Sprintf("gh auth switch --hostname github.com --user %s (or gh auth login --hostname github.com), or fix login for identity %s in config.toml", g.cfg.Login, g.cfg.Name))
		return r.report(), nil
	}
	r.pass("gh api user: " + login)
	return r.report(), nil
}

// hasToken runs `gh auth token` and fails when gh prints no token. The token
// itself is dropped at once.
func (g *GH) hasToken(ctx context.Context) error {
	res, err := g.run.Run(ctx, execx.Cmd{
		Name:    "gh",
		Args:    []string{"auth", "token", "--hostname", "github.com"},
		Timeout: callTimeout,
		Label:   "gh auth token",
	})
	if err != nil {
		return err
	}
	if res.Out() == "" {
		return errors.New("gh auth token printed no token")
	}
	return nil
}
