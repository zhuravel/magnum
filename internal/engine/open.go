package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// requestOpen handles a `magnum open` request: nothing to do for a PR whose
// session is live or whose round is running (the CLI focuses it); otherwise
// the PR's checkout and sessions are restored in a goroutine of their own
// (like a round: dispatch leaves the PR alone meanwhile), which completes
// the request. OpenPayload.Role is any role of the PR's watch, by name or
// alias ("" = its judge). A whiteboard job holding the PR yields to the
// request (yieldWhiteboard), which goes on once the job let go of it.
func (e *Engine) requestOpen(ctx context.Context, id int64, p OpenPayload) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		e.complete(ctx, id, err, "")
		return
	}
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		e.complete(ctx, id, fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName()), "")
		return
	}
	spec, ok := e.cfg.RoleByNameOrAlias(w, p.Role)
	if !ok {
		e.complete(ctx, id, fmt.Errorf("unknown role %q (the roles of %s: %s)", p.Role, repo.FullName(), strings.Join(roleNames(e.cfg.RolesFor(w)), ", ")), "")
		return
	}
	role := spec.Name
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	subject := prSubject(repo, pr.Number)
	if s, err := e.st.LiveSessionByPRRole(ctx, pr.ID, role); err == nil && sessionUp(s, spec) {
		e.complete(ctx, id, nil, fmt.Sprintf("%s %s is live in pane %s; nothing to restore", label, role, deref(s.HerdrPaneID)))
		return
	}
	if e.roundActiveBesidesWhiteboard(pr.ID) || slices.Contains(store.InFlightStates, pr.State) {
		e.complete(ctx, id, nil, fmt.Sprintf("%s has a round running (%s); its sessions start with it", label, pr.State))
		return
	}
	switch {
	case e.d.DryRun:
		e.rec.Record(ctx, subject, "open", fmt.Sprintf("restore the %s session in a slot and pin it", role))
		e.complete(ctx, id, nil, "dry run: would restore "+label+" and pin it")
		return
	case e.d.Agents == nil || e.d.Slots == nil:
		e.complete(ctx, id, errors.New("the daemon has no agents or slots manager"), "")
		return
	}
	if e.yieldWhiteboard(pr.ID, id, "magnum open") { // whiteboard.go: the request goes on once the job let go of the PR
		return
	}

	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, open: true}) {
		cancel()
		e.complete(ctx, id, fmt.Errorf("%s: %s; run `magnum open %s` again shortly", label, e.heldReason(pr.ID), label), "")
		return
	}
	e.mu.Lock()
	e.inflight[id] = true
	e.mu.Unlock()
	e.roundWG.Go(func() {
		defer func() {
			e.unreserve(pr.ID)
			e.mu.Lock()
			delete(e.inflight, id)
			e.mu.Unlock()
		}()
		e.safely(rctx, "open", subject, func() {
			res, err := e.openPR(rctx, repo, *w, pr, spec)
			if rctx.Err() != nil && ctx.Err() != nil {
				return // daemon shutdown: the request stays pending for the next start
			}
			fctx := context.WithoutCancel(rctx)
			if err != nil {
				e.event(fctx, "warn", subject, "pr.open_failed", "magnum open: "+err.Error(), nil)
			}
			e.complete(fctx, id, err, res)
		}, func(ctx context.Context, msg string) {
			e.complete(ctx, id, fmt.Errorf("magnum open panicked: %s", msg), "")
		})
	})
}

// openPR gives pr a checkout and live sessions again for a human: its own
// claimed/held slot, where reviewed_sha (else the head) is checked out when
// the slot holds no checkout yet, else a slot claimed at that commit
// (claimFor: a free pool slot, none free → a slot is provisioned or evicted
// and the request fails with "try again", or a per-PR worktree); the
// workspace gets a pane per role that runs every round (runs = "always") and
// one for role when it runs on demand; the judge, role's agent and every
// other agent with a conversation to resume (ResumeID; never for
// session_source "none") are started or resumed; the PR and the slot are
// pinned and the slot is held. Unless open_hold is 0 or a lasting pin keeps
// the PR already, the pin is a hold that ends by itself (holdOpened,
// open_hold.go). A failure after a fresh Reserve parks what started and
// releases that slot again (a claimed slot is neither evictable nor
// reclaimed otherwise).
func (e *Engine) openPR(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, role config.Role) (res string, err error) {
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	subject := prSubject(repo, pr.Number)
	src := e.d.Identities[pr.Identity]
	if src == nil {
		return "", fmt.Errorf("identity %q is not configured", pr.Identity)
	}
	pool := e.cfg.PoolFor(full)

	// 1. A checkout of the PR.
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	schemaNote := ""
	switch {
	case has && (slot.State == store.SlotClaimed || slot.State == store.SlotHeld):
		// The PR's own slot keeps its databases as they are: a person may
		// have migrated them on purpose.
		if deref(slot.CheckedOutSHA) == "" {
			if slot, err = e.checkoutAt(ctx, slot, pr, pool, reviewedTarget(pr)); err != nil {
				return "", err
			}
		}
	case has && !(slot.Kind == store.SlotKindPerPR && slot.State == store.SlotProvisioning):
		return "", fmt.Errorf("%s: slot %s is %s; try again once it settles (`magnum slots list`)", label, slot.Name, slot.State)
	default:
		slot, schemaNote, err = e.claimFor(ctx, pr, repo, "magnum open")
		if pool != nil && slot.ID != 0 { // a free pool slot reserved just now
			defer func() {
				if err != nil && ctx.Err() == nil { // on shutdown the request is replayed and Reserve finds the slot
					e.releaseReserved(context.WithoutCancel(ctx), pr, slot, *pool, subject)
				}
			}()
		}
		switch {
		case errors.Is(err, slots.ErrNoFreeSlot) && pool != nil:
			return "", fmt.Errorf("no free %s slot right now; one is being provisioned or freed, run `magnum open %s` again shortly", pool.Repo, label)
		case err != nil:
			return "", err
		}
	}

	// 2. Sessions.
	var layout, start []config.Role
	for _, r := range e.cfg.RolesFor(&w) {
		if r.Judge || r.Runs == config.RunsAlways {
			layout = append(layout, r)
		}
		switch {
		case r.IsShell():
		case r.Judge || r.Name == role.Name:
			start = append(start, r)
		case r.Runs == config.RunsAlways:
			if id, _ := e.d.Agents.ResumeID(ctx, pr.ID, agents.Role(r.Name)); id != "" {
				start = append(start, r)
			}
		}
	}
	for _, kind := range agentKinds(start) {
		if err := e.d.Agents.Preflight(ctx, kind); err != nil && isLogin(err) {
			return "", err
		}
	}
	job := &roundJob{pr: pr, repo: repo, watch: w, pool: pool, slot: slot, hasSlot: true}
	env, err := e.paneEnv(ctx, job, src, deref(slot.CheckedOutSHA))
	if err != nil {
		return "", err
	}
	ws, err := e.d.Agents.EnsureWorkspace(ctx, pr, slot.Path, env, fmt.Sprintf("%s#%d", repo.Name, pr.Number), layout)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	if ws.Panes[agents.Role(role.Name)] == "" { // an on-demand role
		if ws, err = e.d.Agents.EnsurePane(ctx, pr, ws, slot.Path, env, role); err != nil {
			return "", fmt.Errorf("%s pane: %w", role.Name, err)
		}
	}
	for _, r := range judgeFirst(start) {
		if _, err := e.ensureAgent(ctx, pr, r, ws, false, effortFull); err != nil {
			return "", fmt.Errorf("start %s: %w", r.Name, err)
		}
	}

	// 3. Pinned to the human: no round, eviction or cleanup touches it. A
	// hold (open_hold.go) ends the pin by itself.
	hold, held, err := e.holdOpened(ctx, pr, pr.Pinned || slot.Pinned)
	if err != nil {
		return "", err
	}
	if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		return "", err
	}
	if !slot.Pinned {
		if err := e.d.Slots.Pin(ctx, slot); err != nil {
			return "", fmt.Errorf("pin %s: %w", slot.Name, err)
		}
	}
	_ = e.st.TransitionSlot(ctx, slot.ID, []string{store.SlotClaimed}, store.SlotHeld, nil)

	pane := ws.Panes[agents.Role(role.Name)]
	if s, err := e.st.LiveSessionByPRRole(ctx, pr.ID, role.Name); err == nil && deref(s.HerdrPaneID) != "" {
		pane = deref(s.HerdrPaneID)
	}
	at := deref(slot.CheckedOutSHA)
	data := map[string]any{"slot": slot.Name, "pane": pane, "role": role.Name}
	pinned, kept := "pinned", fmt.Sprintf("pinned; `magnum unpin %s` lets reviews use it again", label)
	if held {
		now := e.now()
		data["until"] = hold.Until
		pinned = "held until " + waitClock(hold.Until, now)
		kept = fmt.Sprintf("%s, longer while you type in its panes (%s after your last input); `magnum pin %s` keeps it, `magnum unpin %s` hands it back now",
			heldWords(hold.Until, now), humanDuration(e.cfg.Daemon.HumanCooldown.Duration), label, label)
	}
	e.event(ctx, "info", subject, evPROpened, fmt.Sprintf("magnum open: restored in %s at %s, %s", slot.Name, textx.ShortSHA(at), pinned), data)
	return fmt.Sprintf("restored %s in %s at %s (%s); %s pane %s%s",
		label, slot.Name, textx.ShortSHA(at), kept, role.Name, pane, schemaNote), nil
}

// claimAtReviewedHead gives pr, which holds no slot (the pool took it back),
// a checkout of its reviewed head for the fix job (fixClaim), as `magnum
// open` claims one: claimFor, whose events name `magnum fix`. It never pins
// and never holds.
func (e *Engine) claimAtReviewedHead(ctx context.Context, pr store.PR, repo store.Repo) (store.Slot, error) {
	slot, _, err := e.claimFor(ctx, pr, repo, fixBy)
	return slot, err
}

// claimFor claims a slot for pr, which holds none, at its reviewed head
// (reviewedTarget) for by ("magnum open", "magnum fix"), the command its
// events name: a free pool slot (Reserve and a slot.claimed event; none free
// → a slot is provisioned or evicted, needSlot, and the error is
// slots.ErrNoFreeSlot) or, for a repository without a pool, a per-PR
// worktree (CreatePRWorktree). The commit is checked out (a per-PR worktree
// made at it holds it already), and a pool slot's databases are made to fit
// the checkout (EnsureSchema), since a free slot carries the schema its last
// PR loaded (slots.LazySchema). schemaNote is what that did, for the
// caller's answer ("; loaded the schema of …", or why it could not; ""
// otherwise): a load that fails is no failure, the next round reloads it. It
// never pins and never holds. A failure after the Reserve returns the slot
// reserved with the error, for the caller to hand back: a claimed slot is
// neither evictable nor reclaimed otherwise.
func (e *Engine) claimFor(ctx context.Context, pr store.PR, repo store.Repo, by string) (slot store.Slot, schemaNote string, err error) {
	full := repo.FullName()
	target := reviewedTarget(pr)
	pool := e.cfg.PoolFor(full)
	if pool == nil {
		w := e.cfg.WatchFor(full)
		if w == nil {
			return store.Slot{}, "", fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
		}
		if slot, err = e.d.Slots.CreatePRWorktree(ctx, *w, full, pr, target); err != nil {
			return store.Slot{}, "", fmt.Errorf("per-PR worktree: %w", err)
		}
		if deref(slot.CheckedOutSHA) != "" {
			return slot, "", nil
		}
		slot, err = e.checkoutAt(ctx, slot, pr, nil, target)
		return slot, "", err
	}
	subject := prSubject(repo, pr.Number)
	if slot, err = e.d.Slots.Reserve(ctx, pr, *pool); err != nil {
		if errors.Is(err, slots.ErrNoFreeSlot) {
			e.needSlot(ctx, *pool, subject)
		}
		return store.Slot{}, "", err
	}
	e.event(ctx, "info", subject, "slot.claimed", "reserved "+slot.Name+" for "+by, nil)
	reserved := slot
	if slot, err = e.checkoutAt(ctx, reserved, pr, pool, target); err != nil {
		return reserved, "", err
	}
	note, lerr := e.d.Slots.EnsureSchema(ctx, slot, *pool)
	switch {
	case lerr != nil && ctx.Err() != nil:
		return slot, "", lerr
	case lerr != nil:
		schemaNote = "; its databases' schema could not be loaded (the next round reloads it): " + lerr.Error()
		e.event(ctx, "warn", "slot:"+slot.Name, "slot.schema_reset_failed", by+": "+lerr.Error(), nil)
	case strings.HasPrefix(note, "loaded"):
		schemaNote = "; " + note
	}
	return slot, schemaNote, nil
}

// reviewedTarget is the commit a checkout of pr for a person or its fixer
// holds: the reviewed one, else the head.
func reviewedTarget(pr store.PR) string {
	if sha := deref(pr.ReviewedSHA); sha != "" {
		return sha
	}
	return pr.HeadSHA
}

// checkoutAt checks target out in slot, pr's (pool nil for a per-PR
// worktree), and returns the slot's row as it is then.
func (e *Engine) checkoutAt(ctx context.Context, slot store.Slot, pr store.PR, pool *config.Pool, target string) (store.Slot, error) {
	var pl config.Pool
	if pool != nil {
		pl = *pool
	}
	if err := e.d.Slots.Checkout(ctx, slot, pr, pl, target); err != nil {
		return store.Slot{}, fmt.Errorf("check out %s in %s: %w", textx.ShortSHA(target), slot.Name, err)
	}
	return e.st.SlotByID(ctx, slot.ID)
}

// releaseReserved hands back the slot a failed `magnum open` reserved: the
// PR's sessions are parked (and the panes the park retired closed) and the
// slot released (free).
func (e *Engine) releaseReserved(ctx context.Context, pr store.PR, slot store.Slot, pool config.Pool, subject string) {
	if err := e.d.Agents.Park(ctx, pr); err != nil {
		e.log.Warn("magnum open failed: park", "subject", subject, "err", err)
	} else {
		e.closeRetiredPanes(ctx, pr) // retired_panes.go
	}
	if err := e.releaseSlot(ctx, slot, pool, "open failed"); err != nil {
		e.event(ctx, "warn", "slot:"+slot.Name, "slot.release_failed", "release after a failed magnum open: "+err.Error(), nil)
		return
	}
	e.event(ctx, "info", "slot:"+slot.Name, "slot.released", slot.Name+" released after a failed magnum open", nil)
}

// sessionUp reports whether a live session row of role really runs: an
// agent role needs its agent (a "starting" row or a pane without an agent
// is not up), a shell role its pane.
func sessionUp(s store.Session, role config.Role) bool {
	if role.IsShell() {
		return deref(s.HerdrPaneID) != ""
	}
	return s.State == store.SessionLive && deref(s.AgentName) != ""
}

func roleNames(roles []config.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.Name
	}
	return out
}
