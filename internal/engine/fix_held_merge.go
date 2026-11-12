package engine

// A merge of the parent that magnum holds for your y (2026-11-12). The
// fixer's resolved merge that `magnum push` handed to the daemon (exit 4:
// it carries the base's workflow files) is refused by pushMerge when it
// keeps the PR's own workflow changes: an agent wrote workflow content
// that nobody read. When that is its only fault and it merges the tip
// GitHub names for the base, and the PR is the operator's own (ownPR, read
// again from the registry), the settle holds it instead of dropping it
// (holdMerge): a ref under refs/magnum/held/ keeps the commit from git gc
// in the clone the slot's objects live in, the registry keeps a HeldMerge
// (KVPRHeldMerge, a key-value row: no migration), held-merge.diff in the
// PR's report directory shows how the fixer resolved the workflow files'
// conflicts and what the push changes in them, and the PR needs you with
// "merge ready: …" (attention.MergeReadyText). A fix run's fixer merges
// the base before the findings; its merge is held alike, and the findings
// wait for the next fix after the push.
//
// `P` and `magnum merge-parent` then plan its push (requestHeld) and, on
// the y bound to its SHA, its head and its tip, push it (pushHeld) with the
// fixer App's own Workflows token and a lease on the head. The one check of
// pushMerge it skips is that its workflow files are the tip's: the
// operator read them. --yes binds nothing and never pushes it; an n leaves
// it held. A held merge goes (dropHeldMerge) once pushed, and when it went
// stale (heldStale): its PR merged or closed, its head or its base moved,
// or its base's tip moved on GitHub. The poll looks (dropStaleHeldMerges,
// which reads such a PR's Details on every poll, so the tip is seen), and
// so does the push.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRHeldMerge holds the merge of the PR's parent that magnum holds for
// your y (HeldMerge as JSON).
func KVPRHeldMerge(prID int64) string { return fmt.Sprintf("pr.%d.held_merge", prID) }

// HeldMergeRef is the ref that keeps the held merge of PR number of
// owner/repo from git gc, in the clone its objects live in: under
// refs/magnum/held/, never under refs/heads or refs/remotes.
func HeldMergeRef(owner, repo string, number int) string {
	return fmt.Sprintf("refs/magnum/held/%s/%s/%d", owner, repo, number)
}

// HeldMergeFile is the file in the PR's report directory
// (paths.Layout.PRReviews) that shows what a held merge does to the
// workflow files: how the fixer resolved their conflicts, then what its
// push changes in them. The screens' e opens it in the pager.
const HeldMergeFile = "held-merge.diff"

// HeldMerge is a merge of a PR's parent that magnum holds for your y: the
// PR (Repo, owner/name, and Number), the merge commit (Merge), the PR's
// head on GitHub it merges into (Head, its first parent), the base branch
// and the commit of it it merges (Base, Tip: its second parent, the base's
// tip on GitHub when it was held), the workflow files its push changes
// against the head, how many files the fixer resolved conflicts in
// (Conflicts; 0 when not known), the fix role whose fixer resolved it, the
// identity that pushes it and its login, the clone that holds its ref, its
// held-merge.diff (absolute), whether a fix run's findings wait for the
// next fix after the push (Findings) and when it was held.
type HeldMerge struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Merge     string    `json:"merge"`
	Head      string    `json:"head"`
	Base      string    `json:"base"`
	Tip       string    `json:"tip"`
	Workflows []string  `json:"workflows"`
	Conflicts int       `json:"conflicts,omitempty"`
	Role      string    `json:"role"`
	Identity  string    `json:"identity"`
	Login     string    `json:"login"`
	Clone     string    `json:"clone"`
	Diff      string    `json:"diff"`
	Findings  bool      `json:"findings,omitempty"`
	At        time.Time `json:"at"`
}

// ParseHeldMerge reads a KVPRHeldMerge row; false for one that names no
// merge, head or tip.
func ParseHeldMerge(raw string) (HeldMerge, bool) {
	var h HeldMerge
	if json.Unmarshal([]byte(raw), &h) != nil || h.Merge == "" || h.Head == "" || h.Tip == "" || h.Base == "" {
		return HeldMerge{}, false
	}
	return h, true
}

// heldGit is the git a held merge takes beyond parentGit (*gitx.Client has
// it): the ref that keeps it, that ref's delete, its parents, how it
// resolved its conflicts in the workflow files and what it changes there.
// A git without it holds no merge: the refusal goes to you as before.
type heldGit interface {
	UpdateRef(ctx context.Context, dir, ref, sha string) error
	UpdateRefDelete(ctx context.Context, mainClone, ref string) error
	Parents(ctx context.Context, dir, rev string) ([]string, error)
	RemergeDiff(ctx context.Context, dir, merge string, paths ...string) (string, error)
	PathsDiff(ctx context.Context, dir, a, b string, paths ...string) (string, error)
}

// fixMergeHeld is pushedHead's end of a fixer's merge that magnum push
// handed to magnum and that the daemon holds for your y (holdMerge): msg
// is the PR's attention.
type fixMergeHeld struct{ msg string }

func (e fixMergeHeld) Error() string { return e.msg }

// heldPushTimeout bounds the push of a held merge, which runs while the
// daemon answers the y's request.
const heldPushTimeout = 2 * time.Minute

// holdMerge holds merge, the fixer's merge of b's tip into head that
// pushMerge refused only because it keeps the PR's own workflow changes
// (r), for your y: held-merge.diff, the ref and the record (see the file's
// comment), and a fix.merge_held event. It returns the PR's attention, ""
// when it holds nothing: the PR is not the operator's own as the registry
// has it now, or the git holds no merge. id is the fix identity.
func (e *Engine) holdMerge(ctx context.Context, job *fixJob, id *config.Identity, head, merge string, b fixConflict, r pushRefusal) (string, error) {
	hg, ok := e.d.Git.(heldGit)
	if !ok || len(r.own) == 0 {
		return "", nil
	}
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		return "", err
	}
	if !e.ownPR(pr) {
		return "", nil
	}
	clone := cmp.Or(job.slot.MainClone, job.slot.Path)
	diff, err := heldDiff(ctx, hg, clone, head, merge, b.base)
	if err != nil {
		return "", fmt.Errorf("what the merge %s does to the workflow files: %w", textx.ShortSHA(merge), err)
	}
	dir := e.d.Layout.PRReviews(job.repo.Owner, job.repo.Name, job.pr.Number)
	path := filepath.Join(dir, HeldMergeFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := fsx.WriteFileAtomic(path, diff, 0o600); err != nil {
		return "", err
	}
	if err := hg.UpdateRef(ctx, clone, HeldMergeRef(job.repo.Owner, job.repo.Name, job.pr.Number), merge); err != nil {
		return "", fmt.Errorf("the ref of the held merge %s: %w", textx.ShortSHA(merge), err)
	}
	conflicts := 0
	if strings.EqualFold(job.conflict.sha, b.sha) {
		conflicts = len(job.conflict.paths)
	}
	h := HeldMerge{Repo: job.repo.FullName(), Number: job.pr.Number, Merge: merge, Head: head, Base: b.base, Tip: b.sha,
		Workflows: slices.Clone(r.files), Conflicts: conflicts, Role: job.role.Name, Identity: id.Name, Login: id.Login, Clone: clone,
		Diff: path, Findings: job.mode != FixModeMerge, At: e.now().UTC()}
	if err := writeKVJSON(ctx, e, KVPRHeldMerge(pr.ID), "the held merge", h); err != nil {
		return "", err
	}
	e.event(ctx, "info", job.subject(), "fix.merge_held", fmt.Sprintf("magnum holds %s's merge of %s at %s into %s for your y: it keeps the PR's own "+
		"workflow changes (%s); %s", h.Role, h.Base, textx.ShortSHA(h.Tip), job.label(), strings.Join(r.own[:min(3, len(r.own))], ", "), path),
		map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge, "workflows": len(h.Workflows), "own": len(r.own),
			"conflicts": h.Conflicts, "cycle": job.state.Cycle})
	return h.attention(), nil
}

// heldNoConflict is held-merge.diff's line, in place of the remerge diff,
// for a merge that resolved no conflict in a workflow file.
const heldNoConflict = "No conflict in a workflow file was resolved: the workflow changes come unchanged from %s and the PR.\n"

// heldDiff is held-merge.diff of merge, a merge of base into head, in dir:
// how it resolved the conflicts in the workflow files (`git show
// --remerge-diff`), or heldNoConflict's one line when it resolved none,
// then everything its push changes in them against head (`git diff`).
func heldDiff(ctx context.Context, hg heldGit, dir, head, merge, base string) ([]byte, error) {
	remerge, err := hg.RemergeDiff(ctx, dir, merge, workflowsPath)
	if err != nil {
		return nil, err
	}
	plain, err := hg.PathsDiff(ctx, dir, head, merge, workflowsPath)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(remerge) == "":
		remerge = fmt.Sprintf(heldNoConflict, base)
	case !strings.HasSuffix(remerge, "\n"):
		remerge += "\n"
	}
	return []byte(remerge + plain), nil
}

// attention is the PR's attention while h is held: "fix: merge ready:
// claude-fix merged master at c0c0c0c and resolved 5 conflicts. It changes
// .github/workflows/ci.yml and pronto.yml. fixer-app[bot] pushes it only
// after your y: P asks (e shows held-merge.diff).", and for a fix run's
// merge " The findings wait for the next fix after the push."
func (h HeldMerge) attention() string {
	merged := fmt.Sprintf("%s merged %s at %s", h.Role, h.Base, textx.ShortSHA(h.Tip))
	if h.Conflicts > 0 {
		merged += " and resolved " + textx.Count(h.Conflicts, "conflict", "conflicts")
	}
	what := fmt.Sprintf("%s. It changes %s. %s pushes it only after your y: P asks (e shows %s).", merged, workflowWords(h.Workflows), h.Login, HeldMergeFile)
	if h.Findings {
		what += " The findings wait for the next fix after the push."
	}
	return attention.MergeReadyText(what)
}

// workflowWords names files, workflow files, as a held merge's words do:
// the first by its path, the next two by their path under
// .github/workflows/, and how many more: ".github/workflows/ci.yml and
// pronto.yml".
func workflowWords(files []string) string {
	words := make([]string, 0, 4)
	for i, f := range files[:min(3, len(files))] {
		if i > 0 {
			f = strings.TrimPrefix(f, workflowsPath+"/")
		}
		words = append(words, f)
	}
	if more := len(files) - 3; more > 0 {
		words = append(words, fmt.Sprintf("%d more", more))
	}
	return andList(words)
}

// asked is what the y/N of h's push asks about at at: the PR's head, the
// base and its tip, the conflicts and the merge (MergeAsked.Merge).
func (h HeldMerge) asked(at time.Time) MergeAsked {
	return MergeAsked{Head: h.Head, Base: h.Base, Tip: h.Tip, Conflicts: h.Conflicts, Merge: h.Merge, At: at.UTC()}
}

// pushWords say what h's push does, into branch: "magnum pushes
// claude-fix's merge of master at c0c0c0c into feature as fixer-app[bot]
// with the Workflows token; it changes .github/workflows/ci.yml and
// pronto.yml (held-merge.diff)".
func (h HeldMerge) pushWords(branch string) string {
	return fmt.Sprintf("magnum pushes %s's merge of %s at %s into %s as %s with the Workflows token; it changes %s (%s)",
		h.Role, h.Base, textx.ShortSHA(h.Tip), branch, h.Login, workflowWords(h.Workflows), HeldMergeFile)
}

// heldMergeOf is PR prID's held merge; false for none, or a row that does
// not read.
func (e *Engine) heldMergeOf(ctx context.Context, prID int64) (HeldMerge, bool) {
	raw, ok := e.getKV(ctx, KVPRHeldMerge(prID))
	if !ok {
		return HeldMerge{}, false
	}
	return ParseHeldMerge(raw)
}

// heldStale says why h, pr's held merge, went stale, "" when it did not:
// pr merged or closed, is no longer the operator's own, or its head or its
// base moved, as its registry row has them, or, with d (GitHub's Details
// of pr; has false when there are none), its head, its base or its base's
// tip moved on GitHub, or the base branch is gone there.
func (e *Engine) heldStale(pr store.PR, h HeldMerge, d github.PRDetails, has bool) string {
	short := textx.ShortSHA
	switch {
	case pr.GHState == store.GHMerged:
		return "the PR merged"
	case pr.GHState == store.GHClosed:
		return "the PR closed"
	case pr.GHState != store.GHOpen:
		return "GitHub no longer lists the PR as open"
	case !e.ownPR(pr):
		return fmt.Sprintf("the PR is %s's, not yours", cmp.Or(deref(pr.AuthorLogin), "someone else"))
	case !strings.EqualFold(pr.HeadSHA, h.Head):
		return fmt.Sprintf("its head moved from %s to %s", short(h.Head), short(pr.HeadSHA))
	case deref(pr.BaseRef) != h.Base:
		return fmt.Sprintf("its base moved from %s to %s", h.Base, deref(pr.BaseRef))
	case !has:
	case d.HeadRefOid != "" && !strings.EqualFold(d.HeadRefOid, h.Head):
		return fmt.Sprintf("its head moved on GitHub from %s to %s", short(h.Head), short(d.HeadRefOid))
	case d.BaseRefName != "" && d.BaseRefName != h.Base:
		return fmt.Sprintf("its base moved on GitHub from %s to %s", h.Base, d.BaseRefName)
	case d.BaseTipOid == "":
		return h.Base + " is gone on GitHub"
	case !strings.EqualFold(d.BaseTipOid, h.Tip):
		return fmt.Sprintf("%s moved on GitHub from %s to %s", h.Base, short(h.Tip), short(d.BaseTipOid))
	}
	return ""
}

// dropHeldMerge drops h, pr's held merge, for why: its ref, its record and
// its held-merge.diff go. A merge that was pushed (pushed) says nothing
// more; any other drop writes a fix.held_dropped event, and a PR that
// still needs you for it is told that magnum dropped it and why, and that
// P merges the parent again. subject is the PR's event subject.
func (e *Engine) dropHeldMerge(ctx context.Context, subject string, pr store.PR, h HeldMerge, why string, pushed bool) {
	owner, name, _ := strings.Cut(h.Repo, "/")
	if hg, ok := e.d.Git.(heldGit); ok && h.Clone != "" {
		if err := hg.UpdateRefDelete(ctx, h.Clone, HeldMergeRef(owner, name, h.Number)); err != nil {
			e.log.Warn("fix: delete the held merge's ref", "subject", subject, "err", err)
		}
	}
	// the file magnum wrote for it, by its own layout, never a path the record names
	if err := os.Remove(filepath.Join(e.d.Layout.PRReviews(owner, name, h.Number), HeldMergeFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Warn("fix: delete held-merge.diff", "subject", subject, "err", err)
	}
	e.delKV(ctx, KVPRHeldMerge(pr.ID))
	if pushed {
		return
	}
	ref := fmt.Sprintf("%s#%d", h.Repo, h.Number)
	msg := fmt.Sprintf("magnum dropped %s's held merge of %s at %s: %s", h.Role, h.Base, textx.ShortSHA(h.Tip), why)
	e.event(ctx, "warn", subject, "fix.held_dropped", msg, map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge})
	if pr.State != store.PRNeedsAttention || !attention.MergeReady(deref(pr.LastError)) {
		return
	}
	note := "fix: " + msg + "; P (`magnum merge-parent " + ref + "`) merges the parent again"
	if err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", note)
	}); err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: the attention of a dropped held merge", "subject", subject, "err", err)
	}
}

// heldMergesIn are the held merges of repository full's PRs, by PR id.
func (e *Engine) heldMergesIn(ctx context.Context, full string) map[int64]HeldMerge {
	vals, err := e.st.KVValues(ctx, "pr.*.held_merge")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "held merges")
		return nil
	}
	var out map[int64]HeldMerge
	for key, raw := range vals {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(key, "pr."), ".held_merge"), 10, 64)
		if err != nil || KVPRHeldMerge(id) != key {
			continue
		}
		if h, ok := ParseHeldMerge(raw); ok && strings.EqualFold(h.Repo, full) {
			if out == nil {
				out = map[int64]HeldMerge{}
			}
			out[id] = h
		}
	}
	return out
}

// withHeld is always, the PRs whose Details the poll reads whatever their
// radar says (awaitCIRuns), with the PRs of held, whose base's tip only
// the Details name.
func withHeld(always map[int]bool, held map[int64]HeldMerge) map[int]bool {
	if len(held) == 0 {
		return always
	}
	out := maps.Clone(always)
	if out == nil {
		out = make(map[int]bool, len(held))
	}
	for _, h := range held {
		out[h.Number] = true
	}
	return out
}

// dropStaleHeldMerges drops each of held, repository repo's held merges,
// that went stale as the poll sees it (heldStale): on the registry's row,
// which the poll just wrote, and on details, the Details the poll read
// (none, after a failed read, checks no tip).
func (e *Engine) dropStaleHeldMerges(ctx context.Context, repo store.Repo, held map[int64]HeldMerge, details map[int]github.PRDetails) {
	for id, h := range held {
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		d, has := details[pr.Number]
		if why := e.heldStale(pr, h, d, has); why != "" {
			e.dropHeldMerge(ctx, prSubject(repo, pr.Number), pr, h, why, false)
		}
	}
}

// requestHeld answers a merge of pr's parent while magnum holds h for it
// (requestMerge). A dry run plans h's push, after a look at GitHub; a
// merge bound to that plan pushes it (pushHeld); --yes, which binds
// nothing, a binding that names no held merge and one asked about
// otherwise (another merge, mergeAskedWait or longer ago) are refused, and
// h stays. handled is false when h went stale (heldStale): it is dropped,
// and the merge goes on as any, where a question that named h is refused.
func (e *Engine) requestHeld(ctx context.Context, pr store.PR, h HeldMerge, dry bool, asked *MergeAsked) (res string, handled bool, err error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", true, err
	}
	if why := e.heldStale(pr, h, github.PRDetails{}, false); why != "" {
		e.dropHeldMerge(ctx, prSubject(repo, pr.Number), pr, h, why, false)
		return "", false, nil
	}
	job, err := e.heldJob(ctx, repo, pr, h)
	if err != nil {
		return "", true, err
	}
	label := job.label()
	if dry {
		if d, ok := e.prDetails(ctx, job); ok {
			if why := e.heldStale(pr, h, d, true); why != "" {
				e.dropHeldMerge(ctx, job.subject(), pr, h, why, false)
				return "", false, nil
			}
		}
		res, err := MergePlan{Ref: label, Asked: h.asked(e.now()), Text: fmt.Sprintf("%s%s: %s", mergeDryWords, label, h.pushWords(deref(pr.HeadRef)))}.result()
		return res, true, err
	}
	held := fmt.Sprintf("%s's merge of %s at %s", h.Role, h.Base, textx.ShortSHA(h.Tip))
	switch {
	case asked == nil:
		return "", true, fmt.Errorf("%s: magnum holds %s for your y on a terminal: `magnum merge-parent %s` asks it (P on the screens); "+
			"--yes binds nothing and never pushes a held merge: nothing was pushed", label, held, label)
	case asked.Merge == "":
		return "", true, fmt.Errorf("%s: the question named no held merge, and magnum holds %s for your y: nothing was pushed; ask again (`magnum merge-parent %s`)",
			label, held, label)
	}
	if why := askedRefusalOf(label, *asked, h.asked(e.now()), e.now()); why != "" {
		return "", true, errors.New(why)
	}
	res, err = e.pushHeld(ctx, job, h)
	return res, true, err
}

// mergeDryWords start the words of a dry run's plan of a merge of the
// parent (MergePlan.Text), whatever it plans: the CLI's question and its
// check for a daemon from before the plan read them.
const mergeDryWords = "dry run: would merge the parent of "

// heldJob is the job of the push of h, pr's held merge, of repo: the fix
// role and its fixer App as prepareMerge has them, the PR, its head and
// the clone that holds h. It refuses as prepareMerge does a fix role that
// is no fixer App, a loop that runs, a fork, a round in flight and a
// branch magnum never pushes to.
func (e *Engine) heldJob(ctx context.Context, repo store.Repo, pr store.PR, h HeldMerge) (*fixJob, error) {
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	w := e.cfg.WatchFor(full)
	if w == nil {
		return nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return nil, fmt.Errorf("no fix role for %s", full)
	}
	if _, _, app := e.fixerSignature(role.Identity); !app {
		return nil, fmt.Errorf("magnum pushes a held merge as a fixer App, and the fix role's identity %s is not one: set the watch's fix_identity to a dedicated App", role.Identity)
	}
	if cur, running, err := e.fixState(ctx, pr.ID); err != nil {
		return nil, err
	} else if running {
		return nil, errors.New(fixRunningSentence(cur))
	}
	switch {
	case pr.IsCrossRepo:
		return nil, errors.New("the head lives in a fork magnum cannot push to")
	case slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused || e.fixHeld(pr.ID, fixPinRefuses):
		return nil, fmt.Errorf("%s: a round is already in progress (%s)", label, pr.State)
	case refusedHeadBranch(repo, pr, h.Base) != "":
		return nil, fmt.Errorf("%s: %s", label, refusedHeadBranch(repo, pr, h.Base))
	}
	return &fixJob{repo: repo, watch: *w, pool: e.cfg.PoolFor(full), pr: pr, slot: store.Slot{Path: h.Clone, MainClone: h.Clone}, role: role,
		head: h.Head, mode: FixModeMerge, state: FixState{Cap: 1, Mode: FixModeMerge}}, nil
}

// prDetails is GitHub's Details of the job's PR as its poll identity reads
// them; false when it cannot say.
func (e *Engine) prDetails(ctx context.Context, job *fixJob) (github.PRDetails, bool) {
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return github.PRDetails{}, false
	}
	det, _, err := gh.Details(ctx, job.repo.Owner, job.repo.Name, []int{job.pr.Number})
	d, ok := det[job.pr.Number]
	return d, err == nil && ok
}

// pushHeld pushes h, the job's PR's held merge, after the y bound to it:
// with the base fetched into the clone that holds h, GitHub's head of the
// PR, its base and its base's tip are still h's (heldStale), the held ref
// is h's merge, whose parents are h's head and h's tip, and h's tip is on
// the base as GitHub has it and new to the head (baseOnGitHub). Then it
// pushes h's merge as pushMerge pushes one, with a lease on the head and
// the fixer App's own Workflows token (workflowsToken), skipping only the
// check that its workflow files are the tip's: you read them and said
// yes. A held merge that moved on GitHub, or whose ref or parents are not
// h's, is dropped. After
// the push h goes, the PR's row moves to the merge as the poller would
// record it (parentOnRow), its CI starts after the grace (notePush), and
// fix.parent_pushed, the merge's fix.ended and an info toast say so.
func (e *Engine) pushHeld(ctx context.Context, job *fixJob, h HeldMerge) (string, error) {
	pg, okp := e.d.Git.(parentGit)
	hg, okh := e.d.Git.(heldGit)
	id, _, app := e.fixerSignature(job.role.Identity)
	label, short := job.label(), textx.ShortSHA
	if !okp || !okh || !app {
		return "", fmt.Errorf("%s: magnum has no git or fixer App that pushes a held merge: nothing was pushed", label)
	}
	refuse := func(why string, drop bool) (string, error) {
		if drop {
			e.dropHeldMerge(ctx, job.subject(), job.pr, h, why, false)
			why += "; magnum dropped the held merge"
		}
		return "", fmt.Errorf("%s: %s: nothing was pushed", label, why)
	}
	if !filepath.IsAbs(h.Clone) {
		return refuse("the held merge names no clone that holds it", true)
	}
	// the tip GitHub names is in the clone's git after this fetch
	if err := e.fetchBase(ctx, h.Clone, h.Base, fixRequestFetch); err != nil {
		return "", fmt.Errorf("%s: the fetch of %s: %w; nothing was pushed", label, h.Base, err)
	}
	d, ok := e.prDetails(ctx, job)
	if !ok {
		return refuse("GitHub does not say where its head and its base are", false)
	}
	if why := e.heldStale(job.pr, h, d, true); why != "" {
		return refuse(why+" since magnum held the merge", true)
	}
	ref := HeldMergeRef(job.repo.Owner, job.repo.Name, job.pr.Number)
	if got, err := e.d.Git.RevParse(ctx, h.Clone, ref); err != nil || !strings.EqualFold(got, h.Merge) {
		return refuse(fmt.Sprintf("%s no longer holds the merge %s", ref, short(h.Merge)), true)
	}
	if ps, err := hg.Parents(ctx, h.Clone, h.Merge); err != nil || len(ps) != 2 || !strings.EqualFold(ps[0], h.Head) || !strings.EqualFold(ps[1], h.Tip) {
		return refuse(fmt.Sprintf("the merge %s is not one of %s at %s into the head %s", short(h.Merge), h.Base, short(h.Tip), short(h.Head)), true)
	}
	if _, why := e.baseOnGitHub(ctx, job, fixConflict{base: h.Base, sha: h.Tip}, h.Head); why != "" {
		return refuse(why, false)
	}
	at := fmt.Sprintf("the merge of %s at %s", h.Base, short(h.Tip))
	env, release, why, err := e.workflowsToken(ctx, id, at, textx.Count(len(h.Workflows), "file", "files"))
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w; nothing was pushed", label, err)
	case why != "":
		return refuse(why, false)
	}
	pctx, cancel := context.WithTimeout(ctx, heldPushTimeout)
	err = pg.PushLeaseGitHubAs(pctx, h.Clone, job.repo.Owner, job.repo.Name, h.Merge, deref(job.pr.HeadRef), h.Head, env)
	cancel()
	release()
	if err != nil {
		return "", fmt.Errorf("%s: the push of the merge %s: %w", label, short(h.Merge), err)
	}
	e.heldPushed(ctx, job, id, h)
	return fmt.Sprintf("pushed %s's merge of %s at %s into %s as %s with the Workflows token: the head is %s",
		h.Role, h.Base, short(h.Tip), deref(job.pr.HeadRef), id.Login, short(h.Merge)), nil
}

// heldPushed is what follows the push of h, the job's PR's held merge, as
// the fix identity id: see pushHeld.
func (e *Engine) heldPushed(ctx context.Context, job *fixJob, id *config.Identity, h HeldMerge) {
	e.dropHeldMerge(ctx, job.subject(), job.pr, h, "pushed", true)
	e.notePush(ctx, job.pr.ID, h.Merge) // ci_dispatch.go: its CI starts after the grace
	if err := e.parentOnRow(ctx, job, h.Merge); err != nil {
		e.log.Warn("fix: the held merge's push on the PR row", "subject", job.subject(), "err", err)
	}
	data := map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge, "identity": id.Name, "workflows": true, "held": true}
	why := fmt.Sprintf("magnum pushed %s's merge of %s at %s into %s as %s %s after your y: the head is %s", h.Role, h.Base,
		textx.ShortSHA(h.Tip), job.label(), id.Login, workflowsWord, textx.ShortSHA(h.Merge))
	e.event(ctx, "info", job.subject(), "fix.parent_pushed", why, data)
	e.event(ctx, "info", job.subject(), "fix.ended", "the merge of the parent ends: "+why, data)
	title := fmt.Sprintf("magnum: merged the parent of %s#%d", job.repo.Name, job.pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("parent-merged:%d:%s", job.pr.ID, h.Merge), Title: title, Body: job.pr.URL + "\n" + why,
		Line: strings.TrimPrefix(title, "magnum: ") + ": " + why + " " + job.pr.URL, Kind: kindFixEnded})
}
