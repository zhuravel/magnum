package engine

// Pushes that only bring in the base branch (DECISIONS "A push that only
// merges the base branch is not re-reviewed"): an author merged master into
// a reviewed PR, and the comparison of the reviewed commit with the new
// head listed master's 13 commits and 300 files, so a 103-file PR was
// queued for a full re-review although its own code had not changed. What
// a re-review is about is the PR's own diff against its base. When the push
// from the reviewed commit has a merge commit, or GitHub says the two
// diverged (a rebase, a force push), checkDelta compares that diff before
// (base...reviewed) and after (base...head) the push file by file: a
// file's own change is its sequence of added and removed lines, which a
// moved hunk header or changed context around it leaves alone. Unchanged
// everywhere is the trivial class base; otherwise the re-review threshold
// measures only what changed in the PR's own diff. Anything incomplete
// keeps the comparison of reviewed...head.
//
// The rest of the round sees the push the same way (DECISIONS "A push that
// merges the base branch is reviewed by the PR's own diff"): triage reads
// the files whose own change differs and the simplify rerun measures that
// change (measureRange, compare.go), and the re-review prompts say the push
// merged the base branch (pipeline.RoundInput.BaseMerged), so the reviewers
// compare the PR's own diff before and after it instead of reading the
// base branch's commits as the PR's.

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// deltaRecordVersion is the DeltaRecord.Version of a record measured with
// the PR's own diff in view; a record without it predates DeltaBase.
const deltaRecordVersion = 1

// ownDiff is how a PR's own diff against its base changed across a push
// (compareOwnDiffs).
type ownDiff struct {
	changed []string  // the files whose own change differs, sorted
	size    DeltaSize // that difference, for the re-review threshold
	// files are the changed files as a diff to read (triage): each one's
	// own change after the push, or, for a file the PR no longer changes,
	// its change before the push reverted (undone).
	files []github.FileDelta
	// additions and deletions are every line the change of the own diff
	// adds and removes (comments and blank lines too: the since-review
	// size, ownLineDelta); commits are the PR's own commits since the push's
	// start (ownCommits).
	additions, deletions, commits int
}

// prBase is the branch a PR merges into: its base_ref, else the
// repository's default branch ("" when neither is known).
func prBase(repo store.Repo, pr store.PR) string {
	if b := deref(pr.BaseRef); b != "" {
		return b
	}
	return repo.DefaultBranch
}

// ownDiffDelta reads the PR's own diff against base before (base...from)
// and after (base...to) a push, as gh, and compares them
// (compareOwnDiffs), with the PR's own commits since (ownCommits). GitHub's
// three-dot comparison starts at the merge base of the base branch's tip
// with the commit, so each is the PR's diff as of that commit. read is true
// when both calls succeeded: d.commits counts then, even when the
// comparison is incomplete. ok is false when a call fails or the
// comparison is incomplete; d holds nothing else then.
func (e *Engine) ownDiffDelta(ctx context.Context, gh GitHub, repo store.Repo, base, from, to string) (d ownDiff, read, ok bool) {
	before, err := e.comparePush(ctx, gh, repo, base, from)
	if err == nil {
		var after github.PushComparison
		if after, err = e.comparePush(ctx, gh, repo, base, to); err == nil {
			d, ok = compareOwnDiffs(before.Files, after.Files)
			d.commits, read = ownCommits(before, after), true
			if ok {
				return d, true, true
			}
		}
	}
	e.log.Info("delta: the PR's own diff cannot be compared in full; the push is measured since the review",
		"repo", repo.FullName(), "base", base, "from", textx.ShortSHA(from), "to", textx.ShortSHA(to), "err", err)
	return d, read, false
}

// compareOwnDiffs compares a PR's own diff before a push (base...reviewed)
// with the one after it (base...head), file by file. A file is unchanged
// when both list it with the same status, previous path and own change
// (ownChange), or neither does; a file without a patch on either side (an
// empty or binary file, whose diff is empty, not missing) is unchanged only
// when both list it without one and with the same blob. A changed file adds
// to the size: a file the PR now adds (added, renamed or copied, and absent
// before) counts as an added file, any other its changed lines
// (ownChangedLines); one without a patch also counts as MeasureDelta counts
// such a file of a push (a modified binary in Binaries, any other Unread,
// either leaving the size incomplete), so the threshold never holds back a
// change it cannot count. ok is false when the comparison is incomplete: a
// file of either diff marked Truncated (a patch too large to send, or a
// listing at GitHub's file cap, which marks every file).
func compareOwnDiffs(before, after []github.FileDelta) (ownDiff, bool) {
	index := func(files []github.FileDelta, into map[string]github.FileDelta) bool {
		for _, f := range files {
			if f.Truncated {
				return false
			}
			into[f.Path] = f
		}
		return true
	}
	b, a := map[string]github.FileDelta{}, map[string]github.FileDelta{}
	if !index(before, b) || !index(after, a) {
		return ownDiff{}, false
	}
	paths := slices.AppendSeq(slices.Collect(maps.Keys(b)), maps.Keys(a))
	slices.Sort(paths)
	d := ownDiff{size: DeltaSize{Complete: true}}
	for _, p := range slices.Compact(paths) {
		fb, inBefore := b[p]
		fa, inAfter := a[p]
		var old, cur []string
		if inBefore {
			old = ownChange(fb.Patch)
		}
		if inAfter {
			cur = ownChange(fa.Patch)
		}
		same := slices.Equal(old, cur)
		noPatch := (inBefore && fb.Patch == "") || (inAfter && fa.Patch == "")
		if noPatch {
			same = fb.Patch == fa.Patch && fb.BlobSHA == fa.BlobSHA
		}
		if inBefore && inAfter && fb.Status == fa.Status && fb.PreviousPath == fa.PreviousPath && same {
			continue
		}
		d.changed = append(d.changed, p)
		f := fa
		if inAfter {
			d.files = append(d.files, fa)
		} else {
			f = fb
			d.files = append(d.files, undone(fb))
		}
		add, del := ownLineDelta(old, cur)
		d.additions, d.deletions = d.additions+add, d.deletions+del
		if !inBefore && (fa.Status == "added" || fa.Status == "renamed" || fa.Status == "copied") {
			d.size.AddedFiles++
			continue
		}
		d.size.Lines += ownChangedLines(p, old, cur)
		if noPatch {
			d.size.Complete = false
			if f.Status == "modified" && binaryDeltaPath(p) {
				d.size.Binaries = append(d.size.Binaries, p)
			} else {
				d.size.Unread++
			}
		}
	}
	return d, true
}

// ownLineDelta is what one file's own change before a push (its added and
// removed lines, ownChange) became after it, as the push's additions and
// deletions: a line the PR now adds, or no longer removes, is an addition;
// a line it no longer adds, or now removes, a deletion (a multiset
// difference, every line counted, comments and blank lines too).
func ownLineDelta(before, after []string) (additions, deletions int) {
	left := map[string]int{}
	for _, l := range after {
		left[l]++
	}
	for _, l := range before {
		left[l]--
	}
	for l, n := range left {
		switch {
		case n == 0 || l[0] == '\\':
		case (l[0] == '+') == (n > 0):
			additions += max(n, -n)
		default:
			deletions += max(n, -n)
		}
	}
	return additions, deletions
}

// ownCommits counts the PR's own commits since a push's start: the commits
// of base...to that base...from does not have, by SHA (after a merge of the
// base branch, its merge commit and the PR's new commits; after a rebase,
// every rewritten one). When GitHub listed only part of either range
// (github.ComparePushCommits), the difference of their totals stands in.
func ownCommits(before, after github.PushComparison) int {
	if len(before.SHAs) < before.Commits || len(after.SHAs) < after.Commits {
		return max(after.Commits-before.Commits, 0)
	}
	n := 0
	for _, s := range after.SHAs {
		if !slices.Contains(before.SHAs, s) {
			n++
		}
	}
	return n
}

// ownChange is one file's own change in a PR's diff against its base: its
// added and removed lines in order, each with its "+" or "-", without the
// context lines and hunk headers, which move when the base changes around
// the change. A "\ No newline at end of file" marker is kept when it
// follows an added or removed line.
func ownChange(patch string) []string {
	var out []string
	var prev byte
	for l := range strings.SplitSeq(patch, "\n") {
		if l == "" {
			prev = ' '
			continue
		}
		switch l[0] {
		case '+', '-':
			out = append(out, l)
		case '\\':
			if prev == '+' || prev == '-' {
				out = append(out, l)
			}
		}
		prev = l[0]
	}
	return out
}

// undone is a file a push took out of the PR's own diff, as a diff that
// reads like one: its own change before the push, reverted (added and
// removed lines and the hunk header's sides swapped). An added file the PR
// no longer adds reads as removed and the other way round; a rename or a
// copy undone has no such diff (Truncated, so triage treats it as unread).
func undone(f github.FileDelta) github.FileDelta {
	out := github.FileDelta{Path: f.Path, Status: f.Status, Truncated: f.Truncated}
	switch f.Status {
	case "added":
		out.Status = "removed"
	case "removed":
		out.Status = "added"
	case "modified":
	default:
		out.Truncated = true
		return out
	}
	lines := strings.Split(f.Patch, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "@@"):
			// "@@ -a,b +c,d @@ context" → "@@ -c,d +a,b @@ context"
			if parts := strings.SplitN(l, " ", 4); len(parts) >= 3 && strings.HasPrefix(parts[1], "-") && strings.HasPrefix(parts[2], "+") {
				parts[1], parts[2] = "-"+parts[2][1:], "+"+parts[1][1:]
				lines[i] = strings.Join(parts, " ")
			}
		case strings.HasPrefix(l, "+"):
			lines[i] = "-" + l[1:]
		case strings.HasPrefix(l, "-"):
			lines[i] = "+" + l[1:]
		}
	}
	out.Patch = strings.Join(lines, "\n")
	return out
}

// ownChangedLines counts the lines of one file's own change that differ
// before and after a push, as a multiset difference: an added or removed
// line counts once for every copy one side has more of. Like MeasureDelta
// it counts code only: nothing in a documentation file, no blank line, no
// line that is a comment on its own (judged outside any block comment, so
// the inside of a block counts), and a line that only moved in whitespace
// counts nothing.
func ownChangedLines(p string, before, after []string) int {
	if docDeltaPath(p) {
		return 0
	}
	syntax, indent := fileCommentSyntax(p), indentSignificant(p)
	left := map[string]int{}
	tally := func(lines []string, n int) {
		for _, l := range lines {
			if l[0] == '\\' {
				continue
			}
			t := strings.TrimSpace(l[1:])
			if t == "" {
				continue
			}
			if comment, _ := syntax.classify(t, blockOut); comment {
				continue
			}
			left[l[:1]+normalizeDeltaLine(l[1:], indent)] += n
		}
	}
	tally(before, 1)
	tally(after, -1)
	n := 0
	for _, c := range left {
		n += max(c, -c)
	}
	return n
}
