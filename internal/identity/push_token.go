package identity

// The daemon's own push tokens. A fixer App's pane holds its token in the
// App's gh config dir (ConfigDir), narrowed to contents and pull_requests
// write. A push that only the daemon makes, of a merge that carries its
// base's workflow files, needs Workflows write, which the pane must never
// hold: DaemonPushEnv mints a token of its own for it, into a gh config
// dir of its own that no pane names, and revokes it after the push.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// ErrWorkflowsNotGranted is DaemonPushEnv's error when GitHub will not
// mint a token with Workflows write: the App, or its installation, lacks
// the permission (the operator grants it on the App's settings and
// accepts it on the installation).
var ErrWorkflowsNotGranted = errors.New("the App's installation lacks the Workflows permission")

// daemonPushHold is how long a push token the caller did not release
// lives: the first refresh after it (EnsureConfigDir) revokes it and
// removes its dir. A push is bounded well within it.
const daemonPushHold = 15 * time.Minute

// daemonPush is one push token DaemonPushEnv minted and its dir.
type daemonPush struct {
	dir, token string
	at         time.Time
}

// DaemonPushEnv mints a token for one push the daemon makes as the App,
// narrowed as the App's tokens are (token_repositories and
// token_permissions, config.Identity), with workflows = "write" added when
// workflows is set, writes it into a gh config dir of its own beside
// ConfigDir (0700, never ConfigDir, which panes name: the pane's token
// stays as narrow as it was), and returns the env that selects that dir
// (as Env does) and the release that removes the dir and revokes the
// token (once; later calls do nothing). A token never released goes at
// the first refresh after daemonPushHold. GitHub's refusal of Workflows
// is ErrWorkflowsNotGranted. The token is never logged nor returned.
func (a *App) DaemonPushEnv(ctx context.Context, workflows bool) (map[string]string, func(), error) {
	a.sweepPushes(ctx)
	jwt, err := a.privateKeyJWT()
	if err != nil {
		return nil, nil, fmt.Errorf("identity %s: %w", a.cfg.Name, err)
	}
	perms := maps.Clone(a.cfg.TokenPermissions)
	if workflows {
		if len(perms) == 0 { // the whole installation's access would be narrowed to workflows alone
			perms = map[string]string{"contents": "write"}
		}
		perms["workflows"] = "write"
	}
	var out struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", a.cfg.InstallationID)
	if err := a.call(ctx, http.MethodPost, path, jwt, mintBody{Repositories: a.cfg.TokenRepositories, Permissions: perms}, &out); err != nil {
		if ae, ok := isAPIError(err); ok && workflows && ae.Status == http.StatusUnprocessableEntity {
			return nil, nil, fmt.Errorf("identity %s: mint a push token: %w (%s)", a.cfg.Name, ErrWorkflowsNotGranted, ae.Message)
		}
		return nil, nil, fmt.Errorf("identity %s: mint a push token: %w", a.cfg.Name, err)
	}
	if out.Token == "" {
		return nil, nil, fmt.Errorf("identity %s: mint a push token: response without token", a.cfg.Name)
	}
	dir, err := a.writePushDir(out.Token)
	if err != nil {
		a.revoke(context.WithoutCancel(ctx), out.Token)
		return nil, nil, fmt.Errorf("identity %s: %w", a.cfg.Name, err)
	}
	p := &daemonPush{dir: dir, token: out.Token, at: a.now()}
	a.pmu.Lock()
	a.pushes = append(a.pushes, p)
	a.pmu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { a.dropPush(context.WithoutCancel(ctx), p) }) }
	return appEnv(dir), release, nil
}

// writePushDir writes token as gh's hosts.yml (and its config.yml) into a
// new dir in ConfigDir's parent whose name no identity takes (a leading
// dot), and returns it.
func (a *App) writePushDir(token string) (string, error) {
	root := filepath.Dir(a.ConfigDir())
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("write a push token's gh config dir: %w", err)
	}
	dir, err := os.MkdirTemp(root, "."+a.cfg.Name+".push-")
	if err != nil {
		return "", fmt.Errorf("write a push token's gh config dir: %w", err)
	}
	cfg, hosts := filepath.Join(dir, "config.yml"), filepath.Join(dir, "hosts.yml")
	err = errors.Join(fsx.WriteFileAtomic(cfg, renderConfigYML(a.cfg.Name), 0o600), // MkdirTemp made the dir 0700
		fsx.WriteFileAtomic(hosts, renderHostsYML(a.cfg.Name, token, a.cfg.Login), 0o600))
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write a push token's gh config dir: %w", err)
	}
	return dir, nil
}

// dropPush forgets p, removes its dir and revokes its token (best effort:
// a token GitHub does not revoke expires within the hour).
func (a *App) dropPush(ctx context.Context, p *daemonPush) {
	a.pmu.Lock()
	a.pushes = slices.DeleteFunc(a.pushes, func(q *daemonPush) bool { return q == p })
	a.pmu.Unlock()
	_ = os.RemoveAll(p.dir)
	a.revoke(ctx, p.token)
}

// sweepPushes drops the push tokens older than daemonPushHold.
func (a *App) sweepPushes(ctx context.Context) {
	now := a.now()
	a.pmu.Lock()
	var old []*daemonPush
	for _, p := range a.pushes {
		if now.Sub(p.at) >= daemonPushHold {
			old = append(old, p)
		}
	}
	a.pmu.Unlock()
	for _, p := range old {
		a.dropPush(context.WithoutCancel(ctx), p)
	}
}

// revoke revokes an installation token (DELETE /installation/token as it),
// within callTimeout; a failure is not reported.
func (a *App) revoke(ctx context.Context, token string) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_ = a.call(ctx, http.MethodDelete, "/installation/token", token, nil, nil)
}
