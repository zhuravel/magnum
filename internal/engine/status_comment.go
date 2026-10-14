package engine

// The review status comment ([pipeline] status_comment, a [[watch]]'s
// status_comment): one comment on the conversation of each PR magnum means
// to review, posted as the PR's posting identity and edited in place, so the
// PR's people see that a review is scheduled (its head and why it waits), in
// progress or posted, and neither merge before it nor ask the operator where
// it is. The tick reconciles it with the registry (noteStatusComments, after
// noteWaits): its text follows from the PR's state, its recorded wait and
// its last review, so no round, request or poll writes it, a write GitHub
// refused is tried again by a later tick, and a restart goes on from the
// registry. The text is magnum's own: a head's short sha, times in UTC and
// the words of the waits the timing rules make, never a PR's text nor an
// error's.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRStatusComment holds the PR's status comment (statusRecord as JSON):
// its id, who posted it and what was last written.
func kvPRStatusComment(prID int64) string { return fmt.Sprintf("pr.%d.status_comment", prID) }

const (
	// statusMarker is the status comment's first line.
	statusMarker = "<!-- magnum:status -->"
	// statusEvery is the least time between two writes of one PR's comment
	// (a failed one counts): the tick's waits change often, GitHub need not
	// hear of each.
	statusEvery = 2 * time.Minute
	// statusWarnEvery is the least time between two pr.status_comment_failed
	// events of one PR.
	statusWarnEvery = time.Hour
	// statusWritesPerTick caps a tick's writes (switching the comment on
	// with many PRs waiting): the rest go out at the next ticks.
	statusWritesPerTick = 10
)

// The stages of a status comment (statusRecord.Stage, the pr.status_comment
// event's stage).
const (
	statusScheduled    = "scheduled"
	statusInProgress   = "in_progress"
	statusPosted       = "posted"
	statusNotReviewing = "not_reviewing"
	statusMerged       = "merged"
)

// statusStates are the PR states noteStatusComments reads: those of a
// review to come or under way, a posted one, one magnum will not review
// and a close within its grace. A claiming PR keeps what it says until its
// round starts.
var statusStates = []string{store.PRQueued, store.PRRereviewPending, store.PRReviewing, store.PRVerifying, store.PRPaused,
	store.PRReviewed, store.PRNeedsAttention, store.PRIneligible, store.PRClosed, store.PRReleasing}

// statusRecord is what kvPRStatusComment keeps of a PR's status comment.
type statusRecord struct {
	ID       int64     `json:"id,omitempty"`       // the comment's REST id; 0 = none stands
	Identity string    `json:"identity,omitempty"` // the identity that posted it, which edits it
	Sum      string    `json:"sum,omitempty"`      // statusSum of the text last written, its time left out
	Stage    string    `json:"stage,omitempty"`    // the stage last written
	Head     string    `json:"head,omitempty"`     // the head it was last written for
	Kind     string    `json:"kind,omitempty"`     // "review" or "re-review", as last written
	At       time.Time `json:"at,omitzero"`        // the last write or attempt
	Warned   time.Time `json:"warned,omitzero"`    // the last pr.status_comment_failed
	Gone     int       `json:"gone,omitempty"`     // times a person deleted it
	// Off is the head a second deletion came on: no comment until another
	// head, since someone does not want one.
	Off string `json:"off,omitempty"`
}

// statusView is what a status comment says.
type statusView struct {
	stage string
	kind  string    // "review" or "re-review"
	head  string    // the commit it is about
	wait  string    // statusScheduled: why the review waits (statusWaitWords; "" = not known)
	since time.Time // statusInProgress: when the round started (zero = not known)
	link  string    // statusPosted: the review's link ("" = not known)
}

// opens reports whether the stage posts a comment where none stands: a
// review to come or under way. The others only edit one.
func (v statusView) opens() bool { return v.stage == statusScheduled || v.stage == statusInProgress }

// what is the view's subject for an event: "review of a1b2c3d".
func (v statusView) what() string { return v.kind + " of " + textx.ShortSHA(v.head) }

// body is the comment below its header, with the operator's note: the
// checklist (ticked down to the stage) and the fixed sentence for a review
// to come, or one line when magnum will not review the head or GitHub
// merged it first.
func (v statusView) body(note string, now time.Time) string {
	var b strings.Builder
	sha := "`" + textx.ShortSHA(v.head) + "`"
	sentence := ""
	switch v.stage {
	case statusNotReviewing:
		b.WriteString("magnum will not review this head. The operator was told.\n")
	case statusMerged:
		b.WriteString("Merged before the review of " + sha + " posted.\n")
	default:
		line := "- [x] Scheduled: " + v.kind + " of " + sha
		if v.stage == statusScheduled && v.wait != "" {
			line += " " + v.wait
		}
		b.WriteString(line + "\n")
		switch v.stage {
		case statusScheduled:
			b.WriteString("- [ ] In progress\n- [ ] Posted\n")
		case statusInProgress:
			line = "- [x] In progress"
			if !v.since.IsZero() {
				line += " since " + statusClock(v.since, now)
			}
			b.WriteString(line + "\n- [ ] Posted\n")
		case statusPosted:
			line = "- [x] Posted"
			if v.link != "" {
				line += ": " + v.link
			}
			b.WriteString("- [x] In progress\n" + line + "\n")
		}
		if v.stage != statusPosted {
			sentence = "A merge before the review posts skips it."
		}
	}
	if tail := slices.DeleteFunc([]string{sentence, strings.TrimSpace(note)}, func(s string) bool { return s == "" }); len(tail) > 0 {
		b.WriteString("\n" + strings.Join(tail, "\n") + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// statusText is the whole comment: the marker, the header with the time of
// this write, and body.
func statusText(body string, now time.Time) string {
	return statusMarker + "\n**magnum review status** (updated " + statusClock(now, now) + ")\n" + body
}

// statusSum fingerprints a comment's body, which a write compares with the
// last one's (statusRecord.Sum).
func statusSum(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:8])
}

// statusClock is t in UTC as a status comment writes it: "12:34 UTC" on
// now's UTC day, else "Oct 6 12:34 UTC".
func statusClock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04") + " UTC"
	}
	return t.Format("Jan 2 15:04") + " UTC"
}

// statusWaitWords is why a PR's review waits, as its status comment says
// it: the wait's own words (Wait.Detail) for the waits the timing rules
// make, whose words hold only the config's durations and counts, and a
// fixed phrase for the others, whose words can carry an error (a branch's
// name in a failed fetch), a path, an identity or a pool; the wait's end,
// when known, in UTC (rounded up to the minute, as on the screens).
func statusWaitWords(w Wait, now time.Time) string {
	var s string
	switch w.Reason {
	case WaitNext:
		return "starts shortly"
	case WaitRequested:
		if !w.Until.After(now) {
			return "starts shortly"
		}
		s = "waits for " + w.Detail
	case WaitQuiet, WaitBurst, WaitInterval, WaitDraftInterval, WaitOwnInterval, WaitSnoozed, WaitCap, WaitDelta:
		s = "waits for " + w.Detail
	case WaitQuietHours:
		s = "waits for the end of quiet hours" // its Detail names the hours in the operator's zone
	case WaitRetry:
		s = "waits for a retry after a failed attempt"
	case WaitSlot, WaitCapacity:
		s = "waits for a free review slot"
	default:
		s = "is held on magnum's side"
	}
	if w.Until.After(now) {
		until := w.Until
		if m := until.Truncate(time.Minute); m.Before(until) {
			until = m.Add(time.Minute)
		}
		s += ", until " + statusClock(until, now)
	}
	return s
}

// statusCommentsOn reports whether any watch has the status comment on.
func (e *Engine) statusCommentsOn() bool {
	if e.cfg.StatusCommentFor(nil) {
		return true
	}
	for i := range e.cfg.Watches {
		if e.cfg.StatusCommentFor(&e.cfg.Watches[i]) {
			return true
		}
	}
	return false
}

// noteStatusComments brings the status comment of every PR whose watch has
// it on in line with the registry (statusFor), once per tick after
// noteWaits recorded the waits: a PR without one gets it once its review is
// scheduled or under way, one with one has it edited when its text changed,
// at most once every statusEvery, and a tick writes at most
// statusWritesPerTick. Never in a dry run nor on a scratch registry (an eval
// replay). A failure is the PR's alone: the tick and the rounds go on.
func (e *Engine) noteStatusComments(ctx context.Context) {
	if e.d.DryRun || e.d.Layout.Scratch != "" || !e.statusCommentsOn() {
		return
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: statusStates})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: PRs")
		return
	}
	flagged, err := e.st.CodexFlaggedPRs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "status comments: Codex flags")
		return
	}
	now := e.now()
	repos := map[int64]store.Repo{}
	writes := 0
	for _, pr := range prs {
		if writes >= statusWritesPerTick || ctx.Err() != nil {
			return
		}
		repo, ok := repos[pr.RepoID]
		if !ok {
			if repo, err = e.st.RepoByID(ctx, pr.RepoID); err != nil {
				e.warnUnlessStopped(ctx, err, "status comments: repository", "pr", pr.ID)
				continue
			}
			repos[pr.RepoID] = repo
		}
		if w := e.cfg.WatchFor(repo.FullName()); w == nil || !e.cfg.StatusCommentFor(w) {
			continue
		}
		_, unwritten := e.unwrittenFlag(pr.ID)
		if e.writeStatus(ctx, repo, pr, flagged[pr.ID] || unwritten, now) {
			writes++
		}
	}
}

// statusRecordOf is the PR's statusRecord (zero: no comment yet).
func (e *Engine) statusRecordOf(ctx context.Context, prID int64) statusRecord {
	var rec statusRecord
	if v, ok := e.getKV(ctx, kvPRStatusComment(prID)); ok {
		_ = json.Unmarshal([]byte(v), &rec)
	}
	return rec
}

// saveStatus records rec as the PR's statusRecord.
func (e *Engine) saveStatus(ctx context.Context, prID int64, rec statusRecord) {
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, kvPRStatusComment(prID), string(b))
	}
}

// writeStatus posts or edits pr's status comment (flagged: the PR holds a
// Codex flag) when what it says changed and its last write is statusEvery
// old, and reports whether it called GitHub. An edit that finds the comment
// gone posts it once more, the first time a person deleted it; after a
// second deletion the PR gets no comment until another head.
func (e *Engine) writeStatus(ctx context.Context, repo store.Repo, pr store.PR, flagged bool, now time.Time) bool {
	v, ok := e.statusFor(ctx, pr, flagged, now)
	if !ok {
		return false
	}
	rec := e.statusRecordOf(ctx, pr.ID)
	if rec.Off != "" {
		if rec.Off == pr.HeadSHA {
			return false
		}
		rec.Off = "" // a new head: someone may want it now
	}
	if rec.ID == 0 && !v.opens() {
		return false
	}
	if v.stage == statusPosted && rec.Head == v.head && rec.Kind != "" {
		v.kind = rec.Kind // the review the comment followed: a re-review stays one
	}
	body := v.body(e.cfg.Pipeline.StatusCommentNote, now)
	sum := statusSum(body)
	if (rec.ID != 0 && sum == rec.Sum) || (!rec.At.IsZero() && now.Sub(rec.At) < statusEvery) {
		return false
	}
	rec.At = now
	text := statusText(body, now)
	ident := pr.Identity
	if rec.ID != 0 {
		ident = cmp.Or(rec.Identity, pr.Identity)
	}
	var c github.IssueComment
	err := e.statusCall(ident, func(gh GitHub) (err error) {
		if rec.ID == 0 {
			c, err = gh.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, text)
			return err
		}
		c, err = gh.EditIssueComment(ctx, repo.Owner, repo.Name, rec.ID, text)
		return err
	})
	posted := rec.ID == 0
	if rec.ID != 0 && errors.Is(err, github.ErrNotFound) {
		rec.Gone++
		rec.ID, rec.Sum, rec.Stage = 0, "", ""
		if rec.Gone > 1 {
			rec.Off = pr.HeadSHA
			e.saveStatus(ctx, pr.ID, rec)
			e.log.Info("status comment deleted again: none until the next push", "pr", pr.ID, "head", textx.ShortSHA(pr.HeadSHA))
			return true
		}
		e.log.Info("status comment deleted: posting it once more", "pr", pr.ID)
		posted, ident = true, pr.Identity
		err = e.statusCall(ident, func(gh GitHub) (err error) {
			c, err = gh.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, text)
			return err
		})
	}
	if err != nil {
		if isStop(ctx, err) {
			return true // the stop is no failure; the next start writes it
		}
		e.statusFailed(ctx, repo, pr, &rec, v, posted, err, now)
		e.saveStatus(ctx, pr.ID, rec)
		return true
	}
	if c.ID != 0 {
		rec.ID = c.ID
	}
	rec.Identity, rec.Sum, rec.Stage, rec.Head, rec.Kind = ident, sum, v.stage, v.head, v.kind
	e.saveStatus(ctx, pr.ID, rec)
	verb := "updated"
	if posted {
		verb = "posted"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.status_comment",
		fmt.Sprintf("status comment %s: %s (%s)", verb, strings.ReplaceAll(v.stage, "_", " "), v.what()),
		map[string]any{"stage": v.stage, "comment_id": rec.ID, "url": c.URL, "head_sha": v.head, "identity": ident, "posted": posted})
	return true
}

// statusCall runs call with ident's GitHub client; an identity without one
// is a failure as GitHub's would be.
func (e *Engine) statusCall(ident string, call func(GitHub) error) error {
	gh := e.gh(ident)
	if gh == nil {
		return fmt.Errorf("identity %q has no GitHub client", ident)
	}
	return call(gh)
}

// statusFailed records a write GitHub refused: a pr.status_comment_failed
// warning, at most one per PR every statusWarnEvery.
func (e *Engine) statusFailed(ctx context.Context, repo store.Repo, pr store.PR, rec *statusRecord, v statusView, posted bool, err error, now time.Time) {
	if !rec.Warned.IsZero() && now.Sub(rec.Warned) < statusWarnEvery {
		e.log.Debug("status comment failed again", "pr", pr.ID, "err", err)
		return
	}
	rec.Warned = now
	verb := "not updated"
	if posted {
		verb = "not posted"
	}
	e.event(ctx, "warn", prSubject(repo, pr.Number), "pr.status_comment_failed",
		fmt.Sprintf("status comment %s (%s): %v; the review goes on, a later tick tries again", verb, strings.ReplaceAll(v.stage, "_", " "), err),
		map[string]any{"stage": v.stage, "comment_id": rec.ID, "head_sha": v.head})
}

// statusFor is what pr's status comment says now (flagged: the PR holds a
// Codex flag); ok is false when it has nothing to say of the PR: a draft, a
// closed PR (unless GitHub merged it before its review posted), a
// post-merge review, a dry run, a claimed round not started yet, and a PR
// left out of review for a reason of its filters. A round on the reviewed
// head (a reply round, a judge check of red CI, a re-decision) leaves it
// posted. It reads the registry only for a forced PR's dry run and a
// waiting PR's wait: the tick asks it of every PR.
func (e *Engine) statusFor(ctx context.Context, pr store.PR, flagged bool, now time.Time) (statusView, bool) {
	reviewed := deref(pr.ReviewedSHA)
	v := statusView{kind: "review", head: pr.HeadSHA}
	if reviewed != "" {
		v.kind = "re-review"
	}
	switch {
	case pr.IsDraft || pr.HeadSHA == "":
		return v, false
	case postMerge(pr):
		v.stage = statusMerged
		return v, (pr.State == store.PRClosed || pr.State == store.PRReleasing) && pr.MergedUnreviewed()
	case pr.GHState == store.GHClosed || pr.State == store.PRClosed || pr.State == store.PRReleasing:
		return v, false
	case pr.Forced && e.hasDryRun(ctx, pr.ID):
		return v, false
	case pr.HeadSHA == reviewed:
		v.stage, v.kind = statusPosted, "review"
		if id := deref(pr.LastReviewID); id > 0 && pr.URL != "" {
			v.link = fmt.Sprintf("%s#pullrequestreview-%d", pr.URL, id)
		}
		return v, true
	case pr.State == store.PRNeedsAttention:
		v.stage = statusNotReviewing
		return v, true
	case pr.State == store.PRIneligible:
		if pr.Muted || flagged {
			v.stage = statusNotReviewing
			return v, true
		}
		return v, false
	case pr.Muted && !pr.Forced:
		v.stage = statusNotReviewing
		return v, true
	case pr.State == store.PRQueued || pr.State == store.PRRereviewPending:
		v.stage = statusScheduled
		raw, _ := e.getKV(ctx, KVPRWait(pr.ID))
		if w, ok := ParseWait(raw); ok {
			v.wait = statusWaitWords(w, now)
		}
		return v, true
	case pr.State == store.PRReviewing || pr.State == store.PRVerifying || pr.State == store.PRPaused:
		v.stage = statusInProgress
		if pr.LastRoundStartedAt != nil {
			v.since = *pr.LastRoundStartedAt
		}
		return v, true
	}
	return v, false
}

// hasDryRun reports whether a dry-run round is asked for the PR
// (store.KVPRDryRun): it posts nothing, and its comment says nothing of it.
func (e *Engine) hasDryRun(ctx context.Context, prID int64) bool {
	_, ok := e.getKV(ctx, store.KVPRDryRun(prID))
	return ok
}
