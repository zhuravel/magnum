package notes

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const header = "--- a.txt\n+++ b.txt\n"

type unifiedCase struct {
	name string
	a, b string
	ctx  int
	want string
}

func checkUnified(t *testing.T, tests []unifiedCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Unified("a.txt", "b.txt", tc.a, tc.b, tc.ctx)
			if got != tc.want {
				t.Errorf("Unified(%q, %q, ctx=%d)\n got: %q\nwant: %q", tc.a, tc.b, tc.ctx, got, tc.want)
			}
		})
	}
}

// numbered returns the lines "1" to "n".
func numbered(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "%d\n", i)
	}
	return sb.String()
}

// withLine returns text with its line number n (1-based) replaced by repl.
func withLine(text string, n int, repl string) string {
	lines := splitLines(text)
	lines[n-1] = repl + "\n"
	return strings.Join(lines, "")
}

func TestUnifiedReturnsNothingForEqualInputs(t *testing.T) {
	checkUnified(t, []unifiedCase{
		{name: "both empty", a: "", b: "", ctx: 3},
		{name: "same text", a: "a\nb\nc\n", b: "a\nb\nc\n", ctx: 3},
		{name: "same text without a trailing newline", a: "a\nb", b: "a\nb", ctx: 3},
		{name: "same text with zero context", a: "a\n", b: "a\n", ctx: 0},
	})
}

func TestUnifiedShowsAPureInsertion(t *testing.T) {
	checkUnified(t, []unifiedCase{
		{
			name: "in the middle",
			a:    "a\nb\nc\n",
			b:    "a\nb\nX\nc\n",
			ctx:  3,
			want: header + `@@ -1,3 +1,4 @@
 a
 b
+X
 c
`,
		},
		{
			name: "several lines at the start",
			a:    "a\nb\n",
			b:    "X\nY\na\nb\n",
			ctx:  3,
			want: header + `@@ -1,2 +1,4 @@
+X
+Y
 a
 b
`,
		},
		{
			name: "at the end",
			a:    "a\nb\n",
			b:    "a\nb\nX\n",
			ctx:  1,
			want: header + `@@ -2 +2,2 @@
 b
+X
`,
		},
		{
			name: "without context",
			a:    "1\n2\n",
			b:    "1\nX\n2\n",
			ctx:  0,
			want: header + `@@ -1,0 +2 @@
+X
`,
		},
	})
}

func TestUnifiedShowsAPureDeletion(t *testing.T) {
	checkUnified(t, []unifiedCase{
		{
			name: "in the middle",
			a:    "a\nb\nc\nd\n",
			b:    "a\nb\nd\n",
			ctx:  3,
			want: header + `@@ -1,4 +1,3 @@
 a
 b
-c
 d
`,
		},
		{
			name: "several lines at the end",
			a:    "a\nb\nc\nd\n",
			b:    "a\nb\n",
			ctx:  1,
			want: header + `@@ -2,3 +2 @@
 b
-c
-d
`,
		},
		{
			name: "without context",
			a:    "1\n2\n3\n",
			b:    "1\n3\n",
			ctx:  0,
			want: header + `@@ -2 +1,0 @@
-2
`,
		},
	})
}

func TestUnifiedShowsAChangeInTheMiddleWithThreeLinesOfContext(t *testing.T) {
	a := numbered(10)
	checkUnified(t, []unifiedCase{{
		name: "one replaced line",
		a:    a,
		b:    withLine(a, 5, "five"),
		ctx:  3,
		want: header + `@@ -2,7 +2,7 @@
 2
 3
 4
-5
+five
 6
 7
 8
`,
	}, {
		name: "lines replaced by more lines",
		a:    "a\nb\nc\nd\ne\nf\ng\n",
		b:    "a\nb\nc\nX\nY\nZ\ne\nf\ng\n",
		ctx:  3,
		want: header + `@@ -1,7 +1,9 @@
 a
 b
 c
-d
+X
+Y
+Z
 e
 f
 g
`,
	}})
}

func TestUnifiedSplitsChangesFarApartIntoTwoHunks(t *testing.T) {
	a := numbered(20)
	b := withLine(withLine(a, 3, "three"), 18, "eighteen")
	checkUnified(t, []unifiedCase{{
		name: "two replaced lines 14 lines apart",
		a:    a,
		b:    b,
		ctx:  3,
		want: header + `@@ -1,6 +1,6 @@
 1
 2
-3
+three
 4
 5
 6
@@ -15,6 +15,6 @@
 15
 16
 17
-18
+eighteen
 19
 20
`,
	}, {
		name: "without context every change is a hunk of its own",
		a:    "1\n2\n3\n4\n5\n",
		b:    "1\nX\n3\n4\n",
		ctx:  0,
		want: header + `@@ -2 +2 @@
-2
+X
@@ -5 +4,0 @@
-5
`,
	}})
}

func TestUnifiedMergesNearbyChangesIntoOneHunk(t *testing.T) {
	a := numbered(12)
	checkUnified(t, []unifiedCase{{
		name: "two changes four lines apart with three lines of context",
		a:    a,
		b:    withLine(withLine(a, 3, "three"), 8, "eight"),
		ctx:  3,
		want: header + `@@ -1,11 +1,11 @@
 1
 2
-3
+three
 4
 5
 6
 7
-8
+eight
 9
 10
 11
`,
	}, {
		name: "a deletion and an insertion between them",
		a:    "a\nb\nc\nd\ne\nf\n",
		b:    "a\nc\nd\nX\ne\nf\n",
		ctx:  3,
		want: header + `@@ -1,6 +1,6 @@
 a
-b
 c
 d
+X
 e
 f
`,
	}})
}

func TestUnifiedMergesChangesOnlyWhenTheirContextTouches(t *testing.T) {
	a := numbered(10)
	checkUnified(t, []unifiedCase{{
		name: "gap of twice the context merges",
		a:    a,
		b:    withLine(withLine(a, 3, "X3"), 6, "X6"),
		ctx:  1,
		want: header + `@@ -2,6 +2,6 @@
 2
-3
+X3
 4
 5
-6
+X6
 7
`,
	}, {
		name: "gap of twice the context and one splits",
		a:    a,
		b:    withLine(withLine(a, 3, "X3"), 7, "X7"),
		ctx:  1,
		want: header + `@@ -2,3 +2,3 @@
 2
-3
+X3
 4
@@ -6,3 +6,3 @@
 6
-7
+X7
 8
`,
	}})
}

func TestUnifiedTreatsNegativeContextAsZero(t *testing.T) {
	a, b := "1\n2\n3\n", "1\nX\n3\n"
	if got, want := Unified("a.txt", "b.txt", a, b, -2), Unified("a.txt", "b.txt", a, b, 0); got != want {
		t.Errorf("Unified with ctx=-2 = %q, want the result for ctx=0 %q", got, want)
	}
}

func TestUnifiedShowsAnEmptySide(t *testing.T) {
	checkUnified(t, []unifiedCase{
		{
			name: "empty a",
			a:    "",
			b:    "x\ny\n",
			ctx:  3,
			want: header + `@@ -0,0 +1,2 @@
+x
+y
`,
		},
		{
			name: "empty b",
			a:    "x\ny\n",
			b:    "",
			ctx:  3,
			want: header + `@@ -1,2 +0,0 @@
-x
-y
`,
		},
		{
			name: "empty a with one line in b",
			a:    "",
			b:    "x\n",
			ctx:  3,
			want: header + `@@ -0,0 +1 @@
+x
`,
		},
		{
			name: "empty b with one line in a",
			a:    "x\n",
			b:    "",
			ctx:  3,
			want: header + `@@ -1 +0,0 @@
-x
`,
		},
		{
			name: "empty a with zero context",
			a:    "",
			b:    "x\n",
			ctx:  0,
			want: header + `@@ -0,0 +1 @@
+x
`,
		},
	})
}

func TestUnifiedWritesARangeOfOneLineWithoutItsCount(t *testing.T) {
	checkUnified(t, []unifiedCase{{
		name: "one line replaced by one line",
		a:    "a\n",
		b:    "z\n",
		ctx:  3,
		want: header + `@@ -1 +1 @@
-a
+z
`,
	}, {
		name: "one line replaced by two",
		a:    "a\n",
		b:    "z\ny\n",
		ctx:  3,
		want: header + `@@ -1 +1,2 @@
-a
+z
+y
`,
	}})
}

func TestUnifiedMarksALineWithoutTrailingNewline(t *testing.T) {
	checkUnified(t, []unifiedCase{
		{
			name: "b loses the newline of its last line",
			a:    "a\nb\n",
			b:    "a\nb",
			ctx:  3,
			want: header + `@@ -1,2 +1,2 @@
 a
-b
+b
\ No newline at end of file
`,
		},
		{
			name: "b gains the newline of its last line",
			a:    "a\nb",
			b:    "a\nb\n",
			ctx:  3,
			want: header + `@@ -1,2 +1,2 @@
 a
-b
\ No newline at end of file
+b
`,
		},
		{
			name: "neither side has it and the last lines differ",
			a:    "a\nb",
			b:    "a\nc",
			ctx:  3,
			want: header + `@@ -1,2 +1,2 @@
 a
-b
\ No newline at end of file
+c
\ No newline at end of file
`,
		},
		{
			name: "neither side has it and the last line is context",
			a:    "a\nb\nc",
			b:    "a\nX\nc",
			ctx:  3,
			want: header + `@@ -1,3 +1,3 @@
 a
-b
+X
 c
\ No newline at end of file
`,
		},
		{
			name: "a single line gains lines after it",
			a:    "a",
			b:    "a\nb\n",
			ctx:  3,
			want: header + `@@ -1 +1,2 @@
-a
\ No newline at end of file
+a
+b
`,
		},
	})
}

func TestUnifiedKeepsCarriageReturnsInTheirLines(t *testing.T) {
	checkUnified(t, []unifiedCase{{
		name: "a line ending changes from CRLF to LF",
		a:    "a\r\nb\r\n",
		b:    "a\nb\r\n",
		ctx:  3,
		want: header + "@@ -1,2 +1,2 @@\n-a\r\n+a\n b\r\n",
	}, {
		name: "a CRLF file is compared line by line",
		a:    "a\r\nb\r\nc\r\n",
		b:    "a\r\nB\r\nc\r\n",
		ctx:  1,
		want: header + "@@ -1,3 +1,3 @@\n a\r\n-b\r\n+B\r\n c\r\n",
	}})
}

func TestUnifiedNamesBothFilesInItsHeader(t *testing.T) {
	got := Unified("old/notes.md", "new/notes.md", "a\n", "b\n", 3)
	want := "--- old/notes.md\n+++ new/notes.md\n@@ -1 +1 @@\n-a\n+b\n"
	if got != want {
		t.Errorf("Unified = %q, want %q", got, want)
	}
}

func TestLineChangesCountsAddedAndRemovedLines(t *testing.T) {
	tests := []struct {
		name                   string
		a, b                   string
		wantAdded, wantRemoved int
	}{
		{name: "equal inputs", a: "a\nb\n", b: "a\nb\n"},
		{name: "both empty"},
		{name: "a replaced line counts once in each", a: "a\nb\nc\n", b: "a\nX\nc\n", wantAdded: 1, wantRemoved: 1},
		{name: "an insertion", a: "a\nc\n", b: "a\nb\nc\n", wantAdded: 1},
		{name: "several inserted lines", a: "a\n", b: "a\nb\nc\nd\n", wantAdded: 3},
		{name: "a deletion", a: "a\nb\nc\n", b: "a\nc\n", wantRemoved: 1},
		{name: "several deleted lines", a: "a\nb\nc\nd\n", b: "a\n", wantRemoved: 3},
		{name: "empty a", a: "", b: "a\nb\n", wantAdded: 2},
		{name: "empty b", a: "a\nb\n", b: "", wantRemoved: 2},
		{name: "a moved line is removed and added", a: "a\nb\nc\n", b: "b\nc\na\n", wantAdded: 1, wantRemoved: 1},
		{name: "a lost trailing newline changes the last line", a: "a\nb\n", b: "a\nb", wantAdded: 1, wantRemoved: 1},
		{name: "a changed line ending changes the line", a: "a\r\n", b: "a\n", wantAdded: 1, wantRemoved: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			added, removed := LineChanges(tc.a, tc.b)
			if added != tc.wantAdded || removed != tc.wantRemoved {
				t.Errorf("LineChanges(%q, %q) = (%d, %d), want (%d, %d)", tc.a, tc.b, added, removed, tc.wantAdded, tc.wantRemoved)
			}
		})
	}
}

// randomText returns up to maxLines lines drawn from alphabet letters, with the
// last line missing its newline one time in four.
func randomText(rng *rand.Rand, maxLines, alphabet int) string {
	n := rng.IntN(maxLines + 1)
	var sb strings.Builder
	for i := range n {
		sb.WriteByte(byte('a' + rng.IntN(alphabet)))
		if i < n-1 || rng.IntN(4) != 0 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// lcsLength is the length of a longest common subsequence of a and b.
func lcsLength(a, b []string) int {
	prev, cur := make([]int, len(b)+1), make([]int, len(b)+1)
	for i := range a {
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(prev[j+1], cur[j])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@\n$`)

// applyUnified applies diff, made by Unified, to a and returns the result. It
// fails t when a hunk's header disagrees with its lines or its place, when its
// context or removed lines are not the lines of a, or when two hunks touch.
func applyUnified(t *testing.T, a, diff string) string {
	t.Helper()
	la := splitLines(a)
	lines := splitLines(diff)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "--- ") || !strings.HasPrefix(lines[1], "+++ ") {
		t.Fatalf("diff has no header: %q", diff)
	}
	lines = lines[2:]

	var out []string
	next := 0     // the next line of a not copied or consumed yet
	prevEnd := -1 // where the last hunk ended in a
	for len(lines) > 0 {
		m := hunkHeader.FindStringSubmatch(lines[0])
		if m == nil {
			t.Fatalf("want a hunk header, got %q in %q", lines[0], diff)
		}
		lines = lines[1:]
		number := func(s string, def int) int {
			if s == "" {
				return def
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		oldStart, oldCount := number(m[1], 0), number(m[2], 1)
		newStart, newCount := number(m[3], 0), number(m[4], 1)
		if oldCount > 0 { // a range of no lines is numbered after the line before it
			oldStart--
		}
		if newCount > 0 {
			newStart--
		}
		if oldStart <= prevEnd {
			t.Fatalf("hunk after old line %d touches the one before it, which ended at old line %d: %q", oldStart, prevEnd, diff)
		}
		out = append(out, la[next:oldStart]...)
		next = oldStart
		if newStart != len(out) {
			t.Fatalf("hunk header says new line %d, but %d lines precede it: %q", newStart+1, len(out), diff)
		}

		var gotOld, gotNew int
		for len(lines) > 0 && !strings.HasPrefix(lines[0], "@@") {
			text := lines[0][1:]
			prefix := lines[0][0]
			lines = lines[1:]
			if len(lines) > 0 && lines[0] == "\\ No newline at end of file\n" {
				text = strings.TrimSuffix(text, "\n")
				lines = lines[1:]
			}
			switch prefix {
			case ' ', '-':
				if next >= len(la) || la[next] != text {
					t.Fatalf("line %q at old line %d is not a's line: %q", text, next+1, diff)
				}
				next++
				gotOld++
				if prefix == ' ' {
					out = append(out, text)
					gotNew++
				}
			case '+':
				out = append(out, text)
				gotNew++
			default:
				t.Fatalf("line %q has no prefix: %q", text, diff)
			}
		}
		if gotOld != oldCount || gotNew != newCount {
			t.Fatalf("hunk header counts (%d, %d), lines (%d, %d): %q", oldCount, newCount, gotOld, gotNew, diff)
		}
		prevEnd = next
	}
	out = append(out, la[next:]...)
	return strings.Join(out, "")
}

func TestUnifiedPatchesAIntoBForRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 2000 {
		a, b := randomText(rng, 25, 2+rng.IntN(5)), randomText(rng, 25, 2+rng.IntN(5))
		ctx := rng.IntN(5)
		diff := Unified("a", "b", a, b, ctx)
		if (a == b) != (diff == "") {
			t.Fatalf("case %d: Unified(%q, %q, %d) = %q, want empty exactly for equal inputs", i, a, b, ctx, diff)
		}
		if a == b {
			continue
		}
		if got := applyUnified(t, a, diff); got != b {
			t.Fatalf("case %d: applying the diff of %q and %q (ctx %d) gives %q\n%s", i, a, b, ctx, got, diff)
		}
	}
}

func TestLineChangesIsMinimalForRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range 2000 {
		a, b := randomText(rng, 30, 2+rng.IntN(5)), randomText(rng, 30, 2+rng.IntN(5))
		la, lb := splitLines(a), splitLines(b)
		common := lcsLength(la, lb)
		wantAdded, wantRemoved := len(lb)-common, len(la)-common
		added, removed := LineChanges(a, b)
		if added != wantAdded || removed != wantRemoved {
			t.Fatalf("case %d: LineChanges(%q, %q) = (%d, %d), want (%d, %d)", i, a, b, added, removed, wantAdded, wantRemoved)
		}

		// Unified shows the same lines as changed.
		if a == b {
			continue
		}
		var plus, minus int
		for _, l := range splitLines(Unified("a", "b", a, b, 2))[2:] {
			switch l[0] {
			case '+':
				plus++
			case '-':
				minus++
			}
		}
		if plus != wantAdded || minus != wantRemoved {
			t.Fatalf("case %d: Unified(%q, %q) shows (+%d, -%d), want (+%d, -%d)", i, a, b, plus, minus, wantAdded, wantRemoved)
		}
	}
}

func TestLineChangesStaysMinimalForLargeInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	line := func() string { return string(byte('a'+rng.IntN(4))) + "\n" }
	la, lb := make([]string, 3000), make([]string, 3000)
	for i := range la {
		la[i], lb[i] = line(), line()
	}
	// Few changes in a text that repeats a lot, and almost nothing in common.
	lc := slices.Clone(la)
	for range 40 {
		lc[rng.IntN(len(lc))] = line()
	}
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{name: "a few replaced lines", a: la, b: lc},
		{name: "unrelated texts with four distinct lines", a: la, b: lb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common := lcsLength(tc.a, tc.b)
			added, removed := LineChanges(strings.Join(tc.a, ""), strings.Join(tc.b, ""))
			if added != len(tc.b)-common || removed != len(tc.a)-common {
				t.Errorf("LineChanges = (%d, %d), want (%d, %d)", added, removed, len(tc.b)-common, len(tc.a)-common)
			}
		})
	}
}

func TestLineChangesFindsNothingInCommonInDisjointLargeInputs(t *testing.T) {
	var a, b strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	if added, removed := LineChanges(a.String(), b.String()); added != 4000 || removed != 4000 {
		t.Errorf("LineChanges = (%d, %d), want (4000, 4000)", added, removed)
	}
}
