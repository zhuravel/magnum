package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

type ghAccount struct {
	Login string `json:"login"`
	Type  string `json:"type"` // Organization | User
}

type appInfo struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Owner       ghAccount         `json:"owner"`
	Permissions map[string]string `json:"permissions"`
}

type installationInfo struct {
	ID                  int64             `json:"id"`
	Account             ghAccount         `json:"account"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
}

// Check verifies, in order: the private key, the JWT (GET /app), the
// installation and its permissions (pull_requests must be write; contents is
// reported), minting a token, writing ConfigDir, the installation's
// repositories (every WithRepos repo must be listed), and that
// `gh api repos/<first repo>` works with GH_CONFIG_DIR. It keeps going after a
// failure where the next step can still run, so one pass shows every problem.
//
// Check proves minting works by minting a token itself; a failed mint leaves
// the daemon's cached token alone, a successful one replaces it only when it
// outlives it.
func (a *App) Check(ctx context.Context) (Report, error) {
	c := &appCheck{a: a}
	for _, step := range []func(context.Context) (stop bool, err error){
		c.privateKey, c.appInfo, c.installation, c.permissions, c.token, c.configDir, c.repositories, c.ghProbe,
	} {
		stop, err := step(ctx)
		if err != nil {
			return c.r.report(), err
		}
		if stop {
			break
		}
	}
	return c.r.report(), nil
}

// appCheck carries the state the stages of App.Check share. Each stage adds
// its lines to r and returns stop = true when later stages cannot run, or an
// error when the check itself could not complete (network failure, cancelled
// context).
type appCheck struct {
	a     *App
	r     reporter
	jwt   string
	app   appInfo
	inst  installationInfo
	dir   string
	repos repoList
}

// transport turns a non-GitHub failure into a FAIL line plus the error.
func (c *appCheck) transport(what string, err error) (bool, error) {
	c.r.fail(what + ": " + err.Error())
	return true, fmt.Errorf("identity %s check: %s: %w", c.a.cfg.Name, what, err)
}

func (c *appCheck) privateKey(context.Context) (bool, error) {
	a := c.a
	value := a.cfg.PrivateKeyFile
	if value == "" {
		value = a.getenv(a.cfg.PrivateKeyEnv)
	}
	key, err := a.privateKey()
	if err != nil {
		fix := fmt.Sprintf("save the App's PEM as %s (chmod 600)", inConfigDir(a.layout, "keys/"+a.cfg.Name+".pem"))
		if a.cfg.PrivateKeyEnv != "" && a.cfg.PrivateKeyFile == "" {
			fix = fmt.Sprintf("set %s (PEM text or a file path) in magnum's environment, or save the PEM as %s and set private_key_file = %q",
				a.cfg.PrivateKeyEnv, inConfigDir(a.layout, "keys/"+a.cfg.Name+".pem"), inConfigDir(a.layout, "keys/"+a.cfg.Name+".pem"))
		}
		if errors.Is(err, ErrPlaceholderKey) {
			c.r.fail(err.Error(), fix) // already says "the private key in $…"
		} else {
			c.r.fail("private key: "+err.Error(), fix)
		}
		return true, nil
	}
	c.r.pass(fmt.Sprintf("private key from %s (RSA %d bits)", a.keySource(), key.N.BitLen()))
	if path, mode := looseKeyFile(value); path != "" {
		c.r.warn(fmt.Sprintf("private key file of %s is readable by other users (mode %04o)", a.keySource(), mode),
			"chmod 600 "+execx.ShellQuote(path))
	}
	c.jwt, err = signJWT(key, a.issuer(), a.now())
	if err != nil {
		c.r.fail(err.Error())
		return true, nil
	}
	return false, nil
}

func (c *appCheck) appInfo(ctx context.Context) (bool, error) {
	a, r := c.a, &c.r
	if err := a.call(ctx, http.MethodGet, "/app", c.jwt, &c.app); err != nil {
		ae, ok := isAPIError(err)
		if !ok {
			return c.transport("GET /app", err)
		}
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden {
			r.fail("GitHub rejected the App JWT: "+ae.Error(),
				fmt.Sprintf("check client_id %s for identity %s in config.toml and that $%s holds a current private key of that App", a.issuer(), a.cfg.Name, a.cfg.PrivateKeyEnv))
		} else {
			r.fail(ae.Error())
		}
		return true, nil
	}
	app := c.app
	if a.cfg.AppID != 0 && app.ID != a.cfg.AppID {
		r.fail(fmt.Sprintf("the JWT belongs to app %s (id %d), config app_id is %d", app.Slug, app.ID, a.cfg.AppID),
			fmt.Sprintf("make app_id and client_id of identity %s in config.toml describe the same App", a.cfg.Name))
	} else {
		r.pass(fmt.Sprintf("app %s (id %d, owner %s)", app.Slug, app.ID, app.Owner.Login))
	}
	// Reviews by an App are authored by "<slug>[bot]"; a wrong login makes
	// every posted review look like an identity leak.
	if want := app.Slug + "[bot]"; app.Slug != "" && a.cfg.Login != want {
		r.fail(fmt.Sprintf("identity %s has login %q but the App posts as %q", a.cfg.Name, a.cfg.Login, want),
			fmt.Sprintf("set login = %q for identity %s in config.toml", want, a.cfg.Name))
	}
	return false, nil
}

func (c *appCheck) installation(ctx context.Context) (bool, error) {
	a, r := c.a, &c.r
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", a.cfg.InstallationID), c.jwt, &c.inst); err != nil {
		ae, ok := isAPIError(err)
		if !ok {
			return c.transport("GET installation", err)
		}
		if ae.Status == http.StatusNotFound {
			r.fail(fmt.Sprintf("installation %d not found for app %s", a.cfg.InstallationID, c.app.Slug),
				fmt.Sprintf("install the App at https://github.com/apps/%s/installations/new or fix installation_id of identity %s in config.toml", c.app.Slug, a.cfg.Name))
		} else {
			r.fail(ae.Error())
		}
		return true, nil
	}
	inst := c.inst
	if inst.SuspendedAt != nil {
		r.fail(fmt.Sprintf("installation %d is suspended", a.cfg.InstallationID), "unsuspend it at "+installationSettingsURL(inst.Account, a.cfg.InstallationID))
	} else {
		r.pass(fmt.Sprintf("installation %d on %s (repository selection: %s)", a.cfg.InstallationID, inst.Account.Login, inst.RepositorySelection))
	}
	return false, nil
}

func (c *appCheck) permissions(context.Context) (bool, error) {
	a, r, inst := c.a, &c.r, c.inst
	installURL := installationSettingsURL(inst.Account, a.cfg.InstallationID)
	if have := permission(inst.Permissions, "pull_requests"); have == "write" {
		r.pass("permission pull_requests: write")
	} else if permission(c.app.Permissions, "pull_requests") == "write" {
		r.fail(fmt.Sprintf("permission pull_requests: %s (need write)", have),
			fmt.Sprintf("the App already requests write; an owner of %s accepts the new permissions at %s", inst.Account.Login, installURL))
	} else {
		r.fail(fmt.Sprintf("permission pull_requests: %s (need write)", have),
			fmt.Sprintf(`open %s -> Repository permissions -> Pull requests: "Read and write" -> Save changes`, appPermissionsURL(c.app)),
			fmt.Sprintf("then an owner of %s accepts the new permissions at %s", inst.Account.Login, installURL))
	}
	switch contents := permission(inst.Permissions, "contents"); contents {
	case "write":
		r.info("permission contents: write (read is enough)")
	default:
		r.info("permission contents: " + contents)
	}
	return false, nil
}

// token mints an installation token (not through the daemon's cache, which a
// transient failure here must not disturb) and keeps it when it outlives the
// cached one.
func (c *appCheck) token(ctx context.Context) (bool, error) {
	a := c.a
	tok, exp, err := a.mint(ctx)
	if err != nil {
		if _, ok := isAPIError(err); !ok || ctx.Err() != nil {
			return c.transport("mint installation token", err)
		}
		c.r.fail(err.Error(), fmt.Sprintf("check that installation %d belongs to app %s and is not suspended", a.cfg.InstallationID, c.app.Slug))
		return true, nil
	}
	a.install(tok, exp)
	c.r.pass(fmt.Sprintf("installation token minted, expires %s (in %s)", exp.UTC().Format(time.RFC3339), exp.Sub(a.now()).Round(time.Second)))
	return false, nil
}

func (c *appCheck) configDir(ctx context.Context) (bool, error) {
	dir, err := c.a.EnsureConfigDir(ctx)
	if err != nil {
		c.r.fail("gh config dir: "+err.Error(), "make "+c.a.layout.GhRoot()+" writable by you (0700)")
		return true, nil
	}
	c.dir = dir
	c.r.pass("gh config dir " + dir)
	return false, nil
}

func (c *appCheck) repositories(ctx context.Context) (bool, error) {
	a, r := c.a, &c.r
	list, err := a.installationRepos(ctx, a.repos)
	if err != nil {
		if _, ok := isAPIError(err); !ok {
			return c.transport("list installation repositories", err)
		}
		r.fail("list installation repositories: " + err.Error())
		return false, nil
	}
	c.repos = list
	r.pass(fmt.Sprintf("installation repositories (%d): %s", list.Total, summarize(list.Names, list.Total, repoSummaryMax)))
	installURL := installationSettingsURL(c.inst.Account, a.cfg.InstallationID)
	for _, want := range a.repos {
		switch {
		case containsFold(list.Names, want):
		case list.Complete:
			r.fail(fmt.Sprintf("%s is not in installation %d's repository access", want, a.cfg.InstallationID),
				fmt.Sprintf("open %s -> Repository access -> add %s -> Save", installURL, want))
		case c.inst.RepositorySelection == "all" && ownedBy(want, c.inst.Account.Login):
			// "all" covers every repository of the account; the gh probe below
			// still proves access to the first one.
		default:
			// A capped listing cannot show absence: say so instead of "inaccessible".
			r.warn(fmt.Sprintf("cannot confirm that %s is in installation %d's repository access: it has %d repositories and only the first %d were listed",
				want, a.cfg.InstallationID, list.Total, len(list.Names)),
				fmt.Sprintf("check %s -> Repository access by hand", installURL))
		}
	}
	return false, nil
}

func (c *appCheck) ghProbe(ctx context.Context) (bool, error) {
	a, r := c.a, &c.r
	probe := ""
	if len(a.repos) > 0 {
		probe = a.repos[0]
	} else if len(c.repos.Names) > 0 {
		probe = c.repos.Names[0]
	}
	switch {
	case a.run == nil:
		r.info("gh api check skipped: no command runner configured")
	case probe == "":
		r.info("gh api check skipped: no repository to probe")
	default:
		res, err := a.run.Run(ctx, execx.Cmd{
			Name:    "gh",
			Args:    []string{"api", "repos/" + probe, "--hostname", "github.com", "--jq", ".full_name"},
			Env:     appEnv(c.dir),
			Timeout: callTimeout,
			Label:   "gh api repos (identity check)",
		})
		where := fmt.Sprintf("gh api repos/%s with GH_CONFIG_DIR=%s", probe, c.dir)
		switch {
		case err != nil && ctx.Err() != nil:
			return c.transport(where, ctx.Err())
		case err != nil:
			r.fail(where+": "+err.Error(), fmt.Sprintf("run `GH_CONFIG_DIR=%s gh api repos/%s` to see the full error", c.dir, probe))
		case !strings.EqualFold(res.Out(), probe):
			r.fail(fmt.Sprintf("%s returned %q", where, res.Out()))
		default:
			r.pass(where)
		}
	}
	return false, nil
}

// repoList is the installation's repositories as far as they were listed.
type repoList struct {
	Names    []string // listed so far
	Total    int      // total_count GitHub reported
	Complete bool     // Names is every repository
}

const (
	repoPerPage    = 100
	maxRepoPages   = 50 // 5000 repositories
	repoSummaryMax = 20 // names printed in the PASS line
)

// installationRepos lists the full names the installation token can access.
// It stops early once every want is listed and the summary has enough names;
// it stops at maxRepoPages pages with Complete false when the installation
// has more repositories than that, so callers never read absence from a
// capped list. A 401 (revoked token) mints a new one and retries once.
func (a *App) installationRepos(ctx context.Context, want []string) (repoList, error) {
	var list repoList
	for page := 1; page <= maxRepoPages; page++ {
		var out struct {
			Total        int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", repoPerPage, page)
		if err := a.callInstallation(ctx, http.MethodGet, path, &out); err != nil {
			return repoList{}, err
		}
		for _, r := range out.Repositories {
			list.Names = append(list.Names, r.FullName)
		}
		list.Total = max(out.Total, len(list.Names))
		if len(out.Repositories) < repoPerPage || len(list.Names) >= out.Total {
			list.Complete = true
			return list, nil
		}
		if len(list.Names) >= repoSummaryMax && !slices.ContainsFunc(want, func(w string) bool { return !containsFold(list.Names, w) }) {
			return list, nil
		}
	}
	return list, nil
}

// callInstallation performs a REST call authenticated with the installation
// token. When GitHub answers 401 the token was revoked: it invalidates the
// cached one, mints a new one and repeats the call once.
func (a *App) callInstallation(ctx context.Context, method, path string, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := a.Token(ctx)
		if err != nil {
			return err
		}
		err = a.call(ctx, method, path, tok, out)
		if ae, ok := isAPIError(err); !ok || ae.Status != http.StatusUnauthorized || attempt > 0 {
			return err
		}
		a.Invalidate()
	}
}

func permission(perms map[string]string, name string) string {
	if v := perms[name]; v != "" {
		return v
	}
	return "none"
}

func appPermissionsURL(app appInfo) string {
	if app.Owner.Type == "Organization" {
		return fmt.Sprintf("https://github.com/organizations/%s/settings/apps/%s/permissions", app.Owner.Login, app.Slug)
	}
	return fmt.Sprintf("https://github.com/settings/apps/%s/permissions", app.Slug)
}

func installationSettingsURL(account ghAccount, id int64) string {
	if account.Type == "Organization" {
		return fmt.Sprintf("https://github.com/organizations/%s/settings/installations/%d", account.Login, id)
	}
	return fmt.Sprintf("https://github.com/settings/installations/%d", id)
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// summarize lists the first limit names of a total-long list.
func summarize(names []string, total, limit int) string {
	if total <= limit || len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, ... (+%d more)", strings.Join(names[:limit], ", "), total-limit)
}

// ownedBy reports whether fullName ("owner/name") belongs to owner.
func ownedBy(fullName, owner string) bool {
	o, _, ok := strings.Cut(fullName, "/")
	return ok && strings.EqualFold(o, owner)
}

// inConfigDir is rel under the user config's directory (~/.config/magnum),
// in ~ form; "~/.config/magnum/<rel>" when the layout names none.
func inConfigDir(l paths.Layout, rel string) string {
	dir := l.ConfigDir()
	if dir == "" {
		return "~/.config/magnum/" + rel
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home+"/") {
		dir = "~" + strings.TrimPrefix(dir, home)
	}
	return dir + "/" + rel
}
