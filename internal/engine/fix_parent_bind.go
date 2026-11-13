package engine

// The y/N of a merge of the parent (fix_parent.go) binds the merge to what
// it named, as a staging deploy's binds its deploy (staging_request.go): the
// plan, a dry run, fetches the base and answers, beside its words, the PR's
// head, the parent (its base branch), the tip of it the plan read and how
// many files of the merge conflict, and when it planned (MergePlan, as JSON
// on the answer's first line); the y of `magnum merge-parent` and of the
// screens' P sends that back with the merge (FixPayload.Asked). The merge's
// request refuses, after its own fetch, a plan that names nothing, one as
// old as mergeAskedWait and one whose head, base, tip or conflicts moved
// since; the job's guard, which fetches the base again (after a slot's
// claim, minutes later), ends the job on a move too (fixAskedMoved), and
// the daemon pushes a fixer's merge that `magnum push` handed over only
// when it merges the tip the question named (pushHanded). Nothing is
// merged then, and the words say what moved. A daemon run with --dry-run
// fetches nothing and answers each merge with its plan. A merge without a
// binding (`magnum merge-parent --yes`) merges the tip its fetch brings.
// A merge magnum holds for your y (fix_held_merge.go) is planned as its
// push, and the y binds its push to the merge's SHA too (MergeAsked.Merge);
// --yes never pushes it.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/textx"
)

// mergeAskedWait is how long the y/N of a merge of the parent binds its
// merge, as long as a staging deploy waits for its answer
// (StagingDeployWait): a merge asked about that long ago or longer is
// refused, since the base may have moved under a question left open.
const mergeAskedWait = 10 * time.Minute

// MergeAsked is what the y/N of a merge of the parent asked about, which
// the merge it confirms is bound to: the PR's head, the parent (the PR's
// base branch), the tip of it the plan read, how many files the merge of
// that tip conflicts in (0: none, a merge the daemon makes), the merge
// commit magnum holds for your y whose push the plan names (Merge,
// fix_held_merge.go; "" for a plan of a merge to make) and when the daemon
// planned it.
type MergeAsked struct {
	Head      string    `json:"head"`
	Base      string    `json:"base"`
	Tip       string    `json:"tip"`
	Conflicts int       `json:"conflicts,omitempty"`
	Merge     string    `json:"merge,omitempty"`
	At        time.Time `json:"at"`
}

// MergePlan is the daemon's answer to a dry run of a merge of the parent:
// the PR (owner/name#N), what its y/N asks about, and the plan in words
// (Text, the answer's last line: "dry run: would merge the parent of
// <PR>: magnum merges master at c0c0c0c into feature as fixer-app[bot],
// with no conflict").
type MergePlan struct {
	Ref   string     `json:"ref"`
	Asked MergeAsked `json:"asked"`
	Text  string     `json:"text"`
}

// ParseMergePlan reads the answer to a dry run of a merge of the parent
// (its first line); false when it is none: "nothing to merge", or another
// answer's words.
func ParseMergePlan(result string) (MergePlan, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	var p MergePlan
	if json.Unmarshal([]byte(line), &p) != nil || p.Ref == "" || p.Asked.Tip == "" {
		return MergePlan{}, false
	}
	return p, true
}

// result is the dry run's answer: p as JSON, then its words.
func (p MergePlan) result() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b) + "\n" + p.Text, nil
}

// planned is what the job's merge of the parent is about at at: its head,
// and the tip of its base it conflicts with (and in how many files), else
// the tip it lacks and merges cleanly.
func (j *fixJob) planned(at time.Time) MergeAsked {
	a := MergeAsked{Head: j.head, Base: j.behind.Base, Tip: j.behind.SHA, At: at.UTC()}
	if c := j.conflict; c.SHA != "" {
		a.Base, a.Tip, a.Conflicts = c.Base, c.SHA, len(c.Paths)
	}
	return a
}

// askedRefusal says why the job's merge does not run as its y/N asked
// (asked) at now, its fetch done: a binding that names no plan, a question
// mergeAskedWait or longer ago, or what moved since (mergeMoved); "" when
// it runs.
func (j *fixJob) askedRefusal(asked MergeAsked, now time.Time) string {
	return askedRefusalOf(j.label(), asked, j.planned(now), now)
}

// askedRefusalOf says why label's merge does not run as its y/N asked
// (asked) at now, when it is planned as plan: a binding that names no
// plan, a question mergeAskedWait or longer ago, or what moved since
// (mergeMoved); "" when it runs.
func askedRefusalOf(label string, asked, plan MergeAsked, now time.Time) string {
	switch age := now.Sub(asked.At); {
	case asked.Head == "" || asked.Base == "" || asked.Tip == "" || asked.At.IsZero():
		return label + ": the merge names no plan it was asked about: nothing was merged; ask again (`magnum merge-parent " + label + "`)"
	case age >= mergeAskedWait:
		return label + ": asked " + textx.DurationWords(age.Truncate(time.Minute)) + " ago, longer than a merge of the parent waits for its y (" +
			textx.DurationWords(mergeAskedWait) + "): nothing was merged; ask again"
	}
	return mergeMoved(label, asked, plan)
}

// mergeMoved says what moved between asked, what the y/N of label's merge
// asked about, and now, the merge as it is: the PR's head, its base (a
// retarget), the base's tip, the merge's conflicts or the merge magnum
// holds; "" when nothing did.
func mergeMoved(label string, asked, now MergeAsked) string {
	const nothing = " since you asked: nothing was merged; ask again"
	short := textx.ShortSHA
	switch {
	case !strings.EqualFold(asked.Head, now.Head):
		return fmt.Sprintf("%s: its head moved from %s to %s", label, short(asked.Head), short(now.Head)) + nothing
	case asked.Base != now.Base:
		return fmt.Sprintf("%s: its base moved from %s to %s", label, asked.Base, now.Base) + nothing
	case !strings.EqualFold(asked.Tip, now.Tip):
		return fmt.Sprintf("%s: %s moved from %s to %s", label, now.Base, short(asked.Tip), short(now.Tip)) + nothing
	case asked.Conflicts != now.Conflicts:
		return fmt.Sprintf("%s: the merge of %s at %s has %s now, and you were asked about %s: nothing was merged; ask again",
			label, now.Base, short(now.Tip), conflictWords(now.Conflicts), conflictWords(asked.Conflicts))
	case !strings.EqualFold(asked.Merge, now.Merge):
		return fmt.Sprintf("%s: magnum holds the merge %s now, and you were asked about %s: nothing was merged; ask again",
			label, short(now.Merge), short(asked.Merge))
	}
	return ""
}

// conflictWords says how many files a merge conflicts in: "no conflict",
// "2 conflicting files".
func conflictWords(n int) string {
	if n == 0 {
		return "no conflict"
	}
	return textx.Count(n, "conflicting file", "conflicting files")
}

// fixAskedMoved is the guard's end of a merge of the parent bound to its
// y/N whose base moved after the request's check (askedRefusal): nothing
// failed, nothing was merged, and why says what moved.
type fixAskedMoved struct{ why string }

func (e fixAskedMoved) Error() string { return e.why }
