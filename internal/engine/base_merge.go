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

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// deltaRecordVersion is the DeltaRecord.Version of a record measured with
// the PR's own diff in view; a record without it predates DeltaBase and is
// checked again once (recheckDeltas).
const deltaRecordVersion = 1

// ownDiff is how a PR's own diff against its base changed across a push
// (compareOwnDiffs).
type ownDiff struct {
	changed []string  // the files whose own change differs, sorted
	size    DeltaSize // that difference, for the re-review threshold
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
// (compareOwnDiffs). GitHub's three-dot comparison starts at the merge base
// of the base branch's tip with the commit, so each is the PR's diff as of
// that commit. ok is false when a call fails or the comparison is
// incomplete.
func (e *Engine) ownDiffDelta(ctx context.Context, gh GitHub, repo store.Repo, base, from, to string) (ownDiff, bool) {
	before, err := gh.CompareFiles(ctx, repo.Owner, repo.Name, base, from)
	if err == nil {
		var after []github.FileDelta
		if after, err = gh.CompareFiles(ctx, repo.Owner, repo.Name, base, to); err == nil {
			if d, ok := compareOwnDiffs(before, after); ok {
				return d, true
			}
		}
	}
	e.log.Info("delta: the PR's own diff cannot be compared in full; the push is measured since the review",
		"repo", repo.FullName(), "base", base, "from", short(from), "to", short(to), "err", err)
	return ownDiff{}, false
}

// compareOwnDiffs compares a PR's own diff before a push (base...reviewed)
// with the one after it (base...head), file by file. A file is unchanged
// when both list it with the same status, previous path and own change
// (ownChange), or neither does. A changed file adds to the size: a file the
// PR now adds (added, renamed or copied, and absent before) counts as an
// added file, any other its changed lines (ownChangedLines). ok is false
// when the comparison is incomplete: a file of either diff without a
// complete patch (binary, too large, or a listing at GitHub's file cap,
// which marks every file Truncated).
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
		if inBefore && inAfter && fb.Status == fa.Status && fb.PreviousPath == fa.PreviousPath && slices.Equal(old, cur) {
			continue
		}
		d.changed = append(d.changed, p)
		if !inBefore && (fa.Status == "added" || fa.Status == "renamed" || fa.Status == "copied") {
			d.size.AddedFiles++
			continue
		}
		d.size.Lines += ownChangedLines(p, old, cur)
	}
	return d, true
}

// ownChange is one file's own change in a PR's diff against its base: its
// added and removed lines in order, each with its "+" or "-", without the
// context lines and hunk headers, which move when the base changes around
// the change. A "\ No newline at end of file" marker is kept when it
// follows an added or removed line.
func ownChange(patch string) []string {
	var out []string
	var prev byte
	for _, l := range strings.Split(patch, "\n") {
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
