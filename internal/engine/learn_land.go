package engine

// The landing of a skill proposal (DECISIONS "A passing candidate lands as
// a local commit and the daemon restarts when idle"), the end of the
// self-improving loop's fourth stage. A proposal the gate passed lands at a
// tick, at most [learn] land_per_day a local day, and one the operator
// lands from `magnum learn --review` (ReqLearnLand) at once: the checkout
// must have master checked out and nothing in progress, master must still
// hold the proposal's base, the proposal's commit alone is rebased once in
// its worktree when master moved past it (`git rebase --onto`; a conflict
// holds the proposal), the files it changes must be clean in the checkout,
// then master fast-forwards to it. Its branch and worktree go, the
// proposal is landed with the commit, and a learn_versions row records the
// skill and the prompts it landed. The daemon reads its prompts and skill at
// startup, so the landed text reaches rounds after a restart: with [learn]
// restart idle (and launchd to start it again) the daemon exits at the
// first tick with nothing in flight (restartForBuild), with manual (or no
// launchd) a toast and `magnum status` ask for `magnum daemon-restart
// --when-idle`; the next start marks the version restarted, which starts
// its measurement window. A landing that cannot go on holds the proposal
// with why (learn.held). Nothing is pushed.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// KVLearnLandDay counts the landings of a local day (LandDay as JSON),
	// which [learn] land_per_day bounds.
	KVLearnLandDay = "learn.land_day"
	// KVLearnRestart is the landed version the daemon is to restart on
	// (LearnRestartMark as JSON): set by a landing, read by restartForBuild
	// and `magnum status`, and cleared by the next start of the daemon,
	// which marks the version restarted.
	KVLearnRestart = "learn.restart"
	// landRetryAfter is how long a landing that failed short of a hold (a
	// git error) waits before the next try.
	landRetryAfter = 10 * time.Minute
	// landGitTimeout bounds a landing's rebase, fast-forward and the git
	// between them, which run on a context a shutdown does not cancel.
	landGitTimeout = 5 * time.Minute
)

// ErrRestartForLearn is what Tick and Run return when the daemon exits to
// restart on a landed skill version ([learn] restart idle): the exit is
// non-zero, so launchd starts it again, and the new daemon reads the landed
// prompts and skill.
var ErrRestartForLearn = errors.New("a skill version landed and nothing is in flight; exiting for launchd to start the daemon on it")

// LandDay is how many proposals landed on Day (store.DayKey).
type LandDay struct {
	Day    string `json:"day"`
	Landed int    `json:"landed"`
}

// LearnRestartMark is the landed version the daemon waits to restart on
// (KVLearnRestart): Manual when the operator restarts it ([learn] restart
// manual, or no launchd to start it again).
type LearnRestartMark struct {
	Version  int64     `json:"version"`
	Proposal int64     `json:"proposal"`
	Landed   time.Time `json:"landed"`
	Manual   bool      `json:"manual,omitempty"`
}

// landHold is a landing that held its proposal, and why.
type landHold string

func (h landHold) Error() string { return string(h) }

// landUnavailable is why nothing can land ("" = it can): no checkout.
func (e *Engine) landUnavailable() string {
	if e.d.LearnGit == nil || e.d.Checkout == "" {
		return "no checkout of magnum to land on (an installed binary has none)"
	}
	return ""
}

// landedToday is how many proposals landed on today's local day.
func (e *Engine) landedToday(ctx context.Context) int {
	v, ok := e.getKV(ctx, KVLearnLandDay)
	var d LandDay
	if !ok || json.Unmarshal([]byte(v), &d) != nil || d.Day != store.DayKey(e.now()) {
		return 0
	}
	return d.Landed
}

// learnRestart reads KVLearnRestart (false when none is set or it cannot
// be read).
func (e *Engine) learnRestart(ctx context.Context) (LearnRestartMark, bool) {
	v, ok := e.getKV(ctx, KVLearnRestart)
	var m LearnRestartMark
	if !ok || json.Unmarshal([]byte(v), &m) != nil || m.Version == 0 {
		return LearnRestartMark{}, false
	}
	return m, true
}

// restartManual reports whether a landed version waits for the operator's
// restart: [learn] restart manual, or no launchd to start the daemon again.
func (e *Engine) restartManual() bool {
	return e.cfg.Learn.Restart == config.LearnRestartManual || !e.supervised
}

// maybeLearnLand lands the oldest passed proposal (every tick) unless the
// day's landings reached [learn] land_per_day, the daemon has not
// restarted on the last landed version yet (each version runs before the
// next lands), a gate or an edit runs (they add and remove worktrees of the
// checkout), the daemon stops or drains, or a landing that failed waits
// out landRetryAfter. A dry run and `magnum daemon --once` land nothing.
func (e *Engine) maybeLearnLand(ctx context.Context) {
	if e.d.DryRun || e.once || e.landUnavailable() != "" || e.gateBusy() || e.editBusy() || e.now().Before(e.landRetry) ||
		e.stopping(ctx) != "" {
		return
	}
	if e.landedToday(ctx) >= e.cfg.Learn.LandPerDay {
		return
	}
	if _, pending := e.learnRestart(ctx); pending {
		return
	}
	passed, err := e.st.LearnProposals(ctx, []string{store.LearnPassed})
	// A revert lands only on the operator's y (learn_revert.go): the gate
	// never passes one, and one passed anyway waits.
	passed = slices.DeleteFunc(passed, func(p store.LearnProposal) bool { return p.Reverts != 0 })
	if err != nil || len(passed) == 0 {
		if err != nil {
			e.warnUnlessStopped(ctx, err, "skill landing: list the passed proposals")
		}
		return
	}
	p := passed[len(passed)-1] // the oldest
	if _, _, err := e.landProposal(ctx, p, false); err != nil {
		if _, held := errors.AsType[landHold](err); !held {
			e.warnUnlessStopped(ctx, err, "skill landing", "proposal", p.ID)
			e.landRetry = e.now().Add(landRetryAfter)
		}
	}
}

// landProposal lands p, passed or held (requested: the operator asked), on
// the checkout's master. A branch master holds already (a landing a crash
// or a registry error cut short after the fast-forward) is recorded as it
// is, whatever the checkout has checked out since. Else: master checked
// out and nothing in progress; the branch at p's commit or at that commit
// rebased (ensureCandidate); master still holding p's base; p's commit
// alone rebased once in its worktree when master moved past it (a conflict
// holds it); the tree that lands rendering with this build (CheckPrompts,
// as the restart checks); the files it changes clean in the checkout; then
// master fast-forwards to it and the landing is recorded (recordLanding). The
// rebase, the fast-forward and what follows run on a context a shutdown
// does not cancel, bounded by landGitTimeout. A check that fails holds p
// with why (a landHold error); any other error leaves p as it was.
func (e *Engine) landProposal(ctx context.Context, p store.LearnProposal, requested bool) (store.LearnProposal, store.LearnVersion, error) {
	if why := e.landUnavailable(); why != "" {
		return p, store.LearnVersion{}, errors.New(why)
	}
	git, checkout := e.d.LearnGit, e.d.Checkout
	octx := context.WithoutCancel(ctx)
	hold := func(why string) (store.LearnProposal, store.LearnVersion, error) {
		got, err := e.st.DecideLearnProposal(octx, p.ID, []string{store.LearnPassed, store.LearnHeld}, store.LearnHeld,
			textx.Clip(why, gateReasonRunes), e.now(), nil)
		if err != nil {
			return p, store.LearnVersion{}, err
		}
		e.noteHeld(octx, got, "the landing stopped: "+why, !requested)
		return got, store.LearnVersion{}, landHold(why)
	}
	if landed, ok, err := e.landedAlready(ctx, p); err != nil {
		return p, store.LearnVersion{}, err
	} else if ok {
		return e.recordLanding(octx, p, landed, requested)
	}
	branch, err := git.CurrentBranch(ctx, checkout)
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	if branch != learnBase {
		return hold(fmt.Sprintf("the checkout is on %s, not %s", cmp.Or(branch, "a detached HEAD"), learnBase))
	}
	switch op, err := git.InProgress(ctx, checkout); {
	case err != nil:
		return p, store.LearnVersion{}, err
	case op != "":
		return hold(fmt.Sprintf("a %s is in progress in the checkout", op))
	}
	if err := e.ensureCandidate(ctx, p, true); errors.Is(err, errCandidateGone) { // an earlier landing may have rebased it
		return hold(err.Error())
	} else if err != nil {
		return p, store.LearnVersion{}, err
	}
	master, err := git.RevParse(ctx, checkout, "refs/heads/"+learnBase)
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	head, err := git.RevParse(ctx, checkout, "refs/heads/"+p.Branch)
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	// The gate measured the candidate on its base: a master that no longer
	// holds it was reset or rewritten, and a fast-forward would bring back
	// what the operator dropped.
	switch has, err := e.holds(ctx, master, p.BaseSHA); {
	case err != nil:
		return p, store.LearnVersion{}, err
	case !has:
		return hold(fmt.Sprintf("master (%s) no longer holds its base %s: it was reset or rewritten since the edit, so the gate's "+
			"measure is not of this master", learnShort(master), learnShort(p.BaseSHA)))
	}
	// Only the candidate's commit is replayed onto master: the commits after
	// its base, or after the master an earlier landing rebased it onto
	// (ensureCandidate checked it is that one commit).
	upstream := p.BaseSHA
	if head != p.CandidateSHA {
		if upstream, err = git.RevParse(ctx, checkout, head+"^"); err != nil {
			return p, store.LearnVersion{}, err
		}
	}
	lctx, cancel := context.WithTimeout(octx, landGitTimeout) // a stop never cuts the rebase and the merge apart
	defer cancel()
	if upstream != master {
		if err := git.RebaseOnto(lctx, p.Scratch, master, upstream); errors.Is(err, gitx.ErrRebaseConflict) {
			return hold(fmt.Sprintf("master moved to %s since its base %s, and rebasing it onto master conflicts", learnShort(master),
				learnShort(p.BaseSHA)))
		} else if err != nil {
			return hold("rebasing it onto master failed: " + oneLine(err.Error(), retroWhyRunes))
		}
		e.log.Info("skill landing: rebased onto master", "proposal", p.ID, "master", learnShort(master))
	}
	// The tree that lands must render with this build, as the restart on it
	// checks first: an operator's commit that renamed a template field
	// rebases cleanly under a candidate that still uses the old one.
	cfg, err := e.cfg.WithCandidate(p.Scratch)
	if err == nil {
		_, err = CheckPrompts(cfg)
	}
	if err != nil {
		return hold("its prompts do not render with this build once on master: " + oneLine(err.Error(), 600))
	}
	dirty, err := git.DirtyPaths(lctx, checkout, p.Files...)
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	if len(dirty) > 0 {
		return hold(fmt.Sprintf("%s %s uncommitted changes in the checkout: commit or stash them, then `magnum learn --review` lands it",
			strings.Join(dirty, ", "), map[bool]string{true: "has", false: "have"}[len(dirty) == 1]))
	}
	e.event(octx, "info", learnSubject, "learn.land_begin", fmt.Sprintf("landing skill proposal %d: fast-forwarding master to %s", p.ID, p.Branch),
		map[string]any{"proposal": p.ID, "branch": p.Branch})
	if err := git.MergeFFOnly(lctx, checkout, p.Branch); errors.Is(err, gitx.ErrNotFastForward) {
		return hold("master moved during the landing")
	} else if err != nil {
		return hold("the fast-forward of master failed: " + oneLine(err.Error(), retroWhyRunes))
	}
	landed, err := git.RevParse(lctx, checkout, "refs/heads/"+learnBase)
	if err != nil {
		return p, store.LearnVersion{}, err // the next landing finds the branch on master and records it
	}
	return e.recordLanding(octx, p, landed, requested)
}

// landedAlready reports whether master holds p's branch already, and its
// commit: a landing a crash or a registry error cut short after the
// fast-forward, which is recorded rather than landed again, rejected or
// expired. False without a checkout, without the branch or when the branch
// does not descend from p's base.
func (e *Engine) landedAlready(ctx context.Context, p store.LearnProposal) (string, bool, error) {
	if e.landUnavailable() != "" || !strings.HasPrefix(p.Branch, learnBranchPrefix) {
		return "", false, nil
	}
	git, checkout := e.d.LearnGit, e.d.Checkout
	head, err := git.RevParse(ctx, checkout, "refs/heads/"+p.Branch)
	if errors.Is(err, gitx.ErrNoSuchRef) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	if head == p.BaseSHA { // nothing of its own
		return "", false, nil
	}
	// A branch reset below its base holds nothing of the proposal's.
	if above, err := e.holds(ctx, head, p.BaseSHA); err != nil || !above {
		return "", false, err
	}
	master, err := git.RevParse(ctx, checkout, "refs/heads/"+learnBase)
	if err != nil {
		return "", false, err
	}
	on, err := e.holds(ctx, master, head)
	return head, on, err
}

// recordLanding records that p landed on master as commit landed: the
// proposal landed with the commit and the version with the skill's and
// the prompts' hashes, in one transaction (store.LandLearnProposal: a
// failure leaves p as it was, and the next landing finds the branch on
// master and records it), then its worktree and branch go, the day's
// landing is counted, the restart is marked (KVLearnRestart) and a toast
// says so.
func (e *Engine) recordLanding(ctx context.Context, p store.LearnProposal, landed string, requested bool) (store.LearnProposal, store.LearnVersion, error) {
	skill, prompts := learnHashes(p.Scratch)
	if skill == "" || prompts == "" {
		skill, prompts = learnHashes(e.d.Checkout)
	}
	now := e.now()
	got, v, err := e.st.LandLearnProposal(ctx, p.ID, []string{store.LearnPassed, store.LearnHeld}, store.LearnVersion{LandedSHA: landed,
		SkillSHA: cmp.Or(skill, "unknown"), PromptsSHA: cmp.Or(prompts, "unknown"), LandedAt: now})
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	e.dropCandidate(ctx, p.Scratch, p.Branch)
	day := LandDay{Day: store.DayKey(now), Landed: e.landedToday(ctx) + 1}
	if b, err := json.Marshal(day); err == nil {
		e.setKV(ctx, KVLearnLandDay, string(b))
	}
	manual := e.restartManual()
	if b, err := json.Marshal(LearnRestartMark{Version: v.ID, Proposal: p.ID, Landed: now, Manual: manual}); err == nil {
		e.setKV(ctx, KVLearnRestart, string(b))
	}
	restart := "the daemon restarts on it once nothing is in flight"
	if manual {
		restart = "it reaches the rounds after `magnum daemon-restart --when-idle`"
	}
	e.event(ctx, "info", learnSubject, "learn.landed", fmt.Sprintf("skill proposal %d landed on master as %s: skill version %d; %s", p.ID,
		learnShort(landed), v.ID, restart), map[string]any{"proposal": p.ID, "version": v.ID, "commit": learnShort(landed), "manual": manual,
		"requested": requested})
	title := fmt.Sprintf("skill version %d landed", v.ID)
	e.info(notify.Item{Key: "learn-landed:" + strconv.FormatInt(p.ID, 10), Title: title, Line: title,
		Body: fmt.Sprintf("Proposal %d is on master as %s (not pushed); %s.", p.ID, learnShort(landed), restart)})
	if manual {
		title := fmt.Sprintf("restart magnum for skill version %d", v.ID)
		e.info(notify.Item{Key: "learn-restart:" + strconv.FormatInt(v.ID, 10), Title: title, Line: title,
			Body: "`magnum daemon-restart --when-idle` restarts it once nothing is in flight; the rounds use the landed skill and prompts from then on."})
	}
	return got, v, nil
}

// holds reports whether commit tip holds commit c (c is tip or one of its
// ancestors).
func (e *Engine) holds(ctx context.Context, tip, c string) (bool, error) {
	base, err := e.d.LearnGit.MergeBase(ctx, e.d.Checkout, c, tip)
	if errors.Is(err, gitx.ErrNoMergeBase) {
		return false, nil
	}
	return err == nil && base == c, err
}

// rebasedCandidate checks head, where p's branch moved from p's commit:
// only an earlier landing moves it, rebasing the candidate onto master, so
// head must be a single commit (no second parent) on a commit master holds,
// with the candidate's patch id (gitx.PatchID); errCandidateGone for
// anything else (a commit made in the worktree would land ungated).
func (e *Engine) rebasedCandidate(ctx context.Context, p store.LearnProposal, head string) error {
	git, checkout := e.d.LearnGit, e.d.Checkout
	moved := fmt.Errorf("%w: its branch %s moved to %s, which is not its commit %s rebased onto master", errCandidateGone, p.Branch,
		learnShort(head), learnShort(p.CandidateSHA))
	switch _, err := git.RevParse(ctx, checkout, head+"^2"); {
	case err == nil:
		return moved // a merge
	case !errors.Is(err, gitx.ErrNoSuchRef):
		return err
	}
	parent, err := git.RevParse(ctx, checkout, head+"^")
	if errors.Is(err, gitx.ErrNoSuchRef) {
		return moved
	} else if err != nil {
		return err
	}
	master, err := git.RevParse(ctx, checkout, "refs/heads/"+learnBase)
	if err != nil {
		return err
	}
	if on, err := e.holds(ctx, master, parent); err != nil {
		return err
	} else if !on {
		return moved
	}
	want, err := git.PatchID(ctx, checkout, p.CandidateSHA)
	if err != nil {
		return err
	}
	if got, err := git.PatchID(ctx, checkout, head); err != nil {
		return err
	} else if got != want {
		return moved
	}
	return nil
}

// learnHashes are what a landed version records of dir (a checkout or a
// worktree of magnum): the judge skill's hash as its copies are named
// (config.SnapshotPrompts: the first 12 hex digits of its SHA-256, which
// the declines record of the rounds), and one over prompts/*.md, by name;
// "" for one it cannot read.
func learnHashes(dir string) (skill, prompts string) {
	if b, err := fsx.ReadRegular(filepath.Join(dir, filepath.FromSlash(learn.SkillFile)), notes.MaxFileBytes); err == nil {
		sum := sha256.Sum256(b)
		skill = hex.EncodeToString(sum[:])[:12]
	}
	files, err := filepath.Glob(filepath.Join(dir, "prompts", "*.md"))
	if err != nil || len(files) == 0 {
		return skill, ""
	}
	slices.Sort(files)
	h := sha256.New()
	for _, f := range files {
		b, err := fsx.ReadRegular(f, notes.MaxFileBytes)
		if err != nil {
			return skill, ""
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.Base(f), len(b))
		h.Write(b)
	}
	return skill, hex.EncodeToString(h.Sum(nil))[:12]
}

// noteLearnRestarted marks the version the daemon was to restart on
// restarted (this start, whatever caused it: an idle exit, a
// `daemon-restart`, a new build), which starts its measurement window, and
// clears KVLearnRestart. Every other version never restarted on starts now
// too: one whose landing wrote no mark (a crash or a registry error right
// after its transaction; without a mark the newest of them is named), or
// whose mark a later recorded landing replaced (it never ran alone, and its
// window is empty). A dry run marks nothing.
func (e *Engine) noteLearnRestarted(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	m, ok := e.learnRestart(ctx)
	vs, err := e.st.LearnVersions(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "skill versions: list them for the restart")
		return // the mark waits for the next start
	}
	now := e.now()
	for _, v := range vs { // newest first
		if v.RestartedAt != nil {
			continue
		}
		if err := e.st.MarkLearnRestarted(ctx, v.ID, now); err != nil {
			e.warnUnlessStopped(ctx, err, "skill version: mark it restarted", "version", v.ID)
			return
		}
		if !ok {
			m, ok = LearnRestartMark{Version: v.ID, Proposal: v.ProposalID}, true
		}
	}
	e.delKV(ctx, KVLearnRestart)
	if ok {
		e.event(ctx, "info", learnSubject, "learn.restart", fmt.Sprintf("the daemon runs skill version %d (proposal %d) since this start", m.Version,
			m.Proposal), map[string]any{"version": m.Version, "proposal": m.Proposal})
	}
}

// learnRestartDue is the landed version the daemon exits to restart on
// ([learn] restart idle, launchd to start it again, neither a dry run nor
// --once); false when none is due.
func (e *Engine) learnRestartDue(ctx context.Context) (LearnRestartMark, bool) {
	if e.d.DryRun || e.once || e.restartManual() {
		return LearnRestartMark{}, false
	}
	m, ok := e.learnRestart(ctx)
	if !ok || m.Manual {
		return LearnRestartMark{}, false
	}
	return m, true
}

// requestLearnLand serves the y of `magnum learn --review`: the held
// proposal lands at once (landProposal), whatever the day's count says, or
// stays held and the answer says why. It is refused while the daemon has
// not restarted on the version landed last (each version runs before the
// next lands, as at a tick) and while a gate or an edit runs (they add and
// remove worktrees of the checkout).
func (e *Engine) requestLearnLand(ctx context.Context, p LearnDecisionPayload) (string, error) {
	prop, err := e.st.LearnProposalByID(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there is no skill proposal %d (`magnum learn` lists them)", p.ID)
	}
	if err != nil {
		return "", err
	}
	if prop.State != store.LearnHeld {
		return "", fmt.Errorf("proposal %d is %s: only a held proposal waits for review", p.ID, prop.State)
	}
	if e.d.DryRun {
		e.rec.Record(ctx, learnSubject, "learn_land", fmt.Sprintf("land proposal %d (%s)", p.ID, prop.Branch))
		return "dry run: would land proposal " + strconv.FormatInt(p.ID, 10), nil
	}
	m, pending := e.learnRestart(ctx)
	switch {
	case e.landUnavailable() != "":
		return "", fmt.Errorf("proposal %d cannot land: %s", p.ID, e.landUnavailable())
	case pending:
		return "", fmt.Errorf("proposal %d cannot land before the daemon restarted on skill version %d (each version runs before the next "+
			"lands); ask again after the restart", p.ID, m.Version)
	case e.gateBusy():
		return "", fmt.Errorf("proposal %d cannot land while a skill gate runs; ask again once it ends", p.ID)
	case e.editBusy():
		return "", fmt.Errorf("proposal %d cannot land while a skill edit runs; ask again once it ends", p.ID)
	}
	_, v, err := e.landProposal(ctx, prop, true)
	hold, held := errors.AsType[landHold](err)
	switch {
	case held:
		return "", fmt.Errorf("proposal %d stays held: %s", p.ID, string(hold))
	case err != nil:
		return "", fmt.Errorf("proposal %d did not land: %w", p.ID, err)
	}
	msg := fmt.Sprintf("landed proposal %d on master as %s: skill version %d (nothing is pushed)", p.ID, learnShort(v.LandedSHA), v.ID)
	if e.restartManual() {
		return msg + "; `magnum daemon-restart --when-idle` brings it to the rounds", nil
	}
	return msg + "; the daemon restarts on it once nothing is in flight", nil
}
