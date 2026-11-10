package cli

// `magnum comment`: what a chat session writes on a pull request goes out
// as the watch's fixer GitHub App (postreview.RunComment), never as the
// operator's login: a comment, a reply in a review thread, a new title,
// review requests. It mints the App's token as the daemon does
// (app.NewIdentity, identity.App.Env) and records each post in the
// registry (comment.posted).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// commentUsage names no stdin redirect: the comment comes on stdin, as the
// help says (a "<" would read as a required argument).
const commentUsage = "comment <owner/repo#N|PR URL> [--reply-to ID] [--request-review LOGIN]... [--title TEXT] [--dry-run]"

// commentPostedKind is the event of each post (store.Event.Kind).
const commentPostedKind = "comment.posted"

// commentPaneEnv are the variables of the panes magnum comment refuses to
// run in: the review agents' and the staging resolver's (agentPaneEnv) and
// the factory's agents' (MAGNUM_FACTORY_TASK, factory/runtime's paneEnv).
// Their agents read a PR's or a ticket's text, which may steer them; the
// fixer posts with fix-reply and the judge with post-review.
var commentPaneEnv = append(append([]string{}, agentPaneEnv...), "MAGNUM_FACTORY_TASK")

// commentRunner runs gh for magnum comment, commentStdin is where it reads
// the comment, commentStdinTTY says whether that is a terminal, and
// commentOpenStore opens the registry (nil: there is none yet); tests
// replace them.
var (
	commentRunner    = func() execx.Runner { return &execx.Real{} }
	commentStdin     = func() io.Reader { return os.Stdin }
	commentStdinTTY  = func() bool { return app.Interactive(os.Stdin) }
	commentOpenStore = func(c *Context) (*store.Store, error) {
		if !fsx.Exists(c.Layout.DB()) {
			return nil, nil
		}
		return store.OpenWith(c.Layout.DB(), store.Options{BeforeMigrate: migrateGuard(c.Layout)})
	}
)

const commentLong = "Post on a pull request of a watched repository as the watch's fixer GitHub App (its fix_identity, " +
	"for example fixer-app[bot]), never as your own login or whatever gh is logged in as: for chat sessions and " +
	"scripts that write on GitHub for you. The comment comes on standard input (a pipe or a heredoc). Without flags " +
	"it is a comment on the pull request's conversation; --reply-to ID replies in the review thread of that review " +
	"comment (any comment of the thread), after checking that it is one of this pull request's; --title TEXT changes " +
	"the pull request's title; --request-review LOGIN (repeatable) asks that person for a review, again if they " +
	"reviewed already. With only --title or --request-review the comment may be left out. The pull request is " +
	"owner/repo#N or its URL.\n\n" +
	"magnum mints the App's installation token as it does for the fixer's panes, narrowed to the identity's " +
	"token_repositories, and writes it only to the App's own gh config (hosts.yml in gh/<identity> under magnum's " +
	"state dir, mode 0600, the one the fixer's panes read), even on --dry-run: it never reaches the command line, " +
	"stdout, stderr, the events or the registry. It refuses a repository magnum does not watch, a " +
	"watch without fix_identity, a fix_identity that is your gh identity or the App magnum reviews as, and a " +
	"repository outside the App's token_repositories, each naming the key to set; a comment that is empty, carries " +
	"a <!-- magnum: marker, names a local path or is longer than GitHub takes; a reply to a comment that is not this " +
	"pull request's, or in a thread magnum's review started (answer those yourself, or let the fixer); a title that " +
	"is empty, more than one line or longer than 256 characters; a login that is no person's (an App, a team, an @) " +
	"or the pull request's author; and a review agent's pane or a factory agent's.\n\n" +
	"It appends a blank line and <!-- magnum:comment -->, by which magnum knows the comment as its own: never your " +
	"word, never a reply that starts a review round or a fix loop; a reviewer's answer after it counts as usual. To " +
	"answer a review body or a comment on the conversation, start with a quote of its first line (\"> …\"), put the " +
	"remark's own link alone on the next line (its URL, ending in #pullrequestreview-N or #issuecomment-N, and " +
	"nothing else on that line), then a blank line and the answer: magnum then counts it answered. The same comment " +
	"already there with the marker and nobody else's comment after it (on the conversation, nobody else's review " +
	"either) gets nothing more, a title already so is not sent again and a login already asked is skipped, so after " +
	"an unclear failure run the same line with the same text again. Each post is recorded in magnum's registry " +
	"(comment.posted: the pull request, the App, the kind of post and its link, never the text). --dry-run checks " +
	"everything as the App, prints what it would post and as whom, and posts nothing.\n\n" +
	"It prints one JSON object on stdout (status: posted, already_posted, dry_run, invalid or error, with each part " +
	"and the problems) and one line on stderr. Exit 0: done (or, dry run, checked); 2: fix what the problems name " +
	"(the comment, the line or the config) and run it again, and when GitHub shows the comment by another account " +
	"than the App, do what the problem says; 1: anything else, it could not confirm everything went out: run the " +
	"same line with the same text again."

func newCommentCmd(c *Context) *cobra.Command {
	var replyTo int64
	var reviewers []string
	var title string
	var dryRun bool
	var cmd *cobra.Command
	cmd = newCommand(groupAct, commentUsage,
		"post a comment, a reply, a title or review requests on a PR as the watch's fixer GitHub App, never as you (for chat sessions)",
		commentLong,
		func(pos []string) int {
			o := commentFlags{replyTo: replyTo, reviewers: reviewers, dryRun: dryRun}
			if cmd.Flags().Changed("title") {
				o.title = &title
			}
			return runComment(c, o, pos)
		})
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		// A wrong line: exit 2, never 1, which asks to run the same line again.
		problem := err.Error()
		if strings.Contains(problem, `"--reply-to" flag`) {
			problem += "; --reply-to takes the number id of a review comment (its databaseId), not a thread's node id"
		}
		return exitCode(commentInvalid(c, postreview.CommentOutcome{}, problem))
	})
	fs := cmd.Flags()
	fs.Int64Var(&replyTo, "reply-to", 0, "reply in the review thread of this review comment (any comment of the thread)")
	fs.StringArrayVar(&reviewers, "request-review", nil, "ask this person (a GitHub login) for a review; repeatable")
	fs.StringVar(&title, "title", "", "change the pull request's title to this")
	fs.BoolVar(&dryRun, "dry-run", false, "check everything as the App and print what it would do; post nothing")
	return cmd
}

// commentFlags are magnum comment's flags (title nil: not given).
type commentFlags struct {
	replyTo   int64
	reviewers []string
	title     *string
	dryRun    bool
}

// needsBody reports whether the line posts a comment whatever else it
// does: no flag, or --reply-to.
func (o commentFlags) needsBody() bool {
	return o.replyTo != 0 || (o.title == nil && len(o.reviewers) == 0)
}

func runComment(c *Context, f commentFlags, pos []string) int {
	if len(pos) != 1 {
		return commentInvalid(c, postreview.CommentOutcome{}, "give one pull request: owner/repo#N or its URL (usage: magnum "+commentUsage+")")
	}
	for _, k := range commentPaneEnv {
		if os.Getenv(k) != "" {
			return commentInvalid(c, postreview.CommentOutcome{}, fmt.Sprintf("run magnum comment from a chat session or your own terminal: "+
				"a review or factory agent's pane cannot post as the fixer App (%s is set); the fixer posts with magnum fix-reply", k))
		}
	}
	owner, repo, number, err := github.ParseRef(pos[0], "")
	if err != nil {
		return commentInvalid(c, postreview.CommentOutcome{}, fmt.Sprintf("%q names no pull request: give it as owner/repo#N or its URL", pos[0]))
	}
	named := postreview.CommentOutcome{PR: fmt.Sprintf("%s/%s#%d", owner, repo, number)}
	if err := c.LoadConfig(); err != nil {
		named.Status, named.Message = postreview.StatusError, "load the config: "+err.Error()
		return commentPrint(c, named)
	}
	full := owner + "/" + repo
	_, id, err := c.Config.CommentIdentity(full)
	if err != nil {
		return commentInvalid(c, named, err.Error())
	}
	login := id.Login
	o := postreview.CommentOptions{Owner: owner, Repo: repo, Number: number, ReplyTo: f.replyTo, RequestReviews: f.reviewers, Title: f.title,
		Login: login, Identity: id.Name, MagnumLogins: c.Config.MagnumThreadLogins(), DryRun: f.dryRun}
	base, ok := o.Check()
	if !ok {
		return commentPrint(c, base)
	}
	fail := func(format string, args ...any) int {
		out := base
		out.Status, out.Message = postreview.StatusError, fmt.Sprintf(format, args...)
		return commentPrint(c, out)
	}

	var text []byte
	if f.needsBody() || !commentStdinTTY() {
		if commentStdinTTY() {
			return commentInvalid(c, base, "pass the comment on standard input (a pipe or a heredoc)")
		}
		if text, err = io.ReadAll(io.LimitReader(commentStdin(), maxFixReplyRead)); err != nil {
			return fail("read the comment from standard input: %v", err)
		}
	}
	if out, ok := o.CheckText(text); !ok {
		return commentPrint(c, out)
	}

	st, err := commentOpenStore(c)
	if err != nil {
		return fail("open magnum's registry: %v", err)
	}
	if st != nil {
		defer st.Close()
	}
	ctx, stop := signalContext()
	defer stop()
	subject := "pr:" + full + fmt.Sprintf("#%d", number)
	if st != nil {
		r, err := st.RepoByFullName(ctx, full)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return fail("read magnum's registry: %v", err)
		default:
			// The registry's casing, as the PR's other events carry it.
			o.Owner, o.Repo, full = r.Owner, r.Name, r.FullName()
			base.PR = fmt.Sprintf("%s#%d", full, number)
			subject = fmt.Sprintf("pr:%s#%d", full, number)
			p, err := st.PRByRepoNumber(ctx, r.ID, number)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return fail("read magnum's registry: %v", err)
			default:
				// The PR's own logins, as the poll and the threads file
				// take them for magnum's: its identity, its former ones,
				// its last reviewer.
				former, _, err := st.GetKV(ctx, engine.KVPRFormerIdentities(p.ID))
				if err != nil {
					return fail("read magnum's registry: %v", err)
				}
				o.MagnumLogins = c.Config.PRThreadLogins(p.Identity, engine.FormerIdentities(former))
				if p.LastReviewLogin != nil && *p.LastReviewLogin != "" {
					o.MagnumLogins = append(o.MagnumLogins, *p.LastReviewLogin)
				}
			}
		}
	}

	src, gh := app.NewIdentity(c.Config, c.Layout, commentRunner(), *id)
	env, err := src.Env(ctx)
	if err != nil {
		return fail("mint a token of the App %s (identity %s); nothing posted: %v", login, id.Name, err)
	}
	gh.Env = env

	rec := commentRecorder{st: st, subject: subject, full: full, number: number, identity: id.Name, login: login}
	out := postreview.RunComment(ctx, gh, o, text, func(p postreview.Posted) { rec.record(ctx, p) })
	out.Event = rec.note()
	return commentPrint(c, out)
}

// commentRecorder writes each post of magnum comment as an event of the
// PR in the registry; err is the first write that failed.
type commentRecorder struct {
	st              *store.Store
	subject, full   string
	number          int
	identity, login string
	err             error
	missing         bool // no registry: nothing recorded
}

// record writes p's event: the post's kind and link, never its text. A
// comment's goes out once (store.AppendEventOnce, by its id), so a retry
// that finds the comment records it if the first run could not.
func (r *commentRecorder) record(ctx context.Context, p postreview.Posted) {
	if r.st == nil {
		r.missing = true
		return
	}
	data := map[string]any{"repo": r.full, "number": r.number, "identity": r.identity, "login": r.login, "post": p.Kind, "url": p.URL}
	if p.CommentID != 0 {
		data["comment_id"] = p.CommentID
	}
	if p.Thread != 0 {
		data["thread"] = p.Thread
	}
	if len(p.Reviewers) > 0 {
		data["reviewers"] = p.Reviewers
	}
	level, as := "info", "as "+r.login
	if p.Author != "" {
		// GitHub shows the comment by someone else: the warning names whom.
		data["author"] = p.Author
		level, as = "warn", fmt.Sprintf("as %s, but GitHub shows it by %s", r.login, p.Author)
	}
	b, err := json.Marshal(data)
	if err != nil {
		r.err = cmp.Or(r.err, err)
		return
	}
	what := map[string]string{postreview.PostConversation: "a comment on the conversation", postreview.PostReply: "a reply in a review thread",
		postreview.PostTitle: "a new title", postreview.PostReviewRequest: "a review request of " + strings.Join(p.Reviewers, ", ")}[p.Kind]
	ev := store.Event{Level: level, Subject: &r.subject, Kind: commentPostedKind, Message: fmt.Sprintf("magnum comment posted %s %s: %s", what, as, p.URL), Data: b}
	if p.CommentID != 0 {
		_, err = r.st.AppendEventOnce(ctx, ev, fmt.Sprintf("comment:%d", p.CommentID))
	} else {
		_, err = r.st.AppendEvent(ctx, ev)
	}
	if err != nil {
		r.err = cmp.Or(r.err, err)
	}
}

// note is the outcome's event note: "" when every post was recorded.
func (r *commentRecorder) note() string {
	switch {
	case r.err != nil:
		return "not recorded: " + execx.Redact(r.err.Error()) + ": run it again to record it"
	case r.missing:
		return "not recorded: magnum has no registry yet"
	}
	return ""
}

// commentPrint prints out as JSON on stdout and its summary on stderr, and
// returns its exit code.
func commentPrint(c *Context, out postreview.CommentOutcome) int {
	out.Message = execx.Redact(out.Message)
	_ = writeJSON(c.Stdout, out)
	line := out.Summary()
	if out.Event != "" {
		line += "; the registry: " + out.Event
	}
	fmt.Fprintln(c.Stderr, "magnum comment:", execx.Redact(line))
	return out.ExitCode()
}

// commentInvalid prints the refusal of a line the caller got wrong as
// invalid, exit 2: exit 1 would ask to run the same line once more.
func commentInvalid(c *Context, out postreview.CommentOutcome, problem string) int {
	out.Status, out.Problems = postreview.StatusInvalid, []string{execx.Redact(problem)}
	return commentPrint(c, out)
}
