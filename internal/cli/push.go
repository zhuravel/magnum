package cli

// `magnum push`: a fixer App's way to push (the fix prompt names the line,
// agents.PushLine). The pane's env refuses a plain `git push` to github.com
// (identity.FixerGitEnv), and this command pushes HEAD to the PR's branch
// alone, as the App, with a lease on the head it read on GitHub: over
// HTTPS to the PR's repository, whatever form origin has, with gh as git's
// only credential helper (the pane's GH_CONFIG_DIR makes gh answer with the
// App's token). It needs nothing but the pane's env, git and the lock of
// the branch, a file in magnum's state directory: no config, no registry,
// no daemon. One agent pushes a branch at a time through it: the push
// holds the lock of its repository and branch (paths.Layout.PushLock,
// dblock's flock) for its run, and refuses while another agent's push
// holds it, so two fixer Apps' pushes through `magnum push`, of either
// daemon's fixer, never push one PR's branch at once. Only this command
// takes the lock: a gh fixer pushes with a plain `git push` (the fix
// prompt's line when the fixer acts as no fixer App), and the engine's own
// merge pushes (the merge of the parent, a held merge: PushLeaseGitHubAs)
// skip it. pr.<id>.fix_owner, which gives a PR's fixes to one daemon, and
// the leases (a plain push never forces) guard those.

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

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

const pushUsage = "push [--checkout DIR] [--base BRANCH] [--ci]"

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
	// pushDaemon: HEAD is the base merge alone, and it carries the base's
	// own workflow files, which the pane's token may not push: magnum's
	// daemon pushes it once the run ends (the fixer lists it and stops).
	pushDaemon = 4
)

// Statuses of push's JSON outcome.
const (
	pushStatusPushed   = "pushed"
	pushStatusUpToDate = "up_to_date"
	pushStatusRefused  = "refused"
	pushStatusRejected = "rejected"
	pushStatusDaemon   = "daemon"
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
	var ci bool
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
			"MAGNUM_PUSH_NO_WORKFLOWS=1, a change under .github/workflows that differs from the base's on origin (list "+
			"it under Needs you), and with --ci (a CI fix's push line) a change to CI config (.github/workflows, "+
			".github/actions, .github/bin, an action.yml) that differs from the base's. A base merge alone that carries the base's own workflow files, as the base has them, "+
			"is exit 4 (daemon): the pane's token may not push them, and magnum's daemon pushes the merge once the run "+
			"ends. HEAD that does not "+
			"descend from the branch's head on GitHub, or a lease GitHub refuses, is exit 3 (\"fetch first\": run the "+
			"fix-sync line). It reads the branch back after the push.\n\n"+
			"It prints one JSON object on stdout (status: pushed, up_to_date, refused, rejected, daemon or error) and one line on "+
			"stderr. Exit 0: pushed, or the branch is at HEAD already; 2: refused, fix the line or the commits and never "+
			"run the same line again; 3: fetch first; 4: magnum pushes the merge, push nothing more and end the run; "+
			"1: anything else, run it again. It runs no git hook, reads no config "+
			"and no registry, never talks to the daemon and writes no git config.\n\n"+
			"One agent pushes a branch at a time: it holds the lock of the PR's repository and branch, a file in magnum's state "+
			"directory, for its run, and refuses (exit 2, nothing run on the network) while another agent's push holds it, "+
			"naming that agent, and without a magnum state directory to keep the lock in.",
		func(pos []string) int { return runPush(c, checkout, base, ci, pos) })
	// The command needs no magnum layout: it runs wherever the fixer's pane is.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitCode(pushPrint(c, pushOutcome{Status: pushStatusRefused, Message: err.Error()}, pushRefused))
	})
	cmd.Flags().StringVar(&checkout, "checkout", "", "the fixer's checkout (default: the git work tree here)")
	_ = cmd.MarkFlagDirname("checkout")
	cmd.Flags().StringVar(&base, "base", "", "the PR's base (default: the pane's MAGNUM_BASE_BRANCH)")
	cmd.Flags().BoolVar(&ci, "ci", false, "a CI fix's push: refuse a change to CI config (.github/workflows, .github/actions, .github/bin, action.yml) "+
		"that differs from the base's")
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

func runPush(c *Context, checkout, base string, ci bool, pos []string) int {
	lockPath := func(repo, branch string) (string, error) {
		switch {
		case c.layoutErr != nil:
			return "", c.layoutErr
		case !c.Layout.Valid():
			return "", errors.New("no magnum state directory")
		}
		return c.Layout.PushLock(repo, branch), nil
	}
	o, code := pushRun(checkout, base, ci, pos, lockPath)
	return pushPrint(c, o, code)
}

// pushLock takes the lock of branch of repo (owner/name) at the path
// lockPath gives, for this push (one try): ok is false, with the outcome
// and the exit code push ends with, when another agent's push holds it
// (refused: it names that push's agent) or it cannot be taken. agent is the
// pane's agent (MAGNUM_AGENT), else the login it pushes as.
func pushLock(ctx context.Context, lockPath func(repo, branch string) (string, error), repo, branch, agent string) (*dblock.Lock, pushOutcome, int, bool) {
	out := pushOutcome{Status: pushStatusRefused, Branch: branch}
	path, err := lockPath(repo, branch)
	if err != nil {
		out.Message = fmt.Sprintf("magnum push holds the lock of %s for its push, a file in magnum's state directory, and has none here (%v): "+
			"push nothing; list it under Needs you", branch, err)
		return nil, out, pushRefused, false
	}
	lock, err := dblock.Acquire(ctx, path, dblock.Holder{Role: agent, Command: "magnum push " + branch, PID: os.Getpid()}, 0, nil)
	if te, held := errors.AsType[*dblock.TimeoutError](err); held {
		out.Message = fmt.Sprintf("another agent pushes %s of %s now (%s): push nothing; list it under Needs you", branch, repo, te.Holder)
		return nil, out, pushRefused, false
	}
	if err != nil {
		out.Status, out.Message = pushStatusError, fmt.Sprintf("could not take the lock of %s: %v", branch, err)
		return nil, out, pushFailed, false
	}
	return lock, pushOutcome{}, 0, true
}

// pushRun is push's decision and its one push, as (5) of the fixer App's
// design lays it out; its outcome and exit code. base is --base, the PR's
// base as the push line names it each cycle ("" = the pane's
// MAGNUM_BASE_BRANCH, which the pane got when it was made). lockPath is the
// lock file of a repository and branch (paths.Layout.PushLock), which the
// push holds for its run (pushLock).
func pushRun(checkout, base string, ci bool, pos []string, lockPath func(repo, branch string) (string, error)) (pushOutcome, int) {
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
	lock, held, code, ok := pushLock(ctx, lockPath, owner+"/"+repo, branch, cmp.Or(get(agenthooks.EnvAgent), login))
	if !ok {
		return held, code
	}
	defer lock.Release()
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
	commits, err := g.FirstParentIdentities(ctx, dir, expect, sha)
	if err != nil {
		return failed("could not read the commits to push: %v", err)
	}
	if why := pushOwnCommits(ctx, g, dir, commits, email, login, base); why != "" {
		return refuse("%s", why)
	}
	if ci { // a CI fix changes the PR's code, never its CI config
		paths, err := g.ChangedBetween(ctx, dir, expect, sha)
		if err != nil {
			return failed("could not read the files the push changes: %v", err)
		}
		own, err := pushOwnWorkflows(ctx, g, dir, sha, base, slices.DeleteFunc(paths, func(p string) bool { return !engine.CIConfigPath(p) }))
		switch {
		case err != nil:
			return failed("could not compare the CI files with %s's: %v", base, err)
		case len(own) > 0:
			return refuse("the push changes %s, and a CI fix never changes CI config: push nothing, list the job under Needs you as config", own[0])
		}
	}
	if get("MAGNUM_PUSH_NO_WORKFLOWS") == "1" {
		paths, err := g.ChangedBetween(ctx, dir, expect, sha)
		if err != nil {
			return failed("could not read the files the push changes: %v", err)
		}
		paths = slices.DeleteFunc(paths, func(p string) bool { return !strings.HasPrefix(p, workflowsDir) })
		own, err := pushOwnWorkflows(ctx, g, dir, sha, base, paths)
		switch {
		case err != nil:
			return failed("could not compare the workflow files with %s's: %v", base, err)
		case len(own) > 0:
			return refuse("the push changes %s, and the App may not push workflows: list it under Needs you", own[0])
		case len(paths) > 0 && !pushLoneMerge(commits, expect):
			return refuse("the push brings %s's own workflow files with other commits: push the merge of %s alone first", base, base)
		case len(paths) > 0:
			out.Status = pushStatusDaemon
			out.Message = fmt.Sprintf("the merge brings %s's own workflow files (%s), which the pane's token may not push: magnum pushes it once "+
				"your run ends; list it under ## Pushed, push nothing more and end the run", base, textx.Count(len(paths), "file", "files"))
			return out, pushDaemon
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

// pushOwnWorkflows is which of paths, the workflow files the push
// changes, are the fixer's own: those whose content at sha differs from
// origin's base, as a base merge carries the base's as the base has them
// (gitx DifferingPaths). Without a base to compare with, every one is.
func pushOwnWorkflows(ctx context.Context, g *gitx.Client, dir, sha, base string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	ref := "refs/remotes/origin/" + base
	if base == "" || !gitx.ShellSafeRef(base) {
		return paths, nil
	}
	if _, err := g.RevParse(ctx, dir, ref); err == nil {
		return g.DifferingPaths(ctx, dir, sha, ref, paths...)
	}
	return paths, nil // the checkout has no base to compare with
}

// pushLoneMerge reports whether commits, the push's first-parent chain
// over expect, are one merge alone whose first parent is expect: the
// base merge, pushed before any fix (pushOwnCommits checked its other
// parent is on the base).
func pushLoneMerge(commits []gitx.CommitIdentity, expect string) bool {
	return len(commits) == 1 && len(commits[0].Parents) == 2 && commits[0].Parents[0] == expect
}

// pushOwnCommits is why commits, the commits expect..sha on sha's
// first-parent chain, may not go out as the fixer's ("" when they may):
// each must have the pane's email as author and committer, and each
// merge's other parents must be on origin's base already (the base merge
// alone: the base commits it brings were on GitHub).
func pushOwnCommits(ctx context.Context, g *gitx.Client, dir string, commits []gitx.CommitIdentity, email, login, base string) string {
	for _, c := range commits {
		if !strings.EqualFold(c.AuthorEmail, email) || !strings.EqualFold(c.CommitterEmail, email) {
			return fmt.Sprintf("commit %s is not by %s: magnum push pushes the fixer's commits alone", textx.ShortSHA(c.SHA), login)
		}
		for _, p := range c.Parents[min(1, len(c.Parents)):] {
			ok := false
			if base != "" && gitx.ShellSafeRef(base) {
				if on, err := g.IsAncestor(ctx, dir, p, "refs/remotes/origin/"+base); err == nil {
					ok = on
				}
			}
			if !ok {
				return fmt.Sprintf("commit %s merges a branch other than %s: magnum push pushes a merge of the base alone",
					textx.ShortSHA(c.SHA), cmp.Or(base, "the base"))
			}
		}
	}
	return ""
}

// leaseRefused reports whether a push failed because GitHub refused its
// lease or its update (the branch moved since the read).
func leaseRefused(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "stale info") || strings.Contains(msg, "[rejected]") || strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "non-fast-forward")
}
