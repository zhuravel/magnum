package notes

// The line diff `magnum notes --diff` and `--review` print, and the line
// counts of a notes.changed event: a minimal edit script (Myers, linear
// space) with GNU diff's choice among scripts of the same length, so the
// output reads like `diff -u`.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Unified returns a unified diff of a and b, compared line by line, with ctx
// lines of context around each change: the header "--- <nameA>\n+++ <nameB>\n",
// then hunks "@@ -<start>,<count> +<start>,<count> @@\n" followed by lines prefixed
// with ' ', '-' or '+', each ending in "\n". It returns "" when a == b.
//
// Start numbers are 1-based. A range of one line is written as its start alone
// ("@@ -3 +3 @@") and a range of no lines as the number of the line before it
// and a zero count ("-0,0" when a is empty), as GNU diff does. Changes whose
// context would overlap or touch (at most 2*ctx unchanged lines between them)
// share one hunk. Lines are split on "\n" only, so a "\r" stays part of its
// line, and a line without its "\n" (only the last one of a side can be) differs
// from the same text with it. Such a line is printed followed by
// "\n\\ No newline at end of file\n". A negative ctx counts as 0.
func Unified(nameA, nameB, a, b string, ctx int) string {
	if a == b {
		return ""
	}
	ctx = max(ctx, 0)
	la, lb := splitLines(a), splitLines(b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	changes := diffLines(la, lb, ctx)
	for i := 0; i < len(changes); {
		j := i + 1
		for j < len(changes) && changes[j].a0-changes[j-1].a1 <= 2*ctx {
			j++
		}
		writeHunk(&sb, la, lb, changes[i:j], ctx)
		i = j
	}
	return sb.String()
}

// LineChanges counts the lines b adds to a and the lines it removes, with the
// same line matching as Unified (a changed line counts once in each).
func LineChanges(a, b string) (added, removed int) {
	if a == b {
		return 0, 0
	}
	for _, c := range diffLines(splitLines(a), splitLines(b), 0) {
		removed += c.a1 - c.a0
		added += c.b1 - c.b0
	}
	return added, removed
}

// change replaces the lines a[a0:a1] by b[b0:b1]; at least one range is not empty.
type change struct{ a0, a1, b0, b1 int }

// splitLines splits s after each "\n", keeping the newline with its line; the
// last line has none when s does not end in one.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines returns a minimal edit script from a to b as changes in order,
// with the unchanged lines between two changes the same in both. When several
// scripts are as short it returns the one GNU diff -U ctx does: ctx lines of
// the start and the end the two sides share stay in the comparison, as there,
// and where the changes may slide to depends on them.
func diffLines(a, b []string, ctx int) []change {
	ids := make(map[string]int, len(a)+len(b))
	intern := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	xa, ya := intern(a), intern(b)

	// The lines both sides start and end with are the same whatever the rest
	// does (but for ctx of them, kept as GNU diff does), and a line of what
	// is left that the other side lacks is a change: neither takes part in
	// the search, which has less to do.
	lo := 0
	for lo < len(xa) && lo < len(ya) && xa[lo] == ya[lo] {
		lo++
	}
	hiX, hiY := len(xa), len(ya)
	for hiX > lo && hiY > lo && xa[hiX-1] == ya[hiY-1] {
		hiX--
		hiY--
	}
	suffix := len(xa) - hiX
	lo = max(lo-ctx, 0)
	hiX += min(suffix, ctx)
	hiY += min(suffix, ctx)
	inX, inY := make([]bool, len(ids)), make([]bool, len(ids))
	for _, id := range xa[lo:hiX] {
		inX[id] = true
	}
	for _, id := range ya[lo:hiY] {
		inY[id] = true
	}
	delX, insY := make([]bool, len(a)), make([]bool, len(b))
	var xs, ys, xpos, ypos []int
	for i := lo; i < hiX; i++ {
		if inY[xa[i]] {
			xs = append(xs, xa[i])
			xpos = append(xpos, i)
		} else {
			delX[i] = true
		}
	}
	for j := lo; j < hiY; j++ {
		if inX[ya[j]] {
			ys = append(ys, ya[j])
			ypos = append(ypos, j)
		} else {
			insY[j] = true
		}
	}
	s := &seqDiff{
		x:    xs,
		y:    ys,
		off:  len(ys) + 1,
		fd:   make([]int, len(xs)+len(ys)+3),
		bd:   make([]int, len(xs)+len(ys)+3),
		delX: make([]bool, len(xs)),
		insY: make([]bool, len(ys)),
	}
	s.compare(0, len(xs), 0, len(ys))
	for k, del := range s.delX {
		delX[xpos[k]] = del
	}
	for k, ins := range s.insY {
		insY[ypos[k]] = ins
	}
	shiftBoundaries(xa[lo:hiX], delX[lo:hiX], insY[lo:hiY])
	shiftBoundaries(ya[lo:hiY], insY[lo:hiY], delX[lo:hiX])

	var changes []change
	for i, j := 0, 0; i < len(a) || j < len(b); {
		if i < len(a) && !delX[i] && j < len(b) && !insY[j] {
			i++
			j++
			continue
		}
		c := change{a0: i, b0: j}
		for i < len(a) && delX[i] {
			i++
		}
		for j < len(b) && insY[j] {
			j++
		}
		c.a1, c.b1 = i, j
		changes = append(changes, c)
	}
	return changes
}

// shiftBoundaries slides every run of changed lines of one side, whose lines
// are eq with the changed ones marked in changed, over the equal lines next to
// it, which leaves the script as short: as far up as it goes, merging with
// the changes it reaches, then as far down, and back up to a run of the other
// side's changes the run then lines up with. Like GNU diff, it picks what a
// reader expects out of the equally short scripts, such as the added lines of
// a repeated block being the last ones.
func shiftBoundaries(eq []int, changed, other []bool) {
	n := len(eq)
	get := func(marks []bool, i int) bool { return i >= 0 && i < len(marks) && marks[i] }
	i, j := 0, 0 // the line of this side and the line of the other side at the same point
	for {
		// Scan forwards to the beginning of another run of changes.
		for i < n && !changed[i] {
			for { // skip the other side's changes before the matching line
				c := get(other, j)
				j++
				if !c {
					break
				}
			}
			i++
		}
		if i == n {
			return
		}
		start := i
		for { // find the end of the run
			i++
			if !get(changed, i) {
				break
			}
		}
		for get(other, j) {
			j++
		}

		var runLength, corresponding int
		for {
			runLength = i - start

			// Move the run back while the line before it equals its last
			// line, which merges it with the runs before.
			for start > 0 && eq[start-1] == eq[i-1] {
				start--
				changed[start] = true
				i--
				changed[i] = false
				for get(changed, start-1) {
					start--
				}
				for {
					j--
					if !get(other, j) {
						break
					}
				}
			}

			// The end of the run at the last point where it lines up with a
			// run of the other side, n when there is none.
			corresponding = n
			if get(other, j-1) {
				corresponding = i
			}

			// Move it forward while the line after it equals its first line,
			// which merges it with the runs after. Done second, so that
			// without merges the run ends as far down as it goes.
			for i != n && eq[start] == eq[i] {
				changed[start] = false
				start++
				changed[i] = true
				i++
				for get(changed, i) {
					i++
				}
				for {
					j++
					if !get(other, j) {
						break
					}
					corresponding = i
				}
			}

			if runLength == i-start {
				break
			}
		}

		// Move the merged run back to where it lines up with the other side.
		for corresponding < i {
			start--
			changed[start] = true
			i--
			changed[i] = false
			for {
				j--
				if !get(other, j) {
					break
				}
			}
		}
	}
}

// seqDiff finds a shortest edit script between two sequences of line ids with
// Myers' O(ND) algorithm, searching from both ends at once for the middle of
// the script and splitting there (as GNU diff does), so it needs memory linear
// in the sizes of the inputs.
type seqDiff struct {
	x, y   []int
	fd, bd []int  // furthest x reached on each diagonal, searching forward and backward
	off    int    // index of diagonal 0 in fd and bd
	delX   []bool // x[i] is not in the common subsequence
	insY   []bool // y[j] is not in the common subsequence
}

// compare marks the lines of x[xoff:xlim] and y[yoff:ylim] outside a longest
// common subsequence.
func (s *seqDiff) compare(xoff, xlim, yoff, ylim int) {
	for xoff < xlim && yoff < ylim && s.x[xoff] == s.y[yoff] {
		xoff++
		yoff++
	}
	for xoff < xlim && yoff < ylim && s.x[xlim-1] == s.y[ylim-1] {
		xlim--
		ylim--
	}
	switch {
	case xoff == xlim:
		for ; yoff < ylim; yoff++ {
			s.insY[yoff] = true
		}
	case yoff == ylim:
		for ; xoff < xlim; xoff++ {
			s.delX[xoff] = true
		}
	default:
		xmid, ymid := s.middleSnake(xoff, xlim, yoff, ylim)
		s.compare(xoff, xmid, yoff, ymid)
		s.compare(xmid, xlim, ymid, ylim)
	}
}

// middleSnake returns a point (xmid, ymid) on a shortest edit script of
// x[xoff:xlim] and y[yoff:ylim] where the forward search from the top left
// meets the backward search from the bottom right.
func (s *seqDiff) middleSnake(xoff, xlim, yoff, ylim int) (xmid, ymid int) {
	fd, bd, off := s.fd, s.bd, s.off
	dmin, dmax := xoff-ylim, xlim-yoff // valid diagonals
	fmid, bmid := xoff-yoff, xlim-ylim // where the two searches start
	fmin, fmax := fmid, fmid
	bmin, bmax := bmid, bmid
	odd := (fmid-bmid)&1 != 0
	fd[fmid+off] = xoff
	bd[bmid+off] = xlim

	for {
		// Extend the forward search by one edit on each diagonal.
		if fmin > dmin {
			fmin--
			fd[fmin-1+off] = -1
		} else {
			fmin++
		}
		if fmax < dmax {
			fmax++
			fd[fmax+1+off] = -1
		} else {
			fmax--
		}
		for d := fmax; d >= fmin; d -= 2 {
			tlo, thi := fd[d-1+off], fd[d+1+off]
			x0 := tlo + 1
			if tlo < thi {
				x0 = thi
			}
			x, y := x0, x0-d
			for x < xlim && y < ylim && s.x[x] == s.y[y] {
				x++
				y++
			}
			fd[d+off] = x
			if odd && bmin <= d && d <= bmax && bd[d+off] <= x {
				return x, y
			}
		}

		// Extend the backward search the same way.
		if bmin > dmin {
			bmin--
			bd[bmin-1+off] = math.MaxInt
		} else {
			bmin++
		}
		if bmax < dmax {
			bmax++
			bd[bmax+1+off] = math.MaxInt
		} else {
			bmax--
		}
		for d := bmax; d >= bmin; d -= 2 {
			tlo, thi := bd[d-1+off], bd[d+1+off]
			x0 := thi - 1
			if tlo < thi {
				x0 = tlo
			}
			x, y := x0, x0-d
			for xoff < x && yoff < y && s.x[x-1] == s.y[y-1] {
				x--
				y--
			}
			bd[d+off] = x
			if !odd && fmin <= d && d <= fmax && x <= fd[d+off] {
				return x, y
			}
		}
	}
}

// writeHunk writes the hunk for changes, which are close enough to share one,
// with ctx lines of context around them.
func writeHunk(sb *strings.Builder, la, lb []string, changes []change, ctx int) {
	first, last := changes[0], changes[len(changes)-1]
	startA := max(first.a0-ctx, 0)
	endA := min(last.a1+ctx, len(la))
	startB := first.b0 - (first.a0 - startA)
	endB := last.b1 + (endA - last.a1)

	sb.WriteString("@@ -")
	writeRange(sb, startA, endA-startA)
	sb.WriteString(" +")
	writeRange(sb, startB, endB-startB)
	sb.WriteString(" @@\n")

	at := startA // the next line of a not written yet
	for _, c := range changes {
		for _, l := range la[at:c.a0] {
			writeLine(sb, ' ', l)
		}
		for _, l := range la[c.a0:c.a1] {
			writeLine(sb, '-', l)
		}
		for _, l := range lb[c.b0:c.b1] {
			writeLine(sb, '+', l)
		}
		at = c.a1
	}
	for _, l := range la[at:endA] {
		writeLine(sb, ' ', l)
	}
}

// writeRange writes the range of count lines after the first start lines as a
// hunk header does: "N" for one line, "N,M" for several, and for none the number
// of the line before it with ",0".
func writeRange(sb *strings.Builder, start, count int) {
	switch count {
	case 0:
		sb.WriteString(strconv.Itoa(start))
		sb.WriteString(",0")
	case 1:
		sb.WriteString(strconv.Itoa(start + 1))
	default:
		sb.WriteString(strconv.Itoa(start + 1))
		sb.WriteByte(',')
		sb.WriteString(strconv.Itoa(count))
	}
}

// writeLine writes line with its prefix, and the "No newline" marker when the
// line has no "\n" of its own.
func writeLine(sb *strings.Builder, prefix byte, line string) {
	sb.WriteByte(prefix)
	sb.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		sb.WriteString("\n\\ No newline at end of file\n")
	}
}
