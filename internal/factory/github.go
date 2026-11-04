package factory

// The factory's GitHub (spec 6.6): gh as your own login, the gh identity
// the local review runs as (evalrun.LocalIdentity), with your gh config dir
// and keyring login (identity.GH's environment, GH_HOST pinned) and no
// token of the daemon's environment, so a PR the factory opens is yours.
// It opens a PR ready for review (never --draft) and reads PRs back; the
// title and the body file pass to gh as arguments, never through a shell.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/identity"
)

// ghTimeout bounds a gh command of the factory's GitHub.
const ghTimeout = time.Minute

// ghTokens are the variables of the daemon's environment that would make gh
// act as another login than yours: gh never inherits them here.
var ghTokens = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// ghPRFields are the fields a PR is read back with (PRView).
const ghPRFields = "number,url,author,baseRefName,headRefOid,isDraft"

// NewGitHub is the factory's GitHub over run: gh as your gh identity for
// each repository, the one its local review runs as.
func NewGitHub(cfg *config.Config, run execx.Runner) GitHub { return ghClient{cfg: cfg, run: run} }

type ghClient struct {
	cfg *config.Config
	run execx.Runner
}

// gh runs gh with args as your gh identity for repo (label names it).
func (g ghClient) gh(ctx context.Context, repo, label string, mutates bool, args ...string) (execx.Result, error) {
	var w config.Watch
	if p := g.cfg.WatchFor(repo); p != nil {
		w = *p
	}
	name, err := evalrun.LocalIdentity(g.cfg, &w)
	if err != nil {
		return execx.Result{}, fmt.Errorf("%s: %w", label, err)
	}
	id := g.cfg.IdentityByName(name)
	if id == nil {
		return execx.Result{}, fmt.Errorf("%s: no [[identity]] named %s", label, name)
	}
	env, err := identity.NewGH(*id, g.run).Env(ctx)
	if err != nil {
		return execx.Result{}, err
	}
	return g.run.Run(ctx, execx.Cmd{Name: "gh", Args: args, Env: env, Unset: ghTokens, Timeout: ghTimeout, Mutates: mutates,
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
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	IsDraft     bool   `json:"isDraft"`
}

func (p ghPRView) view() PRView {
	return PRView{Number: p.Number, URL: p.URL, Author: p.Author.Login, Base: p.BaseRefName, HeadSHA: p.HeadRefOid, Draft: p.IsDraft}
}

// PRByBranch is the open PR of branch in repo; ok is false when it has none.
func (g ghClient) PRByBranch(ctx context.Context, repo, branch string) (PRView, bool, error) {
	res, err := g.gh(ctx, repo, "gh pr list "+repo+" "+branch, false,
		"pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", ghPRFields, "--limit", "5")
	if err != nil {
		return PRView{}, false, err
	}
	var prs []ghPRView
	if err := json.Unmarshal(res.Stdout, &prs); err != nil {
		return PRView{}, false, fmt.Errorf("gh pr list %s %s: %w", repo, branch, err)
	}
	if len(prs) == 0 {
		return PRView{}, false, nil
	}
	return prs[0].view(), true, nil
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
