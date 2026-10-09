package engine

// The landing of a skill proposal (DECISIONS "A passing candidate lands as
// a local commit and the daemon restarts when idle"), the end of the
// self-improving loop's fourth stage. A proposal the gate passed lands at a
// tick, at most [learn] land_per_day a local day, and one the operator
// lands from `magnum learn --review` (ReqLearnLand) at once: the checkout
// must have master checked out and nothing in progress, the branch is
// rebased once in its worktree when master moved since its base (a
// conflict holds the proposal), the files it changes must be clean in the
// checkout, then master fast-forwards to it. Its branch and worktree go, the
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
// next lands), a gate or an edit runs (the gate replays the checkout's
// prompts), the daemon stops or drains, or a landing that failed waits
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
// the checkout's master: master checked out and nothing in progress; the
// branch rebased once in its worktree when it does not descend from master
// (a conflict holds it); the files it changes clean in the checkout; then
// master fast-forwards to it, its worktree and branch go, it becomes
// landed with the commit and a version records what landed. A branch that
// master already holds (a landing a crash cut short) is recorded without
// a merge. A check that fails holds p with why (a landHold error); any
// other error leaves p as it was.
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
	base, err := git.MergeBase(ctx, checkout, head, master)
	if err != nil && !errors.Is(err, gitx.ErrNoMergeBase) {
		return p, store.LearnVersion{}, err
	}
	landed := head // a branch master holds already landed as it is
	if base != head {
		if base != master {
			if err := git.RebaseOnto(ctx, p.Scratch, master); errors.Is(err, gitx.ErrRebaseConflict) {
				return hold(fmt.Sprintf("master moved to %s since its base %s, and rebasing it onto master conflicts", learnShort(master),
					learnShort(p.BaseSHA)))
			} else if err != nil {
				return hold("rebasing it onto master failed: " + oneLine(err.Error(), retroWhyRunes))
			}
			e.log.Info("skill landing: rebased onto master", "proposal", p.ID, "master", learnShort(master))
		}
		dirty, err := git.DirtyPaths(ctx, checkout, p.Files...)
		if err != nil {
			return p, store.LearnVersion{}, err
		}
		if len(dirty) > 0 {
			return hold(fmt.Sprintf("%s %s uncommitted changes in the checkout: commit or stash them, then `magnum learn --review` lands it",
				strings.Join(dirty, ", "), map[bool]string{true: "has", false: "have"}[len(dirty) == 1]))
		}
		e.event(ctx, "info", learnSubject, "learn.land_begin", fmt.Sprintf("landing skill proposal %d: fast-forwarding master to %s", p.ID, p.Branch),
			map[string]any{"proposal": p.ID, "branch": p.Branch})
		if err := git.MergeFFOnly(ctx, checkout, p.Branch); errors.Is(err, gitx.ErrNotFastForward) {
			return hold("master moved during the landing")
		} else if err != nil {
			return hold("the fast-forward of master failed: " + oneLine(err.Error(), retroWhyRunes))
		}
		if landed, err = git.RevParse(octx, checkout, "refs/heads/"+learnBase); err != nil {
			return p, store.LearnVersion{}, err // the next landing finds the branch on master and records it
		}
	}
	skill, prompts := learnHashes(p.Scratch)
	if skill == "" || prompts == "" {
		skill, prompts = learnHashes(checkout)
	}
	now := e.now()
	got, err := e.st.DecideLearnProposal(octx, p.ID, []string{store.LearnPassed, store.LearnHeld}, store.LearnLanded, "", now, nil)
	if err != nil {
		return p, store.LearnVersion{}, err
	}
	if err := e.st.MarkLearnLanded(octx, p.ID, landed, now); err != nil {
		e.log.Warn("skill landing: record the commit", "proposal", p.ID, "err", err)
	}
	v, err := e.st.CreateLearnVersion(octx, store.LearnVersion{ProposalID: p.ID, LandedSHA: landed, SkillSHA: cmp.Or(skill, "unknown"),
		PromptsSHA: cmp.Or(prompts, "unknown"), LandedAt: now})
	if err != nil {
		e.log.Warn("skill landing: record the version", "proposal", p.ID, "err", err)
	}
	e.dropCandidate(octx, p.Scratch, p.Branch)
	day := LandDay{Day: store.DayKey(now), Landed: e.landedToday(octx) + 1}
	if b, err := json.Marshal(day); err == nil {
		e.setKV(octx, KVLearnLandDay, string(b))
	}
	manual := e.restartManual()
	if v.ID != 0 {
		if b, err := json.Marshal(LearnRestartMark{Version: v.ID, Proposal: p.ID, Landed: now, Manual: manual}); err == nil {
			e.setKV(octx, KVLearnRestart, string(b))
		}
	}
	restart := "the daemon restarts on it once nothing is in flight"
	if manual {
		restart = "it reaches the rounds after `magnum daemon-restart --when-idle`"
	}
	e.event(octx, "info", learnSubject, "learn.landed", fmt.Sprintf("skill proposal %d landed on master as %s: skill version %d; %s", p.ID,
		learnShort(landed), v.ID, restart), map[string]any{"proposal": p.ID, "version": v.ID, "commit": learnShort(landed), "manual": manual,
		"requested": requested})
	title := fmt.Sprintf("skill version %d landed", v.ID)
	e.info(notify.Item{Key: "learn-landed:" + strconv.FormatInt(p.ID, 10), Title: title, Line: title,
		Body: fmt.Sprintf("Proposal %d is on master as %s (not pushed); %s.", p.ID, learnShort(landed), restart)})
	if manual && v.ID != 0 {
		title := fmt.Sprintf("restart magnum for skill version %d", v.ID)
		e.info(notify.Item{Key: "learn-restart:" + strconv.FormatInt(v.ID, 10), Title: title, Line: title,
			Body: "`magnum daemon-restart --when-idle` restarts it once nothing is in flight; the rounds use the landed skill and prompts from then on."})
	}
	return got, v, nil
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
// clears KVLearnRestart. A dry run marks nothing.
func (e *Engine) noteLearnRestarted(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	m, ok := e.learnRestart(ctx)
	if !ok {
		e.delKV(ctx, KVLearnRestart)
		return
	}
	if err := e.st.MarkLearnRestarted(ctx, m.Version, e.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.warnUnlessStopped(ctx, err, "skill version: mark it restarted", "version", m.Version)
		return
	}
	e.delKV(ctx, KVLearnRestart)
	e.event(ctx, "info", learnSubject, "learn.restart", fmt.Sprintf("the daemon runs skill version %d (proposal %d) since this start", m.Version,
		m.Proposal), map[string]any{"version": m.Version, "proposal": m.Proposal})
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
// stays held and the answer says why. It is refused while a gate or an
// edit runs (the gate replays the checkout's prompts).
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
	switch {
	case e.landUnavailable() != "":
		return "", fmt.Errorf("proposal %d cannot land: %s", p.ID, e.landUnavailable())
	case e.gateBusy():
		return "", fmt.Errorf("proposal %d cannot land while a skill gate runs (it replays the checkout's prompts); ask again once it ends", p.ID)
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
