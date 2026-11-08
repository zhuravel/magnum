package escapes

import (
	"slices"
	"testing"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// at is a finding on path, lines [from, to], at head, known by ref.
func at(src Source, ref, path string, from, to int, head string) Finding {
	return Finding{Source: src, Ref: ref, Path: path, StartLine: from, EndLine: to, Priority: "P2", Head: head}
}

func local(ref, path string, from, to int) Finding {
	return at(SourceLocal, ref, path, from, to, headA)
}

func other(ref, path string, from, to int) Finding {
	return at(SourceComment, ref, path, from, to, headA)
}

// escaped is what a test expects of one Escape: the other finding's ref,
// the reason and, when far, the nearest local's ref and the distance.
type escaped struct {
	ref      string
	reason   Reason
	nearest  string
	distance int
}

func summary(es []Escape) []escaped {
	out := []escaped{}
	for _, e := range es {
		out = append(out, escaped{ref: e.Finding.Ref, reason: e.Reason, nearest: e.Nearest.Ref, distance: e.Distance})
	}
	return out
}

func TestMatchCatchesAFindingOnlyOnTheSamePathWithinThreeLines(t *testing.T) {
	cases := []struct {
		name  string
		local []Finding
		other []Finding
		opts  []Option
		want  []escaped
	}{
		{name: "the same line is caught",
			local: []Finding{local("L1", "app/a.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			want:  []escaped{}},
		{name: "overlapping ranges are distance 0",
			local: []Finding{local("L1", "app/a.rb", 10, 20)},
			other: []Finding{other("C1", "app/a.rb", 18, 30)},
			want:  []escaped{}},
		{name: "a range inside a range is caught",
			local: []Finding{local("L1", "app/a.rb", 5, 40)},
			other: []Finding{other("C1", "app/a.rb", 12, 14)},
			want:  []escaped{}},
		{name: "3 lines below is caught",
			local: []Finding{local("L1", "app/a.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 13, 13)},
			want:  []escaped{}},
		{name: "3 lines above is caught",
			local: []Finding{local("L1", "app/a.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 7, 7)},
			want:  []escaped{}},
		{name: "4 lines below is not",
			local: []Finding{local("L1", "app/a.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 14, 14)},
			want:  []escaped{{"C1", ReasonFar, "L1", 4}}},
		{name: "4 lines above is not",
			local: []Finding{local("L1", "app/a.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 6, 6)},
			want:  []escaped{{"C1", ReasonFar, "L1", 4}}},
		{name: "ranges count from their nearest ends: 3 caught",
			local: []Finding{local("L1", "app/a.rb", 1, 10)},
			other: []Finding{other("C1", "app/a.rb", 13, 20)},
			want:  []escaped{}},
		{name: "ranges count from their nearest ends: 4 not",
			local: []Finding{local("L1", "app/a.rb", 1, 10)},
			other: []Finding{other("C1", "app/a.rb", 14, 20)},
			want:  []escaped{{"C1", ReasonFar, "L1", 4}}},
		{name: "a range given backwards is the same range",
			local: []Finding{local("L1", "app/a.rb", 20, 10)},
			other: []Finding{other("C1", "app/a.rb", 22, 22)},
			want:  []escaped{}},
		{name: "a finding with only a line (StartLine 0) is that line",
			local: []Finding{local("L1", "app/a.rb", 0, 10)},
			other: []Finding{other("C1", "app/a.rb", 0, 14), other("C2", "app/a.rb", 0, 13)},
			want:  []escaped{{"C1", ReasonFar, "L1", 4}}},
		{name: "another path is no local finding on the path",
			local: []Finding{local("L1", "app/b.rb", 10, 10)},
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
		{name: "no local finding at all",
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
		{name: "an empty path cannot be placed",
			local: []Finding{local("L1", "", 10, 10)},
			other: []Finding{other("C1", "", 10, 10)},
			want:  []escaped{{"C1", ReasonUnplaced, "", 0}}},
		{name: "line 0 cannot be placed, even beside a local finding of no line",
			local: []Finding{local("L1", "app/a.rb", 0, 0)},
			other: []Finding{other("C1", "app/a.rb", 0, 0)},
			want:  []escaped{{"C1", ReasonUnplaced, "", 0}}},
		{name: "a local finding without a line catches nothing",
			local: []Finding{local("L1", "app/a.rb", 0, 0)},
			other: []Finding{other("C1", "app/a.rb", 1, 1)},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
		{name: "of many locals on the path the nearest is named, the first on a tie",
			local: []Finding{local("L1", "app/a.rb", 100, 100), local("L2", "app/a.rb", 30, 30),
				local("L3", "app/a.rb", 10, 10), local("L4", "app/a.rb", 50, 50)},
			other: []Finding{other("C1", "app/a.rb", 20, 20), other("C2", "app/a.rb", 75, 75)},
			want:  []escaped{{"C1", ReasonFar, "L2", 10}, {"C2", ReasonFar, "L1", 25}}},
		{name: "one of many locals near enough catches it",
			local: []Finding{local("L1", "app/a.rb", 100, 100), local("L2", "app/a.rb", 22, 22)},
			other: []Finding{other("C1", "app/a.rb", 20, 20)},
			want:  []escaped{}},
		{name: "one local catches several",
			local: []Finding{local("L1", "app/a.rb", 10, 12)},
			other: []Finding{other("C1", "app/a.rb", 8, 8), other("C2", "app/a.rb", 11, 11), other("C3", "app/a.rb", 15, 15),
				other("C4", "app/a.rb", 16, 16)},
			want: []escaped{{"C4", ReasonFar, "L1", 4}}},
		{name: "priority never filters",
			local: []Finding{{Source: SourceLocal, Ref: "L1", Path: "app/a.rb", StartLine: 10, EndLine: 10, Priority: "P3", Head: headA}},
			other: []Finding{{Source: SourcePosted, Ref: "P1", Path: "app/a.rb", StartLine: 10, EndLine: 10, Priority: "P0", Head: headA},
				{Source: SourcePosted, Ref: "P2", Path: "app/b.rb", StartLine: 1, EndLine: 1, Priority: "P3", Head: headA}},
			want: []escaped{{"P2", ReasonNoLocal, "", 0}}},
		{name: "the escapes keep other's order",
			other: []Finding{other("C3", "app/c.rb", 1, 1), other("C1", "", 0, 0), other("C2", "app/a.rb", 1, 1)},
			want:  []escaped{{"C3", ReasonNoLocal, "", 0}, {"C1", ReasonUnplaced, "", 0}, {"C2", ReasonNoLocal, "", 0}}},
		{name: "a local finding at another head is not compared",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, headB)},
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
		{name: "one known head and one unknown are not compared",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, "")},
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			opts:  []Option{SameCode(func(string, string, string) bool { return true })},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
		{name: "two unknown heads are compared",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, "")},
			other: []Finding{at(SourceComment, "C1", "app/a.rb", 11, 11, "")},
			want:  []escaped{}},
		{name: "the nearest is named only among comparable heads",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, headB), local("L2", "app/a.rb", 40, 40)},
			other: []Finding{other("C1", "app/a.rb", 10, 10)},
			want:  []escaped{{"C1", ReasonFar, "L2", 30}}},
		{name: "SameCode compares a local finding at another head when the file did not change",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, headB)},
			other: []Finding{other("C1", "app/a.rb", 12, 12)},
			opts: []Option{SameCode(func(path, localHead, otherHead string) bool {
				return path == "app/a.rb" && localHead == headB && otherHead == headA
			})},
			want: []escaped{}},
		{name: "SameCode answering no keeps the heads apart",
			local: []Finding{at(SourceLocal, "L1", "app/a.rb", 10, 10, headB)},
			other: []Finding{other("C1", "app/a.rb", 12, 12)},
			opts:  []Option{SameCode(func(string, string, string) bool { return false })},
			want:  []escaped{{"C1", ReasonNoLocal, "", 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summary(Match(tc.local, tc.other, tc.opts...))
			if !slices.Equal(got, tc.want) {
				t.Errorf("Match = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An escape carries the other finding whole, and a far one its nearest
// local finding whole, so the caller finds both rows again by Ref.
func TestAnEscapeCarriesTheOtherFindingAndItsNearestLocalOne(t *testing.T) {
	l := Finding{Source: SourceLocal, Ref: "7/31", Path: "app/a.rb", StartLine: 3, EndLine: 5, Priority: "P2", TitleHash: "0123456789ab",
		Round: 2, Head: headA}
	o := Finding{Source: SourcePosted, Ref: "run-1/F2", Path: "app/a.rb", StartLine: 20, EndLine: 20, Priority: "P1",
		Title: "The lookup drops the tenant scope", Round: 1, Head: headA}
	got := Match([]Finding{l}, []Finding{o})
	if len(got) != 1 {
		t.Fatalf("Match = %+v, want one escape", got)
	}
	if e := got[0]; e.Finding != o || e.Reason != ReasonFar || e.Nearest != l || e.Distance != 15 {
		t.Errorf("escape = %+v, want %+v far from %+v by 15", e, o, l)
	}
	if got := Match(nil, []Finding{o}); len(got) != 1 || got[0].Nearest != (Finding{}) || got[0].Distance != 0 {
		t.Errorf("an escape with no local finding names a nearest one: %+v", got)
	}
}

// Match asks SameCode only for two set, different heads, with the local
// finding's head first.
func TestMatchAsksSameCodeOnlyForTwoDifferentKnownHeads(t *testing.T) {
	var asked [][3]string
	same := SameCode(func(path, localHead, otherHead string) bool {
		asked = append(asked, [3]string{path, localHead, otherHead})
		return false
	})
	Match([]Finding{
		at(SourceLocal, "L1", "app/a.rb", 1, 1, headA),
		at(SourceLocal, "L2", "app/a.rb", 1, 1, ""),
		at(SourceLocal, "L3", "app/a.rb", 1, 1, headB),
		at(SourceLocal, "L4", "app/b.rb", 1, 1, headB),
	}, []Finding{at(SourceComment, "C1", "app/a.rb", 50, 50, headA)}, same)
	want := [][3]string{{"app/a.rb", headB, headA}}
	if !slices.Equal(asked, want) {
		t.Errorf("SameCode asked %v, want %v", asked, want)
	}
}

func TestDistanceIsTheLinesBetweenTwoRanges(t *testing.T) {
	cases := []struct {
		a, b Finding
		want int
		ok   bool
	}{
		{Finding{StartLine: 10, EndLine: 10}, Finding{StartLine: 10, EndLine: 10}, 0, true},
		{Finding{StartLine: 10, EndLine: 20}, Finding{StartLine: 20, EndLine: 25}, 0, true},
		{Finding{StartLine: 10, EndLine: 20}, Finding{StartLine: 23, EndLine: 25}, 3, true},
		{Finding{StartLine: 23, EndLine: 25}, Finding{StartLine: 10, EndLine: 20}, 3, true},
		{Finding{EndLine: 7}, Finding{StartLine: 1, EndLine: 3}, 4, true},
		{Finding{StartLine: 9, EndLine: 4}, Finding{StartLine: 1, EndLine: 1}, 3, true},
		{Finding{Path: "a.rb", StartLine: 1, EndLine: 1}, Finding{Path: "b.rb", StartLine: 1, EndLine: 1}, 0, true},
		{Finding{StartLine: 5}, Finding{StartLine: 1, EndLine: 1}, 0, false},
		{Finding{StartLine: 1, EndLine: 1}, Finding{}, 0, false},
	}
	for _, tc := range cases {
		got, ok := Distance(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Distance(%+v, %+v) = %d, %v, want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLinesOrdersARangeAndTakesALoneLineForBothEnds(t *testing.T) {
	cases := []struct {
		f        Finding
		from, to int
		ok       bool
	}{
		{Finding{StartLine: 3, EndLine: 9}, 3, 9, true},
		{Finding{StartLine: 9, EndLine: 3}, 3, 9, true},
		{Finding{EndLine: 4}, 4, 4, true},
		{Finding{StartLine: -2, EndLine: 4}, 4, 4, true},
		{Finding{StartLine: 4}, 0, 0, false},
		{Finding{}, 0, 0, false},
	}
	for _, tc := range cases {
		from, to, ok := tc.f.Lines()
		if from != tc.from || to != tc.to || ok != tc.ok {
			t.Errorf("%+v.Lines() = %d, %d, %v, want %d, %d, %v", tc.f, from, to, ok, tc.from, tc.to, tc.ok)
		}
	}
}
