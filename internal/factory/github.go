package factory

// The factory's GitHub (spec 6.6): gh as your own login, the gh identity
// the local review runs as (evalrun.LocalIdentity), and the publish's git
// (its push and its read of the remote branch) as you too: your gh config
// dir (operatorGHDir, where gh and its git credential helper read your
// login), GH_HOST pinned, and none of the GitHub tokens of the daemon's
// environment, which could push or open the PR as someone else
// (asOperator). It opens a PR ready for review (never --draft), finds the
// PR of a branch among the repository's own (never a fork's of the same
// branch name) and reads PRs back; the title and the body file pass to gh
// as arguments, never through a shell.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/identity"
)

// ghTimeout bounds a gh command of the factory's GitHub.
const ghTimeout = time.Minute

// ghTokens are the variables of the daemon's environment that would make gh
// (or git through gh's credential helper) act as another login than yours:
// nothing the publish runs inherits them.
var ghTokens = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// ghPRFields are the fields a PR is read back with (PRView), and the head
// repository a PR of a branch must be (PRByBranch).
const ghPRFields = "number,url,author,baseRefName,headRefOid,isDraft,headRepository,headRepositoryOwner"

// operatorGHDir is your own gh config directory, where gh and its git
// credential helper read your login: $GH_CONFIG_DIR when your environment
// sets one, else $XDG_CONFIG_HOME/gh, else ~/.config/gh (gh's own order),
// named explicitly so that no other config dir can stand in for it.
func operatorGHDir() (string, error) {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "gh"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("your gh config dir: %w", err)
	}
	return filepath.Join(home, ".config", "gh"), nil
}

// asOperator is the environment of what the publish runs as you, the gh
// identity id: identity.GH's (GH_HOST pinned) with your gh config dir, and
// the variables it never inherits: what the factory's commands never do
// (unsetEnv: the GitHub tokens among them) and every GitHub token.
func asOperator(ctx context.Context, cfg *config.Config, id config.Identity, run execx.Runner) (env map[string]string, unset []string, err error) {
	if env, err = identity.NewGH(id, run).Env(ctx); err != nil {
		return nil, nil, err
	}
	dir, err := operatorGHDir()
	if err != nil {
		return nil, nil, err
	}
	env = maps.Clone(env)
	env["GH_CONFIG_DIR"] = dir
	unset = slices.Concat(unsetEnv(cfg), ghTokens)
	slices.Sort(unset)
	return env, slices.Compact(unset), nil
}

// operatorIdentity is the gh identity the publish of repo runs as: the one
// its local review runs as (evalrun.LocalIdentity).
func operatorIdentity(cfg *config.Config, repo string) (config.Identity, error) {
	var w config.Watch
	if p := cfg.WatchFor(repo); p != nil {
		w = *p
	}
	name, err := evalrun.LocalIdentity(cfg, &w)
	if err != nil {
		return config.Identity{}, err
	}
	id := cfg.IdentityByName(name)
	if id == nil {
		return config.Identity{}, fmt.Errorf("no [[identity]] named %s", name)
	}
	return *id, nil
}

// StagingIdentity is the identity the staging status of repo reads as
// (internal/staging: its mirror's fetch and its GitHub reads): the poll
// identity of the watch that covers repo (its identity without a
// poll_identity), as magnum's review daemon reads that repository, so both
// daemons fill the mirror's shared state as one login, the one that holds
// Deployments: read; a repository no watch covers reads as the gh identity
// the factory publishes as (operatorIdentity).
func StagingIdentity(cfg *config.Config, repo string) (config.Identity, error) {
	if w := cfg.WatchFor(repo); w != nil {
		name := cmp.Or(w.PollIdentity, w.Identity)
		if id := cfg.IdentityByName(name); id != nil {
			return *id, nil
		}
		return config.Identity{}, fmt.Errorf("no [[identity]] named %s", name)
	}
	return operatorIdentity(cfg, repo)
}

// operatorRunner runs each command through next as you (asOperator): env
// laid over its own, unset added to its own.
type operatorRunner struct {
	next  execx.Runner
	env   map[string]string
	unset []string
}

func (r operatorRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	env := maps.Clone(c.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, r.env)
	c.Env, c.Unset = env, slices.Concat(c.Unset, r.unset)
	return r.next.Run(ctx, c)
}

// operatorGit is the publish's git for task repo repo: the engine's runner
// (behind the deny list) as you (asOperator), reaching origin as the
// engine's git does (HTTPSFetch).
func (e *Engine) operatorGit(ctx context.Context, repo string) (*gitx.Client, error) {
	if e.run == nil || e.d.Git == nil {
		return nil, errors.New("this factory daemon has no runner to push with")
	}
	id, err := operatorIdentity(e.cfg, repo)
	if err != nil {
		return nil, err
	}
	env, unset, err := asOperator(ctx, e.cfg, id, e.run)
	if err != nil {
		return nil, err
	}
	g := gitx.New(operatorRunner{next: e.run, env: env, unset: unset})
	g.HTTPSFetch = e.d.Git.HTTPSFetch
	return g, nil
}

// NewGitHub is the factory's GitHub over run: gh as your gh identity for
// each repository, the one its local review runs as.
func NewGitHub(cfg *config.Config, run execx.Runner) GitHub { return ghClient{cfg: cfg, run: run} }

type ghClient struct {
	cfg *config.Config
	run execx.Runner
}

// gh runs gh with args as your gh identity for repo (label names it), as
// you only (asOperator).
func (g ghClient) gh(ctx context.Context, repo, label string, mutates bool, args ...string) (execx.Result, error) {
	id, err := operatorIdentity(g.cfg, repo)
	if err != nil {
		return execx.Result{}, fmt.Errorf("%s: %w", label, err)
	}
	env, unset, err := asOperator(ctx, g.cfg, id, g.run)
	if err != nil {
		return execx.Result{}, fmt.Errorf("%s: %w", label, err)
	}
	return g.run.Run(ctx, execx.Cmd{Name: "gh", Args: args, Env: env, Unset: unset, Timeout: ghTimeout, Mutates: mutates,
		Label: label + " (as " + id.Login + ")"})
}

// prURLRe is the number at the end of a PR's URL.
var prURLRe = regexp.MustCompile(`/pull/(\d+)\s*$`)

// CreatePR opens the PR of branch on base in repo, ready for review, with
// title and the body of bodyFile: its number and URL.
func (g ghClient) CreatePR(ctx context.Context, repo, base, branch, title, bodyFile string) (PRView, error) {
	res, err := g.gh(ctx, repo, "gh pr create "+repo+" "+branch, true,
		"pr", "create", "--repo", repo, "--base", base, "--head", branch, "--title", title, "--body-file", bodyFile)
	if err != nil {
		return PRView{}, err
	}
	url := strings.TrimSpace(res.Out())
	if i := strings.LastIndexByte(url, '\n'); i >= 0 {
		url = url[i+1:]
	}
	m := prURLRe.FindStringSubmatch(url)
	if m == nil {
		return PRView{}, fmt.Errorf("gh pr create %s %s: no PR URL in its answer %.80q", repo, branch, res.Out())
	}
	n, _ := strconv.Atoi(m[1])
	return PRView{Number: n, URL: url}, nil
}

// ghPRView is a PR as gh's --json prints it.
type ghPRView struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	BaseRefName    string `json:"baseRefName"`
	HeadRefOid     string `json:"headRefOid"`
	IsDraft        bool   `json:"isDraft"`
	HeadRepository struct {
		Name string `json:"name"`
	} `json:"headRepository"`
	HeadRepositoryOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

// from reports whether p's head branch is in repo (owner/name): the
// repository's own branch, not a fork's.
func (p ghPRView) from(repo string) bool {
	return strings.EqualFold(p.HeadRepositoryOwner.Login+"/"+p.HeadRepository.Name, repo)
}

func (p ghPRView) view() PRView {
	return PRView{Number: p.Number, URL: p.URL, Author: p.Author.Login, Base: p.BaseRefName, HeadSHA: p.HeadRefOid, Draft: p.IsDraft}
}

// PRByBranch is the open PR of branch in repo whose head is repo's own
// branch (a fork's PR of a branch of the same name is none of its); ok is
// false when it has none.
func (g ghClient) PRByBranch(ctx context.Context, repo, branch string) (PRView, bool, error) {
	res, err := g.gh(ctx, repo, "gh pr list "+repo+" "+branch, false,
		"pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", ghPRFields, "--limit", "20")
	if err != nil {
		return PRView{}, false, err
	}
	var prs []ghPRView
	if err := json.Unmarshal(res.Stdout, &prs); err != nil {
		return PRView{}, false, fmt.Errorf("gh pr list %s %s: %w", repo, branch, err)
	}
	if i := slices.IndexFunc(prs, func(p ghPRView) bool { return p.from(repo) }); i >= 0 {
		return prs[i].view(), true, nil
	}
	return PRView{}, false, nil
}

// ActiveLogin is the login gh acts as for repo, as you only (asOperator):
// `gh api user --jq .login`, as identity.GH's check asks it, the account
// active in your gh config dir.
func (g ghClient) ActiveLogin(ctx context.Context, repo string) (string, error) {
	res, err := g.gh(ctx, repo, "gh api user", false, "api", "user", "--jq", ".login", "--hostname", "github.com")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(res.Out())
	if login == "" {
		return "", errors.New("gh api user names no login")
	}
	return login, nil
}

// MergeMethods are the ways repo's GitHub settings let a PR merge, read as
// you (`gh api repos/<repo>`). GitHub shows them only to a login that may
// push to repo: an answer without them, or that allows none, is an error,
// never "none allowed".
func (g ghClient) MergeMethods(ctx context.Context, repo string) (MergeMethods, error) {
	label := "gh api repos/" + repo
	res, err := g.gh(ctx, repo, label, false, "api", "repos/"+repo, "--jq", "{allow_merge_commit, allow_squash_merge, allow_rebase_merge}")
	if err != nil {
		return MergeMethods{}, err
	}
	var s struct {
		Merge  *bool `json:"allow_merge_commit"`
		Squash *bool `json:"allow_squash_merge"`
		Rebase *bool `json:"allow_rebase_merge"`
	}
	if err := json.Unmarshal(res.Stdout, &s); err != nil {
		return MergeMethods{}, fmt.Errorf("%s: %w", label, err)
	}
	if s.Merge == nil || s.Squash == nil || s.Rebase == nil {
		return MergeMethods{}, fmt.Errorf("%s names no merge methods: GitHub shows them only to a login that may push", label)
	}
	m := MergeMethods{Merge: *s.Merge, Squash: *s.Squash, Rebase: *s.Rebase}
	if m == (MergeMethods{}) {
		return MergeMethods{}, fmt.Errorf("%s allows no merge method", label)
	}
	return m, nil
}

// ViewPR reads PR number of repo back.
func (g ghClient) ViewPR(ctx context.Context, repo string, number int) (PRView, error) {
	res, err := g.gh(ctx, repo, fmt.Sprintf("gh pr view %s#%d", repo, number), false,
		"pr", "view", strconv.Itoa(number), "--repo", repo, "--json", ghPRFields)
	if err != nil {
		return PRView{}, err
	}
	var p ghPRView
	if err := json.Unmarshal(res.Stdout, &p); err != nil {
		return PRView{}, fmt.Errorf("gh pr view %s#%d: %w", repo, number, err)
	}
	return p.view(), nil
}
