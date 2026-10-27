package cli

// `magnum fix-sync`: the fixer's way past a push GitHub rejected because the
// PR's branch gained a merge of its base while the fixer worked (GitHub's
// "Update branch", talkable#12114). The fix prompt names the line
// (agents.FixSyncLine) with the run's start head, the branch and the base;
// the command decides with local git alone (gitx.CommitsAfter, OwnDiffID,
// ReplayOnto) and needs nothing else: no config, no registry, no daemon.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

const fixSyncUsage = "fix-sync --from SHA --branch BRANCH --base BRANCH [--checkout DIR]"

// Exit statuses of fix-sync.
const (
	// fixSyncRefused: the command changed nothing (the line on stdout says
	// why), the checkout is as it was.
	fixSyncRefused = 1
	// fixSyncBadArgs: its flags are missing or wrong; it ran nothing.
	fixSyncBadArgs = 2
)

// maxSyncPaths bounds the conflicting paths a refusal names.
const maxSyncPaths = 10

// fixSyncRunner runs git for fix-sync; tests replace it.
var fixSyncRunner = func() execx.Runner { return &execx.Real{} }

type fixSyncOpts struct {
	checkout, from, branch, base string
}

func newFixSyncCmd(c *Context) *cobra.Command {
	var o fixSyncOpts
	cmd := newCommand(groupAct, fixSyncUsage, "replay the fixer's commits onto a merge of the base its PR's branch gained (for the fixer)",
		"For the fixer's pane: the fix prompt names the line, which the fixer runs once when GitHub rejects its push "+
			"because the PR's branch moved, as GitHub's \"Update branch\" button moves it with a merge of the base. --from is "+
			"the head the fixer started from, --branch the PR's branch and --base its base branch; --checkout defaults to the "+
			"git work tree of the current directory.\n\n"+
			"It fetches the branch and the base from origin. When every commit the branch gained since --from is a merge on "+
			"its first-parent chain, and the PR's own diff against the base (from their merge base to the head) has the same "+
			"patch id at --from and at the branch's tip, it replays the fixer's commits (--from to HEAD, none of them a merge, "+
			"none of them on GitHub) onto the tip and prints \"replayed N commits onto <sha>; HEAD is <sha>\": the fixer runs "+
			"the specs of the changed files again and pushes, never with force. Anything else changes nothing and prints "+
			"\"refused: <reason>\": a commit that is not a merge (someone works on the branch), a merge that changed the PR's "+
			"own diff, a rewritten branch, a merge among the fixer's own commits, uncommitted changes to tracked files, an "+
			"operation in progress. A replay that conflicts is aborted, and the checkout is as it was.\n\n"+
			"Exit statuses: 0 replayed; 1 refused, nothing changed; 2 wrong flags, nothing run. It reads no config and no "+
			"registry and never talks to the daemon.",
		func(args []string) int { return runFixSync(c, o, args) })
	// The command needs no magnum layout: it runs wherever the fixer's pane is.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitCode(fixSyncBadUsage(c, "%v", err))
	})
	fs := cmd.Flags()
	fs.StringVar(&o.from, "from", "", "the head the fixer started from (the fix prompt's)")
	fs.StringVar(&o.branch, "branch", "", "the PR's branch, which the fixer pushes to")
	fs.StringVar(&o.base, "base", "", "the PR's base branch")
	fs.StringVar(&o.checkout, "checkout", "", "the fixer's checkout (default: the git work tree here)")
	_ = cmd.MarkFlagDirname("checkout")
	return cmd
}

// fixSyncBadUsage says what is wrong with the command line, and returns
// fixSyncBadArgs.
func fixSyncBadUsage(c *Context, format string, a ...any) int {
	fmt.Fprintf(c.Stderr, "fix-sync: %s (usage: magnum %s)\n", execx.Redact(fmt.Sprintf(format, a...)), fixSyncUsage)
	return fixSyncBadArgs
}

func runFixSync(c *Context, o fixSyncOpts, args []string) int {
	switch {
	case len(args) > 0:
		return fixSyncBadUsage(c, "unexpected argument %q", args[0])
	case o.from == "" || o.branch == "" || o.base == "":
		return fixSyncBadUsage(c, "--from, --branch and --base are required")
	case !gitx.ShellSafeRef(o.branch) || !gitx.ShellSafeRef(o.base):
		return fixSyncBadUsage(c, "--branch and --base take a branch name")
	}
	ctx, stop := signalContext()
	defer stop()
	r := fixSyncRunner()
	dir, err := gitWorkTree(ctx, r, o.checkout)
	if err != nil {
		return fixSyncBadUsage(c, "%v", err)
	}
	g := gitx.New(r)
	from, err := g.RevParse(ctx, dir, o.from)
	if err != nil {
		return fixSyncBadUsage(c, "--from %s is no commit of the checkout (%v)", o.from, err)
	}
	done, refused := fixSync(ctx, g, dir, from, o.branch, o.base)
	if refused != "" {
		fmt.Fprintln(c.Stdout, execx.Redact("refused: "+refused))
		return fixSyncRefused
	}
	fmt.Fprintln(c.Stdout, done)
	return 0
}

// fixSync replays the commits from..HEAD of the checkout at dir onto the
// tip of origin's branch when every commit the branch gained since from is
// a merge and the PR's own diff against base stayed the same (spec 5.7),
// and says what it did; refused is why it changed nothing instead, "" when
// it replayed.
func fixSync(ctx context.Context, g *gitx.Client, dir, from, branch, base string) (done, refused string) {
	switch op, err := g.InProgress(ctx, dir); {
	case err != nil:
		return "", fmt.Sprintf("could not read the checkout: %v", err)
	case op != "":
		return "", fmt.Sprintf("the checkout is in the middle of a %s: finish or abort it first", op)
	}
	switch st, err := g.Status(ctx, dir); {
	case err != nil:
		return "", fmt.Sprintf("could not read the checkout: %v", err)
	case st.Tracked > 0:
		return "", "the checkout has uncommitted changes to tracked files: commit what belongs to a fix and restore the rest first"
	}
	head, err := g.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return "", fmt.Sprintf("could not read HEAD: %v", err)
	}
	still := "; HEAD is still " + textx.ShortSHA(head)
	own, why := fixSyncOwn(ctx, g, dir, from, head)
	if why != "" {
		return "", why + still
	}
	for _, b := range []string{branch, base} {
		if err := g.FetchBranch(ctx, dir, b); err != nil {
			return "", fmt.Sprintf("could not fetch %s: %v%s", b, err, still)
		}
	}
	tip, err := g.RevParse(ctx, dir, "refs/remotes/origin/"+branch)
	if err != nil {
		return "", fmt.Sprintf("could not read the branch's tip: %v%s", err, still)
	}
	if why := fixSyncGained(ctx, g, dir, from, head, tip, base); why != "" {
		return "", why + still
	}
	paths, err := g.ReplayOnto(ctx, dir, tip, from)
	if err != nil {
		what := fmt.Sprintf("replaying your %s onto %s", textx.Count(len(own), "commit", "commits"), textx.ShortSHA(tip))
		switch {
		case errors.Is(err, gitx.ErrRebaseConflict) && len(paths) > 0:
			what += " conflicts in " + namePaths(paths)
		case errors.Is(err, gitx.ErrRebaseConflict):
			what += " conflicts"
		default:
			what += fmt.Sprintf(" failed: %v", err)
		}
		return "", what + fixSyncLeft(ctx, g, dir, head)
	}
	now, err := g.RevParse(ctx, dir, "HEAD")
	if err != nil {
		return "", fmt.Sprintf("replayed your commits onto %s, but could not read HEAD: %v", textx.ShortSHA(tip), err)
	}
	replayed, err := g.CommitsAfter(ctx, dir, tip, now, false)
	if err != nil {
		return "", fmt.Sprintf("replayed your commits onto %s, but could not count them: %v; HEAD is %s", textx.ShortSHA(tip), err, textx.ShortSHA(now))
	}
	return fmt.Sprintf("replayed %s onto %s; HEAD is %s", textx.Count(len(replayed), "commit", "commits"), textx.ShortSHA(tip),
		textx.ShortSHA(now)), ""
}

// fixSyncOwn lists the fixer's commits, from..head, or says why they
// cannot be replayed (why): head must descend from from with at least one
// commit on top, none of them a merge (a merge the fixer made, the base
// merge of a conflicting PR, would be made again without its resolutions).
func fixSyncOwn(ctx context.Context, g *gitx.Client, dir, from, head string) (own []gitx.ListedCommit, why string) {
	switch ok, err := g.IsAncestor(ctx, dir, from, head); {
	case err != nil:
		return nil, fmt.Sprintf("could not compare HEAD with the start head: %v", err)
	case !ok:
		return nil, fmt.Sprintf("HEAD %s does not descend from the start head %s", textx.ShortSHA(head), textx.ShortSHA(from))
	case head == from:
		return nil, "you have no commit to replay: HEAD is the start head"
	}
	own, err := g.CommitsAfter(ctx, dir, from, head, false)
	if err != nil {
		return nil, fmt.Sprintf("could not list your commits: %v", err)
	}
	for _, c := range own {
		if c.Merge() {
			return nil, fmt.Sprintf("your commits include the merge %s, which fix-sync does not replay", textx.ShortSHA(c.SHA))
		}
	}
	return own, ""
}

// fixSyncGained is why the commits the branch gained since from, up to its
// tip, are no merge of the base alone ("" when they are): the branch moved
// and descends from from, each commit on its first-parent chain since then
// is a merge, the oldest of them on top of from, and the PR's own diff
// against the base (gitx.OwnDiffID, the base as fetched) is the same at
// from and at tip.
func fixSyncGained(ctx context.Context, g *gitx.Client, dir, from, head, tip, base string) string {
	switch ok, err := g.IsAncestor(ctx, dir, tip, head); {
	case tip == from:
		return fmt.Sprintf("the branch is still at the start head %s: GitHub rejected the push for another reason", textx.ShortSHA(from))
	case err != nil:
		return fmt.Sprintf("could not compare the branch's tip with HEAD: %v", err)
	case ok:
		return fmt.Sprintf("HEAD already holds the branch's tip %s: GitHub rejected the push for another reason", textx.ShortSHA(tip))
	}
	switch ok, err := g.IsAncestor(ctx, dir, from, tip); {
	case err != nil:
		return fmt.Sprintf("could not compare the branch's tip with the start head: %v", err)
	case !ok:
		return fmt.Sprintf("the branch was rewritten: its tip %s does not descend from the start head %s", textx.ShortSHA(tip), textx.ShortSHA(from))
	}
	gained, err := g.CommitsAfter(ctx, dir, from, tip, true)
	if err != nil {
		return fmt.Sprintf("could not list the branch's commits: %v", err)
	}
	for _, c := range gained {
		if !c.Merge() {
			return fmt.Sprintf("the branch gained %s, which is not a merge: someone works on the branch", textx.ShortSHA(c.SHA))
		}
	}
	if len(gained) == 0 || gained[len(gained)-1].Parents[0] != from {
		return fmt.Sprintf("the branch's merges do not follow the start head %s on their first parent: someone works on the branch", textx.ShortSHA(from))
	}
	baseTip := "refs/remotes/origin/" + base
	before, err := g.OwnDiffID(ctx, dir, baseTip, from)
	if err != nil {
		return fmt.Sprintf("could not read the PR's own diff against %s at the start head: %v", base, err)
	}
	after, err := g.OwnDiffID(ctx, dir, baseTip, tip)
	if err != nil {
		return fmt.Sprintf("could not read the PR's own diff against %s at the branch's tip: %v", base, err)
	}
	if before != after {
		return fmt.Sprintf("the branch's merges changed the PR's own diff against %s (a conflict resolved, or a change beside the merge): someone works on the branch", base)
	}
	return ""
}

// fixSyncLeft is how a failed replay left the checkout at dir, whose HEAD
// was head: "; HEAD is still <head>" when the replay was undone, else what
// is in the way.
func fixSyncLeft(ctx context.Context, g *gitx.Client, dir, head string) string {
	ctx = context.WithoutCancel(ctx)
	op, err := g.InProgress(ctx, dir)
	if err == nil && op != "" {
		return fmt.Sprintf("; a %s is still in progress: run `git %s --abort`", op, op)
	}
	now, err := g.RevParse(ctx, dir, "HEAD")
	switch {
	case err != nil:
		return fmt.Sprintf("; HEAD cannot be read (%v): it was %s", err, textx.ShortSHA(head))
	case now != head:
		return fmt.Sprintf("; HEAD moved to %s: it was %s", textx.ShortSHA(now), textx.ShortSHA(head))
	}
	return "; HEAD is still " + textx.ShortSHA(head)
}

// namePaths names at most maxSyncPaths of paths and counts the rest.
func namePaths(paths []string) string {
	if len(paths) <= maxSyncPaths {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:maxSyncPaths], ", ") + fmt.Sprintf(" and %d more", len(paths)-maxSyncPaths)
}
