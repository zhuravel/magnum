package engine

// The handover of a PR's fixes to the factory, and back (DECISIONS "The fix
// loop moves into the factory"). `magnum fix --handover <pr>` flips the
// PR's owner row to the factory at once, by compare-and-set, when the drain
// proof holds (fixQuiet: none of magnum's fix work in flight); otherwise
// the PR hands over (KVPRFixHandover): magnum starts no new loop on it
// that nobody asked for, a loop in flight ends at its cycle boundary with
// no next cycle, and the tick flips the PR once it is quiet. `magnum fix
// --handover all` (KVFixHandoverAll) does so for every own PR, now and
// later. The flip takes the PR's reservation, so it never meets a fixer's
// start, which writes its loop's row under the same reservation after a
// last read of the owner row (claimFixRow).
//
// The way back is one way and narrow: `magnum fix --handover back <pr>`
// returns only a PR the factory has not worked since its import, which the
// factory's lease says (FixLease.Worked), and refuses one it worked: there
// is no export back. It flips the row to returning, where neither daemon
// starts a fixer, and the tick takes the row away once a fresh read of the
// lease still shows no work of the factory's, or puts it back to the
// factory, under its first flip, when the factory worked the PR before it
// saw the return. `back all` lifts --handover all, calls off the
// handovers that wait, and returns every PR it may.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqFixHandover hands a PR's fixes, or every own PR's, to the factory, or
// one back (HandoverPayload).
const ReqFixHandover = "fix_handover"

// KVFixHandoverAll says that every own PR goes to the factory (a handover
// as JSON), `magnum fix --handover all`.
const KVFixHandoverAll = "fix.handover_all"

// KVPRFixHandover holds the handover of PR prID that waits for its fix work
// to end (a handover as JSON).
func KVPRFixHandover(prID int64) string { return fmt.Sprintf("pr.%d.fix_handover", prID) }

// Who flips an owner row (fixwork.FixOwner.By).
const (
	handoverBy    = "magnum fix --handover"
	handoverAllBy = "magnum fix --handover all"
	handBackBy    = "magnum fix --handover back"
)

// HandoverPayload is a `magnum fix --handover` request: All (every own PR)
// or the PR the target names, to the factory; Back returns that PR to
// magnum, or with All every PR it may and lifts All. DryRun answers what
// would happen and changes nothing.
type HandoverPayload struct {
	PRTarget
	All    bool `json:"all,omitempty"`
	Back   bool `json:"back,omitempty"`
	DryRun bool `json:"dry_run,omitempty"`
}

// handover is a KVPRFixHandover or KVFixHandoverAll row: when it was asked
// and by whom.
type handover struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// errFixHandingOver refuses a fix loop nobody asked for, and a loop's next
// cycle, on a PR that hands over to the factory (handingOver, a handover
// read as there): the loop that waits for it ends at its cycle boundary
// instead.
var errFixHandingOver = errors.New("the PR hands over to the factory")

// handoverAllOf reports whether every own PR goes to the factory
// (KVFixHandoverAll).
func handoverAllOf(ctx context.Context, st *store.Store) (bool, error) {
	_, ok, err := st.GetKV(ctx, KVFixHandoverAll)
	return ok, err
}

// handingOver reports whether pr hands over to the factory: a handover of
// its own waits (KVPRFixHandover), or every own PR goes and pr is one. A
// failed read is an error, which refuses the starts (handoverRefusal).
func (e *Engine) handingOver(ctx context.Context, pr store.PR) (bool, error) {
	if _, ok, err := e.st.GetKV(ctx, KVPRFixHandover(pr.ID)); err != nil || ok {
		return ok, err
	}
	all, err := handoverAllOf(ctx, e.st)
	return all && e.ownPR(pr), err
}

// handoverRefusal is startFix's refusal, while pr hands over, of a loop
// nobody asked for (by autofix) and of a loop's next cycle (prev.Cycle >
// 0): errFixHandingOver; nil otherwise. A read that fails refuses too, with
// a plain error and never errFixHandingOver, which a handover that was read
// alone gets: a loop's next cycle then ends as one that did not start, with
// the PR's attention (fix.failed), not as a handover nobody asked for.
func (e *Engine) handoverRefusal(ctx context.Context, pr store.PR, by string, prev FixState) error {
	if by != autofixBy && prev.Cycle == 0 {
		return nil
	}
	switch handing, err := e.handingOver(ctx, pr); {
	case err != nil:
		return fmt.Errorf("PR #%d: whether it hands over to the factory cannot be read: %w", pr.Number, err)
	case handing:
		return fmt.Errorf("PR #%d hands over to the factory (`magnum fix --handover`): magnum starts no new loop on it: %w", pr.Number, errFixHandingOver)
	}
	return nil
}

// requestHandover handles `magnum fix --handover` (HandoverPayload). A
// handover, of one PR or of all, its dry run too, is refused while
// [factory] fix is not "on": nothing on the factory's side would fix the
// PRs (the engine owner's ruling). The way back goes whatever the key says.
func (e *Engine) requestHandover(ctx context.Context, p HandoverPayload) (string, error) {
	named := p.Ref != "" || p.Number > 0
	switch {
	case p.All && named:
		return "", errors.New("magnum fix --handover: all or one PR, not both")
	case !p.Back && !e.cfg.Factory.Fixes():
		return "", fmt.Errorf("[factory] fix is %q: the factory fixes no PR, so nothing is handed over (set [factory] fix = %q first)",
			cmp.Or(e.cfg.Factory.Fix, config.FactoryFixOff), config.FactoryFixOn)
	case p.All && p.Back:
		return e.returnAllToMagnum(ctx, p.DryRun)
	case p.All:
		return e.handOverAll(ctx, p.DryRun)
	}
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	if p.Back {
		return e.returnToMagnum(ctx, repo, pr, p.DryRun)
	}
	return e.handOverOne(ctx, repo, pr, p.DryRun)
}

// handOverOne hands pr's fixes to the factory: at once when they are quiet
// (flipToFactory), else once they are (KVPRFixHandover, the tick's
// handOvers), and the answer names what is in flight with your action for
// each.
func (e *Engine) handOverOne(ctx context.Context, repo store.Repo, pr store.PR, dry bool) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	switch o, _, ok, err := e.fixOwner(ctx, pr.ID); {
	case !e.ownPR(pr):
		return "", fmt.Errorf("PR #%d is %s's, not yours: the factory fixes your own PRs alone", pr.Number, cmp.Or(deref(pr.AuthorLogin), "someone else"))
	case err != nil:
		return "", fmt.Errorf("%s: who fixes this PR cannot be read (%w): nothing changed", label, err)
	case ok && o.Factory():
		return fmt.Sprintf("the factory fixes %s already (since %s)", label, waitClock(o.At, e.now())), nil
	case ok:
		return "", fmt.Errorf("%s is returning to magnum (since %s): ask again once magnum fixes it", label, waitClock(o.At, e.now()))
	}
	if dry {
		quiet, recs, err := e.fixQuietAs(ctx, repo, pr, true)
		switch {
		case err != nil:
			return "", err
		case quiet:
			return fmt.Sprintf("dry run: would hand %s to the factory now: none of magnum's fix work is in flight on it", label), nil
		}
		return fmt.Sprintf("dry run: would hand %s to the factory once this ends: %s", label, recordsWords(recs)), nil
	}
	// The handover first: from here on no loop nobody asks for starts, and
	// the flip that ends it takes it away.
	key := KVPRFixHandover(pr.ID)
	if err := e.st.SetKV(ctx, key, rowJSON(handover{At: e.now().UTC(), By: handoverBy})); err != nil {
		return "", err
	}
	flipped, recs, err := e.flipToFactory(ctx, repo, pr, handoverBy)
	switch {
	case err != nil:
		e.delKV(ctx, key)
		return "", fmt.Errorf("%s: %w", label, err)
	case flipped:
		return fmt.Sprintf("the factory fixes %s from now on: none of magnum's fix work was in flight on it", label), nil
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.handover_waits", "the PR hands over to the factory once magnum's fix work on it ends: "+recordsWords(recs),
		map[string]any{"records": recordKinds(recs)})
	return fmt.Sprintf("%s hands over to the factory once this ends (magnum starts no new loop on it meanwhile): %s", label, recordsWords(recs)), nil
}

// handOverAll hands every own PR to the factory (KVFixHandoverAll, which
// the tick's handOvers acts on from then on), and flips the quiet ones now.
func (e *Engine) handOverAll(ctx context.Context, dry bool) (string, error) {
	if dry {
		flips, waits, err := e.handoverCandidates(ctx, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("dry run: would hand every own PR to the factory: %d now, %d once their fix work ends", flips, waits), nil
	}
	switch added, err := e.st.AddKV(ctx, KVFixHandoverAll, rowJSON(handover{At: e.now().UTC(), By: handoverAllBy})); {
	case err != nil:
		return "", err
	case added:
		e.event(ctx, "info", "", "fix.handover_all", "every own PR hands over to the factory: magnum starts no new loop, and each PR flips once its fix work ends", nil)
	}
	flipped, waiting := e.handOvers(ctx)
	res := fmt.Sprintf("every own PR goes to the factory: %s now", textx.Count(flipped, "PR", "PRs"))
	if waiting > 0 {
		res += fmt.Sprintf(", %d once their fix work ends (`magnum fix --drained` lists it, with your action for each)", waiting)
	}
	return res, nil
}

// handoverCandidates counts, without a write, the PRs a handover pass
// would flip now and those it would wait for (fixQuiet); all: as if every
// own PR went.
func (e *Engine) handoverCandidates(ctx context.Context, all bool) (flips, waits int, err error) {
	prs, err := e.handoverPRs(ctx, all)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range prs {
		quiet, _, err := e.fixQuietAs(ctx, c.repo, c.pr, true)
		switch {
		case err != nil:
			return 0, 0, err
		case quiet:
			flips++
		default:
			waits++
		}
	}
	return flips, waits, nil
}

// handoverPR is a PR a handover waits for, and its repository.
type handoverPR struct {
	repo store.Repo
	pr   store.PR
	by   string
}

// handoverPRs are the PRs a handover waits for that magnum still fixes:
// those with a handover of their own, and, while every own PR goes (or
// all), the own open PRs.
func (e *Engine) handoverPRs(ctx context.Context, all bool) ([]handoverPR, error) {
	if !all {
		var err error
		if all, err = handoverAllOf(ctx, e.st); err != nil {
			return nil, err
		}
	}
	keys, err := e.st.KVKeys(ctx, "pr.*.fix_handover")
	if err != nil {
		return nil, err
	}
	by := map[int64]string{}
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err == nil && KVPRFixHandover(id) == key {
			by[id] = handoverBy
		}
	}
	if all {
		prs, err := e.st.ListPRs(ctx, store.PRFilter{GHOpen: true})
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			if _, ok := by[pr.ID]; !ok && e.ownPR(pr) {
				by[pr.ID] = handoverAllBy
			}
		}
	}
	var out []handoverPR
	for _, id := range slices.Sorted(maps.Keys(by)) {
		pr, err := e.st.PRByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			e.delKV(ctx, KVPRFixHandover(id))
			continue
		}
		if err != nil {
			return nil, err
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			return nil, err
		}
		switch _, _, owned, err := e.fixOwner(ctx, id); {
		case err != nil:
			continue // the flip's own read refuses it, and the doctor names the row
		case owned: // it is the factory's, or returning: nothing waits for it any more
			e.delKV(ctx, KVPRFixHandover(id))
			continue
		}
		out = append(out, handoverPR{repo: repo, pr: pr, by: by[id]})
	}
	return out, nil
}

// handOvers is the tick's handover pass: each PR a handover waits for
// flips to the factory once its fix work is quiet (flipToFactory), and
// each PR returning to magnum comes back or stays the factory's
// (returnPass). It returns how many flipped and how many still wait.
func (e *Engine) handOvers(ctx context.Context) (flipped, waiting int) {
	if e.d.DryRun {
		return 0, 0
	}
	prs, err := e.handoverPRs(ctx, false)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "fix handover: the PRs it waits for")
	}
	for _, c := range prs {
		ok, _, err := e.flipToFactory(ctx, c.repo, c.pr, c.by)
		switch {
		case err != nil:
			if e.logOnce(fmt.Sprintf("handover:%d", c.pr.ID), err.Error(), e.now()) {
				e.log.Warn("fix handover: the PR does not flip", "subject", prSubject(c.repo, c.pr.Number), "err", err)
			}
			waiting++
		case ok:
			flipped++
		default:
			waiting++
		}
	}
	e.returnPass(ctx)
	return flipped, waiting
}

// flipToFactory flips pr's owner row to the factory (by: who asked), by
// compare-and-set against no row, while the drain proof holds: under the
// PR's reservation (a round or a job that holds it refuses the flip, which
// a later tick tries again), magnum's fix work in flight is read and, with
// none, the row is added; the handover that waited for it goes, and
// fix.handed_over says so. flipped false: the records say what is in
// flight (none when the row was written by another meanwhile).
func (e *Engine) flipToFactory(ctx context.Context, repo store.Repo, pr store.PR, by string) (flipped bool, recs []FixRecord, err error) {
	if !e.reserve(pr.ID, &roundHandle{cancel: func() {}, stop: roundStop{stopping: true}}) {
		if recs, err = e.fixRecordsOf(ctx, repo, pr, true); err != nil || len(recs) > 0 {
			return false, recs, err
		}
		return false, []FixRecord{{Kind: FixRecReservation, What: "a round or a job holds the PR now (" + e.heldReason(pr.ID) + ")",
			Action: "nothing: the handover flips it once that ends"}}, nil
	}
	defer e.unreserve(pr.ID)
	if recs, err = e.fixRecordsOf(ctx, repo, pr, true); err != nil || len(recs) > 0 {
		return false, recs, err
	}
	raw, err := fixwork.FixOwner{Owner: fixwork.OwnerFactory, At: e.now().UTC(), By: by}.Value()
	if err != nil {
		return false, nil, err
	}
	switch added, err := e.st.AddKV(ctx, fixwork.KVPRFixOwner(pr.ID), raw); {
	case err != nil:
		return false, nil, err
	case !added:
		return false, nil, errors.New("its owner row changed meanwhile; nothing flipped")
	}
	e.delKV(ctx, KVPRFixHandover(pr.ID))
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.handed_over", "the factory fixes the PR from now on: none of magnum's fix work was in flight on it ("+by+")",
		map[string]any{"by": by})
	return true, nil, nil
}

// returnToMagnum returns pr's fixes to magnum (`magnum fix --handover back`): a
// handover that waits for it is called off; a PR the factory fixes flips
// to returning, by compare-and-set, only while the factory's lease says it
// did not work the PR since its import and does not fix it now. A lease
// that cannot be read, or no reader, refuses: magnum cannot tell.
func (e *Engine) returnToMagnum(ctx context.Context, repo store.Repo, pr store.PR, dry bool) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	if all, err := handoverAllOf(ctx, e.st); err != nil || all {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s: every own PR goes to the factory (`magnum fix --handover all`); `magnum fix --handover back all` lifts that and returns each PR it may", label)
	}
	return e.returnOneToMagnum(ctx, repo, pr, dry)
}

// returnOneToMagnum is returnToMagnum once --handover all is lifted, or
// would be in a dry run (returnAllToMagnum): it judges pr alone.
func (e *Engine) returnOneToMagnum(ctx context.Context, repo store.Repo, pr store.PR, dry bool) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	o, raw, ok, err := e.fixOwner(ctx, pr.ID)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: who fixes this PR cannot be read (%w): nothing changed", label, err)
	case !ok:
		return e.callOffHandover(ctx, repo, pr, dry)
	case !o.Factory():
		return fmt.Sprintf("%s is returning to magnum already (since %s)", label, waitClock(o.At, e.now())), nil
	}
	if why := e.staysFactorys(ctx, repo, pr); why != "" {
		return "", fmt.Errorf("%s: %s", label, why)
	}
	if dry {
		return fmt.Sprintf("dry run: would return %s to magnum: the factory has not worked it since its import", label), nil
	}
	back, err := fixwork.FixOwner{Owner: fixwork.OwnerReturning, At: e.now().UTC(), By: handBackBy, Flipped: o.At}.Value()
	if err != nil {
		return "", err
	}
	switch swapped, err := e.st.SetKVIf(ctx, fixwork.KVPRFixOwner(pr.ID), raw, back); {
	case err != nil:
		return "", err
	case !swapped:
		return "", fmt.Errorf("%s: its owner row changed meanwhile; nothing changed, ask again", label)
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.returning", "the PR returns to magnum: the factory has not worked it since its import", nil)
	return fmt.Sprintf("%s returns to magnum: no fixer starts on it until magnum takes it back, at a tick that finds the factory's lease still "+
		"without work of its own", label), nil
}

// callOffHandover calls off the handover that waits for pr (magnum fixes
// it, and goes on), or says that magnum fixes it.
func (e *Engine) callOffHandover(ctx context.Context, repo store.Repo, pr store.PR, dry bool) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	switch _, pending, err := e.st.GetKV(ctx, KVPRFixHandover(pr.ID)); {
	case err != nil:
		return "", err
	case !pending:
		return fmt.Sprintf("magnum fixes %s already", label), nil
	case dry:
		return fmt.Sprintf("dry run: would call off the handover of %s: magnum goes on fixing it", label), nil
	}
	if err := e.st.DeleteKV(ctx, KVPRFixHandover(pr.ID)); err != nil {
		return "", err
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.handover_off", "the handover to the factory is called off: magnum goes on fixing the PR", nil)
	return fmt.Sprintf("the handover of %s is called off: magnum goes on fixing it", label), nil
}

// staysFactorys says why pr stays the factory's ("" = it may return): the
// factory worked it since its import, fixes it now, or magnum cannot read
// its lease (no reader in this build, or a failed read).
func (e *Engine) staysFactorys(ctx context.Context, repo store.Repo, pr store.PR) string {
	if e.d.FixLeaseOf == nil {
		return "this daemon reads no factory lease, so it cannot tell whether the factory worked the PR: it stays the factory's"
	}
	l, err := e.d.FixLeaseOf(ctx, repo.FullName(), pr.Number)
	switch {
	case err != nil:
		return fmt.Sprintf("the factory's lease cannot be read (%v): it stays the factory's; ask again", err)
	case l.Fixing:
		return fmt.Sprintf("the factory fixes it now (cycle %d, %s): it stays the factory's", l.Cycle, cmp.Or(l.Phase, "at work"))
	case l.Worked:
		return "the factory worked it since its import: it stays the factory's, as there is no export back (fix it by hand under [factory] fix = \"off\")"
	}
	return ""
}

// returnAllToMagnum lifts --handover all, calls off every handover that waits,
// and returns every PR the factory fixes and may give back (returnOneToMagnum,
// as after the lift: a dry run judges each PR the same way, the lift
// skipped); the answer names those that stay the factory's, and why.
func (e *Engine) returnAllToMagnum(ctx context.Context, dry bool) (string, error) {
	vals, err := e.st.KVValues(ctx, "pr.*.fix_owner")
	if err != nil {
		return "", err
	}
	if !dry {
		if raw, ok, err := e.st.GetKV(ctx, KVFixHandoverAll); err != nil {
			return "", err
		} else if ok {
			if _, err := e.st.DeleteKVIf(ctx, KVFixHandoverAll, raw); err != nil {
				return "", err
			}
			e.event(ctx, "info", "", "fix.handover_all_off", "own PRs no longer all hand over to the factory", nil)
		}
		keys, err := e.st.KVKeys(ctx, "pr.*.fix_handover")
		if err != nil {
			return "", err
		}
		e.delKV(ctx, keys...)
	}
	var back int
	var stay []string
	for _, key := range slices.Sorted(maps.Keys(vals)) {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || fixwork.KVPRFixOwner(id) != key {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		if o, err := fixwork.ParseFixOwner(vals[key]); err == nil && !o.Factory() {
			continue // returning already
		}
		if _, err := e.returnOneToMagnum(ctx, repo, pr, dry); err != nil { // judged as after the lift, which a dry run skips
			stay = append(stay, err.Error())
			continue
		}
		back++
	}
	res := fmt.Sprintf("own PRs no longer all go to the factory; %s to magnum", textx.Count(back, "PR returns", "PRs return"))
	if dry {
		res = fmt.Sprintf("dry run: would lift --handover all; %s to magnum", textx.Count(back, "PR would return", "PRs would return"))
	}
	if len(stay) > 0 {
		res += "; these stay the factory's: " + strings.Join(stay, "; ")
	}
	return res, nil
}

// returnPass is the tick's look at each PR returning to magnum: once a
// fresh read of the factory's lease, after the return began, shows no work
// of the factory's since its import and none now, its row goes and magnum
// fixes it again (fix.returned); a lease that shows work puts it back to
// the factory under the flip it returned from (fix.return_refused, a warn:
// the factory worked it before it saw the return). A lease that cannot be
// read, or no reader, leaves it returning: no fixer starts meanwhile.
func (e *Engine) returnPass(ctx context.Context) {
	vals, err := e.st.KVValues(ctx, "pr.*.fix_owner")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "fix handover: the owner rows")
		return
	}
	for key, raw := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || fixwork.KVPRFixOwner(id) != key {
			continue
		}
		o, err := fixwork.ParseFixOwner(raw)
		if err != nil || o.Owner != fixwork.OwnerReturning {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		e.returnOne(ctx, repo, pr, o, raw)
	}
}

// returnOne decides on pr's return (o, its returning row raw), as
// returnPass says.
func (e *Engine) returnOne(ctx context.Context, repo store.Repo, pr store.PR, o fixwork.FixOwner, raw string) {
	subject := prSubject(repo, pr.Number)
	if e.d.FixLeaseOf == nil {
		return
	}
	l, err := e.d.FixLeaseOf(ctx, repo.FullName(), pr.Number)
	if err != nil || (!l.ReadAt.IsZero() && l.ReadAt.Before(o.At)) {
		if err != nil && e.logOnce(fmt.Sprintf("handback:%d", pr.ID), err.Error(), e.now()) {
			e.log.Warn("fix handover: the factory's lease of a returning PR cannot be read; it stays returning", "subject", subject, "err", err)
		}
		return
	}
	if !l.Fixing && !l.Worked {
		if gone, err := e.st.DeleteKVIf(ctx, fixwork.KVPRFixOwner(pr.ID), raw); err == nil && gone {
			e.event(ctx, "info", subject, "fix.returned", "magnum fixes the PR again: the factory did not work it since its import", nil)
		}
		return
	}
	back, err := fixwork.FixOwner{Owner: fixwork.OwnerFactory, At: o.Flipped, By: handoverBy}.Value()
	if err != nil || o.Flipped.IsZero() {
		e.log.Warn("fix handover: a returning PR the factory worked names no flip; it stays returning", "subject", subject)
		return
	}
	if ok, err := e.st.SetKVIf(ctx, fixwork.KVPRFixOwner(pr.ID), raw, back); err == nil && ok {
		e.event(ctx, "warn", subject, "fix.return_refused", "the PR stays the factory's: the factory worked it before it saw the return", nil)
	}
}

// recordsWords are recs in words: each one's what and your action.
func recordsWords(recs []FixRecord) string {
	words := make([]string, 0, len(recs))
	for _, r := range recs {
		words = append(words, r.What+" ("+r.Action+")")
	}
	return strings.Join(words, "; ")
}

// recordKinds are recs' kinds, for an event's data.
func recordKinds(recs []FixRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Kind)
	}
	return out
}

// rowJSON is v as JSON ("{}" when it cannot be: never for the rows here).
func rowJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
