package learn

import (
	"slices"

	"github.com/zhuravel/magnum/internal/store"
)

// RejectedMax bounds the rejected findings the classifier gets: the newest
// of the pull request's.
const RejectedMax = 40

// Rejected is a finding the judge raised and rejected, as the classifier
// gets it to name the one that reports a comment's problem by meaning
// (Item.Rejected): a test gap sits on the spec file while a person
// comments on the code under test, and the finding nearest the comment
// by line can be about something else. Every field is magnum's own text.
type Rejected struct {
	ID       string `json:"id"` // "<run id>/<finding id>", the miss's FindingRef
	Title    string `json:"title"`
	Path     string `json:"path,omitempty"`
	Reason   string `json:"reason"`             // the judge's reason code
	Priority string `json:"priority,omitempty"` // P0..P3 as the judge wrote it
}

// findingRef is how a miss names a finding: "<run id>/<finding id>".
func findingRef(f store.Finding) string { return f.RunID + "/" + f.FindingID }

// rejectedRows are the RejectedMax newest rejected findings of fs, oldest
// first; a row recorded before findings had titles is left out, as there
// is nothing to match it by.
func rejectedRows(fs []store.Finding) []Rejected {
	fs = slices.Clone(fs)
	slices.SortStableFunc(fs, func(a, b store.Finding) int { return a.CreatedAt.Compare(b.CreatedAt) })
	var out []Rejected
	for _, f := range fs {
		if f.Verdict == store.FindingRejected && f.Title != "" {
			out = append(out, Rejected{ID: findingRef(f), Title: f.Title, Path: f.Path, Reason: f.ReasonCode, Priority: f.Severity})
		}
	}
	return out[max(len(out)-RejectedMax, 0):]
}

// LinkRejected returns a copy of cands in which each candidate whose item
// names a rejected finding of rows (Item.Rejected) raised that finding:
// Raised rejected, FindingRef the finding's and ReasonCode its reason. A
// candidate whose item names none, or that has no item, keeps what Build
// found near it by path and line.
func LinkRejected(cands []Candidate, items map[string]Item, rows []Rejected) []Candidate {
	cands = slices.Clone(cands)
	for i := range cands {
		c := &cands[i]
		id := items[c.ID].Rejected
		if id == "" {
			continue
		}
		if j := slices.IndexFunc(rows, func(r Rejected) bool { return r.ID == id }); j >= 0 {
			c.Raised, c.FindingRef, c.ReasonCode = store.MissRaisedRejected, rows[j].ID, rows[j].Reason
		}
	}
	return cands
}
