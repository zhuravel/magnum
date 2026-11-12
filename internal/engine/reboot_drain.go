package engine

// The review daemon's side of `magnum drain` (control.go has the records).
// While KVDaemonRebootDrain holds, draining reports true, so every gate a
// restart's drain closes is closed: automatic and forced rounds, fix
// cycles and `magnum fix`, merges of the parent, whiteboards, the retro,
// the curations, the skill edit and gate; the staging resolver takes no
// slot for a queued job. Each health check acknowledges it
// (tendRebootDrain): the work that still runs, quiet once none does, and
// once quiet the idle sessions parked through restart-sessions' early park,
// so the reboot closes no pane in the middle of a turn. A start on another
// boot ends a drain until "boot" (startRebootDrain); `magnum resume`
// deletes the record, and the next health check unmarks the sessions the
// drain's park marked. A restart's drain (KVDaemonDraining) is apart: a
// start ends it alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// rebootDrainHold is why the daemon starts nothing new under a drain for a
// reboot (stopping), as the holds and the waits say it.
const rebootDrainHold = "draining for a reboot (magnum drain; magnum resume ends it)"

// rebootHeldRequests are the requests that would start work a reboot cuts
// (refuseUnderRebootDrain): a staging deploy and the slot work of a
// provision, a repair, an adoption, a cleanup or a release. An open's
// restore is refused where it would start one (requestOpen), past its answer
// for a session that is live already.
var rebootHeldRequests = []string{ReqStagingDeploy, ReqProvision, ReqRepair, ReqAdopt, ReqCleanup, ReqRelease}

// errRebootDrainRefusal is their answer under the drain.
var errRebootDrainRefusal = errors.New("the daemon drains for a reboot (`magnum drain`): ask again after the reboot, or after `magnum resume`")

// refuseUnderRebootDrain refuses req while a drain for a reboot holds when it
// would start work a reboot cuts (rebootHeldRequests), and reports whether
// it did. A dry run starts nothing and goes on: the daemon's own, a staging
// deploy's, a cleanup's plan.
func (e *Engine) refuseUnderRebootDrain(ctx context.Context, req store.Request) bool {
	if !slices.Contains(rebootHeldRequests, req.Kind) || e.d.DryRun || requestDry(req) || !e.rebootDraining(ctx) {
		return false
	}
	e.complete(ctx, req.ID, errRebootDrainRefusal, "")
	return true
}

// requestDry reports whether req asks for a dry run: a staging deploy's
// dry_run; a cleanup's plan marked dry when it carries one (cleanupAsync
// applies the plan), else its options marked dry.
func requestDry(req store.Request) bool {
	var p struct {
		DryRun  bool `json:"dry_run"`
		Options struct {
			DryRun bool `json:"dry_run"`
		} `json:"options"`
		Plan *struct {
			DryRun bool `json:"dry_run"`
		} `json:"plan"`
	}
	if json.Unmarshal(req.Payload, &p) != nil {
		return false
	}
	if p.Plan != nil {
		return p.Plan.DryRun
	}
	return p.DryRun || p.Options.DryRun
}

// rebootDrainStarts is when a request queued or held behind a drain for a
// reboot starts, for its answer and its event (drainStarts is a restart
// drain's).
const rebootDrainStarts = "starts once the drain for a reboot ends (the first start after the reboot, or `magnum resume`)"

// QueuedRebootDrain is why a curation waits (CurateQueued.Why) while the
// daemon drains for a reboot alone.
const QueuedRebootDrain = "reboot_drain"

// rebootDrainError refuses a fix cycle or a whiteboard while the daemon
// drains for a reboot alone: it is errFixDraining, in its own words.
type rebootDrainError struct{}

func (rebootDrainError) Error() string {
	return "the daemon drains for a reboot (`magnum drain`): ask again after the reboot, or after `magnum resume`"
}

func (rebootDrainError) Is(target error) bool { return target == errFixDraining }

// onlyRebootDrain reports whether the drain that holds the daemon is a
// drain for a reboot alone: no restart's drain, no move of the panes. The
// answers then name it, never a restart that does not come.
func (e *Engine) onlyRebootDrain(ctx context.Context) bool {
	if !e.rebootDraining(ctx) || e.panesMoving() != "" {
		return false
	}
	v, ok := e.getKV(ctx, KVDaemonDraining)
	return !ok || v == ""
}

// drainStarts is when what a drain holds starts, in its answer's words.
func (e *Engine) drainStarts(ctx context.Context) string {
	if e.onlyRebootDrain(ctx) {
		return rebootDrainStarts
	}
	return drainStarts
}

// queuedDrain is why a curation a drain holds waits (QueuedDrain,
// QueuedRebootDrain).
func (e *Engine) queuedDrain(ctx context.Context) string {
	if e.onlyRebootDrain(ctx) {
		return QueuedRebootDrain
	}
	return QueuedDrain
}

// drainErr is the refusal of a fix cycle or a whiteboard under a drain
// (errFixDraining, or rebootDrainError, which is it).
func (e *Engine) drainErr(ctx context.Context) error {
	if e.onlyRebootDrain(ctx) {
		return rebootDrainError{}
	}
	return errFixDraining
}

// drainedAgain refuses, under a drain, a request it does not hold: what to
// run again after the drain (again), then the rest of the refusal. "the
// daemon drains for a restart: ask again after it; a CI fix is never held",
// or, under a drain for a reboot alone, "the daemon drains for a reboot
// (`magnum drain`): ask again after the reboot, or after `magnum resume`;
// …".
func (e *Engine) drainedAgain(ctx context.Context, again, rest string) error {
	if e.onlyRebootDrain(ctx) {
		return fmt.Errorf("the daemon drains for a reboot (`magnum drain`): %s after the reboot, or after `magnum resume`; %s", again, rest)
	}
	return fmt.Errorf("the daemon drains for a restart: %s after it; %s", again, rest)
}

// drainSince is since when the drain that holds the daemon holds it: a
// restart's (its record's start, either format), else a reboot's.
func (e *Engine) drainSince(ctx context.Context) (time.Time, bool) {
	if v, ok := e.getKV(ctx, KVDaemonDraining); ok && v != "" {
		if d, ok := ParseDrain(v); ok {
			return d.Since, true
		}
	}
	if v, ok := e.getKV(ctx, KVDaemonRebootDrain); ok {
		if d, ok := ParseRebootDrain(v); ok {
			return d.Since, true
		}
	}
	return time.Time{}, false
}

// drainParkWhy is the mark of the sessions a drain for a reboot parks once
// the daemon is quiet (restartMark.Why).
const drainParkWhy = "the drain for a reboot"

// kindDrainHolds counts the toast of a drain for a reboot that still holds
// an hour after the daemon went quiet.
var kindDrainHolds = notify.Kind{One: "drain for a reboot still holds", Many: "drains for a reboot still hold"}

// rebootState is what the health checks keep of the drain for a reboot they
// tend: its generation, since when the daemon is quiet under it, the
// acknowledgement last written and whether its toast went.
type rebootState struct {
	gen        int64
	quietSince time.Time
	ack        string
	toasted    bool
	// boot is the Mac's boot as the start read it.
	boot BootID
}

// rebootDraining reports whether a drain for a reboot holds: any value of
// its row, one this build cannot read included (it holds until `magnum
// resume` deletes it).
func (e *Engine) rebootDraining(ctx context.Context) bool {
	v, ok := e.getKV(ctx, KVDaemonRebootDrain)
	return ok && v != ""
}

// readBoot reads the Mac's boot (Deps.Boot, else sysctl through
// MemoryRunner); unknown when it cannot.
func (e *Engine) readBoot(ctx context.Context) BootID {
	read := e.d.Boot
	if read == nil {
		if e.d.MemoryRunner == nil {
			return BootID{}
		}
		read = func(ctx context.Context) (BootID, error) { return ReadBoot(ctx, e.d.MemoryRunner) }
	}
	b, err := read(ctx)
	if err != nil {
		e.log.Warn("read the Mac's boot: a drain for a reboot lasts until `magnum resume`", "err", err)
		return BootID{}
	}
	return b
}

// recordControls records the controls this build knows (KVDaemonControls,
// for the CLI), right after the start recorded its pid: the CLI reads a pid
// recorded without them as an older build's.
func (e *Engine) recordControls(ctx context.Context) {
	e.setKV(ctx, KVDaemonControls, Controls{Version: ControlsVersion, PID: os.Getpid()}.Value())
}

// startRebootDrain runs at the start: it drops a former daemon's
// acknowledgement (this one acknowledges anew, once its gates hold) and
// ends a drain until "boot" that began on another boot of the Mac. A dry
// run writes nothing.
func (e *Engine) startRebootDrain(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	e.delKV(ctx, KVDaemonDrainAck)
	v, ok := e.getKV(ctx, KVDaemonRebootDrain)
	if !ok || v == "" {
		return
	}
	e.reboot.boot = e.readBoot(ctx)
	d, ok := ParseRebootDrain(v)
	if !ok {
		e.log.Warn("a drain for a reboot this build cannot read holds until `magnum resume`")
		return
	}
	if !d.EndsAtStart(e.reboot.boot) {
		e.log.Info("a drain for a reboot holds: nothing new starts "+d.Words(), "since", d.Since)
		return
	}
	if gone, err := e.st.DeleteKVIf(ctx, KVDaemonRebootDrain, v); err != nil {
		e.warnUnlessStopped(ctx, err, "end the drain for a reboot")
		return
	} else if !gone {
		return // `magnum drain` or `magnum resume` changed it meanwhile
	}
	e.event(ctx, "info", "", "daemon.reboot_drain_ended",
		fmt.Sprintf("the drain for a reboot that began at %s ended: the daemon started after the Mac rebooted",
			d.Since.Local().Format("Jan 2 15:04")), map[string]any{"generation": d.Generation})
}

// tendRebootDrain is the tick's last part of the drain for a reboot, once
// every start path of the tick ran (the requests, the dispatch, the parks,
// the reconcile), so what one of them started meanwhile is in the work it
// names. While one holds it writes the acknowledgement (DrainAck: the work that
// still runs, quiet once none does), parks the idle sessions once quiet
// (parkForRebootDrain) and, an hour after the daemon went quiet, toasts
// once that the drain still holds. With none it drops a stale
// acknowledgement and the marks of the drain's park.
func (e *Engine) tendRebootDrain(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	v, _ := e.getKV(ctx, KVDaemonRebootDrain)
	st := &e.reboot
	if v == "" {
		if st.gen != 0 || st.ack != "" {
			e.delKV(ctx, KVDaemonDrainAck)
			e.event(ctx, "info", "", "daemon.reboot_drain_lifted", "the drain for a reboot was lifted: new work starts again", nil)
		}
		*st = rebootState{boot: st.boot}
		e.unmarkDrainParks(ctx)
		return
	}
	d, ok := ParseRebootDrain(v)
	if !ok {
		return // one this build cannot read holds (rebootDraining), unacknowledged, until `magnum resume`
	}
	now := e.now()
	if st.gen != d.Generation {
		*st = rebootState{gen: d.Generation, boot: st.boot}
		e.event(ctx, "info", "", "daemon.reboot_drain_held",
			fmt.Sprintf("a drain for a reboot holds since %s: nothing new starts, the work in flight ends, then the idle sessions park; it lasts %s",
				d.Since.Local().Format("15:04"), d.Words()), map[string]any{"generation": d.Generation})
	}
	work := e.rebootDrainWork(ctx)
	quiet := len(work) == 0
	switch {
	case !quiet:
		st.quietSince = time.Time{}
	case st.quietSince.IsZero():
		st.quietSince = now
	}
	parked := false
	if quiet {
		parked = e.parkForRebootDrain(ctx)
		if blocked := e.blockedSessions(ctx); len(blocked) > 0 { // they cannot park: the operator answers them
			parked, work = false, blocked
		}
	}
	ack := DrainAck{Generation: d.Generation, PID: os.Getpid(), Quiet: quiet, QuietSince: st.quietSince, Parked: parked, Work: work}
	e.writeDrainAck(ctx, st, ack)
	if quiet && !st.toasted && now.Sub(st.quietSince) >= DrainQuietToastAfter {
		st.toasted = true
		body := fmt.Sprintf("nothing has run since %s and the Mac did not reboot: new work waits; `magnum resume` ends the drain",
			st.quietSince.Local().Format("15:04"))
		e.info(notify.Item{Key: fmt.Sprintf("reboot-drain:%d", d.Generation), Title: "magnum: the drain for a reboot still holds",
			Body: body, Line: "the drain for a reboot still holds: " + body, Kind: kindDrainHolds, Window: 24 * time.Hour})
	}
}

// writeDrainAck writes ack unless its row already holds it: the row, not a
// copy in memory, is what the CLI reads, so a write that failed, or a row
// that went, is written again at the next tick. st keeps the last value
// written, for the event of the first quiet and parked one.
func (e *Engine) writeDrainAck(ctx context.Context, st *rebootState, ack DrainAck) {
	val := ack.Value()
	if cur, _ := e.getKV(ctx, KVDaemonDrainAck); cur == val {
		return
	}
	if err := e.st.SetKV(ctx, KVDaemonDrainAck, val); err != nil {
		e.warnUnlessStopped(ctx, err, "drain for a reboot: write the acknowledgement")
		return
	}
	if ack.Parked && !strings.Contains(st.ack, `"parked":true`) {
		e.event(ctx, "info", "", "daemon.reboot_drain_quiet", "drained for a reboot: nothing runs and the idle sessions are parked", nil)
	}
	st.ack = val
}

// blockedSessions are the live sessions a dialog holds (herdr shows them
// blocked: a hooks review, a permission prompt), which no park takes, each
// with what to do: "codex-judge of talkable/talkable#3 shows a dialog:
// answer it in its pane (`magnum open talkable/talkable#3`)".
func (e *Engine) blockedSessions(ctx context.Context) []string {
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		return []string{"the live sessions (unreadable)"}
	}
	var out []string
	for _, s := range live {
		if herdr.Status(deref(s.AgentStatus)) != herdr.StatusBlocked {
			continue
		}
		pr := e.prWords(ctx, s.PRID)
		out = append(out, fmt.Sprintf("%s of %s shows a dialog: answer it in its pane (`magnum open %s`)", s.Role, pr, pr))
	}
	return out
}

// sessionsAtWork are the live sessions herdr shows working while no round,
// job or reservation holds their PR (counted), pinned PRs included: a
// session you opened and type into works with no round to count it, and the
// park leaves a pinned PR's to you. "talkable/talkable#4 at work in herdr
// (codex-judge)".
func (e *Engine) sessionsAtWork(ctx context.Context, counted func(prID int64) bool) []string {
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		return []string{"the live sessions (unreadable)"}
	}
	var out []string
	for _, s := range live {
		if herdr.Status(deref(s.AgentStatus)) != herdr.StatusWorking || counted(s.PRID) {
			continue
		}
		out = append(out, e.prWords(ctx, s.PRID)+" at work in herdr ("+s.Role+")")
	}
	return out
}

// rebootDrainWork is what the review daemon still runs that a reboot would
// cut, in words: its reservations (rounds, opens, fix jobs, merges of the
// parent, a held merge's push, whiteboards: reservedWords), a round's tail
// before its fix cycle or whiteboard, the background jobs (backgroundBusy),
// the heavy worker's jobs and the GitHub writes as the operator
// (workInFlight), the rounds the registry holds in flight (an unreadable
// registry counts as one), the live sessions at work that none of these
// holds (sessionsAtWork: a pinned PR's you type into), the requests answered
// off the tick (a staging request, an open, an abort), the staging deploys
// queued or running, and the staging resolver's jobs that took their slot or
// that its worker runs.
func (e *Engine) rebootDrainWork(ctx context.Context) []string {
	var out []string
	e.mu.Lock()
	held := maps.Clone(e.rounds)
	e.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(held)) {
		out = append(out, e.reservedWords(ctx, id, held[id]))
	}
	if n := e.roundTails.Load(); n > 0 {
		out = append(out, fmt.Sprintf("the end of %d round(s)", n))
	}
	if b := e.backgroundBusy(); b != "" {
		out = append(out, b)
	}
	for _, w := range e.workInFlight() {
		out = append(out, w.What)
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: store.InFlightStates})
	if err != nil {
		out = append(out, "the registry's rounds (unreadable)")
	}
	rounds := map[int64]bool{}
	for _, pr := range prs {
		rounds[pr.ID] = true
		if _, ok := held[pr.ID]; !ok {
			out = append(out, "round of "+e.prWords(ctx, pr.ID)+" ("+pr.State+")")
		}
	}
	out = append(out, e.sessionsAtWork(ctx, func(id int64) bool { _, ok := held[id]; return ok || rounds[id] })...)
	e.mu.Lock()
	answering := len(e.inflight)
	e.mu.Unlock()
	if answering > 0 {
		out = append(out, fmt.Sprintf("%d request(s) answered off the tick", answering))
	}
	out = append(out, e.deploysInFlight()...)
	return append(out, e.resolvesInFlight(ctx)...)
}

// deploysInFlight are the staging deploys queued or running, which the steps
// take on: "staging deploy of talkable/talkable#12 to void".
func (e *Engine) deploysInFlight() []string {
	s := e.d.Staging
	if s == nil {
		return nil
	}
	var out []string
	for _, repo := range StagingRepos(e.cfg) {
		for _, f := range s.Effects(repo) {
			if f.State == staging.EffectQueued || f.State == staging.EffectRunning {
				out = append(out, fmt.Sprintf("staging deploy of %s#%d to %s", f.Repo, f.PR.Number, f.Env.Name))
			}
		}
	}
	return out
}

// resolvesInFlight are the staging resolver's jobs that took their slot
// (running) and the one its worker runs, "staging resolve of
// talkable/talkable#12 into staging-void"; none without a resolver.
func (e *Engine) resolvesInFlight(ctx context.Context) []string {
	r := e.d.stagingResolver
	if r == nil {
		return nil
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	jobs, err := e.resolveJobs(ctx)
	if err != nil {
		return []string{"the staging resolver's jobs (unreadable)"}
	}
	var out []string
	for _, j := range jobs {
		if j.State == resolveRunning || (j.ID == running && j.live()) {
			out = append(out, "staging resolve of "+j.label()+" into "+j.Target)
		}
	}
	return out
}

// prWords names PR id for the drain's lines: "talkable/talkable#12", or
// "PR 12" (its row's id) when the registry cannot say.
func (e *Engine) prWords(ctx context.Context, id int64) string {
	pr, err := e.st.PRByID(ctx, id)
	if err != nil {
		return fmt.Sprintf("PR %d", id)
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return fmt.Sprintf("PR %d", id)
	}
	return fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
}

// parkForRebootDrain parks the idle sessions once the daemon is quiet under
// the drain: each quiet check marks every session `magnum restart-sessions
// --tool all` would restart now (pinned PRs and shell panes left alone; a
// session marked before keeps its mark), so one that becomes restartable
// later (`magnum unpin`, an open's hold that ended) parks too, and
// restartSessions parks them through the early park as soon as nothing
// blocks them. It reports whether the park is done: no such session is
// still live.
func (e *Engine) parkForRebootDrain(ctx context.Context) bool {
	if e.d.Agents == nil {
		return true
	}
	sessions, _, err := RestartableSessions(ctx, e.st, e.cfg, "all", 0)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "drain for a reboot: the live sessions")
		return false
	}
	if len(sessions) == 0 {
		return true
	}
	if added := e.markRestart(ctx, sessions, drainParkWhy); len(added) > 0 {
		e.log.Info("drain for a reboot: the idle sessions park", "sessions", len(added))
	}
	return false
}

// unmarkDrainParks drops the marks a drain's park left once no drain holds:
// those sessions go on as they were.
func (e *Engine) unmarkDrainParks(ctx context.Context) {
	var ids []int64
	for id, m := range e.restartMarks(ctx) {
		if m.Why == drainParkWhy {
			ids = append(ids, id)
		}
	}
	e.unmarkRestart(ctx, ids...)
}
