package cli

// `magnum post-review`: the judge's posting tool (internal/postreview). The
// judge runs the line its <magnum> block names (post_review); the command
// needs nothing but its flags, gh and git in the checkout.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/postreview"
)

const postReviewUsage = "post-review --repo OWNER/NAME --pr N --head SHA --run-id ID --login LOGIN [--former-login LOGIN]... " +
	"[--gh-config-dir DIR] [--dry-run] [--local-base SHA] (--review FILE | --replies FILE)"

// postReviewRunner runs gh and git for post-review; tests replace it.
var postReviewRunner = func() execx.Runner { return &execx.Real{} }

type postReviewOpts struct {
	repo, head, runID, login, ghDir, localBase, review, replies string
	number                                                      int
	former                                                      []string
	dryRun                                                      bool
}

func newPostReviewCmd(c *Context) *cobra.Command {
	var o postReviewOpts
	cmd := newCommand(groupAct, postReviewUsage, "post the judge's review, or its replies in the review threads: check, post once, read back (for the judge)",
		"For the judge's pane: magnum's judge runs the post_review line of its <magnum> block, with every flag filled in, "+
			"after writing its review to the --review file as JSON: {\"event\":\"COMMENT|REQUEST_CHANGES|APPROVE\",\"body\":\"…\","+
			"\"comments\":[{\"path\":\"app/x.rb\",\"line\":42,\"side\":\"RIGHT\",\"start_line\":40,\"start_side\":\"RIGHT\",\"body\":\"…\"}]}. "+
			"Give exactly one of --review and --replies.\n\n"+
			"It checks the file (the event, the bodies and their length, sides, start_line before line; no magnum footer) and appends "+
			"the run's marker to the body when it lacks it; checks every inline comment's lines against the pull request's diff "+
			"as GitHub shows it (a file without a patch against git diff in the current directory; a line nothing can check is "+
			"kept); looks for a review by --login (or a --former-login) that already carries the marker and then posts nothing; "+
			"else posts the review once on --head (JSON on gh's stdin), retrying a refused self-verdict as COMMENT, and reads it "+
			"back. --dry-run posts nothing and prints the planned review; --local-base (a blind replay, with --dry-run) checks the "+
			"lines against git diff <SHA> <head> and reads nothing from GitHub. --gh-config-dir is the identity's gh config "+
			"(empty: your own gh login).\n\n"+
			"--replies is the replies-only mode, for a round whose head is unchanged since magnum's review and where the authors "+
			"answered its threads: instead of a new review the judge answers in the threads, writing the --replies file as JSON: "+
			"{\"replies\":[{\"comment_id\":123,\"kind\":\"ack|rebuttal|answer\",\"body\":\"…\"}]} (an empty list is valid). "+
			"comment_id is the thread's first comment (threads_file's comment_id); the kind is ack (a reason the judge accepts), "+
			"rebuttal (it keeps the finding) or answer (to a question); one reply per thread. It checks the file (the kinds, "+
			"non-empty bodies within GitHub's limit, one reply per comment_id, no magnum marker of your own), reads the review "+
			"threads once and checks that each comment_id starts a thread of --login (or a --former-login) and that magnum has "+
			"not already rebutted twice there (a rebuttal carrying the marker counts, and so does an own reply with no marker "+
			"at all; after two it stops arguing in the thread and the operator takes over, so any new reply there is refused). "+
			"Then it posts each reply on its thread as --login, in the file's order, its text followed by a blank line and the "+
			"hidden marker <!-- magnum:reply run=<run id> kind=<kind> -->, which the daemon verifies; a thread that already "+
			"holds a reply carrying this run's marker gets nothing more, so after an error run it again unchanged and only the "+
			"replies still missing go out. --dry-run posts nothing and prints the plan; --local-base is for a review's dry run "+
			"only and refused with --replies.\n\n"+
			"It prints one JSON object on stdout (status: posted, already_posted, replied, dry_run, invalid, invalid_anchors, "+
			"rejected, error or readback_mismatch, and its details; replies lists each reply posted, already posted or planned) "+
			"and one line on stderr. Exit 0: posted, already posted, replied or a dry run; 2: fix the review or replies file "+
			"(the comments off the diff list the file's valid line ranges) and run it again; 1: anything else. It reads no "+
			"config and no registry, never talks to the daemon and writes no file.",
		func(pos []string) int { return runPostReview(c, o, pos) })
	// The command needs no magnum layout: it runs wherever the judge's pane is.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	// A flag error is magnum's line, not the judge's file: status error, exit 1.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitCode(postReviewPrint(c, postreview.Outcome{Status: postreview.StatusError, Message: err.Error()}))
	})
	fs := cmd.Flags()
	fs.StringVar(&o.repo, "repo", "", "the pull request's repository, owner/name")
	fs.IntVar(&o.number, "pr", 0, "the pull request number")
	fs.StringVar(&o.head, "head", "", "the reviewed commit (head_sha): the review's commit")
	fs.StringVar(&o.runID, "run-id", "", "the round's run id (run_id): the marker the review carries")
	fs.StringVar(&o.login, "login", "", "the login the review must appear under (reviewer_login)")
	fs.StringArrayVar(&o.former, "former-login", nil, "a login this PR's reviews were posted as before (repeatable)")
	fs.StringVar(&o.ghDir, "gh-config-dir", "", "the identity's gh config directory (gh_config_dir; empty: your gh login)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "check everything, post nothing, print the planned review")
	fs.StringVar(&o.localBase, "local-base", "", "with --dry-run: check the lines against git diff <SHA> <head>; read nothing from GitHub")
	fs.StringVar(&o.review, "review", "", "the review file the judge wrote (JSON)")
	fs.StringVar(&o.replies, "replies", "", "the replies file the judge wrote (JSON): answer in the review threads instead of posting a review")
	_ = cmd.MarkFlagFilename("review", "json")
	_ = cmd.MarkFlagFilename("replies", "json")
	_ = cmd.MarkFlagDirname("gh-config-dir")
	return cmd
}

func runPostReview(c *Context, o postReviewOpts, pos []string) int {
	fail := func(format string, args ...any) int {
		return postReviewPrint(c, postreview.Outcome{Status: postreview.StatusError, Message: fmt.Sprintf(format, args...)})
	}
	if len(pos) > 0 {
		return fail("unexpected arguments: %s", strings.Join(pos, " "))
	}
	switch {
	case o.review != "" && o.replies != "":
		return fail("--review and --replies are exclusive: give exactly one")
	case o.review == "" && o.replies == "":
		return fail("give exactly one of --review (the review file) and --replies (the replies file)")
	case o.replies != "" && o.localBase != "":
		return fail("--local-base is for a review's dry run (--review) only")
	}
	owner, name, ok := strings.Cut(o.repo, "/")
	if !ok {
		return fail("--repo %q is not owner/name", o.repo)
	}
	opts := postreview.Options{Owner: owner, Repo: name, Number: o.number, HeadSHA: o.head, RunID: o.runID,
		Login: o.login, FormerLogins: o.former, DryRun: o.dryRun, LocalBase: o.localBase}
	if err := opts.Check(); err != nil {
		return fail("%v", err)
	}
	file, what := o.review, "review"
	if o.replies != "" {
		file, what = o.replies, "replies"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		// The judge did not write its file (or wrote it elsewhere): its input to fix.
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("no %s file at %s", what, file)
		}
		return postReviewPrint(c, postreview.Outcome{Status: postreview.StatusInvalid, Problems: []string{err.Error()}})
	}
	run := postReviewRunner()
	gh := &github.Client{Run: run}
	if o.ghDir != "" {
		gh.Env = map[string]string{"GH_CONFIG_DIR": o.ghDir}
	}
	ctx, stop := signalContext()
	defer stop()
	deps := postreview.Deps{GitHub: gh, Git: gitx.New(run), Dir: "."}
	if o.replies != "" {
		return postReviewPrint(c, postreview.RunReplies(ctx, deps, opts, data))
	}
	return postReviewPrint(c, postreview.Run(ctx, deps, opts, data))
}

// postReviewPrint prints out as JSON on stdout and its summary on stderr,
// and returns its exit code.
func postReviewPrint(c *Context, out postreview.Outcome) int {
	out.Message = execx.Redact(out.Message)
	_ = writeJSON(c.Stdout, out)
	fmt.Fprintln(c.Stderr, "magnum post-review:", execx.Redact(out.Summary()))
	return out.ExitCode()
}
