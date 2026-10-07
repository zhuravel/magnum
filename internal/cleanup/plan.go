package cleanup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// slugRe is what --slug accepts: a database-name suffix, never a pattern.
var slugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)

// activePRStates are PR states with a round in flight (or about to be).
var activePRStates = []string{store.PRClaiming, store.PRReviewing, store.PRVerifying, store.PRPaused}

// Plan builds the plan for opts. It changes nothing: it reads the registry,
// scans the inventory once (with External for --external) and runs `du -sk`
// on the directories an action would delete. Only a store or inventory
// failure is an error; anything the options select but the plan leaves alone
// is listed in Skipped with a reason.
func (p *Planner) Plan(ctx context.Context, opts Options) (Plan, error) {
	if err := p.check(); err != nil {
		return Plan{}, err
	}
	if err := validate(opts); err != nil {
		return Plan{}, err
	}
	inv, err := p.Inventory.Scan(ctx, inventory.Options{External: opts.External})
	if err != nil {
		return Plan{}, fmt.Errorf("cleanup: inventory: %w", err)
	}
	return p.PlanFrom(ctx, opts, inv)
}

// PlanFrom is Plan over an inventory the caller already scanned (with
// inventory.Options{External: opts.External}), so a reconcile that has just
// scanned plans its cleanups without scanning again. The inventory may be a
// little old: Apply re-checks every action against the current registry.
func (p *Planner) PlanFrom(ctx context.Context, opts Options, inv inventory.Inventory) (Plan, error) {
	if err := p.check(); err != nil {
		return Plan{}, err
	}
	if err := validate(opts); err != nil {
		return Plan{}, err
	}
	b := &builder{p: p, opts: opts, now: p.now(), inv: inv,
		views: map[int64]*inventory.SlotView{}, repos: map[int64]store.Repo{},
		seenSlot: map[int64]bool{}, removed: map[int64]bool{}, seenSlug: map[string]bool{}, seenSkip: map[string]bool{}}
	if err := b.load(ctx); err != nil {
		return Plan{}, err
	}
	if err := b.build(ctx); err != nil {
		return Plan{}, err
	}
	plan := Plan{
		CreatedAt: b.now, DryRun: opts.DryRun, Options: opts,
		Actions: b.actions, Skipped: b.skipped, Warnings: append(slices.Clone(inv.Warnings), b.warnings...),
	}
	if plan.Actions == nil {
		plan.Actions = []Action{}
	}
	if plan.Skipped == nil {
		plan.Skipped = []Skip{}
	}
	plan.Totals = totals(plan.Actions)
	return plan, nil
}

// build runs the planners the options select; with no selector it is the
// default plan (closed PRs, then allowlisted orphans).
func (b *builder) build(ctx context.Context) error {
	o := b.opts
	if o.PR == nil && o.Slot == "" && !o.Orphans && o.Shrink == nil {
		if err := b.closedPRs(ctx); err != nil {
			return err
		}
		b.orphans()
		return nil
	}
	if o.PR != nil {
		if err := b.namedPR(ctx, *o.PR); err != nil {
			return err
		}
	}
	if o.Slot != "" {
		named := b.namedSlot
		if o.External {
			named = b.external
		}
		if err := named(ctx, o.Slot); err != nil {
			return err
		}
	}
	if o.Orphans {
		b.orphans()
		if o.Slug != "" {
			b.namedSlug(o.Slug)
		}
	}
	if o.Shrink != nil {
		return b.shrink(ctx, *o.Shrink)
	}
	return nil
}

func validate(o Options) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrOptions, msg) }
	switch {
	case o.Remove && o.Slot == "":
		return bad("--remove needs --slot")
	case o.Slug != "" && !o.Orphans:
		return bad("--slug needs --orphans")
	case o.Slug != "" && !slugRe.MatchString(o.Slug):
		return bad(fmt.Sprintf("invalid slug %q", o.Slug))
	case o.External && o.Slot == "":
		return bad("--external needs --slot <repoN>")
	case o.External && o.Remove:
		return bad("--external only resets a manual worktree; it never removes one")
	case o.PR != nil && (o.PR.Number <= 0 || strings.Count(o.PR.Repo, "/") != 1):
		return bad(fmt.Sprintf("invalid PR %s", o.PR))
	case o.Shrink != nil && *o.Shrink < 0:
		return bad("--shrink needs a non-negative slot count")
	case o.Idle && o.Shrink == nil:
		return bad("--idle needs --shrink")
	}
	return nil
}

// builder accumulates one plan.
type builder struct {
	p        *Planner
	opts     Options
	now      time.Time
	inv      inventory.Inventory
	slots    []store.Slot                  // every registry slot
	byPR     map[int64]store.Slot          // non-removed slots by pr_id
	views    map[int64]*inventory.SlotView // inventory facts by slot id
	active   map[int64]bool                // PRs with an active run
	repos    map[int64]store.Repo
	seenSlot map[int64]bool
	removed  map[int64]bool // slots with a planned remove_slot or remove_worktree
	seenSlug map[string]bool
	seenSkip map[string]bool
	actions  []Action
	skipped  []Skip
	warnings []string
}

func (b *builder) load(ctx context.Context) error {
	st := b.p.Store
	var err error
	if b.slots, err = st.ListSlots(ctx, store.SlotFilter{}); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	b.byPR = map[int64]store.Slot{}
	for _, sl := range b.slots {
		if sl.PRID != nil && sl.State != store.SlotRemoved {
			b.byPR[*sl.PRID] = sl
		}
	}
	for i := range b.inv.Slots {
		b.views[b.inv.Slots[i].Slot.ID] = &b.inv.Slots[i]
	}
	runs, err := st.ActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	b.active = map[int64]bool{}
	for _, r := range runs {
		b.active[r.PRID] = true
	}
	return nil
}

func (b *builder) add(a Action) {
	if a.SlotID != 0 {
		b.seenSlot[a.SlotID] = true
		if a.Kind != KindRelease {
			b.removed[a.SlotID] = true
		}
	}
	if a.Kind == KindDropDBs {
		b.seenSlug[a.Slug] = true
	}
	b.actions = append(b.actions, a)
}

func (b *builder) skip(subject, reason, detail string) {
	key := subject + "\x00" + reason
	if b.seenSkip[key] {
		return
	}
	b.seenSkip[key] = true
	b.skipped = append(b.skipped, Skip{Subject: subject, Reason: reason, Detail: detail})
}

// closedPRs plans the default release of closed PRs: past their grace and
// without a run in flight (store.ClosedPastGrace), plus PRs a previous apply
// left releasing. Closed PRs with a slot that are still inside the grace or
// have a run are listed as skipped; a closed PR without a slot gets a
// slot-less release (sessions parked, PR → released).
func (b *builder) closedPRs(ctx context.Context) error {
	st := b.p.Store
	due, err := st.ClosedPastGrace(ctx, b.now)
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	releasing, err := st.ListPRs(ctx, store.PRFilter{States: []string{store.PRReleasing}})
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	closed, err := st.ListPRs(ctx, store.PRFilter{States: []string{store.PRClosed}})
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	dueIDs := map[int64]bool{}
	for _, pr := range due {
		dueIDs[pr.ID] = true
		if err := b.prSlot(ctx, pr, false); err != nil {
			return err
		}
	}
	for _, pr := range releasing {
		if err := b.prSlot(ctx, pr, false); err != nil {
			return err
		}
	}
	for _, pr := range closed {
		if dueIDs[pr.ID] {
			continue
		}
		if _, ok := b.byPR[pr.ID]; !ok {
			continue
		}
		subject, err := b.prSubject(ctx, pr)
		if err != nil {
			return err
		}
		if detail := b.prActive(pr); detail != "" {
			b.skip(subject, SkipActiveRun, detail)
			continue
		}
		b.skip(subject, SkipGraceLeft, b.graceLeft(pr))
	}
	return nil
}

func (b *builder) graceLeft(pr store.PR) string {
	if pr.ReleaseAfter == nil {
		return ""
	}
	return humanDuration(pr.ReleaseAfter.Sub(b.now)) + " left"
}

func (b *builder) inGrace(pr store.PR) bool {
	return pr.State == store.PRClosed && pr.ReleaseAfter != nil && pr.ReleaseAfter.After(b.now)
}

// namedPR plans the release of an explicitly named PR's slot.
func (b *builder) namedPR(ctx context.Context, ref PRRef) error {
	subject := ref.String()
	repo, err := b.p.Store.RepoByFullName(ctx, ref.Repo)
	if errors.Is(err, store.ErrNotFound) {
		b.skip(subject, SkipNotFound, "repository not in the registry")
		return nil
	} else if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	pr, err := b.p.Store.PRByRepoNumber(ctx, repo.ID, ref.Number)
	if errors.Is(err, store.ErrNotFound) {
		b.skip(subject, SkipNotFound, "PR not in the registry")
		return nil
	} else if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	return b.prSlot(ctx, pr, true)
}

// prSlot plans the release (pool) or removal (per-PR) of pr's slot. A closed
// or releasing PR without a slot gets a slot-less release that only parks its
// sessions and finishes the PR's move to released.
func (b *builder) prSlot(ctx context.Context, pr store.PR, explicit bool) error {
	subject, err := b.prSubject(ctx, pr)
	if err != nil {
		return err
	}
	sl, ok := b.byPR[pr.ID]
	if !ok && closing(pr.State) {
		sl, ok = b.unclaimedPerPR(pr)
	}
	if !ok && !closing(pr.State) {
		if explicit {
			b.skip(subject, SkipNotFound, "no slot holds this PR")
		}
		return nil
	}
	if detail := b.prActive(pr); detail != "" {
		b.skip(subject, SkipActiveRun, detail)
		return nil
	}
	if b.inGrace(pr) && !(explicit && b.opts.Force) {
		b.skip(subject, SkipGraceLeft, b.graceLeft(pr))
		return nil
	}
	if !ok {
		b.add(Action{Kind: KindRelease, Subject: subject, Why: b.prWhy(pr) + ", no slot", Repo: b.repos[pr.RepoID].FullName(),
			PRID: pr.ID, PRState: pr.State, Reason: closeReason(pr)})
		return nil
	}
	why := b.prWhy(pr)
	if explicit && !closing(pr.State) {
		why = "requested (PR " + strings.ToLower(pr.GHState) + ")"
	}
	b.slotAction(ctx, subject, sl, &pr, why, false)
	return nil
}

// unclaimedPerPR finds the per-PR worktree row of pr that never got its
// pr_id: provisioning failed (broken, lost or still provisioning) before the
// claim. It is found by its name (slots.PRSlotName), so a closed PR's failed
// checkout is removed instead of leaking.
func (b *builder) unclaimedPerPR(pr store.PR) (store.Slot, bool) {
	name := slots.PRSlotName(b.repos[pr.RepoID].FullName(), pr.Number)
	for _, sl := range b.slots {
		if sl.Kind == store.SlotKindPerPR && sl.PRID == nil && sl.Name == name &&
			slices.Contains([]string{store.SlotProvisioning, store.SlotBroken, store.SlotLost, store.SlotRemoving}, sl.State) {
			return sl, true
		}
	}
	return store.Slot{}, false
}

// namedSlot plans the release (or, with Remove, the removal) of a managed slot.
func (b *builder) namedSlot(ctx context.Context, name string) error {
	subject := "slot:" + name
	sl, err := b.p.Store.SlotByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		b.skip(subject, SkipNotFound, "no managed slot with this name (manual worktrees need --external)")
		return nil
	} else if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	why := "requested"
	var pr *store.PR
	if sl.PRID != nil {
		got, err := b.p.Store.PRByID(ctx, *sl.PRID)
		if err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		pr = &got
		ps, err := b.prSubject(ctx, got)
		if err != nil {
			return err
		}
		why += ", holds " + ps
		if closing(got.State) {
			why += ", " + b.prWhy(got)
		}
		if detail := b.prActive(got); detail != "" {
			b.skip(subject, SkipActiveRun, detail)
			return nil
		}
		if b.inGrace(got) && !b.opts.Force {
			b.skip(subject, SkipGraceLeft, b.graceLeft(got))
			return nil
		}
	}
	b.slotAction(ctx, subject, sl, pr, why, b.opts.Remove)
	return nil
}

// shrink plans the removal of free pool slots beyond max(pool.min, n),
// oldest idle first (ties: highest slot number first). Capacity counts the
// usable slots the way the engine provisions (unusableStates excluded) minus
// those this plan already removes.
func (b *builder) shrink(ctx context.Context, n int) error {
	for _, pool := range b.p.Config.Pools {
		keep := max(pool.Min, n)
		var usable, free []store.Slot
		for _, sl := range b.slots {
			if sl.Kind != store.SlotKindPool || !strings.EqualFold(sl.RepoFullName, pool.Repo) ||
				slices.Contains(unusableStates, sl.State) || b.removed[sl.ID] {
				continue
			}
			usable = append(usable, sl)
			if sl.State == store.SlotFree {
				free = append(free, sl)
			}
		}
		excess := len(usable) - keep
		if excess <= 0 {
			continue
		}
		slices.SortStableFunc(free, func(x, y store.Slot) int {
			if c := idleSince(x).Compare(idleSince(y)); c != 0 {
				return c
			}
			return -natural(x.Name, y.Name)
		})
		for _, sl := range free {
			if excess == 0 {
				break
			}
			idle := b.now.Sub(idleSince(sl))
			if b.opts.Idle && idle < pool.IdleRemoveAfter.Duration {
				continue
			}
			why := fmt.Sprintf("shrink %s to %d slots, idle %s", pool.Repo, keep, humanDuration(idle))
			if b.slotAction(ctx, "slot:"+sl.Name, sl, nil, why, true) {
				excess--
			}
		}
	}
	return nil
}

func idleSince(sl store.Slot) time.Time {
	if sl.LastUsedAt != nil {
		return *sl.LastUsedAt
	}
	return sl.CreatedAt
}

func natural(a, b string) int {
	switch {
	case inventory.NaturalLess(a, b):
		return -1
	case inventory.NaturalLess(b, a):
		return 1
	}
	return 0
}

// Slot states each operation accepts: the slots package's lists, plus
// removing, which resumes an interrupted removal.
var (
	removeStates   = append(slices.Clone(slots.RemoveStates), store.SlotRemoving)
	removePRStates = append(slices.Clone(slots.RemovePRStates), store.SlotRemoving)
	// unusableStates are pool slots that hold no capacity (engine
	// activeSlotCount): gone, broken or on their way out.
	unusableStates = []string{store.SlotRemoved, store.SlotLost, store.SlotBroken, store.SlotRemoving}
)

// slotAction applies the slot guards and adds a release, remove_slot or
// remove_worktree action for sl. It reports whether an action was added.
func (b *builder) slotAction(ctx context.Context, subject string, sl store.Slot, pr *store.PR, why string, remove bool) bool {
	if b.seenSlot[sl.ID] {
		return false
	}
	if sl.Kind == store.SlotKindExternal {
		b.skip(subject, SkipNotManaged, "manual worktree; reset it with --external --slot "+sl.Name)
		return false
	}
	if sl.State == store.SlotBusy {
		b.skip(subject, SkipActiveRun, "slot "+sl.Name+" is busy")
		return false
	}
	if sl.Pinned || (pr != nil && pr.Pinned) {
		b.skip(subject, SkipPinned, "unpin first")
		return false
	}
	if sl.HoldReason != nil {
		b.skip(subject, SkipHoldReason, *sl.HoldReason)
		return false
	}
	view := b.views[sl.ID]
	if a, ok := foreignAgent(view); ok {
		b.skip(subject, SkipAgentWorking, agentDetail(a))
		return false
	}
	perPR := sl.Kind == store.SlotKindPerPR
	kind := KindRelease
	switch {
	case perPR:
		kind = KindRemoveWorktree
	case remove:
		kind = KindRemoveSlot
	}
	allowed := slots.ReleaseStates
	switch kind {
	case KindRemoveSlot:
		allowed = removeStates
		if b.opts.Force {
			allowed = slices.Concat(allowed, slots.ForceRemoveStates)
		}
	case KindRemoveWorktree:
		allowed = removePRStates
	}
	if !slices.Contains(allowed, sl.State) {
		detail := "slot " + sl.Name + " is " + sl.State
		if kind == KindRemoveSlot && slices.Contains(slots.ForceRemoveStates, sl.State) {
			detail += " (release it first or use --force)"
		}
		b.skip(subject, SkipSlotState, detail)
		return false
	}
	if !perPR && b.p.Config.PoolFor(sl.RepoFullName) == nil {
		b.skip(subject, SkipNotManaged, "no [[pool]] configured for "+sl.RepoFullName)
		return false
	}
	a := Action{
		Kind: kind, Subject: subject, Why: why, Repo: sl.RepoFullName, Slot: sl.Name, SlotID: sl.ID,
		Path: sl.Path, Slug: slots.SlotDBSlug(sl),
	}
	if pr != nil {
		a.PRID, a.PRState = pr.ID, pr.State
	} else if sl.PRID != nil {
		a.PRID = *sl.PRID
	}
	a.Reason = "cleanup"
	if pr != nil && closing(pr.State) {
		a.Reason = closeReason(*pr)
	}
	// Changes in a slot magnum owns are its own residue (tests rewriting
	// db/schema.rb, bundle install, copy_files) unless the slots package has
	// evidence a human made them (a human's agent was checked above). The
	// slots guard holds a human's changes; residue is discarded with a
	// slot.discarded event.
	st, known := b.status(ctx, sl, view)
	if kind == KindRelease {
		// A release resets tracked changes and keeps untracked files; the
		// slots guard holds a human's whatever --force says.
		if known && st.Tracked > 0 {
			changes := fmt.Sprintf("%d tracked changes in %s", st.Tracked, sl.Path)
			if human, held := b.humanChanges(ctx, sl); held {
				b.skip(subject, SkipDirty, changes+"; "+human+"; commit, stash or discard them")
				return false
			}
			a.Why += fmt.Sprintf(" (discards %d tracked changes magnum left)", st.Tracked)
		}
	} else {
		// A removal deletes the directory: untracked files count too.
		if !known && !b.opts.Force {
			b.skip(subject, SkipDirty, "could not read git status of "+sl.Path)
			return false
		}
		if known && st.Dirty() {
			changes := fmt.Sprintf("%d tracked changes and %d untracked files in %s", st.Tracked, st.Untracked, sl.Path)
			human, held := b.humanChanges(ctx, sl)
			switch {
			case held && !b.opts.Force:
				b.skip(subject, SkipDirty, changes+"; "+human)
				return false
			case held:
				a.Why += fmt.Sprintf(" (--force discards %d tracked changes and %d untracked files: %s)", st.Tracked, st.Untracked, human)
			default:
				a.Why += fmt.Sprintf(" (discards %d tracked changes and %d untracked files magnum left)", st.Tracked, st.Untracked)
			}
		}
		a.Force = b.opts.Force
		if slotExists(sl, view) {
			a.Bytes = b.du(ctx, sl.Path)
		}
	}
	if kind == KindRemoveSlot && view != nil {
		for _, d := range view.Databases {
			if d.Present {
				a.DBNames = append(a.DBNames, d.Name)
				a.DBBytes += mb(d.SizeMB)
			}
		}
	}
	b.add(a)
	return true
}

// humanChanges reports whether the changes in sl's tree are a human's
// (slots HumanEvidence) and why. An error to tell counts as a human's: the
// plan never discards what it cannot classify.
func (b *builder) humanChanges(ctx context.Context, sl store.Slot) (string, bool) {
	why, err := b.p.Slots.HumanEvidence(ctx, sl)
	if err != nil {
		return execx.Redact("could not tell whether a human made them: " + err.Error()), true
	}
	return why, why != ""
}

// status returns the git status of the slot directory and whether it is
// known (a missing directory is clean).
func (b *builder) status(ctx context.Context, sl store.Slot, view *inventory.SlotView) (gitx.Status, bool) {
	if view != nil && view.Dirty != nil {
		return gitx.Status{Tracked: view.Tracked, Untracked: view.Untracked}, true
	}
	if !slotExists(sl, view) {
		return gitx.Status{}, true
	}
	if b.p.Git == nil {
		return gitx.Status{}, false
	}
	st, err := b.p.Git.Status(ctx, sl.Path)
	if err != nil {
		b.warnings = append(b.warnings, execx.Redact(fmt.Sprintf("git status %s: %v", sl.Path, err)))
		return gitx.Status{}, false
	}
	return st, true
}

func slotExists(sl store.Slot, view *inventory.SlotView) bool {
	if view != nil {
		return view.Exists
	}
	_, err := os.Stat(sl.Path)
	return err == nil
}

// orphans plans drops of orphan database slugs on the automatic allowlist
// (the pool guard: prefix talkable_, slug ^review[0-9]+$); other orphan slugs
// are listed as not_managed.
func (b *builder) orphans() {
	if !orphansKnown(b.inv) {
		b.warnings = append(b.warnings, orphansUnknown(b.inv))
		if b.opts.Orphans {
			b.skip("orphans", SkipIncomplete, orphansUnknown(b.inv))
		}
		return
	}
	for _, slug := range orphanSlugs(b.inv) {
		if b.seenSlug[slug] {
			continue
		}
		dbs := orphansOf(b.inv, slug)
		if g, ok := autoGuard(b.p.Config, slug); ok && allPass(g, dbs) {
			b.add(dropAction(slug, dbs, "orphan: no slot or worktree uses the slug", false))
			continue
		}
		if b.opts.Orphans && b.opts.Slug == slug {
			continue // planned by namedSlug
		}
		b.skip("slug:"+slug, SkipNotManaged, "not on the automatic allowlist; drop with --orphans --slug "+slug)
	}
}

// namedSlug plans the drop of one orphan slug outside the allowlist; Apply
// executes it only with typed confirmation.
func (b *builder) namedSlug(slug string) {
	subject := "slug:" + slug
	if b.seenSlug[slug] {
		return
	}
	if !orphansKnown(b.inv) {
		b.skip(subject, SkipIncomplete, orphansUnknown(b.inv))
		return
	}
	dbs := orphansOf(b.inv, slug)
	if len(dbs) == 0 {
		if slices.ContainsFunc(b.inv.Databases, func(d mysqlx.Database) bool { return d.Slug == slug }) {
			b.skip(subject, SkipInUse, "its databases belong to a slot or worktree")
		} else {
			b.skip(subject, SkipNotFound, "no databases with this slug")
		}
		return
	}
	if _, ok := manualGuard(b.p.Config, slug, dbNames(dbs)); !ok {
		b.skip(subject, SkipNotManaged, "database names outside every pool's prefix")
		return
	}
	b.add(dropAction(slug, dbs, "orphan slug named explicitly", true))
}

// orphansKnown reports whether inv's OrphanDBs rest on complete facts.
func orphansKnown(inv inventory.Inventory) bool { return inv.DatabasesListed && inv.OrphansKnown }

// orphansUnknown explains why the inventory computed no orphans.
func orphansUnknown(inv inventory.Inventory) string {
	if !inv.DatabasesListed {
		return "MySQL databases were not listed: orphan databases unknown"
	}
	return "worktree discovery was incomplete (see warnings): orphan databases unknown"
}

func dropAction(slug string, dbs []mysqlx.Database, why string, confirm bool) Action {
	a := Action{Kind: KindDropDBs, Subject: "slug:" + slug, Why: why, Slug: slug, DBNames: dbNames(dbs), Confirm: confirm}
	for _, d := range dbs {
		a.DBBytes += mb(d.SizeMB)
	}
	return a
}

func orphanSlugs(inv inventory.Inventory) []string {
	var out []string
	for _, d := range inv.OrphanDBs {
		if !slices.Contains(out, d.Slug) {
			out = append(out, d.Slug)
		}
	}
	slices.Sort(out)
	return out
}

func orphansOf(inv inventory.Inventory, slug string) []mysqlx.Database {
	var out []mysqlx.Database
	for _, d := range inv.OrphanDBs {
		if d.Slug == slug {
			out = append(out, d)
		}
	}
	return out
}

func dbNames(dbs []mysqlx.Database) []string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, d.Name)
	}
	return out
}

func allPass(g mysqlx.Guard, dbs []mysqlx.Database) bool {
	for _, d := range dbs {
		if g.Check(d.Name) != nil {
			return false
		}
	}
	return len(dbs) > 0
}

// external plans the reset of a manual worktree (the user's worktree-reset):
// placeholder branch <name> at origin/<base>, the main clone's
// .mise.local.toml copied in, databases untouched.
func (b *builder) external(ctx context.Context, name string) error {
	if b.p.Git == nil {
		return errors.New("cleanup: Planner.Git is required for --external")
	}
	var matches []inventory.ExternalView
	for _, ev := range b.inv.External {
		if matchExternal(ev, name) {
			matches = append(matches, ev)
		}
	}
	if len(matches) != 1 {
		detail := "no manual worktree named " + name
		if len(matches) > 1 {
			detail = fmt.Sprintf("%d manual worktrees match %s; pass the path", len(matches), name)
		} else if sl, err := b.p.Store.SlotByName(ctx, name); err == nil && sl.Kind != store.SlotKindExternal {
			detail = name + " is a managed slot; use --slot without --external"
		}
		b.skip("slot:"+name, SkipNotFound, detail)
		return nil
	}
	ev := matches[0]
	subject := "path:" + ev.Path
	if !ev.Exists {
		b.skip(subject, SkipNotFound, "directory missing")
		return nil
	}
	if reason, detail := externalBlock(ctx, b.p.Git, b.inv.AgentsListed, ev, resetBranch(ev.Path), b.opts.Force); reason != "" {
		b.skip(subject, reason, detail)
		return nil
	}
	base, err := b.baseFor(ctx, ev.Repo)
	if err != nil {
		return err
	}
	why := "reset to origin/" + base
	if ev.PRNumber > 0 {
		why += strings.TrimRight(fmt.Sprintf(" (PR #%d %s", ev.PRNumber, ev.GHState), " ") + ")"
	}
	if b.opts.Force {
		// What the force resets past, counted best effort for the question.
		var lost []string
		for _, r := range externalRisks(ctx, b.p.Git, ev, resetBranch(ev.Path)) {
			lost = append(lost, r.detail)
		}
		if len(lost) > 0 {
			why += " (--force discards " + strings.Join(lost, "; ") + ")"
		}
	}
	b.add(Action{
		Kind: KindResetExternal, Subject: subject, Why: why, Repo: ev.Repo, Slot: resetBranch(ev.Path),
		Path: ev.Path, MainClone: ev.MainClone, Branch: resetBranch(ev.Path), Base: base, Force: b.opts.Force,
	})
	return nil
}

// externalBlock returns why a manual worktree must not be reset now: herdr
// agents that could not be listed or an agent working there (never
// overridden), or the first of externalRisks (force overrides those).
func externalBlock(ctx context.Context, git Git, agentsListed bool, ev inventory.ExternalView, branch string, force bool) (reason, detail string) {
	if !agentsListed {
		return SkipIncomplete, "herdr agents were not listed: cannot tell whether an agent works there"
	}
	if a, ok := busyAgent(ev.Agents); ok {
		return SkipAgentWorking, agentDetail(a)
	}
	if force {
		return "", ""
	}
	if risks := externalRisks(ctx, git, ev, branch); len(risks) > 0 {
		return risks[0].reason, risks[0].detail
	}
	return "", ""
}

// risk is one thing resetting a manual worktree would lose (or cannot tell
// it would not), with the skip reason it gets without --force.
type risk struct{ reason, detail string }

// externalRisks lists what resetting the manual worktree ev to branch would
// lose: tracked changes, and commits on no remote that only HEAD or only the
// placeholder branch reaches. A count that cannot be read is a risk too.
func externalRisks(ctx context.Context, git Git, ev inventory.ExternalView, branch string) []risk {
	var out []risk
	st, err := git.Status(ctx, ev.Path)
	switch {
	case err != nil:
		out = append(out, risk{SkipDirty, execx.Redact("could not read git status: " + err.Error())})
	case st.Tracked > 0:
		out = append(out, risk{SkipDirty, fmt.Sprintf("%d tracked changes", st.Tracked)})
	}
	// `git switch -C <branch>` moves HEAD off its commit and force-moves
	// <branch>: commits only HEAD (detached or on <branch>) or only <branch>
	// (when another branch is checked out) reaches would lose their ref.
	// Commits on another checked-out branch stay on it.
	if ev.Detached || ev.Branch == "" || ev.Branch == branch {
		n, err := git.Unpushed(ctx, ev.Path)
		on := "detached HEAD"
		if ev.Branch != "" {
			on = "branch " + ev.Branch
		}
		switch {
		case err != nil:
			out = append(out, risk{SkipUnpushed, execx.Redact("could not count unpushed commits: " + err.Error())})
		case n > 0:
			out = append(out, risk{SkipUnpushed, fmt.Sprintf("%d commits on %s are on no remote", n, on)})
		}
	}
	if ev.Branch != branch {
		n, err := unpushedOnBranch(ctx, git, ev.Path, branch)
		switch {
		case err != nil:
			out = append(out, risk{SkipUnpushed, execx.Redact(fmt.Sprintf("could not count unpushed commits on branch %s: %v", branch, err))})
		case n > 0:
			out = append(out, risk{SkipUnpushed, fmt.Sprintf("%d commits on branch %s (which the reset overwrites) are on no remote", n, branch)})
		}
	}
	return out
}

// unpushedOnBranch counts the commits of local branch in dir that are on no
// remote: 0 when the branch does not exist.
func unpushedOnBranch(ctx context.Context, git Git, dir, branch string) (int, error) {
	ref := "refs/heads/" + branch
	if _, err := git.RevParse(ctx, dir, ref); errors.Is(err, gitx.ErrNoSuchRef) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return git.UnpushedRef(ctx, dir, ref)
}

// matchExternal accepts repoN, the directory name (talkable.repoN) or the path.
func matchExternal(ev inventory.ExternalView, name string) bool {
	if filepath.IsAbs(paths.Expand(name)) {
		return fsx.Canon(paths.Expand(name)) == fsx.Canon(ev.Path)
	}
	return filepath.Base(ev.Path) == name || resetBranch(ev.Path) == name
}

// resetBranch is the placeholder branch of a manual worktree: the directory
// name after its last dot (talkable.repo3 → repo3), like zsh's ${dir:e}.
func resetBranch(path string) string {
	base := filepath.Base(path)
	if i := strings.LastIndexByte(base, '.'); i >= 0 && i < len(base)-1 {
		return base[i+1:]
	}
	return base
}

func (b *builder) baseFor(ctx context.Context, repo string) (string, error) {
	if pool := b.p.Config.PoolFor(repo); pool != nil && pool.Base != "" {
		return pool.Base, nil
	}
	r, err := b.p.Store.RepoByFullName(ctx, repo)
	if err == nil && r.DefaultBranch != "" {
		return r.DefaultBranch, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("cleanup: %w", err)
	}
	return "master", nil
}

// foreignAgent returns a herdr agent in a managed slot that is not
// magnum's, whatever its status: a human's idle session holds the slot too
// (the slots guard refuses it the same way).
func foreignAgent(view *inventory.SlotView) (inventory.AgentView, bool) {
	if view == nil {
		return inventory.AgentView{}, false
	}
	i := slices.IndexFunc(view.Agents, func(a inventory.AgentView) bool { return !a.Magnum })
	if i < 0 {
		return inventory.AgentView{}, false
	}
	return view.Agents[i], true
}

// busyAgent returns an agent, magnum's or not, that is working or blocked.
func busyAgent(agents []inventory.AgentView) (inventory.AgentView, bool) {
	i := slices.IndexFunc(agents, func(a inventory.AgentView) bool {
		return a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked
	})
	if i < 0 {
		return inventory.AgentView{}, false
	}
	return agents[i], true
}

func agentDetail(a inventory.AgentView) string {
	name := cmp.Or(a.Name, a.Agent, "agent")
	if a.PaneID != "" {
		name += " in pane " + a.PaneID
	}
	return name + " is " + cmp.Or(string(a.Status), "idle")
}

func (b *builder) prSubject(ctx context.Context, pr store.PR) (string, error) {
	repo, ok := b.repos[pr.RepoID]
	if !ok {
		var err error
		if repo, err = b.p.Store.RepoByID(ctx, pr.RepoID); err != nil {
			return "", fmt.Errorf("cleanup: %w", err)
		}
		b.repos[pr.RepoID] = repo
	}
	return repo.FullName() + "#" + strconv.Itoa(pr.Number), nil
}

// prActive explains why pr has a round in flight ("" when it has none).
func (b *builder) prActive(pr store.PR) string {
	if b.active[pr.ID] {
		return "a review run is in flight"
	}
	if slices.Contains(activePRStates, pr.State) {
		return "PR is " + pr.State
	}
	return ""
}

// prWhy is "merged 14m ago" / "closed 2h ago" for a closed PR.
func (b *builder) prWhy(pr store.PR) string {
	what, at := "closed", pr.ClosedAt
	if pr.GHState == store.GHMerged {
		what = "merged"
		if pr.MergedAt != nil {
			at = pr.MergedAt
		}
	}
	if !closing(pr.State) && pr.GHState == store.GHOpen {
		return "PR open"
	}
	if at != nil {
		what += " " + humanDuration(b.now.Sub(*at)) + " ago"
	}
	if pr.State == store.PRReleasing {
		what += ", resuming release"
	}
	return what
}

// closeReason is the assignment end reason of a closed PR.
func closeReason(pr store.PR) string {
	if pr.GHState == store.GHMerged {
		return "merged"
	}
	return "closed"
}

// closing reports whether a PR state belongs to the close → release path.
func closing(state string) bool {
	return state == store.PRClosed || state == store.PRReleasing
}

// du is the size of path in bytes from `du -sk` (0 when unknown).
func (b *builder) du(ctx context.Context, path string) int64 {
	if b.p.Runner == nil {
		return 0
	}
	kb, err := inventory.DiskUsageKB(ctx, b.p.Runner, path, "cleanup size")
	if err != nil {
		b.warnings = append(b.warnings, execx.Redact(fmt.Sprintf("du %s: %v", path, err)))
		return 0
	}
	return kb * 1024
}

func mb(sizeMB float64) int64 { return int64(sizeMB * 1024 * 1024) }

// poolGuard is slots.DropGuard (prefix talkable_, slug ^review[0-9]+$)
// with ok = false for a pool whose guard refuses everything.
func poolGuard(pool config.Pool) (mysqlx.Guard, bool) {
	g := slots.DropGuard(pool)
	return g, g.AllowRegexp != nil && g.Prefix != ""
}

func poolPrefix(pool config.Pool) string { return slots.DBPrefix(pool) }

// autoGuard is the automatic-drop guard of the pool whose slot names match
// slug.
func autoGuard(cfg *config.Config, slug string) (mysqlx.Guard, bool) {
	for _, pool := range cfg.Pools {
		if g, ok := poolGuard(pool); ok && g.AllowRegexp.MatchString(slug) {
			return g, true
		}
	}
	return mysqlx.Guard{}, false
}

// manualGuard allows exactly slug under the prefix of a pool that every name
// carries (for --orphans --slug X after typed confirmation).
func manualGuard(cfg *config.Config, slug string, names []string) (mysqlx.Guard, bool) {
	if !slugRe.MatchString(slug) {
		return mysqlx.Guard{}, false
	}
	g := mysqlx.Guard{AllowRegexp: regexp.MustCompile("^" + regexp.QuoteMeta(slug) + "$")}
	for _, pool := range cfg.Pools {
		g.Prefix = poolPrefix(pool)
		if g.Prefix == "" {
			continue
		}
		ok := len(names) > 0
		for _, n := range names {
			if g.Check(n) != nil {
				ok = false
				break
			}
		}
		if ok {
			return g, true
		}
	}
	return mysqlx.Guard{}, false
}
