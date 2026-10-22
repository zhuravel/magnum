package engine

// The review of a notes proposal, as `magnum notes <repo> --review` shows
// it (and --json prints) and the notes critic reads it (review.json): the
// notes' and the archive's diffs, the harness changes with the curator's
// reasons, the sections, the misses it was given, the sizes, the stale
// merge, and what an apply would lose.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// NotesHarnessChange is one harness file of a proposal: how it differs
// from the base, and what the curator said of it.
type NotesHarnessChange struct {
	Name   string `json:"name"`
	Change string `json:"change"` // added | removed | changed | kept
	Action string `json:"action,omitempty"`
	Into   string `json:"into,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// NotesReviewMiss is a miss a proposal was given, and what it did with it.
type NotesReviewMiss struct {
	ID       int64  `json:"id"`
	Severity string `json:"severity,omitempty"`
	Where    string `json:"where,omitempty"`
	Title    string `json:"title,omitempty"`
	Outcome  string `json:"outcome"` // store.MissNoted | store.MissSkipped
	Section  string `json:"section,omitempty"`
	Reason   string `json:"reason,omitempty"`
	State    string `json:"state"` // the miss's state now
}

// NotesMergeView is a stale proposal's merge with the notes' changes since.
type NotesMergeView struct {
	Clean          bool     `json:"clean"`
	NotesConflicts []string `json:"notes_conflicts,omitempty"`   // the base version's lines both changed differently
	Conflicts      []string `json:"harness_conflicts,omitempty"` // harness files both changed in ways that do not merge
	Kept           []string `json:"harness_kept,omitempty"`      // files the proposal deletes that a round changed since: kept
	Merged         []string `json:"harness_merged,omitempty"`    // files both changed whose texts merged
}

// NotesReviewView is a proposal as a review shows it.
type NotesReviewView struct {
	Repo     string              `json:"repo"`
	Proposal store.NotesProposal `json:"proposal"`
	Diff     string              `json:"notes_diff"`
	// ArchiveDiff is the archive's (notes.ArchiveName) unified diff, ""
	// when the proposal leaves it as it is; Harness leaves it out.
	ArchiveDiff string               `json:"archive_diff,omitempty"`
	Harness     []NotesHarnessChange `json:"harness"`
	Sections    []notes.Change       `json:"sections"`
	Misses      []NotesReviewMiss    `json:"misses"`
	Before      notes.Size           `json:"size_before"`
	After       notes.Size           `json:"size_after"`
	Expired     bool                 `json:"expired,omitempty"` // older than ProposalTTL: it can no longer be applied
	// Stale: the notes changed since the proposal was made; Merge is its
	// three-way merge with their changes, and when it is clean the diff,
	// the harness changes and the sizes are those of the merge applied to
	// the notes now.
	Stale bool            `json:"stale"`
	Merge *NotesMergeView `json:"merge,omitempty"`
	// NotPreserved is what applying the proposal would lose of the notes
	// now (PreservationProblems): a proposal with any can only be rejected
	// or curated again.
	NotPreserved []string `json:"not_preserved,omitempty"`

	// Check holds the states the review read (base, proposed, the notes now
	// and the merge); Changes the curator's changes.json.
	Check   StaleCheck    `json:"-"`
	Changes notes.Changes `json:"-"`
}

// Target is what an apply of the reviewed proposal writes: the proposed
// state, or a stale one's clean merge (false when it conflicts).
func (v NotesReviewView) Target() (notes.State, bool) { return ApplyTarget(v.Check) }

// ReviewProposal reviews proposal p of full's notes nr: its base and
// proposed states, the notes now and, when they are no longer its base
// (stale), its merge with their changes since. maxLine is [notes] max_line,
// for the sizes.
func ReviewProposal(ctx context.Context, st *store.Store, maxLine int, full string, nr notes.Repo, p store.NotesProposal) (NotesReviewView, error) {
	check, err := CheckStale(ctx, st, nr, p)
	if err != nil {
		return NotesReviewView{}, err
	}
	return ReviewFrom(ctx, st, maxLine, full, p, check)
}

// ReviewFrom is ReviewProposal from p's states as check read them (a
// restore not proposed yet has no states in the registry to read): the
// notes' and the archive's diffs, the harness changes with the curator's
// reasons, the sections, the misses it was given with what it did with
// them, the sizes before and after (of the proposal against its base, or of
// a clean merge against the notes now) and what an apply would lose.
func ReviewFrom(ctx context.Context, st *store.Store, maxLine int, full string, p store.NotesProposal, check StaleCheck) (NotesReviewView, error) {
	data := NotesReviewView{Repo: full, Proposal: p, Harness: []NotesHarnessChange{}, Sections: []notes.Change{}, Misses: []NotesReviewMiss{},
		Stale: check.Stale, Check: check}
	if len(p.Changes) > 0 {
		if err := json.Unmarshal(p.Changes, &data.Changes); err != nil {
			return data, fmt.Errorf("proposal %d: changes: %w", p.ID, err)
		}
		data.Sections = append(data.Sections, data.Changes.Sections...)
	}
	from, to := check.Base, check.Proposed
	fromName := "now"
	if m := check.Merge; data.Stale {
		data.Merge = &NotesMergeView{Clean: m.Clean, Conflicts: m.Conflicts, Kept: m.Kept, Merged: m.Merged}
		for _, c := range m.NotesConflicts {
			data.Merge.NotesConflicts = append(data.Merge.NotesConflicts, notesLinesText(c))
		}
		if m.Clean {
			from, to = check.Live, m.State
		} else {
			fromName = fmt.Sprintf("version %d", store.Deref(p.BaseVersionID))
		}
	}
	data.Before, data.After = from.Size(maxLine), to.Size(maxLine)
	toName := fmt.Sprintf("proposal %d", p.ID)
	if p.ID == 0 { // a restore not proposed
		toName = fmt.Sprintf("version %d", store.Deref(p.VersionID))
	}
	data.Diff = notes.Unified(fromName, toName, string(from.Notes), string(to.Notes), 3)
	data.ArchiveDiff = notes.Unified(fromName, toName, string(from.Archive()), string(to.Archive()), 3)
	reasons := map[string]notes.Change{}
	for _, ch := range data.Changes.Files {
		reasons[ch.Name] = ch
	}
	kept := map[string]bool{}
	if data.Merge != nil && data.Merge.Clean {
		for _, n := range data.Merge.Kept {
			kept[n] = true
		}
	}
	added, removed, changed := notes.HarnessDelta(notes.Scripts(from.Listing()), notes.Scripts(to.Listing()))
	seen := map[string]bool{}
	add := func(name, change string) {
		seen[name] = true
		ch := reasons[name]
		if kept[name] {
			ch = notes.Change{Action: notes.ActionKept, Reason: "the proposal deletes it, but a round changed it since: kept as the round left it"}
		}
		data.Harness = append(data.Harness, NotesHarnessChange{Name: name, Change: change, Action: ch.Action, Into: ch.Into, Reason: ch.Reason})
	}
	for _, n := range removed {
		add(n, "removed")
	}
	for _, n := range added {
		add(n, "added")
	}
	for _, n := range changed {
		add(n, "changed")
	}
	for _, b := range to.Files {
		if !seen[b.Path] && !notes.IsArchive(b.Path) {
			add(b.Path, "kept")
		}
	}
	links, err := st.ProposalMisses(ctx, p.ID)
	if err != nil {
		return data, err
	}
	for _, l := range links {
		m := *l.Miss
		data.Misses = append(data.Misses, NotesReviewMiss{ID: m.ID, Severity: m.Severity, Where: missPlace(m), Title: m.Title,
			Outcome: l.Outcome, Section: l.Section, Reason: l.Reason, State: m.State})
	}
	if target, ok := ApplyTarget(check); ok {
		if data.NotPreserved, err = PreservationProblems(p, check.Live, target); err != nil {
			return data, err
		}
	}
	return data, nil
}

// missPlace is a miss's place, path:line, its control characters shown as
// spaces ("" for a review summary).
func missPlace(m store.Miss) string {
	if m.Path == "" {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, m.Path)
	if m.Line > 0 {
		s += ":" + strconv.Itoa(m.Line)
	}
	return s
}

// notesLinesText names a conflict's lines of the base version, 1-based:
// "lines 3-5", "line 3", or "at line 3" for lines both sides inserted there.
func notesLinesText(c notes.MergeConflict) string {
	switch {
	case c.End > c.Start+1:
		return fmt.Sprintf("lines %d-%d", c.Start+1, c.End)
	case c.End > c.Start:
		return fmt.Sprintf("line %d", c.Start+1)
	}
	return fmt.Sprintf("at line %d", c.Start+1)
}
