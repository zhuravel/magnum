package cli

// `magnum fix-reply`: the fixer's posting tool (postreview.RunFixReply). The
// fix prompt names the line (agents.FixReplyLine); the command needs
// nothing but its flags, the pane's MAGNUM_PR_URL, the reply on stdin and
// gh.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
)

// fixReplyUsage names no stdin redirect: the reply comes on stdin, as the
// short help says (a "<" would read as a required argument).
const fixReplyUsage = "fix-reply (--comment ID | --issue)"

// fixReplyRunner runs gh for fix-reply, and fixReplyStdin is where it reads
// the reply; tests replace both.
var (
	fixReplyRunner = func() execx.Runner { return &execx.Real{} }
	fixReplyStdin  = func() io.Reader { return os.Stdin }
)

// maxFixReplyRead bounds what fix-reply reads from stdin: GitHub's limit in
// characters at four bytes each, and one byte more to tell a longer reply.
const maxFixReplyRead = 4*postreview.MaxBody + 1

func newFixReplyCmd(c *Context) *cobra.Command {
	var comment int64
	var issue bool
	cmd := newCommand(groupAct, fixReplyUsage, "post one reply of the fixer, read from stdin, signed with the fix marker (for the fixer)",
		"For the fixer's pane: the fix prompt names the line, and the fixer posts every reply through it, the reply on "+
			"standard input. It reads the pull request from the pane's MAGNUM_PR_URL and posts as the pane's gh login, the "+
			"PR's author. With --comment it replies in the thread of that review comment (any comment of the thread; the "+
			"reply goes to its first comment), after checking that the comment is one of this pull request's; with --issue "+
			"it writes a comment on the pull request's conversation.\n\n"+
			"It refuses a reply that is empty, carries a <!-- magnum: marker of its own, names a local path or is longer "+
			"than GitHub takes, and appends a blank line and the fix marker <!-- magnum:fix -->, by which magnum tells the "+
			"fixer's replies from the author's own. A thread, or the conversation, that already holds the same reply with the "+
			"marker and nobody else's comment after it gets nothing more, so after an unclear failure run it again with the "+
			"same reply; the same answer after someone else's comment (new pushback) goes out.\n\n"+
			"It prints one JSON object on stdout (status: replied, already_posted, invalid or error, with the thread, the "+
			"reply's id and url, or the problems) and one line on stderr. Exit 0: the reply is on GitHub; 2: fix the reply "+
			"(or the comment id) and run it again; 1: anything else, the reply could not be confirmed (it may have gone out "+
			"with its answer lost): run it again with the same reply. It reads no config and no registry, never talks to the "+
			"daemon and writes no file.",
		func(pos []string) int { return runFixReply(c, comment, issue, pos) })
	// The command needs no magnum layout: it runs wherever the fixer's pane is.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitCode(fixReplyPrint(c, postreview.FixReplyOutcome{Status: postreview.StatusError, Message: err.Error()}))
	})
	fs := cmd.Flags()
	fs.Int64Var(&comment, "comment", 0, "the review comment whose thread the reply answers")
	fs.BoolVar(&issue, "issue", false, "write the reply as a comment on the pull request's conversation")
	return cmd
}

func runFixReply(c *Context, comment int64, issue bool, pos []string) int {
	fail := func(format string, args ...any) int {
		return fixReplyPrint(c, postreview.FixReplyOutcome{Status: postreview.StatusError, Message: fmt.Sprintf(format, args...)})
	}
	if len(pos) > 0 {
		return fail("unexpected arguments: %s", strings.Join(pos, " "))
	}
	url := os.Getenv("MAGNUM_PR_URL")
	if url == "" {
		return fail("MAGNUM_PR_URL is not set: fix-reply runs in the fixer's pane, which magnum starts with the PR's URL")
	}
	owner, repo, number, err := github.ParseRef(url, "")
	if err != nil {
		return fail("MAGNUM_PR_URL %q names no pull request: %v", url, err)
	}
	o := postreview.FixReplyOptions{Owner: owner, Repo: repo, Number: number, CommentID: comment, Issue: issue}
	if err := o.Check(); err != nil {
		return fail("%v", err)
	}
	text, err := io.ReadAll(io.LimitReader(fixReplyStdin(), maxFixReplyRead))
	if err != nil {
		return fail("read the reply from standard input: %v", err)
	}
	ctx, stop := signalContext()
	defer stop()
	return fixReplyPrint(c, postreview.RunFixReply(ctx, &github.Client{Run: fixReplyRunner()}, o, text))
}

// fixReplyPrint prints out as JSON on stdout and its summary on stderr, and
// returns its exit code.
func fixReplyPrint(c *Context, out postreview.FixReplyOutcome) int {
	out.Message = execx.Redact(out.Message)
	_ = writeJSON(c.Stdout, out)
	fmt.Fprintln(c.Stderr, "magnum fix-reply:", execx.Redact(out.Summary()))
	return out.ExitCode()
}
