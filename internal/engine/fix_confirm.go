package engine

// The settle's confirmation of the fixer's push when GitHub does not answer
// (DECISIONS "A GitHub blip never fails a fix whose push landed"). On
// talkable/talkable#12226 on 2026-11-12 the fixer pushed, the poll saw its
// head, and 29 seconds later the settle failed the fix: gh could not
// connect while confirmPush read the PR's head. confirmPush now retries a
// transient read (transientRead) with a backoff, for about 3 minutes
// (fixConfirmBackoff, fixConfirmWindow), on the job's own context, so a
// shutdown cuts it at once. With no answer by then, the loop stays in its
// settle (FixState.Confirm, phase fixing) and the poll of the next ticks
// tries again on the PR's head as GitHub's radar reads it (settlePushes),
// up to fixConfirmCap in all (capPushes). A fix that still fails only
// because its push was not confirmed (pushNotConfirmed: GitHub's own answer,
// another head, the cap, or a restart that cut the settle) records the
// typed kind fixPushUnconfirmed (kvPRFixPush); the poll that then finds
// exactly the pushed head as the PR's settles it as pushed, once, through
// resumable steps (settleConfirmedPush).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Kinds of a fix's push record (fixPush.Kind).
const (
	// fixPushConfirming: the settle confirms the push (its own retries,
	// then the ticks', up to fixConfirmCap).
	fixPushConfirming = "confirming"
	// fixPushUnconfirmed: the fix failed only because its push was not
	// confirmed on GitHub; the poll settles it late when GitHub shows the
	// pushed head (settlePushes).
	fixPushUnconfirmed = "push_unconfirmed"
)

const (
	// fixConfirmWindow bounds the settle's own retries of a transient read
	// (fixConfirmBackoff bounds their number too): past it, the ticks try
	// again.
	fixConfirmWindow = 3 * time.Minute
	// fixConfirmCap bounds the confirmation in all, from its start
	// (fixPush.Since): past it, the fix fails as unconfirmed.
	fixConfirmCap = 30 * time.Minute
	// fixPushKeep is how long an unconfirmed fix waits for the poll to
	// find its push as the PR's head.
	fixPushKeep = 24 * time.Hour
)

// fixConfirmBackoff are the waits before each new read of the PR's head
// after a transient failure: 3 minutes in all (fixConfirmWindow).
var fixConfirmBackoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, 45 * time.Second}

// kvPRFixPush holds the record of the push the PR's fix settles (fixPush as
// JSON): written before the settle confirms it, gone once it is confirmed,
// kept as fixPushUnconfirmed when the fix failed for want of it.
func kvPRFixPush(prID int64) string { return fmt.Sprintf("pr.%d.fix_push", prID) }

// fixPush is a fixer's push the settle confirms (Kind fixPushConfirming)
// or could not confirm (fixPushUnconfirmed): the run, the pushed head (the
// whole SHA), the short SHAs of the commits the fixer's report lists as
// pushed (Commits: an earlier one GitHub shows is no other head, only one
// not there yet), when the settle began to confirm it (Since, which
// fixConfirmCap counts from), when the fix failed (Failed), whether the
// operator typed into the fixer's pane during the run, whether the settle
// looked for the author's replies without the fix marker in the threads
// (RepliesNamed) and on the conversation (ConversationNamed): a later
// settle looks where it could not, and only there, and the job as the
// settle had it, which a later settle rebuilds (pushJob).
type fixPush struct {
	Kind              string     `json:"kind"`
	Run               string     `json:"run"`
	Pushed            string     `json:"pushed"`
	Commits           []string   `json:"commits,omitempty"`
	Since             time.Time  `json:"since"`
	Failed            time.Time  `json:"failed,omitzero"`
	Typed             bool       `json:"typed,omitempty"`
	RepliesNamed      bool       `json:"replies_named,omitempty"`
	ConversationNamed bool       `json:"conversation_named,omitempty"`
	Job               fixPushJob `json:"job"`
}

// fixPushJob is what a settle reads of its fix job beyond the registry's
// rows (fixJob's fields of the same names).
type fixPushJob struct {
	State       FixState      `json:"state"`
	Role        string        `json:"role"`
	Head        string        `json:"head"`
	Started     time.Time     `json:"started"`
	Login       string        `json:"login,omitempty"`
	Mode        string        `json:"mode,omitempty"`
	Conflict    *fixPushMerge `json:"conflict,omitempty"`
	Merged      *fixPushMerge `json:"merged,omitempty"`
	ForMerge    bool          `json:"for_merge,omitempty"`
	Alone       bool          `json:"alone,omitempty"`
	Described   bool          `json:"described,omitempty"`
	BaseChecked bool          `json:"base_checked,omitempty"`
}

// fixPushMerge is a fixConflict as a record keeps it.
type fixPushMerge struct {
	Base  string   `json:"base"`
	SHA   string   `json:"sha"`
	Paths []string `json:"paths,omitempty"`
}

func mergeRecord(c fixConflict) *fixPushMerge {
	if c.sha == "" {
		return nil
	}
	return &fixPushMerge{Base: c.base, SHA: c.sha, Paths: c.paths}
}

func (m *fixPushMerge) conflict() fixConflict {
	if m == nil {
		return fixConflict{}
	}
	return fixConflict{base: m.Base, sha: m.SHA, paths: m.Paths}
}

// pushRecord is the record of head, the push of the job's fixer, as kind.
func (j *fixJob) pushRecord(kind, head string, since time.Time, typed bool) fixPush {
	return fixPush{Kind: kind, Run: j.run.ID, Pushed: head, Since: since, Typed: typed, Job: fixPushJob{
		State: j.state, Role: j.role.Name, Head: j.head, Started: j.started, Login: j.login, Mode: j.mode,
		Conflict: mergeRecord(j.conflict), Merged: mergeRecord(j.merged), ForMerge: j.forMerge, Alone: j.alone,
		Described: j.described, BaseChecked: j.baseChecked}}
}

// earlierPush reports whether head, the PR's head GitHub shows, is one of
// the commits rec's fixer pushed before its last (fixPush.Commits): GitHub
// has not got to the last yet.
func (rec fixPush) earlierPush(head string) bool {
	return head != rec.Pushed && slices.ContainsFunc(rec.Commits, func(c string) bool { return len(c) >= 7 && strings.HasPrefix(head, c) })
}

// pushNotConfirmed is the settle's failure for want of the push on GitHub:
// GitHub answered with another head, its read failed for good, or GitHub
// did not answer within fixConfirmCap. Its words are as before; the record
// carries its kind (fixPushUnconfirmed), never the words.
type pushNotConfirmed struct {
	head string
	err  error
}

func (p pushNotConfirmed) Error() string { return "push not confirmed on GitHub: " + p.err.Error() }
func (p pushNotConfirmed) Unwrap() error { return p.err }

// pushUnanswered is confirmPush's end when GitHub did not answer within
// fixConfirmWindow, only transient failures: the ticks try again.
type pushUnanswered struct {
	head string
	err  error
}

func (p pushUnanswered) Error() string {
	return "push not confirmed on GitHub yet: GitHub does not answer: " + p.err.Error()
}
func (p pushUnanswered) Unwrap() error { return p.err }

// transientRead reports whether err, a failed read of the PR's head
// (remoteHead), is GitHub or the network failing to answer, which a retry
// may get past: a connection error (no connection, a reset, a DNS
// failure), a timeout, or an HTTP 5xx from GitHub. It reads what the GitHub
// client returns: a typed error (*github.APIError, net.Error), or gh's or
// git's exit and its output (github.ConnectionCause, its rate limits left
// out, then transientExitRe: what git, curl, ssh and Go's HTTP/2 print when
// GitHub fails to answer, which the shared classifier does not name). A 499
// is GitHub's own timeout. An answer cut short is a timeout too: the empty body GraphQL
// answers a query past its time limit with, which the client cannot decode
// ("unexpected end of JSON input"), and a cut one (io.ErrUnexpectedEOF).
// GitHub's own answer (401, 403, 404, a GraphQL error, a rate limit) and
// every other error are not, nor is a stop. remoteHead's joined error (the
// client's read, then `git ls-remote`) is transient only when each part
// is.
func transientRead(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs := j.Unwrap()
		for _, e := range errs {
			if !transientRead(e) {
				return false
			}
		}
		return len(errs) > 0
	}
	if api, ok := errors.AsType[*github.APIError](err); ok {
		return len(api.Errors) == 0 && (api.Status == 499 || api.Status >= 500 && api.Status <= 599)
	}
	if _, ok := errors.AsType[net.Error](err); ok { // a dial, a DNS lookup, a reset, a timeout (context.DeadlineExceeded is one)
		return true
	}
	if se, ok := errors.AsType[*json.SyntaxError](err); ok && se.Offset == 0 && se.Error() == "unexpected end of JSON input" {
		return true // an empty answer: GraphQL's time limit (github's overloaded reads it so too)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if exit, ok := errors.AsType[*execx.ExitError](err); ok {
		if cause := github.ConnectionCause(exit.Stderr); cause != "" {
			return cause != github.RateLimited
		}
		return transientExitRe.MatchString(strings.ToLower(exit.Stderr))
	}
	return false
}

// transientExitRe is what an exit prints, lower case, when GitHub fails to
// answer and github.ConnectionCause names nothing: git's (curl's) HTTP 5xx
// ("The requested URL returned error: 503"), an empty or cut answer, an
// HTTP/2 stream git or gh lost (curl's "HTTP/2 stream 1 was not closed
// cleanly", Go's REFUSED_STREAM, CANCEL, GOAWAY and lost connection, which
// the client's overloaded reads so too), and ssh's connection GitHub
// closed. A 4xx, a missing repository or a refused key match nothing.
var transientExitRe = regexp.MustCompile(`requested url returned error: 5\d\d\b|empty reply from server|failure when receiving data from the peer|` +
	`http/2 stream \d+ was not closed cleanly|refused_stream|cancel; received from peer|server sent goaway|http2: client connection lost|` +
	`connection closed by \S+ port \d+|kex_exchange_identification`)

// confirmPush checks that GitHub has head, the commit the fixer says it
// pushed, as the head of the PR's branch (remoteHead). A head GitHub shows
// otherwise is looked at again fixPushWait later, up to fixPushLooks times;
// a read that fails with a transient error (transientRead) is tried again
// after each wait of fixConfirmBackoff, within fixConfirmWindow, and the
// loop's row says meanwhile that it confirms the push (markConfirming); any
// other failed read ends it at once. It returns nil once GitHub shows head,
// pushUnanswered when GitHub did not answer within the window,
// pushNotConfirmed otherwise, or ctx's error the moment ctx ends: the
// retry holds no shutdown or drain. The errors name the shas, never the
// fixer's words.
func (e *Engine) confirmPush(ctx context.Context, job *fixJob, head string) error {
	start := e.now()
	looks, tries := 0, 0
	for {
		got, err := e.remoteHead(ctx, job)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var wait time.Duration
		switch {
		case err == nil && got == head:
			if tries > 0 { // GitHub answered after a blip: the loop is at work again, its settle no longer confirming
				e.unmarkConfirming(ctx, job)
			}
			return nil
		case err == nil:
			if looks++; looks >= fixPushLooks {
				return pushNotConfirmed{head: head, err: fmt.Errorf("the PR's head is %s, not the pushed %s", textx.ShortSHA(got), textx.ShortSHA(head))}
			}
			wait = fixPushWait
		case !transientRead(err):
			return pushNotConfirmed{head: head, err: err}
		case tries >= len(fixConfirmBackoff) || e.now().Sub(start) >= fixConfirmWindow:
			return pushUnanswered{head: head, err: err}
		default:
			if tries == 0 {
				e.markConfirming(ctx, job, head)
			}
			wait = fixConfirmBackoff[tries]
			tries++
		}
		if err := e.d.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// markConfirming puts on the job's loop row that its settle confirms head
// while GitHub does not answer (FixState.Confirm): the screens say so, and
// `daemon-restart --drain` waits for no such loop (a restart fails it as
// unconfirmed, which the poll settles late). The row is replaced only while
// it is the one read (Store.SetKVIf).
func (e *Engine) markConfirming(ctx context.Context, job *fixJob, head string) {
	st, raw, ok, err := e.fixRow(ctx, job.pr.ID)
	if err != nil || !ok || st.Phase != FixPhaseFixing || st.By == heldPushBy {
		return
	}
	st.Confirm = &FixConfirm{Head: head, Since: e.now().UTC()}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if _, err := e.st.SetKVIf(context.WithoutCancel(ctx), KVPRFix(job.pr.ID), raw, string(b)); err != nil {
		e.log.Warn("fix: mark the loop as confirming its push", "subject", job.subject(), "err", err)
		return
	}
	job.state.Confirm = st.Confirm
}

// unmarkConfirming takes the confirming mark off the job's loop row once
// GitHub confirmed the push after a blip (markConfirming put it there):
// the rest of the settle is the loop at work, which `daemon-restart
// --drain` and `daemon-stop` wait for (the CLI's fixesAtWork), and the
// push's record goes right after, so a restart cut in the rest of the
// settle could not settle the loop late. The row is replaced only while it
// is the one read (Store.SetKVIf).
func (e *Engine) unmarkConfirming(ctx context.Context, job *fixJob) {
	ctx = context.WithoutCancel(ctx)
	job.state.Confirm = nil
	st, raw, ok, err := e.fixRow(ctx, job.pr.ID)
	if err != nil || !ok || st.Confirm == nil {
		return
	}
	st.Confirm = nil
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if _, err := e.st.SetKVIf(ctx, KVPRFix(job.pr.ID), raw, string(b)); err != nil {
		e.log.Warn("fix: take the confirming mark off the loop", "subject", job.subject(), "err", err)
	}
}

// noteFixPush records head, the push the job's settle is about to confirm,
// and the commits rep lists as pushed, as fixPushConfirming (kvPRFixPush):
// a restart that cuts the confirmation then fails the fix as unconfirmed
// (recoverFixes), which the poll can settle late.
func (e *Engine) noteFixPush(ctx context.Context, job *fixJob, rep FixReport, head string, typed bool) {
	rec := job.pushRecord(fixPushConfirming, head, e.now().UTC(), typed)
	for _, c := range rep.Pushed {
		rec.Commits = append(rec.Commits, c.SHA)
	}
	if err := writeKVJSON(ctx, e, kvPRFixPush(job.pr.ID), "fix push", rec); err != nil {
		e.warnUnlessStopped(ctx, err, "record the fixer's push to confirm", "subject", job.subject())
	}
}

// noteRepliesNamed records on the job's push record, when the settle left
// one, where the settle looked for the author's replies without the fix
// marker: in the threads (fixPush.RepliesNamed) and on the conversation
// (fixPush.ConversationNamed), so a later settle of the push looks only
// where it could not.
func (e *Engine) noteRepliesNamed(ctx context.Context, job *fixJob, threads, conversation bool) {
	key := kvPRFixPush(job.pr.ID)
	raw, ok, err := e.st.GetKV(ctx, key)
	var rec fixPush
	if err != nil || !ok || json.Unmarshal([]byte(raw), &rec) != nil || rec.Run != job.run.ID {
		return
	}
	if (!threads || rec.RepliesNamed) && (!conversation || rec.ConversationNamed) {
		return
	}
	rec.RepliesNamed, rec.ConversationNamed = rec.RepliesNamed || threads, rec.ConversationNamed || conversation
	if b, err := json.Marshal(rec); err == nil {
		if _, err := e.st.SetKVIf(ctx, key, raw, string(b)); err != nil {
			e.warnUnlessStopped(ctx, err, "record the look for unmarked replies", "subject", job.subject())
		}
	}
}

// dropFixPush removes the PR's push record, if any.
func (e *Engine) dropFixPush(ctx context.Context, prID int64) {
	e.delKV(ctx, kvPRFixPush(prID))
}

// dropFixPushRun removes the PR's push record, if any, for a new loop that
// starts (claimFixRow), and abandons the fixer's run it names while that
// run is active, as dropConfirming does: an open run would hold the PR's
// slot and its cleanup for good.
func (e *Engine) dropFixPushRun(ctx context.Context, prID int64) {
	if rec, ok, _ := readKVJSON[fixPush](ctx, e, kvPRFixPush(prID), "fix push"); ok {
		if run, err := e.st.RunByID(ctx, rec.Run); err == nil {
			e.abandonFixRun(ctx, run, "a new fix loop started while the push of this run waited for GitHub")
		}
	}
	e.dropFixPush(ctx, prID)
}

// stopConfirming finishes the stop of a loop whose settle confirmed its
// push (st, its row, which the stop removed): no job ends it, so the
// fixer's run at the loop's report is abandoned, the push's record goes,
// and the slot goes back to the head the fixer started from, as a stop of
// a fix job leaves it (stopFix), so a new fix's guard finds the reviewed
// head there. held says the stop holds the PR's reservation; without it the
// slot is put back under one taken here, and is left as it is when a job
// holds the PR. It returns what to add to the stop's note.
func (e *Engine) stopConfirming(ctx context.Context, repo store.Repo, pr store.PR, st FixState, held bool) string {
	ctx = context.WithoutCancel(ctx)
	if run, ok := e.runAtReport(ctx, pr.ID, st.Report); ok {
		e.abandonFixRun(ctx, run, "the fix loop was stopped while its settle confirmed the push")
	}
	e.dropFixPush(ctx, pr.ID)
	if !held {
		if !e.reserve(pr.ID, &roundHandle{cancel: func() {}, fix: true, stop: roundStop{ended: true}}) {
			return ""
		}
		defer e.unreserve(pr.ID)
	}
	job := e.loopJob(ctx, repo, pr, st)
	if job.slot.ID == 0 || st.Head == "" {
		return ""
	}
	if err := e.resyncFixSlot(ctx, job, st.Head); err != nil {
		return fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
	}
	return ", slot " + job.slot.Name + " back at " + textx.ShortSHA(st.Head)
}

// runAtReport is the PR's fix run whose report is report (ok false: none).
func (e *Engine) runAtReport(ctx context.Context, prID int64, report string) (store.Run, bool) {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil || report == "" {
		return store.Run{}, false
	}
	for _, r := range runs {
		if r.Kind == store.RunFix && deref(r.ReportPath) == report {
			return r, true
		}
	}
	return store.Run{}, false
}

// awaitPush leaves the job's loop in its settle when GitHub did not answer
// within fixConfirmWindow (u): the row stays in phase fixing, marked
// confirming, the record stays fixPushConfirming, and the job lets go of
// the PR; the poll of the next ticks tries again (settlePushes), up to
// fixConfirmCap (capPushes). A fix.push_confirming event says so.
func (e *Engine) awaitPush(ctx context.Context, job *fixJob, u pushUnanswered) {
	if job.state.Confirm == nil { // a window with no transient failure cannot end so; the mark failed
		e.markConfirming(ctx, job, u.head)
	}
	data := job.eventData()
	data["head"] = u.head
	e.event(ctx, "info", job.subject(), "fix.push_confirming", fmt.Sprintf("GitHub did not answer for %s while the settle confirmed the push of %s (%s): "+
		"the next ticks try again, for at most %s in all", humanDuration(fixConfirmWindow), textx.ShortSHA(u.head), oneLine(u.err.Error(), 200),
		humanDuration(fixConfirmCap)), data)
}

// failUnconfirmed fails the job's fix as unconfirmed (msg, the settle's
// words): the PR's record turns fixPushUnconfirmed, the typed kind the poll
// settles late (settlePushes), then the fix fails (failFixKind), its
// fix.failed event carrying the kind.
func (e *Engine) failUnconfirmed(ctx context.Context, job *fixJob, head, msg string, typed bool) {
	rec, ok, _ := readKVJSON[fixPush](ctx, e, kvPRFixPush(job.pr.ID), "fix push")
	if !ok || rec.Run != job.run.ID || rec.Pushed != head {
		rec = job.pushRecord(fixPushUnconfirmed, head, e.now().UTC(), typed)
	}
	rec.Kind, rec.Failed = fixPushUnconfirmed, e.now().UTC()
	if err := writeKVJSON(ctx, e, kvPRFixPush(job.pr.ID), "fix push", rec); err != nil {
		e.warnUnlessStopped(ctx, err, "record the unconfirmed push", "subject", job.subject())
	}
	e.failFixKind(ctx, job, msg, "", typed, fixPushUnconfirmed)
}

// fixPushRow is a push record as read: the record and the row as stored.
type fixPushRow struct {
	rec fixPush
	raw string
}

// fixPushes are the push records of the PRs (by registry id), all of them
// or, with repo (ID != 0), those of its PRs; a row that does not read goes.
func (e *Engine) fixPushes(ctx context.Context, repo store.Repo) map[int64]fixPushRow {
	vals, err := e.st.KVValues(ctx, "pr.*.fix_push")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the fixers' pushes to confirm")
		return nil
	}
	return e.pushRows(ctx, vals, repo)
}

// pushRows are the push records among the kv rows vals (fixPushes).
func (e *Engine) pushRows(ctx context.Context, vals map[string]string, repo store.Repo) map[int64]fixPushRow {
	out := map[int64]fixPushRow{}
	for key, raw := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix_push", &id); err != nil || kvPRFixPush(id) != key {
			continue
		}
		var rec fixPush
		if json.Unmarshal([]byte(raw), &rec) != nil {
			e.delKV(ctx, key)
			continue
		}
		if repo.ID != 0 {
			if pr, err := e.st.PRByID(ctx, id); err != nil || pr.RepoID != repo.ID {
				continue
			}
		}
		out[id] = fixPushRow{rec: rec, raw: raw}
	}
	return out
}

// pushSubject is the steps' subject of the settle of a confirmed push of
// run: its own, which no new round or loop resets.
func pushSubject(run string) string { return "fix-push:" + run }

// pushFirstStep is the first step of the settle of rec's push: its name
// tells a late settle from one in the loop's settle.
func pushFirstStep(rec fixPush) string {
	if rec.Kind == fixPushUnconfirmed {
		return "fix.push_confirmed_late"
	}
	return "fix.push_confirmed"
}

// pushStarted reports whether the settle of rec's push began: its first
// step is done, so it is resumed whatever the PR's head is now.
func (e *Engine) pushStarted(ctx context.Context, rec fixPush) bool {
	done, err := e.pushBegan(ctx, rec)
	return err == nil && done
}

// pushBegan is pushStarted with the error of a read of the steps that
// failed (the drain counts such a settle as work: reboot_drain_fix.go).
func (e *Engine) pushBegan(ctx context.Context, rec fixPush) (bool, error) {
	return steps.Done(ctx, e.st, pushSubject(rec.Run), pushFirstStep(rec))
}

// confirmingLoop reports whether pr's loop row is the loop whose settle
// confirms rec's push: in phase fixing, at rec's report, and no held
// merge's push (fix_held_merge.go), whose row names no report and which
// confirms nothing.
func (e *Engine) confirmingLoop(ctx context.Context, pr store.PR, rec fixPush) bool {
	st, ok, err := e.fixState(ctx, pr.ID)
	return err == nil && ok && st.Phase == FixPhaseFixing && st.By != heldPushBy && st.Report == rec.Job.State.Report
}

// settlePushes looks, at the poll of repo once its rows took the radar's
// heads, at the pushes its PRs' fixes settle (kvPRFixPush): radar is the
// PRs GitHub lists open, with their heads. A PR a job holds is left to it.
func (e *Engine) settlePushes(ctx context.Context, repo store.Repo, radar []github.PRRadar) {
	if e.d.DryRun {
		return
	}
	rows := e.fixPushes(ctx, repo)
	if len(rows) == 0 {
		return
	}
	heads := make(map[int]string, len(radar))
	for _, p := range radar {
		heads[p.Number] = p.HeadRefOid
	}
	for id, row := range rows {
		pr, err := e.st.PRByID(ctx, id)
		if err != nil || e.roundActive(id) {
			continue
		}
		head, seen := heads[pr.Number]
		e.decidePush(ctx, repo, pr, row, head, seen)
	}
}

// decidePush decides on row, the push record of pr, at the poll that read
// head as the PR's head on GitHub (seen false: the radar did not list it).
// A settle that began is resumed. A loop still in its settle
// (fixPushConfirming) is settled as usual once GitHub shows its push, waits
// while GitHub shows an earlier push of the fixer's (fixPush.earlierPush),
// and fails as unconfirmed on any other head (GitHub answered); its record
// goes, its run abandoned, when its loop is gone (an abort). An unconfirmed
// fix (fixPushUnconfirmed, the typed kind and no other) is settled late
// once GitHub shows exactly its push; its record goes when the PR moved on
// (another head, a new loop, a review of the push posted since, a PR no
// longer open), and waits while GitHub shows the head the fixer started
// from or an earlier push of the fixer's. A record of any other kind goes.
func (e *Engine) decidePush(ctx context.Context, repo store.Repo, pr store.PR, row fixPushRow, head string, seen bool) {
	rec := row.rec
	if e.pushStarted(ctx, rec) {
		e.settleConfirmedPush(ctx, repo, pr, row)
		return
	}
	switch rec.Kind {
	case fixPushConfirming:
		switch {
		case !e.confirmingLoop(ctx, pr, rec):
			e.dropConfirming(ctx, pr, row)
		case !seen, rec.earlierPush(head):
		case head == rec.Pushed:
			e.settleConfirmedPush(ctx, repo, pr, row)
		default:
			e.failPush(ctx, repo, pr, row, fmt.Errorf("the PR's head is %s, not the pushed %s", textx.ShortSHA(head), textx.ShortSHA(rec.Pushed)))
		}
	case fixPushUnconfirmed:
		if why := e.lateGone(ctx, pr, rec); why != "" {
			e.log.Info("fix: an unconfirmed push is settled no more", "subject", prSubject(repo, pr.Number), "why", why)
			e.dropPushRow(ctx, pr.ID, row)
			return
		}
		switch {
		case !seen, head == rec.Job.Head, rec.earlierPush(head), !lateMayRun(pr):
		case head == rec.Pushed:
			e.settleConfirmedPush(ctx, repo, pr, row)
		default:
			e.log.Info("fix: an unconfirmed push is settled no more", "subject", prSubject(repo, pr.Number), "why",
				"the PR's head moved on to "+textx.ShortSHA(head))
			e.dropPushRow(ctx, pr.ID, row)
		}
	default:
		e.dropPushRow(ctx, pr.ID, row)
	}
}

// lateGone says why the unconfirmed push rec of pr is settled no more ("" =
// it may be): a loop runs on the PR, which is no longer open, or magnum
// posted a review of the pushed head since the fix failed (reviewedSince).
// A reviewed_sha at the push is no review: the poll settles a push of
// comments, whitespace or docs alone as reviewed with no round
// (settleTrivial), which the late settle's forced re-review still follows,
// as the settle's does.
func (e *Engine) lateGone(ctx context.Context, pr store.PR, rec fixPush) string {
	switch _, running, err := e.fixState(ctx, pr.ID); {
	case err != nil || running:
		return "a fix loop runs on the PR"
	case pr.GHState != store.GHOpen:
		return "the PR is " + strings.ToLower(pr.GHState)
	case e.reviewedSince(ctx, pr, rec):
		return "magnum posted a review of " + textx.ShortSHA(rec.Pushed) + " since"
	}
	return ""
}

// reviewedSince reports whether the last review magnum posted on pr is one
// of rec's push, posted since the fix failed (or, failing a time, since the
// settle began to confirm the push).
func (e *Engine) reviewedSince(ctx context.Context, pr store.PR, rec fixPush) bool {
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	sum, ok := sums[pr.ID]
	since := rec.Failed
	if since.IsZero() {
		since = rec.Since
	}
	return err == nil && ok && sum.SHA == rec.Pushed && !sum.At.Before(since)
}

// lateMayRun reports whether pr is where a late settle takes it on: reviewed,
// waiting for a round, or needing attention (the settle clears the fix's).
// Elsewhere (a round in flight, paused, ineligible) it waits.
func lateMayRun(pr store.PR) bool {
	switch pr.State {
	case store.PRReviewed, store.PRNeedsAttention, store.PRQueued, store.PRRereviewPending:
		return true
	}
	return false
}

// dropPushRow removes row, pr's push record, while it is the one read.
func (e *Engine) dropPushRow(ctx context.Context, prID int64, row fixPushRow) {
	if _, err := e.st.DeleteKVIf(ctx, kvPRFixPush(prID), row.raw); err != nil {
		e.warnUnlessStopped(ctx, err, "drop a fixer's push record", "pr", prID)
	}
}

// dropConfirming removes row, the record of a push whose loop is gone (an
// abort ended it while its settle confirmed the push), and abandons the
// fixer's run, which no settle ends now: an active run would hold the PR's
// slot and its cleanup for good.
func (e *Engine) dropConfirming(ctx context.Context, pr store.PR, row fixPushRow) {
	if run, err := e.st.RunByID(ctx, row.rec.Run); err == nil {
		e.abandonFixRun(ctx, run, "the fix loop ended while its settle confirmed the push")
	}
	e.dropPushRow(ctx, pr.ID, row)
}

// capPushes runs each tick, the poll failed or not: a loop still in its
// settle (fixPushConfirming) past fixConfirmCap since the settle began to
// confirm its push fails as unconfirmed, and its record goes, its run
// abandoned, once its loop is gone; an unconfirmed fix's record goes
// fixPushKeep after it failed; a record of any other kind goes. A loop
// marked confirming whose record is gone ends too (endOrphanConfirms). A PR
// a job holds, and a settle that began, are left alone.
func (e *Engine) capPushes(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	now := e.now()
	for id, row := range e.fixPushes(ctx, store.Repo{}) {
		rec := row.rec
		if e.roundActive(id) || e.pushStarted(ctx, rec) {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e.dropPushRow(ctx, id, row)
			continue
		case err != nil:
			continue
		}
		switch rec.Kind {
		case fixPushConfirming:
			if !e.confirmingLoop(ctx, pr, rec) {
				e.dropConfirming(ctx, pr, row)
				continue
			}
			if now.Sub(rec.Since) < fixConfirmCap {
				continue
			}
			repo, err := e.st.RepoByID(ctx, pr.RepoID)
			if err != nil {
				continue
			}
			e.failPush(ctx, repo, pr, row, fmt.Errorf("GitHub did not answer for %s", humanDuration(fixConfirmCap)))
		case fixPushUnconfirmed:
			if now.Sub(rec.Failed) >= fixPushKeep {
				e.dropPushRow(ctx, id, row)
			}
		default:
			e.dropPushRow(ctx, id, row)
		}
	}
	e.endOrphanConfirms(ctx)
}

// endOrphanConfirms ends each loop marked confirming (FixState.Confirm, in
// phase fixing) that no job holds and whose push record is gone or another
// loop's: a record that could not be written, or one a settle that could
// not rebuild its job dropped. Such a loop would hold its PR's rounds until
// a restart.
func (e *Engine) endOrphanConfirms(ctx context.Context) {
	vals, err := e.st.KVValues(ctx, "pr.*.fix")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the fix loops that confirm a push")
		return
	}
	var recs map[int64]fixPushRow
	for key, raw := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix", &id); err != nil || KVPRFix(id) != key {
			continue
		}
		st, ok := ParseFixState(raw)
		if !ok || st.Phase != FixPhaseFixing || st.Confirm == nil || st.By == heldPushBy || e.roundActive(id) {
			continue
		}
		if recs == nil {
			recs = e.fixPushes(ctx, store.Repo{})
		}
		if row, ok := recs[id]; ok && row.rec.Kind == fixPushConfirming && row.rec.Job.State.Report == st.Report {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil || !e.reserve(id, &roundHandle{cancel: func() {}, fix: true, stop: roundStop{ended: true}}) {
			continue
		}
		e.endConfirming(context.WithoutCancel(ctx), repo, pr, "the record of its confirmation is gone")
		e.unreserve(id)
	}
}

// endConfirming ends, as failed for why, the loop of pr marked confirming
// whose push no settle can confirm (its record gone, or naming a run that
// is gone): the fixer's run at the loop's report failed, the slot back at
// the head the fixer started from (the row's, else the reviewed one, as
// the start's recovery puts it back). The caller holds the PR's
// reservation.
func (e *Engine) endConfirming(ctx context.Context, repo store.Repo, pr store.PR, why string) {
	st, ok, err := e.fixState(ctx, pr.ID)
	if err != nil || !ok || st.Phase != FixPhaseFixing || st.Confirm == nil {
		return
	}
	job := e.loopJob(ctx, repo, pr, st)
	if job.role.Name == "" {
		job.role.Name = st.Role
	}
	if run, ok := e.runAtReport(ctx, pr.ID, st.Report); ok {
		job.run = run
	}
	msg := "fix settle failed: " + pushNotConfirmed{head: st.Confirm.Head, err: errors.New(why)}.Error()
	if head := cmp.Or(st.Head, deref(pr.ReviewedSHA)); job.slot.ID != 0 && head != "" { // loopJob names no head: the loop's row does
		if err := e.resyncFixSlot(ctx, job, head); err != nil {
			msg += fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
		}
	}
	e.failFix(ctx, job, msg, st.Report)
}

// pushJob is the fix job of rec, pr's push record, rebuilt for a settle
// that no fix job runs: the loop as the settle had it (loopJob: the
// repository's watch, pool and fix role, the PR's slot), its run and what
// the record keeps of the job.
func (e *Engine) pushJob(ctx context.Context, repo store.Repo, pr store.PR, rec fixPush) (*fixJob, error) {
	run, err := e.st.RunByID(ctx, rec.Run)
	if err != nil {
		return nil, fmt.Errorf("the fixer's run %s: %w", rec.Run, err)
	}
	j := rec.Job
	job := e.loopJob(ctx, repo, pr, j.State)
	if j.Role != "" {
		job.role.Name = j.Role
	}
	job.run, job.head, job.started, job.login, job.mode, job.touched = run, j.Head, j.Started, j.Login, j.Mode, true
	job.conflict, job.merged = j.Conflict.conflict(), j.Merged.conflict()
	job.forMerge, job.alone, job.described, job.baseChecked = j.ForMerge, j.Alone, j.Described, j.BaseChecked
	job.state.Late = rec.Kind == fixPushUnconfirmed // fixAttentionKey: its toasts are not its failure's
	return job, nil
}

// failPush fails the fix whose loop is still in its settle (row, pr's
// record) as unconfirmed, for why: under the PR's reservation and to its
// end, whatever stops the tick meanwhile (the writes of a failure a
// shutdown cut would leave the loop half ended), the slot back at the head
// the fixer started from, as any failed settle leaves it. A job that
// cannot be rebuilt (its run gone) ends the loop all the same, once
// (endConfirming), and the record goes.
func (e *Engine) failPush(ctx context.Context, repo store.Repo, pr store.PR, row fixPushRow, why error) {
	if !e.reserve(pr.ID, &roundHandle{cancel: func() {}, fix: true, stop: roundStop{ended: true}}) {
		return
	}
	defer e.unreserve(pr.ID)
	ctx = context.WithoutCancel(ctx)
	job, err := e.pushJob(ctx, repo, pr, row.rec)
	if err != nil {
		e.endConfirming(ctx, repo, pr, why.Error()+"; "+err.Error())
		e.dropPushRow(ctx, pr.ID, row)
		return
	}
	msg := "fix settle failed: " + pushNotConfirmed{head: row.rec.Pushed, err: why}.Error()
	if job.slot.ID != 0 {
		if err := e.resyncFixSlot(ctx, job, job.head); err != nil {
			msg += fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
		}
	}
	e.failUnconfirmed(ctx, job, row.rec.Pushed, msg, row.rec.Typed)
}

// settleConfirmedPush settles the fix of row (pr's record) as pushed, now
// that GitHub shows its push as the PR's head, under the PR's reservation
// (an abort meanwhile is told to try again shortly) and to its end, as a
// fix job's settle runs, whatever stops the tick meanwhile: a shutdown in
// the middle of a step would leave its writes half done under a step
// marked done. It runs in resumable steps of a subject of its own
// (pushSubject), so a restart between them does none of them twice: the
// first says so (fix.push_confirmed for a loop still in its settle,
// fix.push_confirmed_late for a fix that failed for want of it), the next
// puts the slot at the push, a late one's run goes back from failed and
// its attention is cleared (pushReopen), the author's replies without the
// fix marker are named when the settle could not (pushReplies), then what
// a settle does after a confirmed push (settlePushed: settleReport's
// replies and calls the threads leave, the PR row's head, the push's CI,
// the re-review or the loop's end), and last the run's verdict is checked
// (pushRunEnded): a write of it that failed fails that step alone, so the
// next poll finishes it and nothing else. The record goes last.
func (e *Engine) settleConfirmedPush(ctx context.Context, repo store.Repo, pr store.PR, row fixPushRow) {
	rec := row.rec
	if !e.reserve(pr.ID, &roundHandle{cancel: func() {}, fix: true, stop: roundStop{ended: true}}) {
		return
	}
	defer e.unreserve(pr.ID)
	ctx = github.WithOp(context.WithoutCancel(ctx), opFixSettle) // github_cost.go
	subject := prSubject(repo, pr.Number)
	job, err := e.pushJob(ctx, repo, pr, rec)
	if err == nil {
		_, err = e.readFixReport(job)
	}
	if err != nil {
		e.log.Warn("fix: a confirmed push cannot be settled", "subject", subject, "err", err)
		if rec.Kind == fixPushConfirming { // its loop would hold the PR in its settle for good
			e.endConfirming(ctx, repo, pr, "its push cannot be settled: "+err.Error())
		}
		e.dropPushRow(ctx, pr.ID, row)
		return
	}
	sub, read := pushSubject(rec.Run), e.threadsReader(ctx, job)
	for _, s := range []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{pushFirstStep(rec), func(ctx context.Context) error { e.pushConfirmed(ctx, job, rec); return nil }},
		{"fix.push_slot", func(ctx context.Context) error { e.pushSlot(ctx, job, rec.Pushed); return nil }},
		{"fix.push_reopen", func(ctx context.Context) error { return e.pushReopen(ctx, job, rec) }},
		{"fix.push_replies", func(ctx context.Context) error { return e.pushReplies(ctx, job, rec, read) }},
		{"fix.push_settle", func(ctx context.Context) error { return e.settlePushed(ctx, job, rec, read) }},
		{"fix.push_run", func(ctx context.Context) error { return e.pushRunEnded(ctx, job) }},
	} {
		if err := steps.Step(ctx, e.st, sub, s.name, s.fn); err != nil {
			e.log.Warn("fix: the settle of a confirmed push stopped; the next poll resumes it", "subject", subject, "step", s.name, "err", err)
			return
		}
	}
	e.dropPushRow(ctx, pr.ID, row)
}

// pushConfirmed is the first step of the settle of rec's confirmed push:
// its event.
func (e *Engine) pushConfirmed(ctx context.Context, job *fixJob, rec fixPush) {
	data := job.eventData()
	data["head"] = rec.Pushed
	short, now := textx.ShortSHA(rec.Pushed), e.now()
	if rec.Kind == fixPushUnconfirmed {
		e.event(ctx, "info", job.subject(), "fix.push_confirmed_late", fmt.Sprintf("GitHub shows the push of %s as the PR's head %s after the fix failed "+
			"for want of it: magnum settles the fix as pushed", short, humanDuration(now.Sub(rec.Failed).Round(time.Second))), data)
		return
	}
	e.event(ctx, "info", job.subject(), "fix.push_confirmed", fmt.Sprintf("GitHub shows the push of %s as the PR's head %s after the settle began "+
		"to confirm it: the settle goes on", short, humanDuration(now.Sub(rec.Since).Round(time.Second))), data)
}

// pushSlot puts the job's slot at head, the confirmed push, once it is
// still the PR's; one the pool took back meanwhile is left as it is.
func (e *Engine) pushSlot(ctx context.Context, job *fixJob, head string) {
	if job.slot.ID == 0 {
		return
	}
	if err := e.resyncFixSlot(ctx, job, head); err != nil {
		e.log.Info("fix: the slot at the confirmed push", "subject", job.subject(), "slot", job.slot.Name, "err", err)
	}
}

// pushReopen is a step of the settle of rec's confirmed push: a late one's
// run goes back from failed to ended, so the settle's verdict reaches it,
// and the fix's attention is cleared (its row and its run's).
func (e *Engine) pushReopen(ctx context.Context, job *fixJob, rec fixPush) error {
	if rec.Kind != fixPushUnconfirmed {
		return nil
	}
	err := e.st.TransitionRun(ctx, job.run.ID, []string{store.RunFailed}, store.RunEnded, func(u *store.RunUpdate) {
		u.Set("outcome", nil)
		u.Set("error", nil)
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	if kind, _ := e.getKV(ctx, KVPRAttention(job.pr.ID)); kind == attention.KindFix {
		e.delKV(ctx, KVPRAttention(job.pr.ID), kvPRAttentionRun(job.pr.ID))
	}
	return nil
}

// pushReplies is a step of the settle of rec's confirmed push: the
// author's replies without the fix marker are named where the settle could
// not read them (fixPush.RepliesNamed, the threads, and ConversationNamed),
// and only there, when the run may have replied.
func (e *Engine) pushReplies(ctx context.Context, job *fixJob, rec fixPush, read func() ([]github.Thread, error)) error {
	if rec.RepliesNamed && rec.ConversationNamed {
		return nil
	}
	rep, err := e.readFixReport(job)
	if err != nil {
		return err
	}
	if mayHaveReplied(rep) {
		e.nameUnmarkedReplies(ctx, job, read, !rec.RepliesNamed, !rec.ConversationNamed) // fix_external.go: the settle's own look failed in the outage
	}
	return nil
}

// settlePushed is the step of the settle of rec's confirmed push that does
// what a settle does after a push (settleReport), on the fixer's report
// read again and its run as it is now. It fails only when the report cannot
// be read: a write of settleReport's that fails is its own to log, as in
// any settle, and the run's verdict is checked after it (pushRunEnded).
func (e *Engine) settlePushed(ctx context.Context, job *fixJob, rec fixPush, read func() ([]github.Thread, error)) error {
	rep, err := e.readFixReport(job)
	if err != nil {
		return err
	}
	if run, err := e.st.RunByID(ctx, job.run.ID); err == nil {
		job.run = run
	}
	e.settleReport(ctx, job, rep, true, rec.Pushed, rec.Typed, read)
	return nil
}

// pushRunEnded is the last step of the settle of a confirmed push: the
// fixer's run must have its verdict. A run still active (a write of the
// verdict failed, say on a busy registry) gets it now, verified as pushed
// as the settle gives it, and the step fails while it stays active, so the
// next poll tries this step alone again.
func (e *Engine) pushRunEnded(ctx context.Context, job *fixJob) error {
	active := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	run, err := e.st.RunByID(ctx, job.run.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(active, run.State) {
		return nil
	}
	e.finishFixRun(ctx, run, store.RunVerified, "pushed", "")
	if run, err = e.st.RunByID(ctx, job.run.ID); err != nil {
		return err
	}
	if slices.Contains(active, run.State) {
		return fmt.Errorf("the fixer's run %s is still %s after the settle: its verdict could not be written", run.ID, run.State)
	}
	return nil
}

// recoverConfirming ends, at the start, the loop of pr (st, in phase
// fixing) whose settle confirmed its fixer's push when the daemon stopped
// (its record, fixPushConfirming, of the loop's report): as unconfirmed
// (failUnconfirmed), its fixer stopped and its slot back at the head it
// started from first, so the poll that finds the push on GitHub settles it
// late. One whose settle of the confirmed push began is left to the poll,
// which resumes it. It reports whether it took the loop (false: any other
// loop the recovery ends as lost).
func (e *Engine) recoverConfirming(ctx context.Context, repo store.Repo, pr store.PR, st FixState) bool {
	rec, ok, err := readKVJSON[fixPush](ctx, e, kvPRFixPush(pr.ID), "fix push")
	switch {
	case err != nil, !ok, rec.Kind != fixPushConfirming, rec.Job.State.Report != st.Report:
		return false
	case e.pushStarted(ctx, rec):
		return true
	}
	job, err := e.pushJob(ctx, repo, pr, rec)
	if err != nil {
		e.log.Warn("recovery: the job of a push to confirm", "subject", prSubject(repo, pr.Number), "err", err)
		return false
	}
	job.ciOutcome = ciOutcomeLost // ci_fix.go: a CI fix the restart ended
	why := pushNotConfirmed{head: rec.Pushed, err: errors.New("a daemon restart cut the settle while it confirmed the push of " + textx.ShortSHA(rec.Pushed))}
	e.failUnconfirmed(ctx, job, rec.Pushed, "fix settle failed: "+why.Error()+e.stopFixerAt(ctx, job, job.head), rec.Typed)
	return true
}
