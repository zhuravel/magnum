// Package postreview is `magnum post-review`, the judge's posting tool. The
// judge writes its review (event, body, inline comments) to a file; Run
// checks it (the fields, then every inline anchor against the pull
// request's diff), posts it once as the judge's identity (never twice: a
// review carrying the run's marker counts as posted) and reads it back.
// RunReplies is its replies-only mode: when authors answered the judge's
// threads and the head is unchanged, the judge answers in the threads
// instead, each reply carrying a hidden marker the daemon verifies and
// counts rebuttals by.
// It works from its Options, gh and git in the checkout alone: it loads no
// config, opens no registry, never contacts the daemon and writes no file.
package postreview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// Outcome statuses (Outcome.Status).
const (
	StatusPosted           = "posted"            // posted and read back
	StatusAlreadyPosted    = "already_posted"    // a review carrying the run's marker exists; nothing posted
	StatusDryRun           = "dry_run"           // checked; nothing posted (Outcome.PlannedReview)
	StatusInvalid          = "invalid"           // the review file is wrong (Outcome.Problems)
	StatusInvalidAnchors   = "invalid_anchors"   // inline comments off the diff (Outcome.InvalidAnchors)
	StatusRejected         = "rejected"          // GitHub refused the review and none was created
	StatusError            = "error"             // anything else; Outcome.Message says what
	StatusReadbackMismatch = "readback_mismatch" // posted, but the review reads back differently
)

// Review events the judge may post.
const (
	EventComment        = "COMMENT"
	EventRequestChanges = "REQUEST_CHANGES"
	EventApprove        = "APPROVE"
)

// Sides of an inline comment.
const (
	SideRight = "RIGHT" // the head's lines: added or context
	SideLeft  = "LEFT"  // the base's lines: deleted or context
)

// MaxBody is GitHub's limit for a review body and for a comment body, in
// characters.
const MaxBody = 65536

// FooterMarker starts the footer magnum appends to a verified review
// (pipeline's footerMarker); a judge never writes it.
const FooterMarker = "<!-- magnum:footer -->"

// Review is the file the judge writes: the review to post. Comments use
// GitHub's fields (path, line, side, start_line, start_side, body).
type Review struct {
	Event    string                `json:"event"`
	Body     string                `json:"body"`
	Comments []github.DraftComment `json:"comments"`
}

// RunMarker is the line every review of run carries, on head:
// `<!-- magnum:run=<run id> head=<first 7 of head> -->`.
func RunMarker(runID, head string) string {
	return fmt.Sprintf("<!-- magnum:run=%s head=%s -->", runID, textx.ShortSHA(head))
}

// markerRe matches run's marker exactly (r-…-1 must not match r-…-10), as
// the pipeline looks for it.
func markerRe(runID string) *regexp.Regexp {
	return regexp.MustCompile(`magnum:run=` + regexp.QuoteMeta(runID) + `(?:[^A-Za-z0-9._-]|$)`)
}

// Parse decodes and checks the judge's review file for the run o names. It
// returns the review to post, with each comment's side (RIGHT unless
// given) and a multi-line comment's start side filled in and the run's
// marker appended as the body's last line when the body lacks it
// (appended reports that), or the problems found, each naming its field.
func Parse(data []byte, o Options) (r Review, appended bool, problems []string) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Review{}, false, []string{"the file is not a review object (event, body, comments): " + err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Review{}, false, []string{"the file holds more than one JSON value"}
	}
	switch r.Event {
	case EventComment, EventRequestChanges, EventApprove:
	default:
		problems = append(problems, fmt.Sprintf("event %q is not COMMENT, REQUEST_CHANGES or APPROVE", r.Event))
	}
	switch {
	case strings.TrimSpace(r.Body) == "":
		problems = append(problems, "body is empty")
	case strings.Contains(r.Body, FooterMarker):
		problems = append(problems, "body carries "+FooterMarker+": magnum appends the footer, leave it out")
	case localPath(r.Body) != "":
		problems = append(problems, localPathProblem(r.Body))
	}
	if len(problems) == 0 && !markerRe(o.RunID).MatchString(r.Body) {
		r.Body = strings.TrimRight(r.Body, " \t\r\n") + "\n\n" + RunMarker(o.RunID, o.HeadSHA)
		appended = true
	}
	if n := utf8.RuneCountInString(r.Body); n > MaxBody {
		problems = append(problems, fmt.Sprintf("body is %d characters (the run marker included); GitHub takes at most %d", n, MaxBody))
	}
	for i := range r.Comments {
		problems = append(problems, checkComment(i, &r.Comments[i])...)
	}
	if len(problems) > 0 {
		return Review{}, false, problems
	}
	if r.Comments == nil {
		r.Comments = []github.DraftComment{}
	}
	return r, appended, nil
}

// localPathRe finds a path that may be on the judge's machine (SKILL.md: no
// local path in posted text): under a home directory or macOS's temp
// directories, or /tmp, at a word's start (not inside a repository path, a
// URL or a word; a `~/` not after a slash either), up to a blank, quote,
// backtick or bracket. /home is left out: a PR's Dockerfile or CI config
// names /home/app, and this machine (macOS) keeps no one's home there.
var localPathRe = regexp.MustCompile("(?:^|[^A-Za-z0-9_.~/-])(~/[^\\s`'\"()<>\\[\\]{}]*)" +
	"|(?:^|[^A-Za-z0-9_.~-])((?:/tmp|/private|/var/folders|/Users)/[^\\s`'\"()<>\\[\\]{}]*)")

// lineSuffix is a `:12` or `:12:5` after a file's path.
var lineSuffix = regexp.MustCompile(`(?::\d+){1,2}$`)

// localPath is the first local path text names (localPathRe), without
// trailing punctuation; "" when it names none. A /tmp path counts only when
// it is there on this machine now (as written, or without a line suffix):
// the judge's own scratch files are, a path the PR's code names usually is
// not.
func localPath(text string) string {
	for _, m := range localPathRe.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(m[1]+m[2], ".,;:!?")
		if !strings.HasPrefix(p, "/tmp/") || exists(p) || exists(lineSuffix.ReplaceAllString(p, "")) {
			return p
		}
	}
	return ""
}

// exists reports whether path is there on this machine.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// localPathProblem says which local path a body names (at most 80
// characters of it), for a body localPath finds one in.
func localPathProblem(body string) string {
	return fmt.Sprintf("body names the local path %q: name files by their repository path and describe output instead",
		textx.Clip(localPath(body), 80))
}

// checkComment checks comment i and fills in its sides.
func checkComment(i int, c *github.DraftComment) []string {
	var out []string
	bad := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("comments[%d]: ", i)+fmt.Sprintf(format, args...))
	}
	if strings.TrimSpace(c.Path) == "" {
		bad("path is empty")
	}
	if c.Line < 1 {
		bad("line %d is not a line number (1 or more)", c.Line)
	}
	switch {
	case strings.TrimSpace(c.Body) == "":
		bad("body is empty")
	case utf8.RuneCountInString(c.Body) > MaxBody:
		bad("body is %d characters; GitHub takes at most %d", utf8.RuneCountInString(c.Body), MaxBody)
	case localPath(c.Body) != "":
		bad("%s", localPathProblem(c.Body))
	}
	if c.Side == "" {
		c.Side = SideRight
	}
	if c.Side != SideRight && c.Side != SideLeft {
		bad("side %q is not RIGHT or LEFT", c.Side)
	}
	switch {
	case c.StartLine == 0 && c.StartSide != "":
		bad("start_side without start_line")
	case c.StartLine < 0:
		bad("start_line %d is not a line number (1 or more)", c.StartLine)
	case c.StartLine > 0:
		if c.StartLine >= c.Line {
			bad("start_line %d is not before line %d", c.StartLine, c.Line)
		}
		if c.StartSide == "" {
			c.StartSide = c.Side
		}
		if c.StartSide != c.Side {
			bad("start_side %q differs from side %q: both lines of a comment are on one side", c.StartSide, c.Side)
		}
	}
	return out
}
