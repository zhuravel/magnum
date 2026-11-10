package postreview

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// CommentMarker ends every comment `magnum comment` posts: a chat
// session's comment, posted as the watch's fixer App. By it magnum tells
// these comments from the author's replies and from the fixer's
// (FixReplyMarker): they are magnum's own word, never the author's, and
// never a reply that starts a review round or a fix loop. As with the fix
// marker, it means that only in a comment of one of magnum's logins.
const CommentMarker = "<!-- magnum:comment -->"

// IsComment reports whether body carries CommentMarker.
func IsComment(body string) bool { return strings.Contains(body, CommentMarker) }

// IsOwnAnswer reports whether body is an answer of magnum's side to
// someone else's comment: a fixer's reply or a magnum comment. A reader
// asks it only of a comment by one of magnum's logins (or, for a fix
// reply, the author's, whom a gh fixer posts as).
func IsOwnAnswer(body string) bool { return IsFixReply(body) || IsComment(body) }

// The kinds of post magnum comment makes (Posted.Kind, CommentPart.Kind).
const (
	PostConversation  = "conversation"   // a comment on the PR's conversation
	PostReply         = "reply"          // a reply in a review thread
	PostTitle         = "title"          // a new title
	PostReviewRequest = "review_request" // a review request
)

// maxTitle is the most characters GitHub takes in a pull request's title.
const maxTitle = 256

// CommentOptions name what `magnum comment` does on a pull request: a
// comment on the conversation, or with ReplyTo a reply in the review
// thread of that comment (any comment of it); a new Title; review requests
// of RequestReviews (users' logins). Login is the App it posts as (Account
// form), the one GitHub must show as the author, and Identity the name of
// its [[identity]]; MagnumLogins are the logins whose threads magnum's
// review started (config.Config.MagnumThreadLogins, or for a PR in the
// registry PRThreadLogins, and the PR's last reviewer), where it refuses
// to reply. DryRun reads and checks
// everything and posts nothing.
type CommentOptions struct {
	Owner, Repo    string
	Number         int
	ReplyTo        int64
	RequestReviews []string
	Title          *string
	Login          string
	Identity       string
	MagnumLogins   []string
	DryRun         bool
}

// needsBody reports whether o posts a comment whatever it is given: on the
// conversation (no flag) or as a reply. With only a title or review
// requests a comment is optional.
func (o CommentOptions) needsBody() bool {
	return o.ReplyTo != 0 || (o.Title == nil && len(o.RequestReviews) == 0)
}

// Check is the outcome RunComment refuses o with (ok false) before it
// reads anything: what the line got wrong (the pull request, the comment
// id, the title, a login) is StatusInvalid, exit 2, the caller's to fix;
// no App login is the caller's own error.
func (o CommentOptions) Check() (out CommentOutcome, ok bool) {
	out = o.outcome()
	if o.Login == "" {
		out.Status, out.Message = StatusError, "no App login to post as"
		return out, false
	}
	var problems []string
	switch {
	case !ownerRe.MatchString(o.Owner) || !repoRe.MatchString(o.Repo) || o.Repo == "." || o.Repo == "..":
		problems = append(problems, fmt.Sprintf("the repository %q is not owner/name", o.Owner+"/"+o.Repo))
	case o.Number < 1:
		problems = append(problems, fmt.Sprintf("the pull request number %d is not 1 or more", o.Number))
	}
	if o.ReplyTo < 0 {
		problems = append(problems, fmt.Sprintf("--reply-to %d is not a comment id (1 or more)", o.ReplyTo))
	}
	if o.Title != nil {
		problems = append(problems, titleProblems(strings.TrimSpace(*o.Title))...)
	}
	for _, l := range o.RequestReviews {
		if !github.ValidLogin(strings.TrimSpace(l)) {
			problems = append(problems, fmt.Sprintf("--request-review %q is no person's login: GitHub asks people for a review by their login "+
				"(no @, no [bot], no team)", l))
		}
	}
	if len(problems) > 0 {
		out.Status, out.Problems = StatusInvalid, problems
		return out, false
	}
	return out, true
}

// CheckText is Check with text, the comment read from stdin, checked
// too: one is needed for the conversation and for a reply, and it follows
// fix-reply's rules (not empty, no magnum marker, no local path, not too
// long once marked). The caller asks it before it mints a token.
func (o CommentOptions) CheckText(text []byte) (out CommentOutcome, ok bool) {
	if out, ok = o.Check(); !ok {
		return out, false
	}
	var problems []string
	switch hasText := strings.TrimSpace(string(text)) != ""; {
	case o.needsBody() && !hasText:
		problems = []string{"the comment is empty: pass it on standard input (a pipe or a heredoc)"}
	case hasText:
		problems = checkSigned(string(text), CommentMarker, "magnum comment", "the comment", "the comment marker")
	}
	if len(problems) > 0 {
		out.Status, out.Problems = StatusInvalid, problems
		return out, false
	}
	return out, true
}

// titleProblems is what is wrong with title, trimmed, as a pull request's
// new title (nil: none).
func titleProblems(title string) []string {
	switch {
	case title == "":
		return []string{"the title is empty: give --title the new title"}
	case strings.ContainsAny(title, "\r\n"):
		return []string{"the title is more than one line"}
	case utf8.RuneCountInString(title) > maxTitle:
		return []string{fmt.Sprintf("the title is %d characters; GitHub takes at most %d", utf8.RuneCountInString(title), maxTitle)}
	case strings.Contains(title, anyMarker):
		return []string{fmt.Sprintf("the title carries a %s… marker: leave it out", anyMarker)}
	case localPath(title) != "":
		return []string{strings.Replace(localPathProblem(title), "body", "the title", 1)}
	}
	return nil
}

// outcome is the outcome of o with nothing done yet.
func (o CommentOptions) outcome() CommentOutcome {
	return CommentOutcome{PR: fmt.Sprintf("%s/%s#%d", o.Owner, o.Repo, o.Number), Identity: o.Identity, As: o.Login}
}

// CommentGitHub is what RunComment reads and writes on GitHub
// (*github.Client, as the watch's fixer App).
type CommentGitHub interface {
	FixReplyGitHub
	RecentReviews(ctx context.Context, owner, repo string, number int, stop func(github.Review) bool) ([]github.Review, bool, error)
	Pull(ctx context.Context, owner, repo string, number int) (github.Pull, error)
	RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error
	UpdatePullTitle(ctx context.Context, owner, repo string, number int, title string) (github.Pull, error)
}

// Posted is one post RunComment made, or (Already) a comment it found
// already there from an earlier run: what the caller records.
type Posted struct {
	Kind      string   // PostConversation, PostReply, PostTitle or PostReviewRequest
	CommentID int64    // the comment's (PostConversation, PostReply)
	Thread    int64    // the first comment of the reply's thread (PostReply)
	URL       string   // the comment's link; the pull request's for a title or a review request
	Reviewers []string // the logins asked (PostReviewRequest)
	Already   bool     // the comment was there already: nothing posted now
	// Author is who GitHub shows wrote the comment when that is not the
	// App (Account form; "an account it does not name" when it named
	// none): a wrong login in the config, or a post under a person's
	// login. "" when it is the App.
	Author string
}

// CommentPart is the comment of an outcome: on the conversation or a
// reply in the thread of Thread (its first comment), its id and link,
// posted, found already there, or (dry run) planned.
type CommentPart struct {
	Kind    string `json:"kind"`
	Thread  int64  `json:"thread,omitempty"`
	ID      int64  `json:"id,omitempty"`
	URL     string `json:"url,omitempty"`
	Already bool   `json:"already"`
	Planned bool   `json:"planned,omitempty"`
	// Author is who GitHub shows wrote the comment posted when that is not
	// the App (Posted.Author); "" when it is.
	Author string `json:"author,omitempty"`
}

// TitlePart is the title of an outcome: changed, already so, or planned.
type TitlePart struct {
	Changed bool `json:"changed"`
	Already bool `json:"already"`
	Planned bool `json:"planned,omitempty"`
}

// ReviewRequestPart is the review requests of an outcome: the logins
// asked (or, planned, to ask) and the ones asked already, whom it skipped.
type ReviewRequestPart struct {
	Requested []string `json:"requested"`
	Already   []string `json:"already"`
	Planned   bool     `json:"planned,omitempty"`
}

// CommentOutcome is what RunComment printed: a status (StatusPosted,
// StatusAlreadyPosted, StatusDryRun, StatusInvalid or StatusError), the
// pull request, the identity and the login it posts as, and each part
// done, found done or planned. Event is the CLI's note of a registry
// record it could not write.
type CommentOutcome struct {
	Status         string             `json:"status"`
	Message        string             `json:"message,omitempty"`
	Problems       []string           `json:"problems,omitempty"`
	PR             string             `json:"pr"`
	Identity       string             `json:"identity,omitempty"`
	As             string             `json:"as,omitempty"`
	Comment        *CommentPart       `json:"comment,omitempty"`
	Title          *TitlePart         `json:"title,omitempty"`
	ReviewRequests *ReviewRequestPart `json:"review_requests,omitempty"`
	Event          string             `json:"event,omitempty"`
}

// ExitCode is 0 when everything is on GitHub (posted, already there) or a
// dry run checked it, 2 when the comment or the line must change, and 1
// otherwise: something could not be confirmed, so run the same line with
// the same text again.
func (o CommentOutcome) ExitCode() int {
	switch o.Status {
	case StatusPosted, StatusAlreadyPosted, StatusDryRun:
		return 0
	case StatusInvalid:
		return 2
	}
	return 1
}

// parts says each part of o in a few words (ids and links, no text).
func (o CommentOutcome) parts() []string {
	var out []string
	if c := o.Comment; c != nil {
		where, there := "a comment on the conversation", "the comment is already on the conversation"
		if c.Kind == PostReply {
			where, there = fmt.Sprintf("a reply in the thread of comment %d", c.Thread), fmt.Sprintf("the reply is already in the thread of comment %d", c.Thread)
		}
		switch {
		case c.Already:
			out = append(out, there+": "+c.URL)
		case c.Planned:
			out = append(out, "would post "+where)
		case c.ID != 0 && c.Author != "":
			out = append(out, "posted "+where+", which GitHub shows by "+c.Author+": "+c.URL)
		case c.ID != 0:
			out = append(out, "posted "+where+": "+c.URL)
		}
	}
	if t := o.Title; t != nil {
		switch {
		case t.Already:
			out = append(out, "the title is already so")
		case t.Planned:
			out = append(out, "would change the title")
		case t.Changed:
			out = append(out, "changed the title")
		}
	}
	if r := o.ReviewRequests; r != nil {
		if len(r.Requested) > 0 {
			verb := "requested"
			if r.Planned {
				verb = "would request"
			}
			out = append(out, verb+" a review from "+strings.Join(r.Requested, ", "))
		}
		if len(r.Already) > 0 {
			out = append(out, "a review is already requested from "+strings.Join(r.Already, ", "))
		}
	}
	return out
}

// Summary is the outcome in one line (ids and links, no comment or title
// text).
func (o CommentOutcome) Summary() string {
	parts := strings.Join(o.parts(), "; ")
	done := ""
	if parts != "" {
		done = "; done before it, as " + o.As + ": " + parts
	}
	switch o.Status {
	case StatusPosted, StatusAlreadyPosted:
		return "as " + o.As + ": " + parts
	case StatusDryRun:
		return fmt.Sprintf("dry run as %s (identity %s): nothing posted; %s", o.As, o.Identity, parts)
	case StatusInvalid:
		n := textx.Count(len(o.Problems), "problem", "problems")
		if done == "" {
			return fmt.Sprintf("refused (%s); nothing posted: %s", n, strings.Join(o.Problems, "; "))
		}
		return fmt.Sprintf("refused (%s): %s%s", n, strings.Join(o.Problems, "; "), done)
	}
	return o.Message + done
}

// RunComment does what o names on its pull request as the App o.Login
// (gh is its client): text, when it says something, signed with
// CommentMarker, as a reply in the thread of o.ReplyTo (to its first
// comment) or on the conversation; then the title; then the review
// requests. Everything is checked before the first write: the line and
// text (fix-reply's rules: empty, a magnum marker, a local path, too long),
// the pull request (its author is never asked for a review), the comment
// id (one of this pull request's, and in no thread magnum's review
// started: the daemon's poll reads no comment text and would take the
// App's reply there for the author's). A comment the same as one of the
// App's already there with nobody else's after it (postedLast; on the
// conversation nobody else's review either, reviewedSince) is not posted
// again, a title already so is not sent, and a login asked already is
// skipped, so after an unclear failure the same line runs again safely.
// GitHub must show the App as each comment's author: another account
// there is StatusInvalid (otherAuthor) and nothing more goes out, and the
// newest copy of the same comment found by another account is
// StatusInvalid before the first write (alreadyByOther). onPosted
// (nil: none) hears of each post, with the author GitHub showed when it is
// not the App, and of a comment found already there.
func RunComment(ctx context.Context, gh CommentGitHub, o CommentOptions, text []byte, onPosted func(Posted)) CommentOutcome {
	out, ok := o.CheckText(text)
	if !ok {
		return out
	}
	if onPosted == nil {
		onPosted = func(Posted) {}
	}
	hasText := strings.TrimSpace(string(text)) != ""
	var problems []string
	var title string
	if o.Title != nil {
		title = strings.TrimSpace(*o.Title)
	}
	logins := uniqueLogins(o.RequestReviews)

	var pr github.Pull
	if o.Title != nil || len(logins) > 0 {
		var err error
		if pr, err = gh.Pull(ctx, o.Owner, o.Repo, o.Number); err != nil {
			out.Status, out.Message = StatusError, "read the pull request: "+err.Error()
			return out
		}
		for _, l := range logins {
			if github.SameAccount(l, pr.Author) {
				problems = append(problems, fmt.Sprintf("%s opened %s: GitHub asks no pull request's author for a review", pr.Author, out.PR))
			}
		}
	}
	prURL := cmp.Or(pr.URL, fmt.Sprintf("https://github.com/%s/%s/pull/%d", o.Owner, o.Repo, o.Number))
	body := signedBody(string(text), CommentMarker)
	var target signedTarget
	if hasText {
		mine := func(author string) bool { return github.SameAccount(author, o.Login) }
		var found []string
		var err error
		target, found, err = findSigned(ctx, gh, o.Owner, o.Repo, o.Number, o.ReplyTo, o.ReplyTo == 0, body, mine, o.magnumThread)
		if err == nil && o.ReplyTo == 0 && target.Already != nil {
			var later bool
			if later, err = o.reviewedSince(ctx, gh, target.Already.CreatedAt); later {
				target.Already = nil // a new round: the same words go out again
			}
		}
		if err != nil {
			out.Status, out.Message = StatusError, err.Error()
			return out
		}
		problems = append(problems, found...)
		if f := target.Other; f != nil {
			problems = append(problems, o.alreadyByOther(*f))
		}
	}
	if len(problems) > 0 {
		out.Status, out.Problems = StatusInvalid, problems
		return out
	}

	// The plan: every part, what of it is so already.
	if hasText {
		kind := PostConversation
		if o.ReplyTo != 0 {
			kind = PostReply
		}
		out.Comment = &CommentPart{Kind: kind, Thread: target.Thread, Planned: o.DryRun}
		if a := target.Already; a != nil {
			out.Comment.ID, out.Comment.URL, out.Comment.Already = a.ID, a.URL, true
		}
	}
	titleSo := o.Title != nil && pr.Title == title
	var ask, asked []string
	for _, l := range logins {
		if slices.ContainsFunc(pr.RequestedReviewers, func(r string) bool { return github.SameAccount(r, l) }) {
			asked = append(asked, l)
		} else {
			ask = append(ask, l)
		}
	}
	if o.DryRun {
		if o.Title != nil {
			out.Title = &TitlePart{Already: titleSo, Planned: true}
		}
		if len(logins) > 0 {
			out.ReviewRequests = &ReviewRequestPart{Requested: nonNil(ask), Already: nonNil(asked), Planned: true}
		}
		out.Status = StatusDryRun
		return out
	}

	// The writes, in order; each failure says what went out before it.
	fail := func(what string, err error) CommentOutcome {
		if apiErr, ok := errors.AsType[*github.APIError](err); ok && apiErr.Status == 422 {
			out.Status, out.Problems = StatusInvalid, []string{fmt.Sprintf("GitHub refused %s: %v", what, err)}
			return out
		}
		out.Status, out.Message = StatusError, err.Error()
		return out
	}
	if c := out.Comment; c != nil {
		if c.Already {
			onPosted(Posted{Kind: c.Kind, CommentID: c.ID, Thread: c.Thread, URL: c.URL, Already: true})
		} else {
			posted, err := postSigned(ctx, gh, o.Owner, o.Repo, o.Number, target, body)
			if err != nil {
				out.Comment = nil
				out.Status, out.Message = StatusError, err.Error()
				return out
			}
			if !github.SameAccount(posted.Author, o.Login) {
				c.ID, c.URL, c.Author = posted.ID, posted.URL, cmp.Or(posted.Author, "an account it does not name")
				onPosted(Posted{Kind: c.Kind, CommentID: c.ID, Thread: c.Thread, URL: c.URL, Author: c.Author})
				out.Status, out.Problems = StatusInvalid, []string{o.otherAuthor(posted, c.Author)}
				return out
			}
			c.ID, c.URL = posted.ID, posted.URL
			onPosted(Posted{Kind: c.Kind, CommentID: c.ID, Thread: c.Thread, URL: c.URL})
		}
	}
	if o.Title != nil {
		if !titleSo {
			if _, err := gh.UpdatePullTitle(ctx, o.Owner, o.Repo, o.Number, title); err != nil {
				return fail("the title", fmt.Errorf("change the title: %w", err))
			}
			onPosted(Posted{Kind: PostTitle, URL: prURL})
		}
		out.Title = &TitlePart{Changed: !titleSo, Already: titleSo}
	}
	if len(logins) > 0 {
		if len(ask) > 0 {
			if err := gh.RequestReviewers(ctx, o.Owner, o.Repo, o.Number, ask); err != nil {
				return fail("to request a review from "+strings.Join(ask, ", "), fmt.Errorf("request the reviews: %w", err))
			}
			onPosted(Posted{Kind: PostReviewRequest, URL: prURL, Reviewers: ask})
		}
		out.ReviewRequests = &ReviewRequestPart{Requested: nonNil(ask), Already: nonNil(asked)}
	}
	out.Status = StatusAlreadyPosted
	if (out.Comment != nil && !out.Comment.Already) || (out.Title != nil && out.Title.Changed) || len(ask) > 0 {
		out.Status = StatusPosted
	}
	return out
}

// otherAuthor is the problem of a comment GitHub shows by by, not by the
// App o.Login. The token is the App's, so another App there means login of
// the identity is wrong in the config: once it names that App, the same
// line finds the comment and posts nothing twice. A person, or no account
// named, means it went out under someone's login: delete it. Neither asks
// to run the line again as it is, which would post it again.
func (o CommentOptions) otherAuthor(posted signedFound, by string) string {
	head := fmt.Sprintf("GitHub shows comment %d (%s) by %s, not %s: ", posted.ID, posted.URL, by, o.Login)
	if strings.HasSuffix(strings.ToLower(posted.Author), "[bot]") {
		return head + fmt.Sprintf("the App's token posted it, so login of [[identity]] %s is wrong; set it to %s and run the same line again "+
			"(it finds the comment and posts nothing twice)", o.Identity, posted.Author)
	}
	return head + "it went out under a person's login, not the App's; delete it on GitHub and do not run it again until magnum identities check passes"
}

// alreadyByOther is the problem of the same comment found on the pull
// request by found.Author, not by the App o.Login: an earlier run posted
// it under another account (a person's login, or the App of a login the
// config named before). Only the App's own comments count as already
// there, so posting would add another copy on every run.
func (o CommentOptions) alreadyByOther(found signedFound) string {
	by := cmp.Or(found.Author, "an account GitHub does not name")
	return fmt.Sprintf("the same comment is already on the pull request: GitHub shows comment %d (%s) by %s, not %s, so an earlier run posted it "+
		"under another account; nothing posted: delete it on GitHub, and once magnum identities check passes the same line posts it as the App",
		found.ID, found.URL, by, o.Login)
}

// reviewedSince reports whether someone other than the App o.Login
// submitted a review of the pull request after at: GitHub makes every
// review comment, a reply in a thread too, part of a review, and none of
// them is a comment on the conversation, so without this a colleague's
// review between two rounds would leave the App's same words of the next
// round taken for a retry's. It reads the reviews from the newest back to
// the first one submitted before at, so a pull request's many reviews never
// hide one; a list cut short before it reached one may: true. A review not
// submitted yet has no time and counts for nothing.
func (o CommentOptions) reviewedSince(ctx context.Context, gh CommentGitHub, at time.Time) (bool, error) {
	before := func(r github.Review) bool { return !r.SubmittedAt.IsZero() && r.SubmittedAt.Before(at) }
	reviews, complete, err := gh.RecentReviews(ctx, o.Owner, o.Repo, o.Number, before)
	if err != nil {
		return false, fmt.Errorf("read the pull request's reviews: %w", err)
	}
	if slices.ContainsFunc(reviews, func(r github.Review) bool {
		return r.SubmittedAt.After(at) && !github.SameAccount(github.Account(r.AuthorLogin, r.AuthorType), o.Login)
	}) {
		return true, nil
	}
	return !complete, nil
}

// magnumThread refuses a thread magnum's review started (its first
// comment by one of o.MagnumLogins): a comment of magnum's own there
// answers nothing, and the daemon's poll would read the App's reply as
// the author's.
func (o CommentOptions) magnumThread(thread []github.ThreadComment) []string {
	root := thread[0]
	by := github.Account(root.AuthorLogin, root.AuthorType)
	if !slices.ContainsFunc(o.MagnumLogins, func(l string) bool { return github.SameAccount(l, by) }) {
		return nil
	}
	return []string{fmt.Sprintf("comment %d is in a thread magnum's review started (%s): a comment of magnum's own there answers nothing; "+
		"answer it on GitHub yourself, or let the fixer answer it (magnum fix)", o.ReplyTo, by)}
}

// uniqueLogins is logins trimmed, each once (any case), in order.
func uniqueLogins(logins []string) []string {
	var out []string
	for _, l := range logins {
		l = strings.TrimSpace(l)
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, l) }) {
			out = append(out, l)
		}
	}
	return out
}

// nonNil is s, or an empty list for nil (JSON [] rather than null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
