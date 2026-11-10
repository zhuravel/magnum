package identity

// The daemon's own push tokens. A fixer App's pane holds its token in the
// App's gh config dir (ConfigDir), narrowed to contents and pull_requests
// write. A push that only the daemon makes, of a merge that carries its
// base's workflow files, needs Workflows write, which the pane must never
// hold: DaemonPushEnv mints a token of its own for it and hands it in the
// env of that one push alone (GH_TOKEN, which gh answers git with before
// any hosts.yml), never in a file: the App's panes run as the same user and
// could read any file the daemon writes beside their own. Its release
// revokes the token, and one nobody released is revoked a few minutes on.
// The daemon's Actions calls as such an App (engine ci_actions.go: the
// start of a CI workflow, the re-run of a run that started no job, the
// reads of runs and jobs) need Actions, which the pane must never hold
// either: ActionsEnv mints their token the same way.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// ErrWorkflowsNotGranted is DaemonPushEnv's error when GitHub will not
// mint a token with Workflows write: the App, or its installation, lacks
// the permission (the operator grants it on the App's settings and
// accepts it on the installation).
var ErrWorkflowsNotGranted = errors.New("the App's installation lacks the Workflows permission")

// ErrActionsNotGranted is ActionsEnv's error when GitHub will not mint a
// token with Actions write: the App, or its installation, lacks the
// permission (the operator grants Actions: write on the App's settings and
// accepts it on the installation).
var ErrActionsNotGranted = errors.New("the App's installation lacks the Actions permission")

// daemonPushHold is how long a token of the daemon's own (a push's, a
// batch of Actions calls') the caller did not release lives: the first
// refresh after it (EnsureConfigDir) revokes it. One push, one batch, is
// bounded well within it.
const daemonPushHold = 5 * time.Minute

// daemonPush is one token of the daemon's own that DaemonPushEnv or
// ActionsEnv minted.
type daemonPush struct {
	token string
	at    time.Time
}

// DaemonPushEnv mints a token for one push the daemon makes as the App,
// narrowed as the App's tokens are (token_repositories and
// token_permissions, config.Identity), with workflows = "write" added when
// workflows is set, and returns the env of that push: Env's, with the
// token as GH_TOKEN, which gh's credential helper answers git with before
// ConfigDir's hosts.yml (the pane's token, which stays as narrow as it
// was), and no file written; and the release that revokes the token (once;
// later calls do nothing). A token never released is revoked at the first
// refresh after daemonPushHold. GitHub's refusal of Workflows is
// ErrWorkflowsNotGranted. The token is never logged; the env is for that
// one git command alone.
func (a *App) DaemonPushEnv(ctx context.Context, workflows bool) (map[string]string, func(), error) {
	perms := maps.Clone(a.cfg.TokenPermissions)
	var refused error
	if workflows {
		if len(perms) == 0 { // the whole installation's access would be narrowed to workflows alone
			perms = map[string]string{"contents": "write"}
		}
		perms["workflows"] = "write"
		refused = ErrWorkflowsNotGranted
	}
	return a.daemonEnv(ctx, "a push token", perms, refused)
}

// ActionsEnv mints a token for one batch of the daemon's Actions calls as
// the App (engine ci_actions.go), narrowed to its token_repositories with
// its token_permissions and actions = "write" added (write includes read),
// and returns the env of those gh calls and the token's release as
// DaemonPushEnv does: the token as GH_TOKEN over ConfigDir, whose hosts.yml
// keeps the pane's token without Actions, no file written, revoked once
// released or past daemonPushHold. GitHub's refusal of Actions is
// ErrActionsNotGranted.
func (a *App) ActionsEnv(ctx context.Context) (map[string]string, func(), error) {
	perms := maps.Clone(a.cfg.TokenPermissions)
	if perms == nil {
		perms = map[string]string{}
	}
	perms["actions"] = "write"
	return a.daemonEnv(ctx, "an Actions token", perms, ErrActionsNotGranted)
}

// daemonEnv mints a token of the daemon's own with perms (what names it in
// errors), keeps it for sweepPushes, and returns the env that carries it
// and its release; GitHub's 422 is refused (nil: no such error).
func (a *App) daemonEnv(ctx context.Context, what string, perms map[string]string, refused error) (map[string]string, func(), error) {
	a.sweepPushes(ctx)
	jwt, err := a.privateKeyJWT()
	if err != nil {
		return nil, nil, fmt.Errorf("identity %s: %w", a.cfg.Name, err)
	}
	var out struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", a.cfg.InstallationID)
	if err := a.call(ctx, http.MethodPost, path, jwt, mintBody{Repositories: a.cfg.TokenRepositories, Permissions: perms}, &out); err != nil {
		if ae, ok := isAPIError(err); ok && refused != nil && ae.Status == http.StatusUnprocessableEntity {
			return nil, nil, fmt.Errorf("identity %s: mint %s: %w (%s)", a.cfg.Name, what, refused, ae.Message)
		}
		return nil, nil, fmt.Errorf("identity %s: mint %s: %w", a.cfg.Name, what, err)
	}
	if out.Token == "" {
		return nil, nil, fmt.Errorf("identity %s: mint %s: response without token", a.cfg.Name, what)
	}
	p := &daemonPush{token: out.Token, at: a.now()}
	a.pmu.Lock()
	a.pushes = append(a.pushes, p)
	a.pmu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { a.dropPush(context.WithoutCancel(ctx), p) }) }
	env := appEnv(a.ConfigDir())
	env["GH_TOKEN"] = out.Token
	return env, release, nil
}

// dropPush forgets p and revokes its token (best effort: a token GitHub
// does not revoke expires within the hour).
func (a *App) dropPush(ctx context.Context, p *daemonPush) {
	a.pmu.Lock()
	a.pushes = slices.DeleteFunc(a.pushes, func(q *daemonPush) bool { return q == p })
	a.pmu.Unlock()
	a.revoke(ctx, p.token)
}

// sweepPushes drops the daemon's own tokens older than daemonPushHold.
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
