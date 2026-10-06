package postreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// Reply kinds (Reply.Kind).
const (
	ReplyAck      = "ack"      // a reason the judge accepts: one short acknowledgement
	ReplyRebuttal = "rebuttal" // the judge keeps the finding: one sentence with evidence
	ReplyAnswer   = "answer"   // an answer to a question
)

// StatusReplied: the replies are posted, or were all posted already.
const StatusReplied = "replied"

// maxRebuttals is how many times magnum argues in one thread: after that
// RunReplies refuses any reply there and the operator takes over.
const maxRebuttals = 2

// anyMarker starts every marker magnum writes; a judge's reply never
// carries one of its own (RunReplies appends the reply marker).
const anyMarker = "<!-- magnum:"

// replyMarkerRe reads a reply marker: its run id (no spaces) and kind.
var replyMarkerRe = regexp.MustCompile(`<!-- magnum:reply run=(\S+) kind=(\S+) -->`)

// ReplyMarker is the line every reply of run carries:
// `<!-- magnum:reply run=<run id> kind=<kind> -->`. The daemon finds a
// round's replies on GitHub by it, and counts magnum's rebuttals per thread.
func ReplyMarker(runID, kind string) string {
	return fmt.Sprintf("<!-- magnum:reply run=%s kind=%s -->", runID, kind)
}

// ParseReplyMarker reads the reply marker in body: its run id and kind; ok
// is false when body has none.
func ParseReplyMarker(body string) (runID, kind string, ok bool) {
	m := replyMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// RepliesFile is the file the judge writes for --replies.
type RepliesFile struct {
	Replies []Reply `json:"replies"`
}

// Reply is one answer in a review thread.
type Reply struct {
	CommentID int64  `json:"comment_id"` // the thread's first comment (threads_file's comment_id)
	Kind      string `json:"kind"`
	Body      string `json:"body"`
}

// PostedReply is a reply RunReplies posted, found already posted by this
// run, or (dry run) planned.
type PostedReply struct {
	CommentID int64  `json:"comment_id"`
	ID        int64  `json:"id,omitempty"`
	URL       string `json:"url,omitempty"`
	Kind      string `json:"kind"`
	Already   bool   `json:"already,omitempty"` // posted by an earlier run of the tool for this run id: not posted again
}

// replyBody is what is posted for r: its text, a blank line and the run's
// reply marker.
func replyBody(runID string, r Reply) string {
	return strings.TrimRight(r.Body, " \t\r\n") + "\n\n" + ReplyMarker(runID, r.Kind)
}

// parseReplies decodes and checks the judge's replies file for the run o
// names, or returns the problems found, each naming its reply.
func parseReplies(data []byte, o Options) (RepliesFile, []string) {
	var f RepliesFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return RepliesFile{}, []string{"the file is not a replies object (replies): " + err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return RepliesFile{}, []string{"the file holds more than one JSON value"}
	}
	if f.Replies == nil {
		return RepliesFile{}, []string{"replies is missing: it is an array, empty when there is nothing to answer"}
	}
	var problems []string
	first := map[int64]int{} // comment_id → the reply that named it first
	for i, r := range f.Replies {
		bad := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("replies[%d]: ", i)+fmt.Sprintf(format, args...))
		}
		switch j, seen := first[r.CommentID]; {
		case r.CommentID < 1:
			bad("comment_id %d is not a comment id (1 or more)", r.CommentID)
		case seen:
			bad("comment_id %d repeats replies[%d]: one reply per thread", r.CommentID, j)
		default:
			first[r.CommentID] = i
		}
		switch r.Kind {
		case ReplyAck, ReplyRebuttal, ReplyAnswer:
		default:
			bad("kind %q is not %s, %s or %s", r.Kind, ReplyAck, ReplyRebuttal, ReplyAnswer)
		}
		switch {
		case strings.TrimSpace(r.Body) == "":
			bad("body is empty")
		case strings.Contains(r.Body, anyMarker):
			bad("body carries a %s… marker: magnum appends the reply marker, leave it out", anyMarker)
		default:
			if n := utf8.RuneCountInString(replyBody(o.RunID, r)); n > MaxBody {
				bad("body is %d characters (the reply marker included); GitHub takes at most %d", n, MaxBody)
			}
		}
	}
	if len(problems) > 0 {
		return RepliesFile{}, problems
	}
	return f, nil
}

// RunReplies posts the judge's replies file data for the run o names (o's
// head is only checked for form: a reply answers a thread, whatever its
// commit). It checks the file, reads the review threads once, and checks
// that each reply answers the first comment of a thread of the reviewer
// (or a former login) and that magnum has not already rebutted twice
// there; a thread that already holds a reply carrying the run's marker
// gets nothing more, so the tool is safe to run again after a failure. Then
// it posts the replies in file order (or, in a dry run, plans them) and
// stops at the first failure.
func RunReplies(ctx context.Context, d Deps, o Options, data []byte) Outcome {
	if err := o.Check(); err != nil {
		return Outcome{Status: StatusError, Message: err.Error()}
	}
	if o.LocalBase != "" {
		return Outcome{Status: StatusError, Message: "a local base is for a review's dry run only"}
	}
	f, problems := parseReplies(data, o)
	if len(problems) > 0 {
		return Outcome{Status: StatusInvalid, Problems: problems}
	}
	if len(f.Replies) == 0 {
		return Outcome{Status: StatusReplied}
	}
	threads, err := d.GitHub.ReviewThreads(ctx, o.Owner, o.Repo, o.Number)
	if err != nil {
		return Outcome{Status: StatusError, Message: "read the review threads: " + err.Error()}
	}
	r := &run{d: d, o: o}
	plan, problems := r.planReplies(f.Replies, threads)
	if len(problems) > 0 {
		return Outcome{Status: StatusInvalid, Problems: problems}
	}
	out := Outcome{Status: StatusReplied}
	if o.DryRun {
		out.Status = StatusDryRun
	}
	for i, rp := range f.Replies {
		if plan[i] != nil {
			out.Replies = append(out.Replies, *plan[i])
			continue
		}
		if o.DryRun {
			out.Replies = append(out.Replies, PostedReply{CommentID: rp.CommentID, Kind: rp.Kind})
			continue
		}
		posted, err := d.GitHub.ReplyToReviewComment(ctx, o.Owner, o.Repo, o.Number, rp.CommentID, replyBody(o.RunID, rp))
		if err != nil {
			out.Status, out.Message = StatusError, fmt.Sprintf("post the reply to comment %d: %v", rp.CommentID, err)
			return out
		}
		out.Replies = append(out.Replies, PostedReply{CommentID: rp.CommentID, ID: posted.ID, URL: posted.URL, Kind: rp.Kind})
	}
	return out
}

// planReplies checks each reply against threads. plan[i] is non-nil for a
// reply already posted by this run (Already); problems lists the replies
// that cannot be posted at all.
func (r *run) planReplies(replies []Reply, threads []github.Thread) (plan []*PostedReply, problems []string) {
	byFirst := make(map[int64]github.Thread, len(threads))
	for _, t := range threads {
		if len(t.Comments) > 0 {
			byFirst[t.Comments[0].ID] = t
		}
	}
	plan = make([]*PostedReply, len(replies))
	for i, rp := range replies {
		bad := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("replies[%d]: ", i)+fmt.Sprintf(format, args...))
		}
		t, ok := byFirst[rp.CommentID]
		if !ok || !r.ownComment(t.Comments[0]) {
			bad("comment %d does not start a thread of %s", rp.CommentID, r.o.Login)
			continue
		}
		rebuttals := 0
		for _, c := range t.Comments[1:] {
			if !r.ownComment(c) {
				continue
			}
			runID, kind, marked := ParseReplyMarker(c.Body)
			if marked && runID == r.o.RunID && plan[i] == nil {
				plan[i] = &PostedReply{CommentID: rp.CommentID, ID: c.ID, URL: c.URL, Kind: kind, Already: true}
			}
			// Magnum's rebuttals before the reply marker existed carry none.
			if !marked || kind == ReplyRebuttal {
				rebuttals++
			}
		}
		if plan[i] == nil && rebuttals >= maxRebuttals {
			bad("you already rebutted twice in this thread; magnum stops arguing there and asks the operator")
		}
	}
	return plan, problems
}

// ownComment reports whether c was written by the reviewer login or a
// former one.
func (r *run) ownComment(c github.ThreadComment) bool {
	return r.ownLogin(github.Account(c.AuthorLogin, c.AuthorType))
}

// repliesSummary is the one line of a replies outcome (counts, no text).
func (o Outcome) repliesSummary() string {
	var posted, already int
	byKind := map[string]int{}
	for _, rp := range o.Replies {
		if rp.Already {
			already++
			continue
		}
		posted++
		byKind[rp.Kind]++
	}
	var kinds []string
	for _, k := range []struct{ kind, one, many string }{
		{ReplyAck, "ack", "acks"}, {ReplyRebuttal, "rebuttal", "rebuttals"}, {ReplyAnswer, "answer", "answers"},
	} {
		if n := byKind[k.kind]; n > 0 {
			kinds = append(kinds, textx.Count(n, k.one, k.many))
		}
	}
	detail := ""
	if len(kinds) > 0 {
		detail = " (" + strings.Join(kinds, ", ") + ")"
	}
	tail := ""
	if already > 0 {
		tail = fmt.Sprintf("; %d already posted", already)
	}
	replies := textx.Count(posted, "reply", "replies")
	switch {
	case o.Status == StatusDryRun:
		return fmt.Sprintf("dry run: nothing posted; %s planned%s%s", replies, detail, tail)
	case len(o.Replies) == 0:
		return "no replies to post"
	case posted == 0:
		return fmt.Sprintf("%s already posted; nothing posted", textx.Count(already, "reply is", "replies are"))
	}
	return fmt.Sprintf("posted %s%s%s", replies, detail, tail)
}
