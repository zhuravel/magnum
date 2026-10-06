package notes

// The three-way merge of a curation proposal that went stale: the notes
// changed after the proposal started from them. It merges the notes text and
// every harness file the way `git merge-file` does, line by line, but reports
// the lines both sides changed differently instead of writing conflict markers.

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// MergeConflict is a range of base lines both sides changed differently:
// lines [Start, End) of base, 0-based.
type MergeConflict struct{ Start, End int }

// mergeRegion is a run of base lines [lo, hi) that one side or both changed:
// the changes of each side that fall in it, in base's line numbers.
type mergeRegion struct {
	lo, hi       int
	ours, theirs []change
}

// Merge3 merges the changes ours and theirs made to base, line by line (diff3,
// as `git merge-file` does it). A region of base that only one side changed
// takes that side's lines, and one both changed the same way takes them once.
// Changes that overlap or touch (an insertion next to the other side's change
// included, as git treats changes on adjacent lines) are one region, and when
// the two sides' lines for it differ the region is a conflict: it is listed in
// conflicts and merged keeps base's lines there, so merged means something only
// when conflicts is empty.
func Merge3(base, ours, theirs string) (merged string, conflicts []MergeConflict) {
	switch {
	case ours == theirs, theirs == base:
		return ours, nil
	case ours == base:
		return theirs, nil
	}
	b, o, t := splitLines(base), splitLines(ours), splitLines(theirs)
	var sb strings.Builder
	at := 0                    // the next line of base not written yet
	offOurs, offTheirs := 0, 0 // how many lines each side has gained before the region
	for _, r := range mergeRegions(diffLines(b, o, 0), diffLines(b, t, 0)) {
		for _, l := range b[at:r.lo] {
			sb.WriteString(l)
		}
		at = r.hi
		// Between the changes a side's lines are base's, so a region's lines in
		// a side are the same range of it moved by what the side gained before.
		growOurs, growTheirs := mergeGrowth(r.ours), mergeGrowth(r.theirs)
		oursLines := o[r.lo+offOurs : r.hi+offOurs+growOurs]
		theirsLines := t[r.lo+offTheirs : r.hi+offTheirs+growTheirs]
		offOurs += growOurs
		offTheirs += growTheirs
		var take []string
		switch {
		case len(r.theirs) == 0, slices.Equal(oursLines, theirsLines):
			take = oursLines
		case len(r.ours) == 0:
			take = theirsLines
		default:
			conflicts = append(conflicts, MergeConflict{Start: r.lo, End: r.hi})
			take = b[r.lo:r.hi]
		}
		for _, l := range take {
			sb.WriteString(l)
		}
	}
	for _, l := range b[at:] {
		sb.WriteString(l)
	}
	return sb.String(), conflicts
}

// mergeRegions groups the changes of both sides, in base's line numbers, into
// regions in order. A change joins the region before it when it starts at or
// before that region's end, so changes that overlap and changes that touch (an
// insertion at the boundary of a change included) share a region, whichever
// sides they come from.
func mergeRegions(ours, theirs []change) []mergeRegion {
	type sided struct {
		change
		theirs bool
	}
	all := make([]sided, 0, len(ours)+len(theirs))
	for _, c := range ours {
		all = append(all, sided{change: c})
	}
	for _, c := range theirs {
		all = append(all, sided{change: c, theirs: true})
	}
	slices.SortStableFunc(all, func(x, y sided) int { return cmp.Compare(x.a0, y.a0) })
	var regions []mergeRegion
	for _, c := range all {
		if len(regions) == 0 || c.a0 > regions[len(regions)-1].hi {
			regions = append(regions, mergeRegion{lo: c.a0, hi: c.a1})
		}
		r := &regions[len(regions)-1]
		r.hi = max(r.hi, c.a1)
		if c.theirs {
			r.theirs = append(r.theirs, c.change)
		} else {
			r.ours = append(r.ours, c.change)
		}
	}
	return regions
}

// mergeGrowth is how many lines more than base a side has after cs, its
// changes: the replacing lines less the replaced ones.
func mergeGrowth(cs []change) int {
	n := 0
	for _, c := range cs {
		n += (c.b1 - c.b0) - (c.a1 - c.a0)
	}
	return n
}

// StateMerge is the three-way merge of a stale curation proposal: base is
// the state the proposal started from, live the notes now, proposed the
// proposal's state.
type StateMerge struct {
	State          State           // the merged state; apply it only when Clean
	Clean          bool            // neither the notes text nor a harness file conflicts
	NotesConflicts []MergeConflict // in base's lines of the notes
	Kept           []string        // harness files the proposal deletes that changed since base: kept as live has them
	Conflicts      []string        // harness files both sides changed in ways that do not merge
	Merged         []string        // harness files both sides changed whose texts merged line by line
}

// MergeStates merges proposed into live, both changes of base. The notes text
// merges line by line (Merge3). Each harness file, compared by its presence and
// its hash, goes by what each side did to it:
//
//   - live left it as base had it: the proposal's version wins, deletion included;
//   - the proposal left it as base had it, or both made it the same: live's version stays;
//   - the proposal deletes it but live changed it: live's version stays and the
//     path is in Kept, so a file written since the proposal started survives;
//   - live deleted it and the proposal changed it, or both added it with
//     different texts: a conflict;
//   - both changed it: their texts merge line by line, and when they do not the
//     file is a conflict.
//
// A conflicting file stays as live has it (absent when live deleted it), as
// the notes text keeps base's lines where its conflicts are; the merged state
// is to be applied only when Clean. Kept, Conflicts and Merged are in path
// order, nil when empty, and so are the state's files, which have SHA256 set.
// The inputs are not modified.
func MergeStates(base, live, proposed State) StateMerge {
	var m StateMerge
	text, conflicts := Merge3(string(base.Notes), string(live.Notes), string(proposed.Notes))
	m.NotesConflicts = conflicts
	m.State.Notes = []byte(text)
	switch {
	case live.Exists == base.Exists:
		m.State.Exists = proposed.Exists
	case proposed.Exists == base.Exists:
		m.State.Exists = live.Exists
	default:
		m.State.Exists = text != ""
	}

	bf, lf, pf := mergeVersions(base.Files), mergeVersions(live.Files), mergeVersions(proposed.Files)
	paths := make(map[string]bool, len(bf)+len(lf)+len(pf))
	for _, fs := range []map[string]mergeFile{bf, lf, pf} {
		for p := range fs {
			paths[p] = true
		}
	}
	// In path order, so the files and the lists come out sorted.
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		b, l, p := bf[path], lf[path], pf[path]
		var take mergeFile
		switch {
		case l.same(b):
			take = p
		case p.same(b), l.same(p):
			take = l
		case !p.present:
			take = l
			m.Kept = append(m.Kept, path)
		case !l.present:
			take = l
			m.Conflicts = append(m.Conflicts, path)
		case b.present:
			merged, clashes := Merge3(string(b.body), string(l.body), string(p.body))
			if len(clashes) > 0 {
				take = l
				m.Conflicts = append(m.Conflicts, path)
				break
			}
			body := []byte(merged)
			take = mergeFile{present: true, sha: TextSHA(body), body: body}
			m.Merged = append(m.Merged, path)
		default:
			take = l
			m.Conflicts = append(m.Conflicts, path)
		}
		if take.present {
			m.State.Files = append(m.State.Files, Blob{Path: path, SHA256: take.sha, Body: take.body})
		}
	}
	m.Clean = len(m.NotesConflicts) == 0 && len(m.Conflicts) == 0
	return m
}

// mergeFile is one version of a harness file; the zero value is the file
// being absent.
type mergeFile struct {
	present bool
	sha     string
	body    []byte
}

// same reports whether f and o are both absent or both present with the same hash.
func (f mergeFile) same(o mergeFile) bool { return f.present == o.present && f.sha == o.sha }

// mergeVersions indexes files by path, hashing a body that comes without its hash.
func mergeVersions(files []Blob) map[string]mergeFile {
	out := make(map[string]mergeFile, len(files))
	for _, b := range files {
		sha := b.SHA256
		if sha == "" {
			sha = TextSHA(b.Body)
		}
		out[b.Path] = mergeFile{present: true, sha: sha, body: b.Body}
	}
	return out
}
