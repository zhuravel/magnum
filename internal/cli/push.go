package cli

// `magnum push`: a fixer App's way to push (the fix prompt names the line,
// agents.PushLine). The pane's env refuses a plain `git push` to github.com
// (identity.FixerGitEnv), and this command pushes HEAD to the PR's branch
// alone, as the App, with a lease on the head it read on GitHub: over
// HTTPS to the PR's repository, whatever form origin has, with gh as git's
// only credential helper (the pane's GH_CONFIG_DIR makes gh answer with the
// App's token). It needs nothing but the pane's env and git: no config, no
// registry, no daemon.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

const pushUsage = "push [--checkout DIR] [--base BRANCH]"

// Exit statuses of push. 0: pushed, or the branch is at HEAD already.
const (
	// pushFailed: anything else (the network, auth, a push GitHub does
	// not show afterwards): nothing may have changed, or the push may have
	// gone with its answer lost; run the same line again.
	pushFailed = 1
	// pushRefused: the line, the pane or the commits are wrong (a branch
	// it may not push to, a commit not by the fixer, a merge of another
	// branch, a workflow change): fix them, never run the same line again.
	pushRefused = 2
	// pushRejected: the branch moved on GitHub since HEAD's base ("fetch
	// first"): the fixer runs the fix-sync line.
	pushRejected = 3
)

// Statuses of push's JSON outcome.
const (
	pushStatusPushed   = "pushed"
	pushStatusUpToDate = "up_to_date"
	pushStatusRefused  = "refused"
	pushStatusRejected = "rejected"
	pushStatusError    = "error"
)

// pushLsRemoteTimeout bounds one read of the branch's head on GitHub.
const pushLsRemoteTimeout = time.Minute

// pushRunner runs git for push, pushGetenv reads the pane's env, and
// pushTarget is the URL push reaches owner/repo at (GitHub's HTTPS URL);
// tests replace them.
var (
	pushRunner = func() execx.Runner { return &execx.Real{} }
	pushGetenv = os.Getenv
	pushTarget = gitx.GitHubHTTPSURL
)

// workflowsDir is where GitHub keeps workflows, which an App without the
// workflows permission may not push.
const workflowsDir = ".github/workflows/"

// pushOutcome is push's JSON answer: the status, the branch, HEAD's sha,
// the head it leased on GitHub, and a sentence (redacted).
type pushOutcome struct {
	Status  string `json:"status"`
	Branch  string `json:"branch,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Expect  string `json:"expect,omitempty"`
	Message string `json:"message,omitempty"`
}

func newPushCmd(c *Context) *cobra.Command {
	var checkout, base string
	cmd := newCommand(groupAct, pushUsage, "push the fixer App's commits to its PR's branch, with a lease (for the fixer)",
		"For a fixer App's pane, whose env refuses a plain `git push` to github.com: the fix prompt names the line. It "+
			"pushes HEAD of the checkout (--checkout, default: the git work tree here) to the PR's branch alone "+
			"(MAGNUM_PUSH_BRANCH; it takes no branch argument), over HTTPS to the repository of the pane's MAGNUM_PR_URL "+
			"whatever form origin has, with gh as git's only credential helper (the pane's GH_CONFIG_DIR makes it the App), "+
			"and with a lease on the head it read there: never --force. The PR's base is --base, which the push line "+
			"names each cycle, else the pane's MAGNUM_BASE_BRANCH (set when the pane was made: a PR retargeted since "+
			"has another).\n\n"+
			"It refuses (exit 2, nothing run on the network) a branch that is master, main, the default branch "+
			"(MAGNUM_DEFAULT_BRANCH), the base or the pane's MAGNUM_BASE_BRANCH, a --base that names no branch, an "+
			"argument, a pane without the bot's "+
			"GIT_AUTHOR_EMAIL and GIT_COMMITTER_EMAIL, and an origin of another repository; then (exit 2) a commit on "+
			"the first-parent chain it would add that is not the bot's, a merge of anything but the base, and, with "+
			"MAGNUM_PUSH_NO_WORKFLOWS=1, a change under .github/workflows (list it under Needs you). HEAD that does not "+
			"descend from the branch's head on GitHub, or a lease GitHub refuses, is exit 3 (\"fetch first\": run the "+
			"fix-sync line). It reads the branch back after the push.\n\n"+
			"It prints one JSON object on stdout (status: pushed, up_to_date, refused, rejected or error) and one line on "+
			"stderr. Exit 0: pushed, or the branch is at HEAD already; 2: refused, fix the line or the commits and never "+
			"run the same line again; 3: fetch first; 1: anything else, run it again. It runs no git hook, reads no config "+
			"and no registry, never talks to the daemon and writes no git config.",
		func(pos []string) int { return runPush(c, checkout, base, pos) })
	// The command needs no magnum layout: it runs wherever the fixer's pane is.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitCode(pushPrint(c, pushOutcome{Status: pushStatusRefused, Message: err.Error()}, pushRefused))
	})
	cmd.Flags().StringVar(&checkout, "checkout", "", "the fixer's checkout (default: the git work tree here)")
	_ = cmd.MarkFlagDirname("checkout")
	cmd.Flags().StringVar(&base, "base", "", "the PR's base (default: the pane's MAGNUM_BASE_BRANCH)")
	return cmd
}

// pushPrint prints out as JSON on stdout and one line on stderr, both
// redacted, and returns code.
func pushPrint(c *Context, out pushOutcome, code int) int {
	out.Message = execx.Redact(out.Message)
	_ = writeJSON(c.Stdout, out)
	line := out.Status
	if out.Message != "" {
		line += ": " + out.Message
	}
	fmt.Fprintln(c.Stderr, "magnum push:", line)
	return code
}

func runPush(c *Context, checkout, base string, pos []string) int {
	o, code := pushRun(checkout, base, pos)
	return pushPrint(c, o, code)
}

// pushRun is push's decision and its one push, as (5) of the fixer App's
// design lays it out; its outcome and exit code. base is --base, the PR's
// base as the push line names it each cycle ("" = the pane's
// MAGNUM_BASE_BRANCH, which the pane got when it was made).
func pushRun(checkout, base string, pos []string) (pushOutcome, int) {
	get := pushGetenv
	branch := get("MAGNUM_PUSH_BRANCH")
	refused := func(format string, a ...any) (pushOutcome, int) {
		return pushOutcome{Status: pushStatusRefused, Branch: branch, Message: fmt.Sprintf(format, a...)}, pushRefused
	}
	if len(pos) > 0 {
		return refused("magnum push takes no argument (%q): it always pushes HEAD to MAGNUM_PUSH_BRANCH", pos[0])
	}
	owner, repo, _, err := github.ParseRef(get("MAGNUM_PR_URL"), "")
	if err != nil {
		return refused("MAGNUM_PR_URL names no pull request (%v): magnum push runs in the fixer's pane, which magnum starts with the PR's URL", err)
	}
	paneBase, def := get("MAGNUM_BASE_BRANCH"), get("MAGNUM_DEFAULT_BRANCH")
	switch {
	case branch == "":
		return refused("MAGNUM_PUSH_BRANCH is not set: magnum push pushes only to the PR's branch its pane names")
	case !gitx.ShellSafeRef(branch):
		return refused("MAGNUM_PUSH_BRANCH %q is no branch name magnum pushes to", branch)
	case base != "" && !gitx.ShellSafeRef(base):
		return refused("--base %q is no branch name", base)
	case slices.ContainsFunc([]string{"master", "main", def, paneBase, base}, func(b string) bool { return b != "" && strings.EqualFold(b, branch) }):
		return refused("magnum push never pushes to %s (master, main, the default branch or the PR's base)", branch)
	}
	base = cmp.Or(base, paneBase)
	email := get("GIT_AUTHOR_EMAIL")
	if email == "" || !strings.EqualFold(get("GIT_COMMITTER_EMAIL"), email) {
		return refused("GIT_AUTHOR_EMAIL and GIT_COMMITTER_EMAIL must both be the bot's: magnum push is a fixer App's command, whose pane sets them")
	}
	login := cmp.Or(get("GIT_AUTHOR_NAME"), email)

	ctx, stop := signalContext()
	defer stop()
	r := pushRunner()
	dir, err := gitWorkTree(ctx, r, checkout)
	if err != nil {
		return refused("%v", err)
	}
	g := gitx.New(r)
	failed := func(format string, a ...any) (pushOutcome, int) {
		return pushOutcome{Status: pushStatusError, Branch: branch, Message: fmt.Sprintf(format, a...)}, pushFailed
	}
	originURL, err := g.RemoteURL(ctx, dir)
	if err != nil {
		return refused("could not read origin of %s: %v", dir, err)
	}
	if rem, ok := gitx.ParseRemote(originURL); !ok || rem.Host != "github.com" || !strings.EqualFold(rem.Owner, owner) || !strings.EqualFold(rem.Repo, repo) {
		return refused("origin is %s, not github.com/%s/%s, the PR's repository", originURL, owner, repo)
	}
	sha, err := g.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return failed("could not read HEAD: %v", err)
	}
	url := pushTarget(owner, repo)
	expect, err := pushRemoteHead(ctx, g, dir, url, branch)
	switch {
	case err != nil:
		return failed("could not read %s on GitHub: %v", branch, err)
	case expect == "":
		return failed("%s is gone on GitHub: the PR's branch was deleted", branch)
	}
	out := pushOutcome{Branch: branch, SHA: sha, Expect: expect}
	if sha == expect {
		out.Status, out.Message = pushStatusUpToDate, fmt.Sprintf("%s is at %s already", branch, textx.ShortSHA(sha))
		return out, 0
	}
	rejected := func(why string) (pushOutcome, int) {
		out.Status, out.Message = pushStatusRejected, why+": fetch first (run the fix-sync line of your prompt)"
		return out, pushRejected
	}
	refuse := func(format string, a ...any) (pushOutcome, int) {
		out.Status, out.Message = pushStatusRefused, fmt.Sprintf(format, a...)
		return out, pushRefused
	}
	if _, err := g.RevParse(ctx, dir, expect); err != nil {
		return rejected(fmt.Sprintf("%s moved on GitHub to %s, which the checkout does not have", branch, textx.ShortSHA(expect)))
	}
	if ok, err := g.IsAncestor(ctx, dir, expect, sha); err != nil || !ok {
		return rejected(fmt.Sprintf("HEAD %s does not descend from %s's head %s on GitHub", textx.ShortSHA(sha), branch, textx.ShortSHA(expect)))
	}
	if why, err := pushOwnCommits(ctx, g, dir, expect, sha, email, login, base); err != nil {
		return failed("could not read the commits to push: %v", err)
	} else if why != "" {
		return refuse("%s", why)
	}
	if get("MAGNUM_PUSH_NO_WORKFLOWS") == "1" {
		paths, err := g.ChangedBetween(ctx, dir, expect, sha)
		if err != nil {
			return failed("could not read the files the push changes: %v", err)
		}
		if i := slices.IndexFunc(paths, func(p string) bool { return strings.HasPrefix(p, workflowsDir) }); i >= 0 {
			return refuse("the push changes %s, and the App may not push workflows: list it under Needs you", paths[i])
		}
	}
	if err := g.PushLeaseVia(ctx, dir, url, sha, branch, expect); err != nil {
		if errors.Is(err, gitx.ErrNotDescendant) || leaseRefused(err) {
			return rejected(fmt.Sprintf("GitHub refused the lease on %s at %s: the branch moved", branch, textx.ShortSHA(expect)))
		}
		out.Status, out.Message = pushStatusError, fmt.Sprintf("the push failed: %v", err)
		return out, pushFailed
	}
	got, err := pushRemoteHead(ctx, g, dir, url, branch)
	if err != nil || got != sha {
		out.Status, out.Message = pushStatusError, fmt.Sprintf("pushed %s, but it is not seen on GitHub (%s is at %s)", textx.ShortSHA(sha), branch,
			cmp.Or(textx.ShortSHA(got), "unknown"))
		return out, pushFailed
	}
	out.Status, out.Message = pushStatusPushed, fmt.Sprintf("pushed %s to %s (over %s)", textx.ShortSHA(sha), branch, textx.ShortSHA(expect))
	return out, 0
}

// pushRemoteHead is the head of branch at url ("" when there is no such
// branch).
func pushRemoteHead(ctx context.Context, g *gitx.Client, dir, url, branch string) (string, error) {
	out, err := g.LsRemoteVia(ctx, dir, url, pushLsRemoteTimeout, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(out) {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && ref == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", nil
}

// pushOwnCommits is why the commits expect..sha on sha's first-parent
// chain may not go out as the fixer's ("" when they may): each must have
// the pane's email as author and committer, and each merge's other parents
// must be on origin's base already (the base merge alone: the base commits
// it brings were on GitHub).
func pushOwnCommits(ctx context.Context, g *gitx.Client, dir, expect, sha, email, login, base string) (string, error) {
	commits, err := g.FirstParentIdentities(ctx, dir, expect, sha)
	if err != nil {
		return "", err
	}
	for _, c := range commits {
		if !strings.EqualFold(c.AuthorEmail, email) || !strings.EqualFold(c.CommitterEmail, email) {
			return fmt.Sprintf("commit %s is not by %s: magnum push pushes the fixer's commits alone", textx.ShortSHA(c.SHA), login), nil
		}
		for _, p := range c.Parents[min(1, len(c.Parents)):] {
			ok := false
			if base != "" && gitx.ShellSafeRef(base) {
				if ok, err = g.IsAncestor(ctx, dir, p, "refs/remotes/origin/"+base); err != nil {
					ok = false
				}
			}
			if !ok {
				return fmt.Sprintf("commit %s merges a branch other than %s: magnum push pushes a merge of the base alone",
					textx.ShortSHA(c.SHA), cmp.Or(base, "the base")), nil
			}
		}
	}
	return "", nil
}

// leaseRefused reports whether a push failed because GitHub refused its
// lease or its update (the branch moved since the read).
func leaseRefused(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "stale info") || strings.Contains(msg, "[rejected]") || strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "non-fast-forward")
}
