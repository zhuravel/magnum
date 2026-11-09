package engine

// The y/N of a merge of the parent (fix_parent.go) binds the merge to what
// it named, as a staging deploy's binds its deploy (staging_request.go): the
// plan, a dry run, fetches the base and answers, beside its words, the PR's
// head, the parent (its base branch), the tip of it the plan read and how
// many files of the merge conflict, and when it planned (MergePlan, as JSON
// on the answer's first line); the y of `magnum merge-parent` and of the
// screens' P sends that back with the merge (FixPayload.Asked).

import (
	"encoding/json"
	"strings"
	"time"
)

// MergeAsked is what the y/N of a merge of the parent asked about, which
// the merge it confirms is bound to: the PR's head, the parent (the PR's
// base branch), the tip of it the plan read, how many files the merge of
// that tip conflicts in (0: none, a merge the daemon makes) and when the
// daemon planned it.
type MergeAsked struct {
	Head      string    `json:"head"`
	Base      string    `json:"base"`
	Tip       string    `json:"tip"`
	Conflicts int       `json:"conflicts,omitempty"`
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
	a := MergeAsked{Head: j.head, Base: j.behind.base, Tip: j.behind.sha, At: at.UTC()}
	if c := j.conflict; c.sha != "" {
		a.Base, a.Tip, a.Conflicts = c.base, c.sha, len(c.paths)
	}
	return a
}
