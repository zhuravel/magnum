package factory

// The base merge's check (the operator's decision of 2026-11-13): a task's
// head that differs from the head its local review passed only by a merge
// of the base gets no full round. Task 9 (PR-27708) reached its ready card,
// origin/master conflicted with it in two files, the writer merged the
// base, and the merge got a full round of three reviewers, about 1.5
// hours, for a change that was the merge alone. Before a fresh loop starts
// at a head the review did not pass (reviewPass: mergedBase), the factory
// reads from git, never from the writer's words, whether that head is a
// merge of two parents whose first is the head the review passed (its
// review-summary.json) and whose second is a commit of origin/<base>
// (checkBaseMerge, in a repository of the factory's own: gitx.Isolated, so
// no merge driver, attribute or commit-graph of the copy's applies, and
// gitx reads the real objects, GIT_NO_REPLACE_OBJECTS):
//   - git's own merge of the two (merge-tree) is the merge's tree: a clean
//     merge gets no round. The review's end is carried to the head
//     (review-summary.json with BaseMerge) and the stage passes; the
//     factory's proof and precheck ran at the head before the stage, as at
//     any head.
//   - the merge resolved conflicts, or changed what git's own merge would
//     not have (an evil merge): one narrow round of the judge alone on `git
//     show --remerge-diff <head>` (evalrun.LocalCase.Remerge), which counts
//     toward max_rounds as any round does and whose findings follow a
//     round's rules; its loop keeps what the review before the merge left
//     (withMerge), and no second opinion follows it.
//   - anything else (commits besides the merge, a merge of something other
//     than the base, parents that are not the reviewed head and the base, a
//     conflict the merge left as git's merge wrote it, a remerge diff git
//     cannot produce) gets a full round, as before, and an event says why.
// base-merge.json records the check of the head before the factory acts on
// it, so a restart decides alike, and a narrow round that runs again in its
// cycle stays narrow.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
)

// baseMergeFile is base-merge.json in the task's directory: the newest
// check of a head that merged the base (baseMergeRecord).
const baseMergeFile = "base-merge.json"

// The kinds of a base merge's check (BaseMerge.Kind).
const (
	mergeClean  = "clean"  // git's own merge: no round
	mergeNarrow = "narrow" // conflicts resolved, or changes git's merge would not make: the judge on the remerge diff
	mergeFull   = "full"   // no merge of the base alone: a full round, as before
)

// BaseMerge is the check of a head (Head, the task's) that merges the base
// into the head the local review passed: its first parent (Reviewed), the
// base's ref (Base, origin/<base>) and its second parent (BaseSHA), what
// the check made of it (Kind), and for a narrow one the files of its
// remerge diff (Files: the conflicts it resolved and the changes git's own
// merge would not make) and those git's merge conflicts in (Conflicts);
// Why is why a full one is no merge of the base alone.
type BaseMerge struct {
	Head      string   `json:"head"`
	Reviewed  string   `json:"reviewed"`
	Base      string   `json:"base"`
	BaseSHA   string   `json:"base_sha,omitempty"`
	Kind      string   `json:"kind"`
	Files     []string `json:"files,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	Why       string   `json:"why,omitempty"`
}

// baseMergeRecord is base-merge.json: the check and the summary of the
// review that passed at its Reviewed (Carried), which a clean merge carries
// to the head and a narrow round's loop keeps (withMerge).
type baseMergeRecord struct {
	BaseMerge
	Carried ReviewSummary `json:"carried"`
}

// readBaseMerge reads base-merge.json in dir; ok is false when there is
// none, or it does not parse.
func readBaseMerge(dir string) (baseMergeRecord, bool) {
	var rec baseMergeRecord
	b, err := os.ReadFile(filepath.Join(dir, baseMergeFile))
	return rec, err == nil && json.Unmarshal(b, &rec) == nil
}

// readSummary reads review-summary.json in dir; ok is false when there is
// none, or it does not parse.
func readSummary(dir string) (ReviewSummary, bool) {
	var sum ReviewSummary
	b, err := os.ReadFile(filepath.Join(dir, reviewSummaryFile))
	return sum, err == nil && json.Unmarshal(b, &sum) == nil
}

// passedExits are the exits of review-summary.json of a review that passed
// at its head, the ones the ready card takes (readyProblem).
var passedExits = []string{registry.ExitClean, registry.ExitOscillation, exitCapFixed, exitRoundLimit, exitByYou}

// mergedCleanAt reports whether sum was carried to head over a clean merge
// of the base: the review passed there with no round, so no cycle of the
// registry ended there.
func (sum ReviewSummary) mergedCleanAt(head string) bool {
	return head != "" && sum.Head == head && sum.BaseMerge != nil && sum.BaseMerge.Kind == mergeClean && sum.BaseMerge.Head == head
}

// narrowAt reports whether a round at head is the narrow round of a base
// merge (base-merge.json in dir names head, narrow).
func narrowAt(dir, head string) bool {
	rec, ok := readBaseMerge(dir)
	return ok && head != "" && rec.Head == head && rec.Kind == mergeNarrow
}

// mergedBase is the base merge's check where a fresh loop of task t's
// local review would start, in copy c (reviewPass): handled is true when
// it took the pass, done when the stage passes. A head the review passed
// before (review-summary.json, one of passedExits) gets no check; a clean
// merge of the base into it passes the stage with no round (passCleanMerge),
// and its pass holds at the next pass, when the summary is at the head
// already. A narrow or full check, and a head that is no merge, are not
// handled: the round starts (startRound reads a narrow one from the
// record).
func (e *Engine) mergedBase(ctx context.Context, t registry.Task, c Copy) (handled, done bool, err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	sum, ok := readSummary(dir)
	switch {
	case !ok || t.HeadSHA == "" || sum.Head == "" || !slices.Contains(passedExits, sum.Exit):
		return false, false, nil
	case sum.Head == t.HeadSHA:
		passed := sum.mergedCleanAt(t.HeadSHA)
		return passed, passed, nil
	}
	rec, ok, err := e.baseMergeOf(ctx, t, c, sum)
	if err != nil || !ok || rec.Kind != mergeClean {
		return false, false, err
	}
	return e.passCleanMerge(ctx, t, c, rec)
}

// baseMergeOf is the check of task t's head against sum, the summary of
// the review that passed at the head's first parent: base-merge.json when
// it names that head and that review, else the check made now
// (checkBaseMerge), recorded with sum before its event. ok is false for a
// head that is no merge (an ordinary change, which a fresh loop reviews as
// ever, with no record).
func (e *Engine) baseMergeOf(ctx context.Context, t registry.Task, c Copy, sum ReviewSummary) (baseMergeRecord, bool, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if rec, ok := readBaseMerge(dir); ok && rec.Head == t.HeadSHA && rec.Reviewed == sum.Head {
		return rec, true, nil
	}
	bm, ok, err := e.checkBaseMerge(ctx, t, c, sum.Head)
	if err != nil || !ok {
		return baseMergeRecord{}, false, err
	}
	rec := baseMergeRecord{BaseMerge: bm, Carried: sum}
	if err := writeJSON(filepath.Join(dir, baseMergeFile), rec); err != nil {
		return rec, false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseInfo, bm.decided())
	return rec, true, nil
}

// checkBaseMerge checks task t's head, in copy c, against reviewed, the
// head its local review passed, from git alone, in a repository of the
// factory's own (Isolated: no merge driver, attribute or commit-graph of
// the copy's), with origin/<base> read in the copy. ok is false for a head
// that is no merge and has none since reviewed: an ordinary change. A read
// git cannot make, but for a cancelled one, is a full round with why, never
// a stop of the stage: a full round is what every head got before.
func (e *Engine) checkBaseMerge(ctx context.Context, t registry.Task, c Copy, reviewed string) (BaseMerge, bool, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return BaseMerge{}, false, err
	}
	git := e.d.Git
	bm := BaseMerge{Head: t.HeadSHA, Reviewed: reviewed, Base: "origin/" + baseOf(repo), Kind: mergeFull}
	full := func(why string, err error) (BaseMerge, bool, error) {
		if ctx.Err() != nil {
			return bm, false, ctx.Err()
		}
		bm.Kind, bm.Why = mergeFull, why
		if err != nil {
			bm.Why += ": " + roundError(err.Error())
		}
		return bm, true, nil
	}
	tip, err := git.RevParse(ctx, c.Path, bm.Base)
	if err != nil {
		return full("git cannot read "+bm.Base, err)
	}
	var merge bool
	var why string
	err = git.Isolated(ctx, c.Path, func(iso string) error {
		var err error
		merge, why, err = e.readBaseMerge(ctx, iso, &bm, tip)
		return err
	})
	switch {
	case err != nil:
		return full(cmp.Or(why, "git cannot read the merge"), err)
	case why != "":
		return full(why, nil)
	}
	return bm, merge, nil
}

// readBaseMerge reads bm's head in iso, a repository of the factory's own
// with the copy's objects, against bm.Reviewed and tip, origin/<base>'s
// commit, and fills in bm: merge is false for a head that is no merge and
// has none since the reviewed head; why says why a merge gets a full round
// (with err, a read git could not make).
func (e *Engine) readBaseMerge(ctx context.Context, iso string, bm *BaseMerge, tip string) (merge bool, why string, err error) {
	git := e.d.Git
	parents, err := git.Parents(ctx, iso, bm.Head)
	if err != nil {
		return true, "git cannot read its parents", err
	}
	if len(parents) != 2 {
		// the branch's own commits since the reviewed head, along its first parents
		commits, err := git.CommitsAfter(ctx, iso, bm.Reviewed, bm.Head, true)
		if err != nil {
			return true, "git cannot list its commits since " + short(bm.Reviewed), err
		}
		merges := len(slices.DeleteFunc(slices.Clone(commits), func(c gitx.ListedCommit) bool { return !c.Merge() }))
		if merges == 0 {
			return false, "", nil // an ordinary change
		}
		return true, fmt.Sprintf("it has %s besides the merge since %s, which the local review passed", plural(len(commits)-merges, "commit"),
			short(bm.Reviewed)), nil
	}
	bm.BaseSHA = parents[1]
	if parents[0] != bm.Reviewed {
		return true, fmt.Sprintf("its first parent %s is not %s, which the local review passed", short(parents[0]), short(bm.Reviewed)), nil
	}
	onBase, err := git.IsAncestor(ctx, iso, parents[1], tip)
	switch {
	case err != nil:
		return true, "git cannot tell whether its second parent is a commit of " + bm.Base, err
	case !onBase:
		return true, fmt.Sprintf("its second parent %s is no commit of %s", short(parents[1]), bm.Base), nil
	}
	tree, conflicts, err := git.MergeTree(ctx, iso, parents[0], parents[1])
	if err != nil {
		return true, "git cannot merge its parents again", err
	}
	files, err := git.RemergeFiles(ctx, iso, bm.Head)
	if err != nil {
		return true, "git cannot produce its remerge diff", err
	}
	own, err := git.TreeOf(ctx, iso, bm.Head)
	if err != nil {
		return true, "git cannot read its tree", err
	}
	kind, why := mergeKind(len(conflicts) == 0 && tree == own, conflicts, files)
	bm.Kind = kind
	if kind == mergeNarrow {
		bm.Files, bm.Conflicts = files, conflicts
	}
	return true, why, nil
}

// mergeKind is what the check makes of a merge of a commit of the base into
// the head the review passed (why for a full round): same when git's own
// merge of its parents has no conflict and the merge's tree, conflicts the
// files that merge conflicts in, files those of the merge's remerge diff. A
// clean merge is git's own. Any other is narrow when its remerge diff shows
// what it changed: every conflict among its files, since a conflict the
// merge left as git's merge wrote it (its markers and labels too) is in no
// remerge diff, and a narrow round would never see it.
func mergeKind(same bool, conflicts, files []string) (kind, why string) {
	left := slices.DeleteFunc(slices.Clone(conflicts), func(p string) bool { return slices.Contains(files, p) })
	switch {
	case same:
		return mergeClean, ""
	case len(files) == 0:
		return mergeFull, "it is not git's own merge, and `git show --remerge-diff` names no file"
	case len(left) > 0:
		return mergeFull, "it keeps git's conflict in " + andList(left) + " as git's merge wrote it"
	}
	return mergeNarrow, ""
}

// decided is the event of the check bm.
func (bm BaseMerge) decided() string {
	what := fmt.Sprintf("%s merges %s at %s into %s, which the local review passed", short(bm.Head), bm.Base, short(bm.BaseSHA), short(bm.Reviewed))
	switch bm.Kind {
	case mergeClean:
		return what + ", as git's own merge does: base merged cleanly, no review round"
	case mergeNarrow:
		return fmt.Sprintf("%s, %s: one narrow round, the judge alone on `git show --remerge-diff %s`", what, bm.narrowWhat(), short(bm.Head))
	}
	return fmt.Sprintf("%s is not a merge of %s alone into %s, which the local review passed: %s; a full round", short(bm.Head), bm.Base,
		short(bm.Reviewed), bm.Why)
}

// narrowWhat is what a narrow check found, in a line: the conflicts the
// merge resolved, else the files it changed beyond git's own merge.
func (bm BaseMerge) narrowWhat() string {
	if len(bm.Conflicts) > 0 {
		return "resolving conflicts in " + filesWords(bm.Files)
	}
	return "with changes git's own merge would not make in " + filesWords(bm.Files)
}

// mergeFilesShown bounds the files the card's line of a narrow round names.
const mergeFilesShown = 3

// filesWords are files in a line: the first mergeFilesShown, then how many
// more.
func filesWords(files []string) string {
	if n := len(files) - mergeFilesShown; n > 0 {
		return strings.Join(files[:mergeFilesShown], ", ") + fmt.Sprintf(" and %d more", n)
	}
	return strings.Join(files, ", ")
}

// cardWords are the ready card's words of the check the review summary
// carries (bm, nil for none): a clean merge's, a narrow round's with its
// files, none for a full round, which is a round as any.
func (bm *BaseMerge) cardWords() string {
	switch {
	case bm == nil:
		return ""
	case bm.Kind == mergeClean:
		return "base merged cleanly: no review round"
	case bm.Kind == mergeNarrow && len(bm.Conflicts) > 0:
		return "base merged with conflicts: a narrow round reviewed its resolutions in " + filesWords(bm.Files)
	case bm.Kind == mergeNarrow:
		return "base merged with changes git's own merge would not make: a narrow round reviewed them in " + filesWords(bm.Files)
	}
	return ""
}

// passCleanMerge passes the review stage of task t at its head, a clean
// merge of the base (rec), in copy c: what a check kept in the copy and the
// branch are read first, as a round's start reads them (keptFirst,
// branchHead: the copy at the task's head, the one the checks passed);
// then the review that passed at rec.Reviewed is carried to the head
// (review-summary.json with the check), and an event says so. No round
// runs and no cycle begins, so the round limit counts none.
func (e *Engine) passCleanMerge(ctx context.Context, t registry.Task, c Copy, rec baseMergeRecord) (handled, done bool, err error) {
	if p, pending, err := e.keptFirst(ctx, &t); err != nil || pending {
		return true, false, err
	} else if p != nil {
		return true, false, e.stageFailed(ctx, t, needsYou{p.why})
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return true, false, err
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return true, false, err
	case bh.Problem != "":
		return true, false, e.stageFailed(ctx, t, needsYou{"the local review cannot pass the merge of the base: " + bh.Problem})
	case bh.Head != t.HeadSHA:
		return true, false, e.headMoved(ctx, t, bh.Head)
	}
	sum, bm := rec.Carried, rec.BaseMerge
	sum.Head, sum.BaseMerge = t.HeadSHA, &bm
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reviewSummaryFile), sum); err != nil {
		return true, false, err
	}
	e.event(ctx, t.ID, "review", registry.PhaseOK, fmt.Sprintf("base merged cleanly at %s: no review round; the local review's end at %s holds there "+
		"(%s)", short(t.HeadSHA), short(rec.Reviewed), endedText(sum)))
	return true, true, nil
}

// withMerge is sum, the end of a loop, with the base merge whose narrow
// round began it (base-merge.json names loop's first head, narrow): the
// check, for the ready card, and what the review that passed before the
// merge left that sum lacks (its leftovers, doubts, the findings fixed
// after its last round and the reviewers its rounds went without), since
// the narrow round read the merge alone. Any other sum is returned as it
// is.
func withMerge(dir string, loop []registry.Cycle, sum ReviewSummary) ReviewSummary {
	if len(loop) == 0 {
		return sum
	}
	rec, ok := readBaseMerge(dir)
	if !ok || rec.Kind != mergeNarrow || rec.Head != loop[0].HeadBefore {
		return sum
	}
	bm, was := rec.BaseMerge, rec.Carried
	sum.BaseMerge = &bm
	sum.Leftovers, sum.Doubts = mergeItems(sum.Leftovers, was.Leftovers), mergeItems(sum.Doubts, was.Doubts)
	sum.FixedAfter = mergeItems(sum.FixedAfter, was.FixedAfter)
	missing := slices.Concat(sum.Missing, was.Missing)
	slices.Sort(missing)
	sum.Missing = slices.Compact(missing)
	return sum
}

// narrowRoles are the roles of task t's round in cycle cy (nil: a new one,
// at the task's head), roles the round's otherwise, and narrow is true for
// the narrow round of a base merge (narrowAt): the judge among roles alone.
// A second opinion's round, or one with no judge, is never narrow.
func (e *Engine) narrowRoles(t registry.Task, cy *registry.Cycle, roles []string, second bool) ([]string, bool) {
	head := t.HeadSHA
	if cy != nil {
		head = cy.HeadBefore
	}
	judge := slices.DeleteFunc(slices.Clone(roles), func(name string) bool {
		r, ok := e.cfg.RoleByNameOrAlias(nil, name)
		return !ok || !r.Judge
	})
	if second || len(judge) == 0 || !narrowAt(e.d.Layout.FactoryTaskDir(t.ID), head) {
		return roles, false
	}
	return judge, true
}
