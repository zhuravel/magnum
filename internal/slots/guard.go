package slots

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Guard refuses to let magnum touch a slot a human may be using. It returns
// an ErrHold when:
//   - the slot is pinned (HoldPinned) or has a hold_reason (that reason);
//   - herdr shows an agent magnum did not start (name not starting with
//     AgentPrefix), idle or not, in a pane whose cwd or foreground cwd is the
//     slot or below it or in the herdr workspace of the slot's PR
//     (HoldForeignAgent), or a pane in the slot without an agent whose
//     foreground process is not its shell (HoldForegroundProcess). Panes of
//     magnum's own live sessions are ignored, and so is the process of any
//     pane in their workspaces;
//   - the slot's HEAD is not the recorded checked_out_sha (HoldHeadDrift) or,
//     with no recorded sha, HEAD (or, for a pool slot, its placeholder
//     branch, which Release resets wherever HEAD is) carries commits on no
//     remote or magnum ref (HoldUnpushed). A HEAD equal to the commit an
//     interrupted Checkout was switching to is magnum's own and is recorded
//     instead;
//   - the working tree has changes to tracked files and a human was there
//     (HoldDirtyWorktree): a foreign agent or process as above, or the PR's
//     human_active_at after the slot's last checkout (last_used_at), which the
//     agents package sets when someone types into the PR's magnum panes.
//
// Without such evidence changes are magnum's own residue (tests rewriting
// db/schema.rb, bundle install touching Gemfile.lock, copy_files the repo
// does not ignore): Guard passes, and the step that discards them records a
// slot.discarded event naming them (noteDiscard).
//
// Drift, unpushed commits and held changes are persisted as hold_reason, so
// the slot stays held until Unpin; live holds (agent, process) on a clean
// tree are transient. A herdr snapshot failure is returned as a plain error
// (not a hold): the caller retries later. Git checks are skipped when the
// slot directory is missing.
func (m *Manager) Guard(ctx context.Context, slot store.Slot) error {
	return m.guard(ctx, slot, false)
}

// guard is Guard; with untracked, untracked files count as changes too (a
// removal deletes the whole directory). Ignored files never count.
func (m *Manager) guard(ctx context.Context, slot store.Slot, untracked bool) error {
	sl := slot
	if slot.ID != 0 {
		var err error
		if sl, err = m.reload(ctx, slot); err != nil {
			return err
		}
	}
	if sl.Pinned {
		return ErrHold{Reason: HoldPinned, Detail: sl.Name + " is pinned"}
	}
	if sl.HoldReason != nil && *sl.HoldReason != "" {
		return ErrHold{Reason: *sl.HoldReason, Detail: sl.Name + " is held"}
	}
	pr, err := m.guardPR(ctx, sl)
	if err != nil {
		return err
	}
	if err := m.guardLive(ctx, sl, pr.ID); err != nil {
		live, ok := AsHold(err)
		if !ok {
			return err
		}
		// A human is in the slot now: changes in the tree are theirs too, and
		// stay held after they leave.
		if err := m.holdChanges(ctx, sl, untracked, live.Detail); err != nil {
			if _, held := AsHold(err); held {
				return err
			}
			return errors.Join(live, err)
		}
		return live
	}
	if err := m.guardHead(ctx, sl); err != nil {
		return err
	}
	if why := humanActivity(sl, pr); why != "" {
		return m.holdChanges(ctx, sl, untracked, why)
	}
	// Changes, if any, are magnum's residue.
	return nil
}

// guardPR is the PR the slot holds (slotPR) or, for a per-PR worktree not
// claimed yet (provisioning, broken), the PR its name is for; the zero PR
// when none.
func (m *Manager) guardPR(ctx context.Context, sl store.Slot) (store.PR, error) {
	if sl.ID == 0 && sl.PRID == nil {
		return store.PR{}, nil
	}
	pr, ok, err := m.slotPR(ctx, sl)
	if err == nil && !ok && sl.Kind == store.SlotKindPerPR && sl.RepoID != nil {
		if n := perPRNumber(sl.Name); n > 0 {
			if pr, err = m.d.Store.PRByRepoNumber(ctx, *sl.RepoID, n); errors.Is(err, store.ErrNotFound) {
				pr, err = store.PR{}, nil
			}
		}
	}
	if err != nil {
		return store.PR{}, fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	return pr, nil
}

// guardLive checks herdr for human activity in the slot; prID is the
// slot's PR (0 when none), whose magnum workspace counts as the slot's.
func (m *Manager) guardLive(ctx context.Context, sl store.Slot, prID int64) error {
	if m.d.Snapshot == nil {
		return nil
	}
	snap, err := m.d.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("slots: guard %s: herdr snapshot: %w", sl.Name, err)
	}
	ownedPanes, ownedWorkspaces, prWorkspaces := map[string]bool{}, map[string]bool{}, map[string]bool{}
	live, err := m.d.Store.LiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	for _, s := range live {
		if s.HerdrPaneID != nil {
			ownedPanes[*s.HerdrPaneID] = true
		}
		if ws := store.Deref(s.HerdrWorkspaceID); ws != "" {
			ownedWorkspaces[ws] = true
			prWorkspaces[ws] = prWorkspaces[ws] || (prID != 0 && s.PRID == prID)
		}
	}
	root := canonPath(sl.Path)
	inSlot := func(cwd, fgCwd string) bool { return under(cwd, root) || under(fgCwd, root) }
	// where places a foreign agent: in the slot, else in the PR's workspace
	// ("" when neither).
	where := func(cwd, fgCwd, ws string) string {
		switch {
		case inSlot(cwd, fgCwd):
			return "in " + sl.Path
		case prWorkspaces[ws]:
			return "in workspace " + ws + " of " + sl.Path
		}
		return ""
	}

	agentPanes := map[string]bool{}
	for _, a := range snap.Agents {
		agentPanes[a.PaneID] = true
		at := where(a.Cwd, a.ForegroundCwd, a.WorkspaceID)
		if at == "" || ownedPanes[a.PaneID] || strings.HasPrefix(a.Name, AgentPrefix) {
			continue
		}
		// A human's agent holds the slot whatever its status: an idle
		// session is still the human's, and a switch would move its tree.
		return ErrHold{Reason: HoldForeignAgent, Detail: fmt.Sprintf("pane %s: %s agent %q is %s %s",
			a.PaneID, a.Agent, a.Name, orIdle(a.AgentStatus), at)}
	}
	for _, p := range snap.Panes {
		if agentPanes[p.ID] || ownedPanes[p.ID] {
			continue
		}
		if p.Agent != "" {
			// An agent pane missing from the agents list (not magnum's: its
			// panes are ownedPanes).
			if at := where(p.Cwd, p.ForegroundCwd, p.WorkspaceID); at != "" {
				return ErrHold{Reason: HoldForeignAgent, Detail: fmt.Sprintf("pane %s: %s agent is %s %s",
					p.ID, p.Agent, orIdle(p.AgentStatus), at)}
			}
			continue
		}
		if !inSlot(p.Cwd, p.ForegroundCwd) || ownedWorkspaces[p.WorkspaceID] || m.d.ProcessInfo == nil {
			continue
		}
		pi, err := m.d.ProcessInfo(ctx, p.ID)
		if err != nil {
			if herdr.IsCode(err, herdr.CodePaneNotFound) {
				continue
			}
			return fmt.Errorf("slots: guard %s: process info of pane %s: %w", sl.Name, p.ID, err)
		}
		if !herdr.IdleShell(pi) {
			return ErrHold{Reason: HoldForegroundProcess, Detail: fmt.Sprintf("pane %s runs %s in %s",
				p.ID, processNames(pi), sl.Path)}
		}
	}
	return nil
}

// orIdle names an agent status, "idle" when herdr reports none.
func orIdle(st herdr.Status) string {
	if st == "" {
		return "idle"
	}
	return string(st)
}

func processNames(pi herdr.ProcessInfo) string {
	var names []string
	for _, p := range pi.Foreground {
		if p.PID == pi.ShellPID {
			continue
		}
		name := p.Name
		if p.Cmdline != "" {
			name = p.Cmdline
		} else if len(p.Argv) > 0 {
			name = strings.Join(p.Argv, " ")
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Sprintf("foreground group %d", pi.ForegroundPGID)
	}
	return strings.Join(names, ", ")
}

// guardHead detects commits a human made in the slot.
func (m *Manager) guardHead(ctx context.Context, sl store.Slot) error {
	if sl.Path == "" || !exists(sl.Path) {
		return nil
	}
	if want := store.Deref(sl.CheckedOutSHA); want != "" {
		head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
		if err != nil {
			return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
		}
		if head == want {
			return nil
		}
		if ok, err := m.resumeSwitch(ctx, sl, head); err != nil || ok {
			return err
		}
		return m.persistHold(ctx, sl, HoldHeadDrift, fmt.Sprintf("HEAD is %s, magnum checked out %s",
			short(head), short(want)))
	}
	n, err := m.git.Unpushed(ctx, sl.Path)
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	if n > 0 {
		return m.persistHold(ctx, sl, HoldUnpushed, fmt.Sprintf("HEAD has %d commit(s) on no remote", n))
	}
	return m.guardPlaceholder(ctx, sl)
}

// guardPlaceholder holds a pool slot whose placeholder branch carries commits
// on no remote while HEAD is elsewhere: Release resets that branch
// (`git switch -C`) wherever HEAD is, which would orphan them. A branch at
// HEAD was counted with HEAD, and a missing one has nothing to lose.
func (m *Manager) guardPlaceholder(ctx context.Context, sl store.Slot) error {
	if sl.Kind != store.SlotKindPool {
		return nil
	}
	branch := placeholder(sl)
	ref := "refs/heads/" + branch
	tip, err := m.git.RevParse(ctx, sl.Path, ref)
	if errors.Is(err, gitx.ErrNoSuchRef) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	if head == tip {
		return nil
	}
	n, err := m.git.UnpushedRef(ctx, sl.Path, ref)
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	if n > 0 {
		return m.persistHold(ctx, sl, HoldUnpushed, fmt.Sprintf("branch %s has %d commit(s) on no remote", branch, n))
	}
	return nil
}

// switchingKey is the kv key holding the commit a Checkout is switching slot
// id to: written before the switch, deleted once checked_out_sha records it.
func switchingKey(id int64) string { return fmt.Sprintf("slot.%d.switching", id) }

// resumeSwitch reports whether head is the commit an interrupted Checkout of
// sl was switching to (magnum's own switch, not a human's commit) and, if
// so, records it as checked_out_sha.
func (m *Manager) resumeSwitch(ctx context.Context, sl store.Slot, head string) (bool, error) {
	if sl.ID == 0 {
		return false, nil
	}
	pending, ok, err := m.d.Store.GetKV(ctx, switchingKey(sl.ID))
	if err != nil {
		return false, fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	if !ok || pending != head {
		return false, nil
	}
	if m.d.DryRun {
		return true, nil
	}
	if err := m.recordSwitch(ctx, sl, head); err != nil {
		return false, fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	m.event(ctx, "slot:"+sl.Name, "info", "slot.switch_recorded",
		fmt.Sprintf("HEAD %s is the commit an interrupted checkout switched to; recorded as checked out", short(head)))
	return true, nil
}

// recordSwitch records head as the slot's checked_out_sha and forgets the
// pending switch.
func (m *Manager) recordSwitch(ctx context.Context, sl store.Slot, head string) error {
	if err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", head) }); err != nil {
		return err
	}
	return m.d.Store.DeleteKV(context.WithoutCancel(ctx), switchingKey(sl.ID))
}

// HumanEvidence explains why changes in slot sl's tree would be a human's
// rather than magnum's residue: its PR's human_active_at is after the slot's
// last checkout. "" when nothing says so. Live evidence (a human's agent or
// process in the slot) is Guard's; read-only, for planners that want to show
// what a release or removal would hold on.
func (m *Manager) HumanEvidence(ctx context.Context, sl store.Slot) (string, error) {
	pr, err := m.guardPR(ctx, sl)
	if err != nil {
		return "", err
	}
	return humanActivity(sl, pr), nil
}

// humanActivity explains the PR's human_active_at when it is after the
// slot's last checkout (last_used_at; any time when the slot has none):
// someone typed into the PR's magnum panes since, so changes in the tree may
// be theirs. "" when it is not.
func humanActivity(sl store.Slot, pr store.PR) string {
	at := pr.HumanActiveAt
	if at == nil || (sl.LastUsedAt != nil && !at.After(*sl.LastUsedAt)) {
		return ""
	}
	why := fmt.Sprintf("someone typed into PR #%d's magnum panes at %s", pr.Number, store.FormatTime(*at))
	if sl.LastUsedAt != nil {
		why += ", after magnum's checkout at " + store.FormatTime(*sl.LastUsedAt)
	}
	return why
}

// holdChanges persists HoldDirtyWorktree when the slot's tree has changes
// (untracked files only with untracked); why names the human evidence.
func (m *Manager) holdChanges(ctx context.Context, sl store.Slot, untracked bool, why string) error {
	if sl.Path == "" || !exists(sl.Path) {
		return nil
	}
	ch, err := m.worktreeChanges(ctx, sl.Path)
	if err != nil {
		return fmt.Errorf("slots: guard %s: %w", sl.Name, err)
	}
	if len(ch.paths(untracked)) == 0 {
		return nil
	}
	return m.persistHold(ctx, sl, HoldDirtyWorktree, fmt.Sprintf("%s has %s; %s", sl.Path, ch.describe(untracked), why))
}

// maxListed caps the paths a slot.discarded event names.
const maxListed = 20

// noteDiscard runs right before a step that discards the slot's changes (a
// switch or reset with --discard-changes; with untracked, a worktree removal
// with --force) and reports whether there are any. It records a
// slot.discarded event with their counts and the first maxListed paths, so
// what the guard took for magnum's residue can be audited. A switch or reset
// keeps untracked files: they count only with untracked.
func (m *Manager) noteDiscard(ctx context.Context, sl store.Slot, untracked bool) (bool, error) {
	if sl.Path == "" || !exists(sl.Path) {
		return false, nil
	}
	ch, err := m.worktreeChanges(ctx, sl.Path)
	if err != nil {
		return false, err
	}
	paths := ch.paths(untracked)
	if len(paths) == 0 {
		return false, nil
	}
	listed := strings.Join(paths[:min(len(paths), maxListed)], ", ")
	if more := len(paths) - maxListed; more > 0 {
		listed += fmt.Sprintf(" (+%d more)", more)
	}
	m.event(ctx, "slot:"+sl.Name, "info", "slot.discarded",
		fmt.Sprintf("discarding %s in %s: %s", ch.describe(untracked), sl.Path, listed))
	return true, nil
}

// changes are a working tree's changed paths, relative to its root.
type changes struct {
	tracked, untracked []string
}

func (c changes) paths(untracked bool) []string {
	if !untracked {
		return c.tracked
	}
	return slices.Concat(c.tracked, c.untracked)
}

func (c changes) describe(untracked bool) string {
	var what []string
	if n := len(c.tracked); n > 0 {
		what = append(what, fmt.Sprintf("%d tracked change(s)", n))
	}
	if n := len(c.untracked); untracked && n > 0 {
		what = append(what, fmt.Sprintf("%d untracked file(s)", n))
	}
	return strings.Join(what, " and ")
}

// worktreeChanges lists dir's changes with gitx.StatusPaths (`git status
// --porcelain=v1 -z --untracked-files=normal`, read-only, ignored files
// excluded), the entries gitx.Status counts.
func (m *Manager) worktreeChanges(ctx context.Context, dir string) (changes, error) {
	entries, err := m.git.StatusPaths(ctx, dir)
	if err != nil {
		return changes{}, fmt.Errorf("slots: git status %s: %w", dir, err)
	}
	return changesOf(entries), nil
}

// changesOf sorts status entries into tracked changes and untracked files. A
// rename or copy counts once, by its destination path.
func changesOf(entries []gitx.StatusEntry) changes {
	var ch changes
	for _, e := range entries {
		switch {
		case e.Ignored():
		case e.Untracked():
			ch.untracked = append(ch.untracked, e.Path)
		default:
			ch.tracked = append(ch.tracked, e.Path)
		}
	}
	return ch
}

// persistHold records reason as the slot's hold_reason (and last_error) and
// returns the ErrHold.
func (m *Manager) persistHold(ctx context.Context, sl store.Slot, reason, detail string) error {
	hold := ErrHold{Reason: reason, Detail: detail}
	if m.d.DryRun {
		m.dryRun("hold %s: %s", sl.Name, hold.Error())
		return hold
	}
	if sl.ID != 0 {
		err := m.d.Store.UpdateSlotFields(context.WithoutCancel(ctx), sl.ID, func(u *store.SlotUpdate) {
			u.Set("hold_reason", reason)
			u.Set("last_error", hold.Error())
		})
		if err != nil {
			return errors.Join(hold, fmt.Errorf("slots: persist hold on %s: %w", sl.Name, err))
		}
	}
	m.event(ctx, "slot:"+sl.Name, "warn", "slot.hold", hold.Error())
	return hold
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Pin marks the slot pinned: no automatic claim, checkout, release or
// removal until Unpin.
func (m *Manager) Pin(ctx context.Context, slot store.Slot) error {
	if m.d.DryRun {
		m.dryRun("pin %s", slot.Name)
		return nil
	}
	if err := m.d.Store.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("pinned", true) }); err != nil {
		return fmt.Errorf("slots: pin %s: %w", slot.Name, err)
	}
	return nil
}

// Unpin hands the slot back to automation: pinned and hold_reason are
// cleared. A head_drift or unpushed_commits hold is acknowledged by taking
// the current HEAD as the new checked_out_sha, so the next guard passes and
// the next release resets the slot. A dirty_worktree hold comes back at the
// next guard while the changes are there and a human still shows (their
// agent or process in the slot, or human_active_at after the checkout):
// commit, stash or discard them first (or remove the slot with force).
func (m *Manager) Unpin(ctx context.Context, slot store.Slot) error {
	if m.d.DryRun {
		m.dryRun("unpin %s", slot.Name)
		return nil
	}
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return err
	}
	var rebase string
	if r := sl.HoldReason; r != nil && (*r == HoldHeadDrift || *r == HoldUnpushed) && exists(sl.Path) {
		if rebase, err = m.git.RevParse(ctx, sl.Path, "HEAD"); err != nil {
			return fmt.Errorf("slots: unpin %s: %w", sl.Name, err)
		}
	}
	err = m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) {
		u.Set("pinned", false)
		u.Set("hold_reason", nil)
		if rebase != "" {
			u.Set("checked_out_sha", rebase)
		}
	})
	if err != nil {
		return fmt.Errorf("slots: unpin %s: %w", sl.Name, err)
	}
	return nil
}
