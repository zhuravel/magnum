package engine

// The whiteboard job: after a review magnum posted on a head, the whiteboard
// role's agent writes a dev.fast Whiteboard of the PR at that head (an
// interactive document with diagrams and code links, in Whiteboard Desktop)
// for the operator to read when they choose. It starts by itself after a
// posted review when the watch says whiteboard = true (whiteboardAfterRound,
// after the fix loop's hook), or when `magnum whiteboard <ref>` asks for it
// (ReqWhiteboard). Like the fix job (fix.go) it holds the PR with a
// reservation of its own (roundHandle.whiteboard) for its whole run, in the
// PR's slot, but yields it to the operator: a review, fix or open request
// stops the writer and goes on once the job let go of the PR
// (yieldWhiteboard). Its steps are begin/ok/fail steps on the PR's subject:
// guard (the slot is the PR's, nobody is in it, it holds the reviewed head),
// start (the writer's session in the PR's workspace, a run of kind
// whiteboard, its prompt) and wait (the run's end; a push, a close or an
// abort stops it). The settle reads the writer's two-line report and keeps
// the whiteboard in the PR's KVPRWhiteboard row, so the next head's
// whiteboard moves the same one. Whiteboard down, the PR busy or a failure
// asks nothing of the operator: an event says so, and the next posted
// review is the next chance. A drain for a restart is the exception: its
// skip is remembered, and the drain's end makes it (whiteboard_pending.go).
// The job is no round: the daily cap, the interval and quiet hours do not
// apply, and max_concurrent_reviews does not count it.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqWhiteboard starts the whiteboard job on a PR's reviewed head
// (WhiteboardPayload).
const ReqWhiteboard = "whiteboard"

// WhiteboardPayload is a `magnum whiteboard` request: write the whiteboard
// of the PR's reviewed head.
type WhiteboardPayload struct {
	PRTarget
}

// WhiteboardCommand is the Whiteboard CLI magnum runs: `whiteboard` on PATH
// (the daemon's LaunchAgent PATH has ~/.local/bin).
const WhiteboardCommand = "whiteboard"

// whiteboardCmdTimeout bounds a call of the Whiteboard CLI.
const whiteboardCmdTimeout = 15 * time.Second

// WhiteboardStatusCmd is Whiteboard's readiness check, `whiteboard api
// whiteboard_status`: it exits 0 only while a Whiteboard server runs
// (Whiteboard Desktop, or `whiteboard server start`). A failed exit is an
// answer (Probe).
func WhiteboardStatusCmd() execx.Cmd {
	return execx.Cmd{Name: WhiteboardCommand, Args: []string{"api", "whiteboard_status"}, Timeout: whiteboardCmdTimeout, Probe: true,
		Label: "whiteboard status"}
}

// WhiteboardOpenCmd opens the whiteboard session in Whiteboard Desktop,
// focused: `whiteboard app pick --session <id> --focus`.
func WhiteboardOpenCmd(session string) execx.Cmd {
	return execx.Cmd{Name: WhiteboardCommand, Args: []string{"app", "pick", "--session", session, "--focus"}, Timeout: whiteboardCmdTimeout,
		Label: "whiteboard open"}
}

// whiteboardDownText is why no whiteboard starts while Whiteboard is down,
// errWhiteboardDown its error.
const whiteboardDownText = "Whiteboard is not running (start Whiteboard Desktop or `whiteboard server start`)"

var errWhiteboardDown = errors.New(whiteboardDownText) //nolint:staticcheck // ST1005: Whiteboard is the app's name

// whiteboardLostText is why a writer a restart left at work failed.
const whiteboardLostText = "whiteboard run lost in a restart"

const (
	// whiteboardAuto is who starts a job after a posted review.
	whiteboardAuto = "the review"
	// whiteboardBy is who starts one by hand.
	whiteboardBy = "magnum whiteboard"
	// whiteboardDone is a written whiteboard's run outcome.
	whiteboardDone = "done"
)

// whiteboardSessionRe is a Whiteboard session id as a report may give it:
// it goes into a later prompt and into `whiteboard app pick`'s argv, so it
// is a plain token, never the writer's words.
var whiteboardSessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// KVPRWhiteboard holds the whiteboard of a PR's latest written head
// (WhiteboardRecord as JSON).
func KVPRWhiteboard(prID int64) string { return fmt.Sprintf("pr.%d.whiteboard", prID) }

// WhiteboardRecord is a PR's whiteboard: its Whiteboard session, the head it
// describes, when it was written and by which role.
type WhiteboardRecord struct {
	Session string    `json:"session"`
	Head    string    `json:"head"`
	At      time.Time `json:"at"`
	Role    string    `json:"role"`
}

// ParseWhiteboardRecord reads a KVPRWhiteboard value; ok is false for one
// that names no session or head.
func ParseWhiteboardRecord(s string) (WhiteboardRecord, bool) {
	var r WhiteboardRecord
	if s == "" || json.Unmarshal([]byte(s), &r) != nil || r.Session == "" || r.Head == "" {
		return WhiteboardRecord{}, false
	}
	return r, true
}

// whiteboardRecord reads the PR's whiteboard; ok is false when it has none.
func (e *Engine) whiteboardRecord(ctx context.Context, prID int64) (WhiteboardRecord, bool, error) {
	v, ok, err := e.st.GetKV(ctx, KVPRWhiteboard(prID))
	if err != nil || !ok {
		return WhiteboardRecord{}, false, err
	}
	rec, ok := ParseWhiteboardRecord(v)
	return rec, ok, nil
}

// whiteboardJob is one whiteboard: the PR, its watch and slot, the role,
// the head it describes, the whiteboard of an earlier head it moves
// (session, "" = a first one) and the review it ends with.
type whiteboardJob struct {
	repo    store.Repo
	watch   config.Watch
	pool    *config.Pool
	pr      store.PR
	slot    store.Slot
	role    config.Role
	head    string
	session string
	review  store.ReviewSummary
	round   int       // the review's round, the run's
	run     store.Run // the writer's run, once whiteboardStart created it
	started time.Time
	// touched: the writer may have changed the slot (its prompt was sent),
	// so the job's end resyncs the slot at the head.
	touched bool
}

func (j *whiteboardJob) label() string   { return fmt.Sprintf("%s#%d", j.repo.FullName(), j.pr.Number) }
func (j *whiteboardJob) subject() string { return prSubject(j.repo, j.pr.Number) }

// asRound is the job as a roundJob, for the helpers rounds share with it
// (paneEnv).
func (j *whiteboardJob) asRound() *roundJob {
	return &roundJob{pr: j.pr, repo: j.repo, watch: j.watch, pool: j.pool, slot: j.slot, hasSlot: j.slot.ID != 0}
}

// what is the job in words: "writes the whiteboard of a1a1a1a in review1",
// or "moves the whiteboard <session> to a1a1a1a in review1".
func (j *whiteboardJob) what() string {
	if j.session != "" {
		return fmt.Sprintf("moves the whiteboard %s to %s in %s", j.session, textx.ShortSHA(j.head), j.slot.Name)
	}
	return fmt.Sprintf("writes the whiteboard of %s in %s", textx.ShortSHA(j.head), j.slot.Name)
}

// eventData is the data every whiteboard event carries.
func (j *whiteboardJob) eventData() map[string]any {
	return map[string]any{"run": j.run.ID, "role": j.role.Name, "head": j.head, "session": j.session, "report": deref(j.run.ReportPath)}
}

// requestWhiteboardAsync handles a `magnum whiteboard` request off the
// tick, as an open's restore is: Whiteboard's readiness check may take up
// to whiteboardCmdTimeout, so requestWhiteboard (its refusals, the check,
// the reservation and the start) runs on a goroutine of its own, which
// completes the request; the request stays in flight meanwhile. ctx is the
// daemon's, which the job's derives from.
func (e *Engine) requestWhiteboardAsync(ctx context.Context, id int64, p WhiteboardPayload) {
	e.mu.Lock()
	e.inflight[id] = true
	e.mu.Unlock()
	e.roundWG.Go(func() {
		defer func() {
			e.mu.Lock()
			delete(e.inflight, id)
			e.mu.Unlock()
		}()
		e.safely(ctx, "whiteboard request", "", func() {
			res, err := e.requestWhiteboard(ctx, p)
			if err != nil && ctx.Err() != nil {
				return // daemon shutdown: the request stays pending for the next start
			}
			e.complete(context.WithoutCancel(ctx), id, err, res)
		}, func(ctx context.Context, msg string) {
			e.complete(ctx, id, fmt.Errorf("magnum whiteboard panicked: %s", msg), "")
		})
	})
}

// requestWhiteboard handles `magnum whiteboard` (requestWhiteboardAsync's
// goroutine): the job starts on the PR's reviewed head unless one of
// prepareWhiteboard's refusals applies, or Whiteboard is down, and the
// answer says where it writes. A dry run answers what would start.
func (e *Engine) requestWhiteboard(ctx context.Context, p WhiteboardPayload) (string, error) {
	_, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	if e.d.DryRun {
		job, err := e.prepareWhiteboard(ctx, pr, whiteboardBy)
		if err != nil {
			return "", err
		}
		e.rec.Record(ctx, job.subject(), "whiteboard", job.role.Name+" "+job.what())
		return fmt.Sprintf("dry run: would write the whiteboard of %s in %s", textx.ShortSHA(job.head), job.slot.Name), nil
	}
	job, err := e.startWhiteboard(ctx, pr, whiteboardBy)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("writing the whiteboard of %s in %s", textx.ShortSHA(job.head), job.slot.Name), nil
}

// whiteboardAfterRound starts the whiteboard job after a round that posted
// a review on the PR's head (autoWhiteboard). launch calls it on the
// round's goroutine once the round's reservation is gone and the fix loop's
// hook ran, with the daemon's context.
func (e *Engine) whiteboardAfterRound(ctx context.Context, pr store.PR, res pipeline.RoundResult) {
	if ctx.Err() != nil || e.d.DryRun || res.Outcome != pipeline.OutcomePosted {
		return
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil {
		e.log.Warn("whiteboard: the PR after its round", "pr", pr.ID, "err", err)
		return
	}
	repo, err := e.st.RepoByID(ctx, cur.RepoID)
	if err != nil {
		e.log.Warn("whiteboard: the PR's repository", "pr", pr.ID, "err", err)
		return
	}
	e.autoWhiteboard(ctx, repo, cur, cmp.Or(res.TargetSHA, res.ReviewCommit))
}

// autoWhiteboard starts the whiteboard job on cur's head target, a head
// magnum posted a review on, when its watch asks for whiteboards and has a
// whiteboard role, the PR is still open, not muted and at that head, and
// the head has no whiteboard yet; otherwise it starts none and says why
// (none, for a log line; "" when it started the job or recorded a skip).
// Whiteboard down, or the PR held (a fix loop the hook started, a round),
// records whiteboard.skipped and nothing else: the next posted review is
// the next chance. A drain for a restart records the skip too, but
// remembers the head (rememberWhiteboard), which the drain's end makes
// (startPendingWhiteboards): a drain waits for the rounds in flight, so
// their reviews post inside it. ctx is the daemon's.
func (e *Engine) autoWhiteboard(ctx context.Context, repo store.Repo, cur store.PR, target string) (none string) {
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil || !e.cfg.WhiteboardFor(w) {
		return "the watch asks for no whiteboards"
	}
	if _, ok := e.cfg.WhiteboardRoleFor(w); !ok {
		return "the watch has no whiteboard role"
	}
	switch {
	case cur.GHState != store.GHOpen:
		return "the PR is not open"
	case cur.Muted:
		return "the PR is muted"
	case target == "" || cur.HeadSHA != target || deref(cur.ReviewedSHA) != target:
		return "the PR is no longer at the reviewed head"
	}
	subject := prSubject(repo, cur.Number)
	switch rec, ok, err := e.whiteboardRecord(ctx, cur.ID); {
	case err != nil:
		e.log.Warn("whiteboard: the PR's whiteboard cannot be read; none starts", "subject", subject, "err", err)
		return "the PR's whiteboard cannot be read"
	case ok && rec.Head == target:
		return "the head has its whiteboard"
	}
	_, err := e.startWhiteboard(ctx, cur, whiteboardAuto)
	if err == nil {
		return ""
	}
	msg := err.Error()
	if errors.Is(err, errFixDraining) && e.rememberWhiteboard(ctx, subject, cur.ID, target) {
		msg = whiteboardDrainText
	}
	e.event(ctx, "info", subject, "whiteboard.skipped", msg, map[string]any{"head": target})
	return ""
}

// busyRefusal is the refusal of a PR that held is why held: the hook's
// "the PR is busy (...)", the request's "...; try again after it".
func busyRefusal(held, by string) error {
	if by == whiteboardAuto {
		return fmt.Errorf("the PR is busy (%s)", held)
	}
	return fmt.Errorf("%s; try again after it", held)
}

// prepareWhiteboard decides whether the whiteboard job may start on pr and
// gathers what it needs; the error is the refusal, a sentence for the
// request's answer or the skip's event. by is who asks (whiteboardAuto,
// whiteboardBy), which words the refusal of a busy PR.
func (e *Engine) prepareWhiteboard(ctx context.Context, pr store.PR, by string) (*whiteboardJob, error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return nil, err
	}
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	if e.draining(ctx) {
		return nil, errFixDraining
	}
	w := e.cfg.WatchFor(full)
	if w == nil {
		return nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
	}
	role, ok := e.cfg.WhiteboardRoleFor(w)
	switch name := strings.TrimSpace(e.cfg.WhiteboardRoleName(w)); {
	case !ok && name == "":
		return nil, errors.New("no whiteboard role: whiteboard_role is empty")
	case !ok:
		return nil, fmt.Errorf("no whiteboard role: whiteboard_role %q names no declared role with whiteboard = true", name)
	}
	if pr.GHState != store.GHOpen {
		return nil, fmt.Errorf("%s is not open (GitHub %s): its whiteboard has no reader", label, pr.GHState)
	}
	if e.roundActive(pr.ID) {
		return nil, busyRefusal(e.heldReason(pr.ID), by)
	}
	if slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused {
		return nil, busyRefusal(fmt.Sprintf("a round is in progress (%s)", pr.State), by)
	}
	head := textx.ShortSHA(pr.HeadSHA)
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return nil, err
	}
	sum, ok := sums[pr.ID]
	switch {
	case !ok || sum.SHA != pr.HeadSHA || deref(pr.ReviewedSHA) != pr.HeadSHA:
		return nil, fmt.Errorf("no review on %s", head)
	case sum.URL == "":
		return nil, fmt.Errorf("%s: the review on %s has no URL", label, head)
	}
	rec, recorded, err := e.whiteboardRecord(ctx, pr.ID)
	switch {
	case err != nil:
		return nil, err
	case recorded && rec.Head == pr.HeadSHA:
		return nil, fmt.Errorf("%s has its whiteboard %s; magnum whiteboard %s --open opens it", head, rec.Session, label)
	}
	reviewRun, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		return nil, fmt.Errorf("%s: the review's run %s: %w", label, sum.RunID, err)
	}
	slot, has, err := e.slotOf(ctx, pr.ID)
	switch {
	case err != nil:
		return nil, err
	case !has:
		return nil, fmt.Errorf("%s has no checkout of its reviewed head: its slot was handed back", label)
	case slot.State == store.SlotBroken || slot.State == store.SlotLost:
		return nil, fmt.Errorf("%s: %s", label, unusableSlotSentence(slot))
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return nil, fmt.Errorf("%s: slot %s is %s", label, slot.Name, slot.State)
	case slot.Pinned:
		return nil, fmt.Errorf("%s: slot %s is pinned (magnum unpin)", label, slot.Name)
	case slot.HoldReason != nil:
		return nil, fmt.Errorf("%s: %s", label, slotHeldReason(slot))
	case deref(slot.CheckedOutSHA) != pr.HeadSHA:
		return nil, fmt.Errorf("slot %s holds %s, not the reviewed head %s", slot.Name, textx.ShortSHA(deref(slot.CheckedOutSHA)), head)
	}
	if why := e.kindPauseReason(ctx, []string{role.AgentKind()}); why != "" {
		return nil, fmt.Errorf("%s: %s", label, why)
	}
	job := &whiteboardJob{repo: repo, watch: *w, pool: e.cfg.PoolFor(full), pr: pr, slot: slot, role: role, head: pr.HeadSHA,
		review: sum, round: reviewRun.Round}
	if recorded {
		job.session = rec.Session // an earlier head's: the writer moves it
	}
	return job, nil
}

// whiteboardReady checks that Whiteboard runs (WhiteboardStatusCmd through
// the daemon's runner); the error is errWhiteboardDown.
func (e *Engine) whiteboardReady(ctx context.Context) error {
	if e.d.Runner == nil {
		return errWhiteboardDown
	}
	if _, err := e.d.Runner.Run(ctx, WhiteboardStatusCmd()); err != nil {
		e.log.Info("whiteboard: not ready", "err", err)
		return errWhiteboardDown
	}
	return nil
}

// startWhiteboard starts the whiteboard job on pr under prepareWhiteboard's
// refusals once Whiteboard answers: the job reserves the PR
// (roundHandle.whiteboard) and runs on a goroutine of its own under the
// reservation (runWhiteboard), whose context derives from ctx: pass the
// daemon's, never a round's. It returns a copy of the job as it started.
func (e *Engine) startWhiteboard(ctx context.Context, pr store.PR, by string) (*whiteboardJob, error) {
	job, err := e.prepareWhiteboard(ctx, pr, by)
	if err != nil {
		return nil, err
	}
	if e.d.Agents == nil || e.d.Slots == nil {
		return nil, errors.New("the daemon has no agents or slots manager")
	}
	if err := e.whiteboardReady(ctx); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, whiteboard: true}) {
		cancel()
		return nil, busyRefusal(e.heldReason(pr.ID), by)
	}
	job.started = e.now()
	started := *job
	e.roundWG.Go(func() {
		defer e.unreserveWhiteboard(pr.ID)
		defer e.workEnded(rctx, pr.ID) // the idle park's clock (park.go), before the reservation drops
		e.safely(rctx, "whiteboard", job.subject(), func() { e.runWhiteboard(rctx, job) },
			func(ctx context.Context, msg string) { e.whiteboardPanicked(ctx, job, msg) })
	})
	return &started, nil
}

// whiteboardStopped is the wait's stop of a job whose PR moved on: why is
// "the head moved to <sha7>" or "the PR closed".
type whiteboardStopped struct{ why string }

func (s *whiteboardStopped) Error() string { return s.why }

// runWhiteboard runs the job under its reservation: guard, start and wait,
// then the settle. `magnum abort` cancels ctx and leaves its orders on the
// reservation, a review, fix or open request its yield (yieldWhiteboard), a
// close on GitHub its mark (stopClosedRound): the writer is stopped and the
// job ends with whiteboard.stopped (stopWhiteboard), as it does when the
// wait sees a push or a close; a writer whose run ended before a yield
// stopped it is settled. A daemon shutdown cancels ctx with none of them:
// the run stays for the next start (recoverWhiteboards).
func (e *Engine) runWhiteboard(ctx context.Context, job *whiteboardJob) {
	subject := job.subject()
	if err := steps.ResetSubject(ctx, e.st, subject); err != nil {
		e.log.Warn("whiteboard: new step generation", "subject", subject, "err", err)
	}
	failed := ""
	err := steps.Step(ctx, e.st, subject, "whiteboard.guard", func(ctx context.Context) error { return e.whiteboardGuard(ctx, job) })
	if err != nil {
		failed = "whiteboard guard failed: "
	} else if err = steps.Step(ctx, e.st, subject, "whiteboard.start", func(ctx context.Context) error { return e.whiteboardStart(ctx, job) }); err != nil {
		failed = "whiteboard run failed: "
	} else if err = steps.Step(ctx, e.st, subject, "whiteboard.wait", func(ctx context.Context) error { return e.whiteboardWait(ctx, job) }); err != nil {
		failed = "whiteboard run failed: "
	}
	fctx := context.WithoutCancel(ctx)
	orders, closed, superseded := e.closeWhiteboardHandle(job.pr.ID)
	var stopped *whiteboardStopped
	switch {
	case len(orders) > 0:
		e.stopWhiteboard(fctx, job, "aborted", orders)
	case superseded != "" && err == nil:
		e.settleWhiteboard(fctx, job) // the writer was done: its whiteboard stands
	case superseded != "":
		e.stopWhiteboard(fctx, job, "superseded by "+superseded, nil)
	case closed:
		e.stopWhiteboard(fctx, job, "the PR closed", nil)
	case errors.As(err, &stopped):
		e.stopWhiteboard(fctx, job, stopped.why, nil)
	case ctx.Err() != nil:
		e.log.Info("whiteboard interrupted by a shutdown; the next start fails its run", "subject", subject)
	case err != nil:
		e.failWhiteboard(fctx, job, failed+err.Error())
	default:
		e.settleWhiteboard(fctx, job)
	}
}

// closeWhiteboardHandle ends the job's part that an abort, a yield or a
// close can stop (closeFixHandle's) and returns the abort and ignore orders
// that arrived before, whether a close on GitHub stopped it, and who the
// first request it yields to is ("" = none): a request that yields later
// waits for the job's end (unreserveWhiteboard).
func (e *Engine) closeWhiteboardHandle(prID int64) (orders []stopOrder, closed bool, superseded string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.rounds[prID]
	if h == nil {
		return nil, false, ""
	}
	h.stop.ended = true
	if len(h.stop.yields) > 0 {
		superseded = h.stop.yields[0].by
	}
	return slices.Clone(h.stop.reqs), h.stop.closed != nil, superseded
}

// whiteboardGuard checks that the writer may work in the job's slot: the
// slot is still the PR's, no round is in flight, the head is still the
// reviewed one and the slot holds it, the head has no whiteboard yet, and
// Slots.Guard finds no person in the slot and no HEAD drift. The slot's
// last_used_at moves, as for the fixer.
func (e *Engine) whiteboardGuard(ctx context.Context, job *whiteboardJob) error {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		return err
	}
	job.pr = pr
	slot, has, err := e.slotOf(ctx, pr.ID)
	switch {
	case err != nil:
		return err
	case !has || slot.ID != job.slot.ID:
		return fmt.Errorf("slot %s is no longer the PR's", job.slot.Name)
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return fmt.Errorf("slot %s is %s", slot.Name, slot.State)
	}
	job.slot = slot
	head := job.head
	switch {
	case slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused:
		return fmt.Errorf("a round is in progress (%s)", pr.State)
	case pr.HeadSHA != head:
		return fmt.Errorf("the head moved to %s since the review on %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(head))
	case deref(pr.ReviewedSHA) != head:
		return fmt.Errorf("the reviewed head is %s, not %s", textx.ShortSHA(deref(pr.ReviewedSHA)), textx.ShortSHA(head))
	case deref(slot.CheckedOutSHA) != head:
		return fmt.Errorf("slot %s holds %s, not the reviewed head %s", slot.Name, textx.ShortSHA(deref(slot.CheckedOutSHA)), textx.ShortSHA(head))
	}
	if rec, ok, err := e.whiteboardRecord(ctx, pr.ID); err != nil {
		return err
	} else if ok && rec.Head == head {
		return fmt.Errorf("%s has its whiteboard %s already", textx.ShortSHA(head), rec.Session)
	}
	if err := e.d.Slots.Guard(ctx, slot); err != nil {
		return err
	}
	return e.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("last_used_at", e.now()) })
}

// whiteboardBaseRef is the ref the writer takes the merge base from:
// origin/<the PR's base branch>, a ref already qualified kept as it is (the
// pipeline's baseRef).
func whiteboardBaseRef(repo store.Repo, pr store.PR) string {
	b := cmp.Or(prBase(repo, pr), "master")
	if strings.HasPrefix(b, "origin/") || strings.HasPrefix(b, "refs/") {
		return b
	}
	return "origin/" + b
}

// whiteboardStart starts or resumes the writer in the PR's workspace (its
// pane gets the watch's poll identity's env, the account its gh reads the
// PR as), creates its run of kind whiteboard in the review's round, clears
// a report an earlier run of the head left, and sends it the whiteboard
// prompt. A run that Submit refused before sending is abandoned; one whose
// prompt may have arrived is waited for.
func (e *Engine) whiteboardStart(ctx context.Context, job *whiteboardJob) error {
	pr := job.pr
	poll := cmp.Or(job.watch.PollIdentity, job.watch.Identity)
	src := e.d.Identities[poll]
	if src == nil {
		return fmt.Errorf("poll identity %q is not configured", poll)
	}
	if err := e.d.Agents.Preflight(ctx, job.role.AgentKind()); err != nil && isLogin(err) {
		return err
	}
	env, err := e.paneEnv(ctx, job.asRound(), src, job.head)
	if err != nil {
		return err
	}
	// Only the writer's pane, as for the fixer: a pane laid out for a round
	// role nobody starts leaves a "starting" session row behind.
	ws, err := e.d.Agents.EnsureWorkspace(ctx, pr, job.slot.Path, env, fmt.Sprintf("%s#%d", job.repo.Name, pr.Number), []config.Role{job.role})
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if ws.Panes[agents.Role(job.role.Name)] == "" {
		if ws, err = e.d.Agents.EnsurePane(ctx, pr, ws, job.slot.Path, env, job.role); err != nil {
			return fmt.Errorf("%s pane: %w", job.role.Name, err)
		}
	}
	if _, err := e.ensureAgent(ctx, pr, job.role, ws, false, effortFull); err != nil {
		return fmt.Errorf("start %s: %w", job.role.Name, err)
	}
	run, err := e.d.Agents.NewRun(ctx, pr, agents.Role(job.role.Name), store.RunWhiteboard, job.round)
	if err != nil {
		return err
	}
	job.run = run
	report := deref(run.ReportPath)
	if err := os.Remove(report); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.abandonFixRun(ctx, run, err.Error())
		return fmt.Errorf("an earlier report at %s: %w", report, err)
	}
	text, err := e.d.Agents.WhiteboardPrompt(pr, agents.Role(job.role.Name), run, agents.WhiteboardPromptInput{
		Owner: job.repo.Owner, Repo: job.repo.Name, Checkout: job.slot.Path, BaseRef: whiteboardBaseRef(job.repo, pr),
		ReviewURL:  job.review.URL,
		ReviewFile: filepath.Join(e.d.Layout.ReviewDir(job.repo.Owner, job.repo.Name, pr.Number, job.review.SHA), agents.PostReviewFile),
		Session:    job.session,
	})
	if err != nil {
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	job.touched = true
	if serr := e.d.Agents.Submit(ctx, run, text); serr != nil {
		cur, err := e.st.RunByID(context.WithoutCancel(ctx), run.ID)
		switch {
		case err != nil:
			return errors.Join(serr, err)
		case cur.State == store.RunPending: // refused before sending
			job.touched = false
			e.abandonFixRun(ctx, run, serr.Error())
			return serr
		case cur.State == store.RunFailed || cur.State == store.RunAbandoned || ctx.Err() != nil:
			return serr
		}
		e.log.Warn("whiteboard: prompt not acknowledged as working; waiting for the run", "subject", job.subject(), "run", run.ID, "err", serr)
	}
	data := job.eventData()
	data["slot"] = job.slot.Name
	e.event(ctx, "info", job.subject(), "whiteboard.start", fmt.Sprintf("%s %s (run %s)", job.role.Name, job.what(), run.ID), data)
	return nil
}

// whiteboardWait waits for the writer's run to end (waitAgentRun) and looks
// at the PR on every poll: a head that moved or a PR GitHub closed stops
// the job (whiteboardStopped). A run past the role's timeout fails.
func (e *Engine) whiteboardWait(ctx context.Context, job *whiteboardJob) error {
	timeout := job.role.Timeout.Duration
	err := e.waitAgentRun(ctx, job.run.ID, timeout, func(ctx context.Context) error {
		pr, err := e.st.PRByID(ctx, job.pr.ID)
		switch {
		case err != nil:
			return err
		case pr.HeadSHA != job.head:
			return &whiteboardStopped{why: "the head moved to " + textx.ShortSHA(pr.HeadSHA)}
		case pr.GHState != store.GHOpen:
			return &whiteboardStopped{why: "the PR closed"}
		}
		return nil
	})
	if errors.Is(err, errFixTimeout) {
		return fmt.Errorf("%s ran out of its %s", job.role.Name, humanDuration(timeout))
	}
	return err
}

// settleWhiteboard ends a job whose writer's run ended: its report names
// the whiteboard of the head (the record, a verified run, whiteboard.done)
// or why there is none (failWhiteboard). The slot is resynced at the head.
func (e *Engine) settleWhiteboard(ctx context.Context, job *whiteboardJob) {
	session, why := e.readWhiteboardReport(job)
	if why != "" {
		e.failWhiteboard(ctx, job, why)
		return
	}
	rec := WhiteboardRecord{Session: session, Head: job.head, At: e.now().UTC(), Role: job.role.Name}
	if err := writeKVJSON(ctx, e, KVPRWhiteboard(job.pr.ID), "whiteboard record", rec); err != nil {
		e.failWhiteboard(ctx, job, "whiteboard record: "+err.Error())
		return
	}
	e.finishFixRun(ctx, job.run, store.RunVerified, whiteboardDone, "")
	took := e.now().Sub(job.started)
	msg := fmt.Sprintf("%s wrote the whiteboard %s of %s in %s", job.role.Name, session, textx.ShortSHA(job.head), humanDuration(took))
	msg += e.resyncWhiteboardSlot(ctx, job)
	data := job.eventData()
	data["session"], data["seconds"] = session, int(took.Seconds())
	e.event(ctx, "info", job.subject(), "whiteboard.done", msg, data)
}

// readWhiteboardReport reads the writer's report of the job's run: the
// session of a whiteboard written ("" with why otherwise). Exactly the lines
// `session: <id>` and `head: <the job's head>` are a whiteboard; a `failed:
// <one sentence>` line is a failure that names the role and the report, not
// the sentence, which may quote the PR (the operator reads it in the
// report); anything else, or no report, is a failure that names the path,
// never a line of it.
func (e *Engine) readWhiteboardReport(job *whiteboardJob) (session, why string) {
	path := deref(job.run.ReportPath)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", job.role.Name + " wrote no report"
	case err != nil:
		return "", "the report: " + err.Error()
	}
	var lines []string
	for l := range strings.Lines(string(b)) {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "failed:") {
			return "", fmt.Sprintf("%s failed (its report: %s)", job.role.Name, path)
		}
	}
	bad := fmt.Sprintf("the report at %s is not the two lines the prompt asks for (the session, and head %s)", path, textx.ShortSHA(job.head))
	if len(lines) != 2 {
		return "", bad
	}
	id, ok := strings.CutPrefix(lines[0], "session:")
	head, hok := strings.CutPrefix(lines[1], "head:")
	if id = strings.TrimSpace(id); !ok || !hok || strings.TrimSpace(head) != job.head || !whiteboardSessionRe.MatchString(id) {
		return "", bad
	}
	return id, ""
}

// failWhiteboard ends the job as failed with msg: a writer still at work is
// interrupted, its run fails, the slot is resynced at the head, and
// whiteboard.failed says why. The operator is asked nothing: no attention,
// no toast (the whiteboard is optional).
func (e *Engine) failWhiteboard(ctx context.Context, job *whiteboardJob, msg string) {
	if job.run.ID != "" {
		if job.touched && e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 && !e.waitAgentsQuiet(ctx, job.pr.ID) {
			e.log.Info("the whiteboard writer still works after the interrupt", "subject", job.subject())
		}
		e.finishFixRun(ctx, job.run, store.RunFailed, "failed", msg)
	}
	msg += e.resyncWhiteboardSlot(ctx, job)
	e.event(ctx, "warn", job.subject(), "whiteboard.failed", msg, job.eventData())
}

// resyncWhiteboardSlot puts a slot the writer may have touched back at the
// head (resyncFixSlot's Slots.Resync). It returns what to add to the
// event's message ("" when the slot is back or untouched).
func (e *Engine) resyncWhiteboardSlot(ctx context.Context, job *whiteboardJob) string {
	if !job.touched || job.slot.ID == 0 {
		return ""
	}
	if err := e.resyncSlot(ctx, job.pr.ID, job.slot, job.head); err != nil {
		return fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
	}
	return ""
}

// stopWhiteboard ends a job a push, a close or the abort and ignore orders
// stopped (why): the writer is interrupted and its run abandoned, the slot
// is resynced at the head, and whiteboard.stopped says why; an ignore also
// mutes the PR as `magnum ignore` does. The orders are completed with what
// happened.
func (e *Engine) stopWhiteboard(ctx context.Context, job *whiteboardJob, why string, orders []stopOrder) {
	var done []string
	var problems []error
	if job.run.ID != "" {
		if e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 {
			done = append(done, "writer interrupted")
			if !e.waitAgentsQuiet(ctx, job.pr.ID) {
				e.log.Info("the whiteboard writer still works after the interrupt", "subject", job.subject())
			}
		}
		reason := "stopped: " + why
		if len(orders) > 0 {
			reason = abortedBy
		}
		if e.abandonFixRun(ctx, job.run, reason) {
			done = append(done, "run "+job.run.ID+" abandoned")
		}
	}
	if job.touched {
		if err := e.resyncSlot(ctx, job.pr.ID, job.slot, job.head); err != nil {
			problems = append(problems, fmt.Errorf("slot %s not put back: %w", job.slot.Name, err))
		} else {
			done = append(done, "slot "+job.slot.Name+" back at "+textx.ShortSHA(job.head))
		}
	}
	ignore := slices.ContainsFunc(orders, func(o stopOrder) bool { return o.ignore })
	res := "the whiteboard of " + job.label() + " stopped: " + why
	if len(orders) > 0 {
		res = "aborted the whiteboard of " + job.label()
		if ignore {
			res = "ignored the whiteboard of " + job.label()
			if to, err := e.settleStopped(ctx, job.pr, true); err != nil {
				problems = append(problems, err)
			} else if to != "" {
				done = append(done, "PR "+to)
			}
		}
	}
	if len(done) > 0 {
		res += ": " + strings.Join(done, ", ")
	}
	if ignore {
		res += "; muted until `magnum unmute " + job.label() + "`"
	}
	perr := errors.Join(problems...)
	level := "info"
	if perr != nil {
		level, res = "warn", res+"; "+perr.Error()
	}
	e.event(ctx, level, job.subject(), "whiteboard.stopped", res, job.eventData())
	for _, o := range orders {
		if perr != nil {
			e.complete(ctx, o.id, errors.New(res), "")
		} else {
			e.complete(ctx, o.id, nil, res)
		}
		e.mu.Lock()
		delete(e.inflight, o.id)
		e.mu.Unlock()
	}
}

// whiteboardPanicked settles a job that panicked as a failed one; the abort
// and ignore orders it held hear why (stopsPanicked), and the requests it
// yields to go on after it (unreserveWhiteboard).
func (e *Engine) whiteboardPanicked(ctx context.Context, job *whiteboardJob, msg string) {
	if orders, _, _ := e.closeWhiteboardHandle(job.pr.ID); len(orders) > 0 {
		e.stopsPanicked(ctx, orders, msg)
	}
	e.failWhiteboard(ctx, job, "whiteboard job panicked: "+msg)
}

// whiteboardHolds reports whether a whiteboard job holds the PR.
func (e *Engine) whiteboardHolds(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.rounds[prID]
	return h != nil && h.whiteboard
}

// whiteboardWaitOf is the wait of a PR whose whiteboard is being written
// (ok false when no whiteboard job holds it): role is the watch's
// whiteboard role.
func (e *Engine) whiteboardWaitOf(prID int64, w *config.Watch) (Wait, bool) {
	if !e.whiteboardHolds(prID) {
		return Wait{}, false
	}
	role := ""
	if r, ok := e.cfg.WhiteboardRoleFor(w); ok {
		role = r.Name
	}
	detail := "the whiteboard in progress"
	if role != "" {
		detail += " (" + role + ")"
	}
	return Wait{Reason: WaitWhiteboard, Subject: role, Detail: detail}, true
}

// recoverWhiteboards fails the whiteboard runs a stopped daemon left at
// work: no writer's run survives a restart, so its run is abandoned and
// whiteboard.failed says it was lost, asking nothing of the operator.
// herdr keeps the writer's pane across a crash, a drain that timed out or
// `daemon-restart --now`, so a writer still at work is interrupted until
// quiet and the slot is put back at the run's head first, as the fix job's
// recovery does (stopFixerAt): with no reservation left, the next round
// could take the slot under it.
func (e *Engine) recoverWhiteboards(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	runs, err := e.st.ActiveRuns(ctx)
	if err != nil {
		e.log.Warn("recovery: whiteboard runs", "err", err)
		return
	}
	for _, r := range runs {
		if r.Kind != store.RunWhiteboard || !e.abandonFixRun(ctx, r, whiteboardLostText) {
			continue
		}
		pr, err := e.st.PRByID(ctx, r.PRID)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		job := e.lostWhiteboardJob(ctx, repo, pr, r)
		if e.interruptSessions(ctx, pr, &job.watch, r.Role) > 0 && !e.waitAgentsQuiet(ctx, pr.ID) {
			e.log.Info("the whiteboard writer still works after the interrupt", "subject", job.subject())
		}
		msg := whiteboardLostText
		if job.slot.ID == 0 {
			e.log.Info("recovery: the lost whiteboard run's PR has no slot to put back", "subject", job.subject(), "run", r.ID)
		} else {
			msg += e.resyncWhiteboardSlot(ctx, job)
		}
		e.event(ctx, "warn", job.subject(), "whiteboard.failed", msg, job.eventData())
	}
}

// lostWhiteboardJob is the whiteboard job of run r, which a restart left at
// work, for the recovery's stop: the PR, its watch and slot (none when it
// has none now), the run's role and head. The writer may have touched the
// slot unless the run never left pending (its prompt was never sent).
func (e *Engine) lostWhiteboardJob(ctx context.Context, repo store.Repo, pr store.PR, r store.Run) *whiteboardJob {
	job := &whiteboardJob{repo: repo, pr: pr, role: config.Role{Name: r.Role}, head: r.TargetSHA, run: r, touched: r.State != store.RunPending}
	if w := e.cfg.WatchFor(repo.FullName()); w != nil {
		job.watch, job.pool = *w, e.cfg.PoolFor(repo.FullName())
	}
	switch slot, has, err := e.slotOf(ctx, pr.ID); {
	case err != nil:
		e.log.Warn("recovery: the lost whiteboard run's slot", "subject", job.subject(), "err", err)
	case has:
		job.slot = slot
	}
	return job
}

// whiteboardYield is a request a whiteboard job yields the PR to
// (roundStop.yields): its id and who asks ("magnum review", "magnum fix",
// "magnum open").
type whiteboardYield struct {
	id int64
	by string
}

// whiteboardHeld is the refusal of a review or fix request whose PR a
// whiteboard job holds, once every other refusal passed: answerYielding
// stops the job for the request (yieldWhiteboard) instead of answering with
// it. Its text is the refusal the request had before. carry is what the
// request read on its way to the refusal (nil = nothing), which it is handed
// when it is answered after the yield, so it reads only once.
type whiteboardHeld struct {
	prID    int64
	refusal error
	carry   any
}

func (w *whiteboardHeld) Error() string { return w.refusal.Error() }

// heldByWhiteboard is the whiteboardHeld refusal of a request on pr:
// "<label>: a round is already in progress (<state>)", carrying carry.
func heldByWhiteboard(repo store.Repo, pr store.PR, carry any) error {
	return &whiteboardHeld{prID: pr.ID, carry: carry,
		refusal: fmt.Errorf("%s#%d: a round is already in progress (%s)", repo.FullName(), pr.Number, pr.State)}
}

// roundActiveBesidesWhiteboard is roundActive for a request a whiteboard
// job yields to: whether a reservation other than a whiteboard job's holds
// the PR.
func (e *Engine) roundActiveBesidesWhiteboard(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.rounds[prID]
	return ok && !h.whiteboard
}

// fixHeld is prepareFix's roundActive: a reservation holds the PR against
// a fix cycle, but a whiteboard job's does not for the pass of `magnum fix`
// without side effects (fixPinWouldUnpin): the job yields to the request
// (requestFix).
func (e *Engine) fixHeld(prID int64, pin fixPin) bool {
	if pin == fixPinWouldUnpin {
		return e.roundActiveBesidesWhiteboard(prID)
	}
	return e.roundActive(prID)
}

// answerYielding is answer for a request a whiteboard job yields the PR to
// (by names it: "magnum review", "magnum fix"): when fn refuses because such
// a job holds the PR (whiteboardHeld), the job is stopped and the request
// is answered once it let go of the PR (yieldWhiteboard), by the request
// dispatch, as if the job had never been; a job that let go meanwhile
// leaves the request pending for the next pass. What the refusal carries
// (whiteboardHeld.carry) is kept for the request and handed to fn on its
// next pass (nil on the first).
func answerYielding[T any](e *Engine, ctx context.Context, req store.Request, by string, fn func(context.Context, T, any) (string, error)) {
	p, ok := payload[T](e, ctx, req)
	if !ok {
		return
	}
	e.mu.Lock()
	carry := e.yieldCarry[req.ID]
	delete(e.yieldCarry, req.ID)
	e.mu.Unlock()
	res, err := fn(ctx, p, carry)
	if held, ok := errors.AsType[*whiteboardHeld](err); ok {
		if held.carry != nil {
			e.mu.Lock()
			e.yieldCarry[req.ID] = held.carry
			e.mu.Unlock()
		}
		if !e.yieldWhiteboard(held.prID, req.ID, by) {
			e.Kick()
		}
		return
	}
	e.complete(ctx, req.ID, err, res)
}

// yieldWhiteboard hands the PR a whiteboard job holds to request id (by):
// the request stays in flight, the job is stopped unless it is past the
// point a stop reaches (its run ended: it settles), and once the job let go
// of the PR the request goes back to the request dispatch
// (unreserveWhiteboard), which answers it as if the job had never been, so
// it is answered once and the tick never waits for the writer. It reports
// whether a whiteboard job held the PR.
func (e *Engine) yieldWhiteboard(prID, id int64, by string) bool {
	e.mu.Lock()
	h := e.rounds[prID]
	if h == nil || !h.whiteboard {
		e.mu.Unlock()
		return false
	}
	h.stop.yields = append(h.stop.yields, whiteboardYield{id: id, by: by})
	e.inflight[id] = true
	cancel, ended := h.cancel, h.stop.ended
	e.mu.Unlock()
	e.log.Info("the whiteboard job yields to a request", "pr", prID, "request", id, "by", by)
	if !ended {
		cancel()
	}
	return true
}

// unreserveWhiteboard drops a whiteboard job's reservation (unreserve) and
// hands the requests it yields to back to the request dispatch: they leave
// the in-flight set, still pending, and the loop ticks now (Kick), so the
// next pass answers them.
func (e *Engine) unreserveWhiteboard(prID int64) {
	e.mu.Lock()
	h := e.rounds[prID]
	delete(e.rounds, prID)
	yielded := 0
	if h != nil {
		for _, y := range h.stop.yields {
			delete(e.inflight, y.id)
		}
		yielded = len(h.stop.yields)
	}
	e.mu.Unlock()
	if h != nil {
		h.cancel()
	}
	if yielded > 0 {
		e.Kick()
	}
}
