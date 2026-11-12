package engine

// A merge of the parent that magnum holds for your y (2026-11-12). The
// fixer's resolved merge that `magnum push` handed to the daemon (exit 4:
// it carries the base's workflow files) is refused by pushMerge when it
// keeps the PR's own workflow changes: an agent wrote workflow content
// that nobody read. When that is its only fault and it merges the tip
// GitHub names for the base, the PR is the operator's own (ownPR, read
// again from the registry) and the fixer App can mint its Workflows token
// (minted and released at once, so no merge is held that no y could push),
// the settle holds it instead of dropping it (holdMerge): a ref
// refs/magnum/held/<PR id> keeps the commit from git gc in the clone the
// slot's objects live in, held-merge.diff in the PR's report directory
// names the merge and shows how the fixer resolved the workflow files'
// conflicts and what the push changes in them (its control characters
// escaped), and, once the PR's attention says "merge ready: …"
// (attention.MergeReadyText) with the calls the loop carried, the registry
// keeps a HeldMerge (KVPRHeldMerge, a key-value row: no migration). Any
// failure of the hold leaves nothing and needs you as before (merge by
// hand). A fix run's fixer merges the base before the findings; its merge
// is held alike, and the findings wait for the next fix after the push.
//
// `P` and `magnum merge-parent` then plan its push (requestHeld) and, on
// the y bound to its SHA, its head and its tip, push it off the daemon's
// loop, as a merge of the parent runs, under the PR's reservation and a
// loop row (startHeldPush, pushHeld), with the fixer App's own Workflows
// token and a lease on the head. The one check of pushMerge it skips is
// that its workflow files are the tip's: the operator read them. --yes
// binds nothing and never pushes it; an n leaves it held. A held merge
// goes (dropHeldMerge: the row first, by a compare with the row read, then
// its ref and its file) once pushed, and when it went stale (heldStale):
// its PR merged or closed or is no longer yours, its row names another PR
// or another fix identity, its head or its base moved, or its base's tip
// moved on GitHub. The poll looks (dropStaleHeldMerges, which reads such a
// PR's Details on every poll, so the tip is seen), and so do the plan and
// the push. A drop or a push keeps the calls the PR's attention asks after
// the merge's words.

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
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRHeldMerge holds the merge of the PR's parent that magnum holds for
// your y (HeldMerge as JSON).
func KVPRHeldMerge(prID int64) string { return fmt.Sprintf("pr.%d.held_merge", prID) }

// HeldMergeRef is the ref that keeps the held merge of the PR whose
// registry id is prID from git gc, in the clone its objects live in: under
// refs/magnum/held/, never under refs/heads or refs/remotes, and by the
// id, so no repository's name (".github") makes a name git refuses.
func HeldMergeRef(prID int64) string { return fmt.Sprintf("refs/magnum/held/%d", prID) }

// HeldMergeFile is the file in the PR's report directory
// (paths.Layout.PRReviews) that shows what a held merge does to the
// workflow files: the merge, how the fixer resolved their conflicts, then
// what its push changes in them. The screens' e opens it in the pager.
const HeldMergeFile = "held-merge.diff"

// HeldMerge is a merge of a PR's parent that magnum holds for your y: the
// PR it was held for (Repo, owner/name, and Number: checked against the
// registry's rows, never a path's source), the merge commit (Merge), the
// PR's head on GitHub it merges into (Head, its first parent), the base
// branch and the commit of it it merges (Base, Tip: its second parent, the
// base's tip on GitHub when it was held), the workflow files its push
// changes against the head, how many files the fixer resolved conflicts in
// (Conflicts; 0 when not known), the fix role whose fixer resolved it, the
// identity that pushes it and its login, the clone that holds its ref, its
// held-merge.diff (absolute), whether a fix run's findings wait for the
// next fix after the push (Findings), the words the PR's attention starts
// with while it is held (Attention) and when it was held.
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
	Attention string    `json:"attention"`
	At        time.Time `json:"at"`
}

// ParseHeldMerge reads a KVPRHeldMerge row; false for one that names no
// merge, head, base or tip.
func ParseHeldMerge(raw string) (HeldMerge, bool) {
	var h HeldMerge
	if json.Unmarshal([]byte(raw), &h) != nil || h.Merge == "" || h.Head == "" || h.Tip == "" || h.Base == "" {
		return HeldMerge{}, false
	}
	return h, true
}

// heldRow is a held merge as its row was read: the merge, the row as
// stored (raw: a drop deletes only that row) and its PR's number.
type heldRow struct {
	h      HeldMerge
	raw    string
	number int
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

var _ heldGit = (*gitx.Client)(nil)

// fixMergeHeld is pushedHead's end of a fixer's merge that magnum push
// handed to magnum and that the daemon holds for your y (holdMerge): h,
// whose row the settle writes after the attention (settleHeld), and the
// words pushMerge refused it with, the merge by hand when that row cannot
// be written.
type fixMergeHeld struct {
	h       HeldMerge
	refused string
}

func (e fixMergeHeld) Error() string { return e.h.Attention }

// heldPushTimeout bounds the push of a held merge.
const heldPushTimeout = 2 * time.Minute

// heldFindings reports whether the job's findings wait for the next fix
// after the push of its held merge: a fix of findings that did not start
// for the merge alone (neither a merge of the parent, a CI fix, nor a job
// for the conflict alone).
func heldFindings(job *fixJob) bool { return job.mode == "" && !job.forMerge }

// holdMerge prepares the hold of merge, the fixer's merge of b's tip into
// head that pushMerge refused only because it keeps the PR's own workflow
// changes (r), for your y: the fix identity id mints its Workflows token
// (released at once), the ref, then held-merge.diff, last, and a
// fix.merge_held event; the settle writes the row after the attention
// (settleHeld). It returns the held merge, or why it holds nothing, the
// refusal the operator then gets: r's words, or the token's ("needs
// Workflows on the App") when the App cannot mint it. A hold that fails
// leaves no ref and no file.
func (e *Engine) holdMerge(ctx context.Context, job *fixJob, id *config.Identity, head, merge string, b fixConflict, r pushRefusal) (HeldMerge, string) {
	hg, ok := e.d.Git.(heldGit)
	if !ok || len(r.own) == 0 {
		return HeldMerge{}, r.why
	}
	if pr, err := e.st.PRByID(ctx, job.pr.ID); err != nil || !e.ownPR(pr) {
		return HeldMerge{}, r.why
	}
	at, files := fmt.Sprintf("the merge of %s at %s", b.base, textx.ShortSHA(b.sha)), textx.Count(len(r.files), "file", "files")
	_, release, why, err := e.workflowsToken(ctx, id, at, files) // no merge is held that no y could push
	release()
	switch {
	case err != nil:
		e.log.Warn("fix: the Workflows token of a merge to hold", "subject", job.subject(), "err", err)
		return HeldMerge{}, r.why
	case why != "":
		return HeldMerge{}, why
	}
	conflicts := 0
	if strings.EqualFold(job.conflict.sha, b.sha) {
		conflicts = len(job.conflict.paths)
	}
	clone := cmp.Or(job.slot.MainClone, job.slot.Path)
	dir := e.d.Layout.PRReviews(job.repo.Owner, job.repo.Name, job.pr.Number)
	h := HeldMerge{Repo: job.repo.FullName(), Number: job.pr.Number, Merge: merge, Head: head, Base: b.base, Tip: b.sha,
		Workflows: slices.Clone(r.files), Conflicts: conflicts, Role: job.role.Name, Identity: id.Name, Login: id.Login, Clone: clone,
		Diff: filepath.Join(dir, HeldMergeFile), Findings: heldFindings(job), At: e.now().UTC()}
	h.Attention = h.attention()
	failed := func(what string, err error) (HeldMerge, string) {
		e.log.Warn("fix: hold the merge of the parent: "+what, "subject", job.subject(), "err", err)
		return HeldMerge{}, r.why
	}
	diff, err := heldDiff(ctx, hg, clone, h)
	if err != nil {
		return failed("its diff", err)
	}
	ref := HeldMergeRef(job.pr.ID)
	if err := hg.UpdateRef(ctx, clone, ref, merge); err != nil {
		return failed("its ref", err)
	}
	if err := writeHeldDiff(dir, h.Diff, diff); err != nil {
		if derr := hg.UpdateRefDelete(context.WithoutCancel(ctx), clone, ref); derr != nil {
			e.log.Warn("fix: delete the ref of a merge not held", "subject", job.subject(), "err", derr)
		}
		return failed("held-merge.diff", err)
	}
	e.event(ctx, "info", job.subject(), "fix.merge_held", fmt.Sprintf("magnum holds %s's merge %s of %s at %s into %s for your y: it keeps the "+
		"PR's own workflow changes (%s); %s", h.Role, textx.ShortSHA(h.Merge), h.Base, textx.ShortSHA(h.Tip), job.label(),
		strings.Join(r.own[:min(3, len(r.own))], ", "), h.Diff),
		map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge, "workflows": len(h.Workflows), "own": len(r.own),
			"conflicts": h.Conflicts, "cycle": job.state.Cycle})
	return h, ""
}

// writeHeldDiff writes diff at path, in dir, made private when missing.
func writeHeldDiff(dir, path string, diff []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, diff, 0o600)
}

// settleHeld ends the settle of the job whose fixer's merge magnum holds
// (held): the PR's attention, h's words with the calls the loop carried
// (carriedText; fixAttention keeps the decision calls an earlier attention
// asked, keepDecisions), a fix.needs_you event, an urgent toast on a PR
// whose state takes no attention (a PR magnum never reviewed: needs
// attention would queue its review at the next push), then h's row: a
// drop before it finds no row, so "merge ready" never stands without one.
// A row that cannot be written leaves the merge by hand as before.
func (e *Engine) settleHeld(ctx context.Context, job *fixJob, held fixMergeHeld) {
	msg := held.h.Attention
	if carried := carriedText(job); carried != "" { // fix_carry.go: the calls of the loop's earlier cycles stay the author's
		msg = strings.TrimSuffix(msg, ".") + "; " + carried
	}
	e.event(ctx, "warn", job.subject(), "fix.needs_you", msg, job.eventData())
	e.heldAttention(ctx, job, msg)
	if err := writeKVJSON(ctx, e, KVPRHeldMerge(job.pr.ID), "the held merge", held.h); err != nil {
		e.log.Warn("fix: record the held merge", "subject", job.subject(), "err", err)
		e.unholdFiles(ctx, job.repo, job.pr, held.h)
		e.replaceHeldWords(ctx, job.subject(), job.pr.ID, held.h, "fix: "+job.handRefusedWords(held.refused))
	}
}

// heldAttention is fixAttention of msg, a held merge's or its push's, and
// an urgent toast of it on a PR whose state takes no attention, which
// fixAttention leaves with the events alone: a PR magnum never reviewed,
// whose needs attention would queue its review at its next push.
func (e *Engine) heldAttention(ctx context.Context, job *fixJob, msg string) {
	e.fixAttention(ctx, job, msg)
	if pr, err := e.st.PRByID(ctx, job.pr.ID); err == nil && pr.State != store.PRReviewed && pr.State != store.PRNeedsAttention &&
		!slices.Contains(waitingStates, pr.State) {
		e.fixCallToast(job, msg)
	}
}

// handRefusedWords are the operator's words for the fixer's merge the
// daemon did not push (why): the merge by hand (mergeByHand).
func (j *fixJob) handRefusedWords(why string) string {
	return fmt.Sprintf("magnum did not push %s's merge of the parent of %s: %s; %s", j.role.Name, j.label(), why, j.mergeByHand(why))
}

// heldNoConflict is held-merge.diff's line, in place of the remerge diff,
// for a merge that resolved no conflict in a workflow file.
const heldNoConflict = "No conflict in a workflow file was resolved: the workflow changes come unchanged from %s and the PR.\n"

// heldDiff is held-merge.diff of h in dir: a first line that names the
// merge, how it resolved the conflicts in the workflow files (`git show
// --remerge-diff`), or heldNoConflict's one line when it resolved none,
// then everything its push changes in them against its head (`git diff`),
// every control character but tab and newline escaped (escapeControls).
func heldDiff(ctx context.Context, hg heldGit, dir string, h HeldMerge) ([]byte, error) {
	remerge, err := hg.RemergeDiff(ctx, dir, h.Merge, workflowsPath)
	if err != nil {
		return nil, err
	}
	plain, err := hg.PathsDiff(ctx, dir, h.Head, h.Merge, workflowsPath)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(remerge) == "":
		remerge = fmt.Sprintf(heldNoConflict, h.Base)
	case !strings.HasSuffix(remerge, "\n"):
		remerge += "\n"
	}
	head := fmt.Sprintf("Held merge %s: %s's merge of %s at %s into the PR's head %s.\n", h.Merge, h.Role, h.Base, textx.ShortSHA(h.Tip),
		textx.ShortSHA(h.Head))
	return []byte(escapeControls(head + remerge + plain)), nil
}

// escapeControls is s with every control character but tab and newline
// (C0, DEL and C1) written as \xNN or \uNNNN: workflow content an agent
// wrote reaches a pager that runs with LESS=R, where an escape sequence or
// a carriage return could hide a line. A byte that is no UTF-8 stays.
func escapeControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteByte(s[i])
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
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

// words name h: "claude-fix's merge d3d3d3d of master at c0c0c0c".
func (h HeldMerge) words() string {
	return fmt.Sprintf("%s's merge %s of %s at %s", h.Role, textx.ShortSHA(h.Merge), h.Base, textx.ShortSHA(h.Tip))
}

// pushWords say what h's push does, into branch: "magnum pushes
// claude-fix's merge d3d3d3d of master at c0c0c0c into feature as
// fixer-app[bot] with the Workflows token; it changes
// .github/workflows/ci.yml and pronto.yml (held-merge.diff)". They name
// the merge, so a merge held again of the same tip asks another question.
func (h HeldMerge) pushWords(branch string) string {
	return fmt.Sprintf("magnum pushes %s into %s as %s with the Workflows token; it changes %s (%s)",
		h.words(), branch, h.Login, workflowWords(h.Workflows), HeldMergeFile)
}

// heldMergeOf is PR prID's held merge as its row reads now; false for
// none, or a row that does not read.
func (e *Engine) heldMergeOf(ctx context.Context, pr store.PR) (heldRow, bool) {
	raw, ok := e.getKV(ctx, KVPRHeldMerge(pr.ID))
	if !ok {
		return heldRow{}, false
	}
	h, ok := ParseHeldMerge(raw)
	return heldRow{h: h, raw: raw, number: pr.Number}, ok
}

// heldIdentity is the identity repository full's fix role acts as now, ""
// for none.
func (e *Engine) heldIdentity(full string) string {
	w := e.cfg.WatchFor(full)
	if w == nil {
		return ""
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return ""
	}
	return role.Identity
}

// heldStale says why h, pr's (of repo) held merge, went stale, "" when it
// did not: pr merged or closed or is no longer the operator's own, h names
// another PR than pr or another fix identity than the fix role's now, or
// pr's head or its base moved, as its registry row has them, or, with d
// (GitHub's Details of pr; has false when there are none), its head, its
// base or its base's tip moved on GitHub, or the base branch is gone
// there.
func (e *Engine) heldStale(repo store.Repo, pr store.PR, h HeldMerge, d github.PRDetails, has bool) string {
	short := textx.ShortSHA
	switch id := e.heldIdentity(repo.FullName()); {
	case pr.GHState == store.GHMerged:
		return "the PR merged"
	case pr.GHState == store.GHClosed:
		return "the PR closed"
	case pr.GHState != store.GHOpen:
		return "GitHub no longer lists the PR as open"
	case !e.ownPR(pr):
		return fmt.Sprintf("the PR is %s's, not yours", cmp.Or(deref(pr.AuthorLogin), "someone else"))
	case !strings.EqualFold(h.Repo, repo.FullName()) || h.Number != pr.Number:
		return fmt.Sprintf("its row names %s#%d", h.Repo, h.Number)
	case id != h.Identity:
		return fmt.Sprintf("the fix identity is %s now, not %s", cmp.Or(id, "none"), h.Identity)
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

// dropHeldMerge drops row, the held merge of pr of repo, for why. It
// deletes the row first, and only the row it read (a compare): one a push
// or another hold changed since is left alone, and so are the ref and the
// file then. It reports whether it dropped it. A merge that was pushed
// (pushed) says nothing more; any other drop writes a fix.held_dropped
// event, and a PR whose attention still starts with the merge's words gets
// the drop's in their place (attention.MergeDroppedText), the calls after
// them kept.
func (e *Engine) dropHeldMerge(ctx context.Context, repo store.Repo, pr store.PR, row heldRow, why string, pushed bool) bool {
	subject := prSubject(repo, pr.Number)
	gone, err := e.st.DeleteKVIf(ctx, KVPRHeldMerge(pr.ID), row.raw)
	if err != nil || !gone {
		if err != nil {
			e.log.Warn("fix: delete the held merge's row", "subject", subject, "err", err)
		}
		return false
	}
	e.unholdFiles(ctx, repo, pr, row.h)
	if pushed {
		return true
	}
	h := row.h
	what := fmt.Sprintf("magnum dropped %s's held merge %s of %s at %s: %s", h.Role, textx.ShortSHA(h.Merge), h.Base, textx.ShortSHA(h.Tip), why)
	e.event(ctx, "warn", subject, "fix.held_dropped", what, map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge})
	e.replaceHeldWords(ctx, subject, pr.ID, h, attention.MergeDroppedText(what))
	return true
}

// unholdFiles deletes h's ref (by the PR's id) in the clone that holds it
// and its held-merge.diff in pr's report directory of repo, by the rows,
// never by what the row names.
func (e *Engine) unholdFiles(ctx context.Context, repo store.Repo, pr store.PR, h HeldMerge) {
	if hg, ok := e.d.Git.(heldGit); ok && filepath.IsAbs(h.Clone) {
		if err := hg.UpdateRefDelete(context.WithoutCancel(ctx), h.Clone, HeldMergeRef(pr.ID)); err != nil {
			e.log.Warn("fix: delete the held merge's ref", "subject", prSubject(repo, pr.Number), "err", err)
		}
	}
	if err := os.Remove(filepath.Join(e.d.Layout.PRReviews(repo.Owner, repo.Name, pr.Number), HeldMergeFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.log.Warn("fix: delete held-merge.diff", "subject", prSubject(repo, pr.Number), "err", err)
	}
}

// heldRest is what pr's stored attention says after h's words
// (attention.MergeReadyText): the calls the loop carried and the decisions
// kept ("; …"), and any lines under them; ok is false when it does not
// start with h's words. A PR a push moved out of needs attention keeps the
// words until magnum writes others, so its state does not matter here.
func heldRest(pr store.PR, h HeldMerge) (rest string, ok bool) {
	if h.Attention == "" {
		return "", false
	}
	rest, ok = strings.CutPrefix(deref(pr.LastError), strings.TrimSuffix(h.Attention, "."))
	return strings.TrimPrefix(rest, "."), ok
}

// replaceHeldWords puts words in the place of h's at the start of PR
// prID's stored attention, the rest kept (heldRest), with a compare on the
// attention read: nothing changes for a PR whose attention says anything
// else now, and its state stays.
func (e *Engine) replaceHeldWords(ctx context.Context, subject string, prID int64, h HeldMerge, words string) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return
	}
	rest, ok := heldRest(pr, h)
	if !ok {
		return
	}
	err = e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
		u.Where("last_error", deref(pr.LastError))
		u.Set("last_error", words+rest)
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: the attention of a held merge", "subject", subject, "err", err)
	}
}

// heldMergesIn are the held merges of repo's PRs, by PR id, as their rows
// read now. A row whose PR's row is gone (magnum never deletes one) is
// dropped here, with its ref.
func (e *Engine) heldMergesIn(ctx context.Context, repo store.Repo) map[int64]heldRow {
	vals, err := e.st.KVValues(ctx, "pr.*.held_merge")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "held merges")
		return nil
	}
	var out map[int64]heldRow
	for key, raw := range vals {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(key, "pr."), ".held_merge"), 10, 64)
		if err != nil || KVPRHeldMerge(id) != key {
			continue
		}
		h, ok := ParseHeldMerge(raw)
		if !ok {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e.dropGoneHeld(ctx, id, heldRow{h: h, raw: raw})
			continue
		case err != nil || pr.RepoID != repo.ID:
			continue
		}
		if out == nil {
			out = map[int64]heldRow{}
		}
		out[id] = heldRow{h: h, raw: raw, number: pr.Number}
	}
	return out
}

// dropGoneHeld drops row, the held merge of PR id whose registry row is
// gone: its row (a compare) and its ref; no row names its file's
// directory any more.
func (e *Engine) dropGoneHeld(ctx context.Context, id int64, row heldRow) {
	if gone, err := e.st.DeleteKVIf(ctx, KVPRHeldMerge(id), row.raw); err != nil || !gone {
		return
	}
	if hg, ok := e.d.Git.(heldGit); ok && filepath.IsAbs(row.h.Clone) {
		if err := hg.UpdateRefDelete(context.WithoutCancel(ctx), row.h.Clone, HeldMergeRef(id)); err != nil {
			e.log.Warn("fix: delete the ref of a held merge whose PR is gone", "pr", id, "err", err)
		}
	}
	e.log.Info("fix: dropped the held merge of a PR whose row is gone", "pr", id, "merge", textx.ShortSHA(row.h.Merge))
}

// withHeld is always, the PRs whose Details the poll reads whatever their
// radar says (awaitCIRuns), with the PRs of held, whose base's tip only
// the Details name.
func withHeld(always map[int]bool, held map[int64]heldRow) map[int]bool {
	if len(held) == 0 {
		return always
	}
	out := maps.Clone(always)
	if out == nil {
		out = make(map[int]bool, len(held))
	}
	for _, row := range held {
		out[row.number] = true
	}
	return out
}

// dropStaleHeldMerges drops each of held, repo's held merges as the poll
// read them, that went stale as the poll sees it (heldStale): on the
// registry's row, which the poll just wrote, and on details, the Details
// the poll read (none, after a failed read, checks no tip). A PR that a
// job holds (its held merge's push, a fix) is left to it, and a row that
// changed since the poll read it is left alone (dropHeldMerge).
func (e *Engine) dropStaleHeldMerges(ctx context.Context, repo store.Repo, held map[int64]heldRow, details map[int]github.PRDetails) {
	for id, row := range held {
		pr, err := e.st.PRByID(ctx, id)
		if err != nil || e.roundActive(id) {
			continue
		}
		d, has := details[pr.Number]
		if why := e.heldStale(repo, pr, row.h, d, has); why != "" {
			e.dropHeldMerge(ctx, repo, pr, row, why, false)
		}
	}
}

// requestHeld answers a merge of pr's parent while magnum holds row's
// merge for it (requestMerge). A dry run plans its push, after a look at
// GitHub; a merge bound to that plan starts the push (startHeldPush) and
// answers that it started; --yes, which binds nothing, a binding that
// names no held merge and one asked about otherwise (another merge,
// mergeAskedWait or longer ago) are refused, and the merge stays held, as
// it does while a job holds the PR. handled is false when the merge went
// stale (heldStale): it is dropped, and the merge goes on as any, where a
// question that named it is refused.
func (e *Engine) requestHeld(ctx context.Context, pr store.PR, row heldRow, dry bool, asked *MergeAsked) (res string, handled bool, err error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", true, err
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	switch cur, running, err := e.fixState(ctx, pr.ID); {
	case err != nil:
		return "", true, err
	case running: // its push, or a fix loop
		return "", true, fmt.Errorf("%s: %s", label, fixRunningSentence(cur))
	case e.roundActive(pr.ID):
		return "", true, fmt.Errorf("%s: %s; ask again shortly", label, e.heldReason(pr.ID))
	}
	if why := e.heldStale(repo, pr, row.h, github.PRDetails{}, false); why != "" {
		e.dropHeldMerge(ctx, repo, pr, row, why, false)
		return "", false, nil
	}
	job, err := e.heldJob(ctx, repo, pr, row.h)
	if err != nil {
		return "", true, err
	}
	h := row.h
	if dry {
		if d, ok := e.prDetails(ctx, job); ok {
			if why := e.heldStale(repo, pr, h, d, true); why != "" {
				e.dropHeldMerge(ctx, repo, pr, row, why, false)
				return "", false, nil
			}
		}
		res, err := MergePlan{Ref: label, Asked: h.asked(e.now()), Text: fmt.Sprintf("%s%s: %s", mergeDryWords, label, h.pushWords(deref(pr.HeadRef)))}.result()
		return res, true, err
	}
	switch {
	case asked == nil:
		return "", true, fmt.Errorf("%s: magnum holds %s for your y on a terminal: `magnum merge-parent %s` asks it (P on the screens); "+
			"--yes binds nothing and never pushes a held merge: nothing was pushed", label, h.words(), label)
	case asked.Merge == "":
		return "", true, fmt.Errorf("%s: the question named no held merge, and magnum holds %s for your y: nothing was pushed; ask again (`magnum merge-parent %s`)",
			label, h.words(), label)
	}
	if why := askedRefusalOf(label, *asked, h.asked(e.now()), e.now()); why != "" {
		return "", true, errors.New(why)
	}
	if err := e.startHeldPush(ctx, job, row); err != nil {
		return "", true, err
	}
	return fmt.Sprintf("pushing %s into %s as %s with the Workflows token; `magnum abort %s` stops it", h.words(), deref(pr.HeadRef), h.Login, label), true, nil
}

// mergeDryWords start the words of a dry run's plan of a merge of the
// parent (MergePlan.Text), whatever it plans: the CLI's question and its
// check for a daemon from before the plan read them.
const mergeDryWords = "dry run: would merge the parent of "

// heldJob is the job of the push of h, pr's held merge, of repo: the fix
// role and its fixer App as prepareMerge has them, the PR, its head and
// the clone that holds h. It refuses as prepareMerge does a fix role that
// is no fixer App, a fork, a round in flight and a branch magnum never
// pushes to.
func (e *Engine) heldJob(_ context.Context, repo store.Repo, pr store.PR, h HeldMerge) (*fixJob, error) {
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

// startHeldPush starts the push of row's merge off the daemon's loop, as
// a merge of the parent starts (startMerge): the PR reserved as a fix job
// holds it, the loop's row in phase fixing in FixModeMerge, which `magnum
// abort` ends and `--wait` follows, and runHeldPush on a goroutine of its
// own under the reservation, on the daemon's context.
func (e *Engine) startHeldPush(ctx context.Context, job *fixJob, row heldRow) error {
	st := FixState{Cycle: 1, Cap: 1, Since: job.head, By: fixMergeBy, StartedAt: e.now().UTC(), Phase: FixPhaseFixing, Head: job.head,
		Role: job.role.Name, Mode: FixModeMerge}
	job.state = st
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(job.pr.ID, &roundHandle{cancel: cancel, fix: true}) {
		cancel()
		return fmt.Errorf("%s: %s; ask again shortly", job.label(), e.heldReason(job.pr.ID))
	}
	if err := e.claimFixRow(ctx, job, FixState{}, st); err != nil {
		e.unreserve(job.pr.ID)
		cancel()
		return err
	}
	e.roundWG.Go(func() {
		defer e.unreserve(job.pr.ID)
		defer e.workEnded(rctx, job.pr.ID)
		e.safely(rctx, "fix", job.subject(), func() { e.runHeldPush(rctx, job, row) },
			func(ctx context.Context, msg string) { e.fixPanicked(ctx, job, msg) })
	})
	return nil
}

// runHeldPush is the job startHeldPush starts: the push (pushHeld), then
// its end: what follows a push (heldPushed), or a fix.failed event and an
// urgent toast with why nothing was pushed (a held merge that went stale
// was dropped, with its words in the attention's; one that stays keeps
// them). A stop (`magnum abort`) during the push ends the loop as any
// stop does; one that comes after the push hears that it came too late.
// A shutdown leaves the loop's row for the next start.
func (e *Engine) runHeldPush(ctx context.Context, job *fixJob, row heldRow) {
	id, _, _ := e.fixerSignature(job.role.Identity)
	why, err := e.pushHeld(ctx, job, id, row)
	fctx := context.WithoutCancel(ctx)
	pushed := why == "" && err == nil
	if pushed {
		e.endFixState(fctx, job)
		e.heldPushed(fctx, job, id, row)
	}
	if orders := e.closeFixHandle(job.pr.ID); len(orders) > 0 {
		if !pushed {
			e.stopFix(fctx, job, orders)
			return
		}
		for _, o := range orders {
			e.complete(fctx, o.id, nil, "nothing to abort: magnum pushed the held merge of "+job.label()+" already")
			e.mu.Lock()
			delete(e.inflight, o.id)
			e.mu.Unlock()
		}
		return
	}
	if pushed {
		return
	}
	if ctx.Err() != nil {
		e.log.Info("the push of a held merge interrupted by a shutdown; the next start ends its loop", "subject", job.subject())
		return
	}
	if err != nil {
		why = fmt.Sprintf("%s: %s: nothing was pushed", job.label(), oneLine(err.Error(), 300))
	}
	e.endFixState(fctx, job)
	e.event(fctx, "warn", job.subject(), "fix.failed", why, job.eventData())
	e.fixCallToast(job, why)
}

// pushHeld pushes row's merge, the job's PR's held merge, after the y
// bound to it, as the fix identity id: with the base fetched into the
// clone that holds it, GitHub's head of the PR, its base and its base's
// tip, which is then the merge's second parent on the base, are still the
// held ones (heldStale), the held ref is the merge, and its parents are
// the head and the tip. Then it pushes the merge as pushMerge pushes one,
// with a lease on the head and the fixer App's own Workflows token
// (workflowsToken), within heldPushTimeout, skipping only the check that
// its workflow files are the tip's: you read them and said yes. It returns
// why nothing was pushed (a merge that moved on GitHub, or whose ref or
// parents are not the held ones, is dropped), or the push's error.
func (e *Engine) pushHeld(ctx context.Context, job *fixJob, id *config.Identity, row heldRow) (string, error) {
	pg, okp := e.d.Git.(parentGit)
	hg, okh := e.d.Git.(heldGit)
	h, label, short := row.h, job.label(), textx.ShortSHA
	refuse := func(why string, drop bool) (string, error) {
		if drop {
			e.dropHeldMerge(context.WithoutCancel(ctx), job.repo, job.pr, row, why, false)
			why += "; magnum dropped the held merge"
		}
		return fmt.Sprintf("%s: %s: nothing was pushed", label, why), nil
	}
	switch {
	case !okp || !okh || id == nil:
		return refuse("magnum has no git or fixer App that pushes a held merge", false)
	case !filepath.IsAbs(h.Clone):
		return refuse("the held merge names no clone that holds it", true)
	}
	if err := e.fetchBase(ctx, h.Clone, h.Base, fixRequestFetch); err != nil {
		return "", fmt.Errorf("the fetch of %s: %w", h.Base, err)
	}
	d, ok := e.prDetails(ctx, job)
	if !ok {
		return refuse("GitHub does not say where its head and its base are", false)
	}
	if why := e.heldStale(job.repo, job.pr, h, d, true); why != "" {
		return refuse(why+" since magnum held the merge", true)
	}
	ref := HeldMergeRef(job.pr.ID)
	if got, err := e.d.Git.RevParse(ctx, h.Clone, ref); err != nil || !strings.EqualFold(got, h.Merge) {
		return refuse(fmt.Sprintf("%s no longer holds the merge %s", ref, short(h.Merge)), true)
	}
	if ps, err := hg.Parents(ctx, h.Clone, h.Merge); err != nil || len(ps) != 2 || !strings.EqualFold(ps[0], h.Head) || !strings.EqualFold(ps[1], h.Tip) {
		return refuse(fmt.Sprintf("the merge %s is not one of %s at %s into the head %s", short(h.Merge), h.Base, short(h.Tip), short(h.Head)), true)
	}
	at := fmt.Sprintf("the merge of %s at %s", h.Base, short(h.Tip))
	env, release, why, err := e.workflowsToken(ctx, id, at, textx.Count(len(h.Workflows), "file", "files"))
	switch {
	case err != nil:
		return "", err
	case why != "":
		return refuse(why, false)
	}
	pctx, cancel := context.WithTimeout(ctx, heldPushTimeout)
	err = pg.PushLeaseGitHubAs(pctx, h.Clone, job.repo.Owner, job.repo.Name, h.Merge, deref(job.pr.HeadRef), h.Head, env)
	cancel()
	release()
	if err != nil {
		return "", fmt.Errorf("the push of the merge %s: %w", short(h.Merge), err)
	}
	return "", nil
}

// heldPushed is what follows the push of row's merge, the job's PR's held
// merge, as the fix identity id: its row, ref and file go, the PR's row
// moves to the merge as the poller would record it (parentOnRow), its CI
// starts after the grace (notePush), and fix.parent_pushed says so. A fix
// run's merge (its findings wait), or one whose attention asked calls
// after its words, then needs you: a fix.needs_you event and the
// attention say that the findings wait for `magnum fix` (f on the
// screens), with the calls kept (heldAttention). A push the poller's rules
// queue a re-review for (a merge of the base that is not a trivial delta)
// leaves the PR waiting for that review, which f needs on the head: the
// words say it runs first, and the event and the urgent toast carry them
// (fixAttention raises no attention over a queued round). Any other ends
// with the merge's fix.ended (which --wait reads) and an info toast.
func (e *Engine) heldPushed(ctx context.Context, job *fixJob, id *config.Identity, row heldRow) {
	h := row.h
	rest := ""
	if cur, err := e.st.PRByID(ctx, job.pr.ID); err == nil {
		rest, _ = heldRest(cur, h)
	}
	e.dropHeldMerge(ctx, job.repo, job.pr, row, "pushed", true)
	e.notePush(ctx, job.pr.ID, h.Merge) // ci_dispatch.go: its CI starts after the grace
	if err := e.parentOnRow(ctx, job, h.Merge); err != nil {
		e.log.Warn("fix: the held merge's push on the PR row", "subject", job.subject(), "err", err)
	}
	data := map[string]any{"base": h.Base, "tip": h.Tip, "head": h.Head, "merge": h.Merge, "identity": id.Name, "workflows": true, "held": true}
	why := fmt.Sprintf("magnum pushed %s into %s as %s %s after your y: the head is %s", h.words(), job.label(), id.Login, workflowsWord,
		textx.ShortSHA(h.Merge))
	e.event(ctx, "info", job.subject(), "fix.parent_pushed", why, data)
	if h.Findings || strings.TrimSpace(rest) != "" {
		msg := fmt.Sprintf("fix: magnum pushed %s's held merge %s after your y", h.Role, textx.ShortSHA(h.Merge))
		if h.Findings {
			first := "" // f fixes the findings of a review on the head: one the push queued runs first
			if cur, err := e.st.PRByID(ctx, job.pr.ID); err == nil && slices.Contains(waitingStates, cur.State) {
				first = fmt.Sprintf("the re-review of %s runs first, then ", textx.ShortSHA(h.Merge))
			}
			msg += fmt.Sprintf("; the findings wait: %s`magnum fix %s` (f on the screens) runs %s again", first, job.label(), h.Role)
		}
		msg += rest
		e.event(ctx, "warn", job.subject(), "fix.needs_you", msg, data)
		e.heldAttention(ctx, job, msg)
		return
	}
	e.event(ctx, "info", job.subject(), "fix.ended", "the merge of the parent ends: "+why, data)
	title := fmt.Sprintf("magnum: merged the parent of %s#%d", job.repo.Name, job.pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("parent-merged:%d:%s", job.pr.ID, h.Merge), Title: title, Body: job.pr.URL + "\n" + why,
		Line: strings.TrimPrefix(title, "magnum: ") + ": " + why + " " + job.pr.URL, Kind: kindFixEnded})
}
