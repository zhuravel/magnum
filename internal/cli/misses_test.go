package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/store"
)

// missSeed stores m (SourceKind, Reviewer and ReviewedSHA default to the
// usual values) and returns the stored row.
func missSeed(t *testing.T, st *store.Store, pr store.PR, m store.Miss) store.Miss {
	t.Helper()
	m.PRID = pr.ID
	if m.SourceKind == "" {
		m.SourceKind = store.MissSourceThread
	}
	if m.Reviewer == "" {
		m.Reviewer = "alice"
	}
	m.ReviewedSHA = "abcdef0123456789abcdef0123456789abcdef01"
	got, err := st.UpsertMiss(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// missesFixture seeds two closed PRs with misses of every class and state:
//
//	talkable#101: "Coupon never expires" (miss, new, rejected by the judge), "Old miss" (miss, used),
//	              "Rename it" (style, new), "Not a bug" (not_issue, dismissed)
//	example/widgets#7: "Missing guard" (miss, new, a review summary), "Unsorted" (unclassified, new)
func missesFixture(t *testing.T) *inspFixture {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	closed := func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) }
	_, a := inspSeedPR(t, st, "talkable/talkable", 101, store.PRReleased, closed)
	_, b := inspSeedPR(t, st, "example/widgets", 7, store.PRReleased, closed)
	missSeed(t, st, a, store.Miss{SourceURL: "https://example.com/c/1", Path: "app/models/coupon.rb", Line: 42, Class: store.MissMiss,
		Severity: "P1", Raised: store.MissRaisedRejected, ReasonCode: "low_confidence", Title: "Coupon never expires",
		Lesson: "When a coupon has an end date, check that lookups filter on it because expired codes still redeem."})
	missSeed(t, st, a, store.Miss{SourceURL: "https://example.com/c/2", Path: "lib/a.rb", Line: 3, Class: store.MissMiss, Severity: "P2",
		Title: "Old miss", Lesson: "A used lesson.", State: store.MissUsed})
	missSeed(t, st, a, store.Miss{SourceURL: "https://example.com/c/3", Path: "lib/b.rb", Line: 9, Class: store.MissStyle, Title: "Rename it"})
	missSeed(t, st, a, store.Miss{SourceURL: "https://example.com/c/4", Path: "lib/c.rb", Class: store.MissNotIssue, Title: "Not a bug",
		State: store.MissDismissed})
	missSeed(t, st, b, store.Miss{SourceURL: "https://example.com/c/5", SourceKind: store.MissSourceReview, Reviewer: "rev-ann", Class: store.MissMiss,
		Severity: "P2", Title: "Missing guard", Lesson: "When a handler takes an id, check the caller may read it."})
	missSeed(t, st, b, store.Miss{SourceURL: "https://example.com/c/6", Reviewer: "bob-rev", Path: "x.rb", Line: 1, Title: "Unsorted"})
	st.Close()
	return f
}

// missesTitles are the titles the text output lists, in order.
func missesTitles(out string) []string {
	var titles []string
	for _, title := range []string{"Coupon never expires", "Old miss", "Rename it", "Not a bug", "Missing guard", "Unsorted"} {
		if strings.Contains(out, title) {
			titles = append(titles, title)
		}
	}
	return titles
}

func missesRun(t *testing.T, f *inspFixture, args ...string) string {
	t.Helper()
	if code := f.run("misses", args...); code != 0 {
		t.Fatalf("misses %v: exit %d: %s", args, code, f.Err.String())
	}
	return f.Out.String()
}

func TestMissesDefaultShowsOnlyNewMissesOfClassMiss(t *testing.T) {
	f := missesFixture(t)
	out := missesRun(t, f)
	if got, want := missesTitles(out), []string{"Coupon never expires", "Missing guard"}; !slices.Equal(got, want) {
		t.Fatalf("titles = %v, want %v:\n%s", got, want, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if got := norm(lines[0]); got != "PR REVIEWER WHERE CLASS SEV RAISED TITLE LESSON" {
		t.Errorf("header = %q", got)
	}
	// Newest first; a PR of the default repository's owner is name#N, another owner's is owner/name#N.
	if got, want := norm(lines[2]), "talkable#101 alice app/models/coupon.rb:42 miss P1 rejected:low_confidence Coupon never expires When a coupon has an end date, check that lookups filter on it because expired codes still redeem."; got != want {
		t.Errorf("row = %q\nwant  %q", got, want)
	}
	if got, want := norm(lines[1]), "example/widgets#7 rev-ann - miss P2 Missing guard When a handler takes an id, check the caller may read it."; got != want {
		t.Errorf("review-summary row = %q\nwant           %q", got, want)
	}
}

func TestMissesAllListsEveryClassAndState(t *testing.T) {
	f := missesFixture(t)
	out := missesRun(t, f, "--all")
	if got := missesTitles(out); len(got) != 6 {
		t.Fatalf("titles = %v:\n%s", got, out)
	}
}

func TestMissesClassFiltersAndAllLiftsTheState(t *testing.T) {
	f := missesFixture(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--class", "style"}, []string{"Rename it"}},
		{[]string{"--class", "miss", "--all"}, []string{"Coupon never expires", "Old miss", "Missing guard"}},
		{[]string{"--class", "not_issue", "--all"}, []string{"Not a bug"}},
		{[]string{"--class", "unclassified"}, []string{"Unsorted"}},
		{[]string{"--class", "Style"}, []string{"Rename it"}},
	} {
		if got := missesTitles(missesRun(t, f, tc.args...)); !slices.Equal(got, tc.want) {
			t.Errorf("misses %v = %v, want %v", tc.args, got, tc.want)
		}
	}
	// A dismissed not_issue is not new: nothing, and the message says what was looked for.
	out := missesRun(t, f, "--class", "not_issue")
	actContains(t, out, "no new not_issue comments yet", "--all")
}

func TestMissesRefRestrictsToOnePR(t *testing.T) {
	f := missesFixture(t)
	if got := missesTitles(missesRun(t, f, "talkable#101")); !slices.Equal(got, []string{"Coupon never expires"}) {
		t.Errorf("talkable#101 = %v", got)
	}
	if got := missesTitles(missesRun(t, f, "101", "--all")); !slices.Equal(got, []string{"Coupon never expires", "Old miss", "Rename it", "Not a bug"}) {
		t.Errorf("101 --all = %v", got)
	}
	if got := missesTitles(missesRun(t, f, "example/widgets#7")); !slices.Equal(got, []string{"Missing guard"}) {
		t.Errorf("example/widgets#7 = %v", got)
	}
	// A PR without misses is an empty list, not an error.
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 102, store.PRReleased, nil)
	st.Close()
	actContains(t, missesRun(t, f, "102"), "no new misses yet")
}

func TestMissesUnknownRefAndBadFlagsFail(t *testing.T) {
	f := missesFixture(t)
	if code := f.run("misses", "talkable#999"); code != 1 || !strings.Contains(f.Err.String(), "talkable#999 is not in the registry") {
		t.Errorf("unknown ref: exit %d: %s", code, f.Err.String())
	}
	if code := f.run("misses", "--class", "bug"); code != 2 {
		t.Errorf("bad class: exit %d", code)
	}
	actContains(t, f.Err.String(), `--class "bug"`, "usage: magnum misses [<ref>]")
	if code := f.run("misses", "101", "102"); code != 2 {
		t.Errorf("two refs: exit %d", code)
	}
}

func TestMissesRaisedColumnShowsTheRejection(t *testing.T) {
	f := missesFixture(t)
	st := f.store()
	_, c := inspSeedPR(t, st, "talkable/talkable", 103, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHClosed) })
	missSeed(t, st, c, store.Miss{SourceURL: "https://example.com/c/7", Class: store.MissMiss, Severity: "P3", Raised: store.MissRaisedRejected, Title: "No reason"})
	st.Close()
	out := missesRun(t, f)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var raised []string
	for _, l := range lines[1:] {
		fields := strings.Fields(l)
		raised = append(raised, strings.Join(fields[4:6], " "))
	}
	// Rows are newest first: no-reason (rejected without a code), the summary (never raised), the coupon.
	want := []string{"P3 rejected", "P2 Missing", "P1 rejected:low_confidence"}
	if !slices.Equal(raised, want) {
		t.Errorf("SEV/RAISED cells = %q, want %q\n%s", raised, want, out)
	}
}

func TestMissesCleansUntrustedText(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 101, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	noise := "\x1b[31mred\x1b]0;pwned\a\x1b[0m\r\nnext\ttab"
	missSeed(t, st, pr, store.Miss{SourceURL: "https://example.com/c/1", Reviewer: "ev" + noise, Path: "a/" + noise + ".rb", Line: 4,
		Class: store.MissMiss, Severity: "P1", Raised: store.MissRaisedRejected, ReasonCode: "r" + noise, Title: "t" + noise, Lesson: "l" + noise})
	st.Close()
	out := missesRun(t, f)
	statusNoControls(t, "misses", out)
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 {
		t.Errorf("untrusted text broke the table into %d lines:\n%q", len(lines), out)
	}
	actContains(t, out, "red", "pwned", "next tab")
}

func TestMissesLessonIsCutToFitTheOutput(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 101, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	long := strings.Repeat("word ", 80)
	missSeed(t, st, pr, store.Miss{SourceURL: "https://example.com/c/1", Path: "a.rb", Line: 1, Class: store.MissMiss, Severity: "P2", Title: "Title", Lesson: long})
	st.Close()
	lastCell := func(out string) string {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return lines[len(lines)-1]
	}

	// Not a terminal: the lesson is cut at 120 runes.
	out := missesRun(t, f)
	if i := strings.Index(lastCell(out), "word"); i < 0 || utf8.RuneCountInString(lastCell(out)[i:]) != missesLessonRunes || !strings.HasSuffix(lastCell(out), "…") {
		t.Errorf("non-terminal lesson is not %d runes ending in an ellipsis:\n%s", missesLessonRunes, out)
	}

	// A terminal: every line fits its width.
	prev := missesTermWidth
	t.Cleanup(func() { missesTermWidth = prev })
	for _, width := range []int{100, 120, 160} {
		missesTermWidth = func(io.Writer) int { return width }
		out := missesRun(t, f)
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if n := utf8.RuneCountInString(l); n > width {
				t.Errorf("width %d: a line is %d runes: %q", width, n, l)
			}
		}
		if !strings.HasSuffix(lastCell(out), "…") {
			t.Errorf("width %d: the lesson is not cut:\n%s", width, out)
		}
	}
	// A terminal too narrow for the other columns still keeps part of the lesson.
	missesTermWidth = func(io.Writer) int { return 20 }
	out = missesRun(t, f)
	if i := strings.Index(lastCell(out), "word"); i < 0 || utf8.RuneCountInString(lastCell(out)[i:]) != missesMinLesson {
		t.Errorf("narrow terminal: want a %d-rune lesson:\n%s", missesMinLesson, out)
	}
	// A short lesson is never padded or cut.
	st = f.store()
	_, pr2 := inspSeedPR(t, st, "talkable/talkable", 102, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	missSeed(t, st, pr2, store.Miss{SourceURL: "https://example.com/c/2", Class: store.MissMiss, Title: "Short", Lesson: "Check it."})
	st.Close()
	missesTermWidth = func(io.Writer) int { return 100 }
	if !strings.HasSuffix(strings.TrimSpace(missesRun(t, f, "102")), "Check it.") {
		t.Errorf("short lesson altered:\n%s", f.Out.String())
	}
}

func TestMissesLongPathKeepsItsTail(t *testing.T) {
	m := store.Miss{Path: "app/" + strings.Repeat("deep/", 20) + "coupon_exporter.rb", Line: 12}
	got := missesWhere(m)
	if utf8.RuneCountInString(got) != missesWhereRunes || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "coupon_exporter.rb:12") {
		t.Errorf("where = %q", got)
	}
	if got := missesWhere(store.Miss{Path: "a.rb"}); got != "a.rb" {
		t.Errorf("a path without a line = %q", got)
	}
	if got := missesWhere(store.Miss{}); got != "-" {
		t.Errorf("a review summary = %q", got)
	}
}

func TestMissesJSONPrintsTheListAndAnEmptyOne(t *testing.T) {
	f := missesFixture(t)
	var ms []store.Miss
	if err := json.Unmarshal([]byte(missesRun(t, f, "--json")), &ms); err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Title != "Missing guard" || ms[1].Title != "Coupon never expires" {
		t.Fatalf("default list = %+v", ms)
	}
	if ms[1].Repo != "talkable/talkable" || ms[1].Number != 101 || ms[1].Raised != store.MissRaisedRejected || ms[1].ReasonCode != "low_confidence" || ms[1].Line != 42 {
		t.Errorf("miss = %+v", ms[1])
	}
	if err := json.Unmarshal([]byte(missesRun(t, f, "--json", "--all")), &ms); err != nil || len(ms) != 6 {
		t.Fatalf("--all list: %d (%v)", len(ms), err)
	}
	if got := strings.TrimSpace(missesRun(t, f, "--json", "--class", "not_issue")); got != "[]" {
		t.Errorf("empty list = %q", got)
	}
}

func TestMissesNoneYetExplainsHowToGetSome(t *testing.T) {
	f := newInspFixture(t)
	f.store().Close()
	out := missesRun(t, f)
	actContains(t, out, "no new misses yet", "`magnum retro` runs a retro now", "[learn] enabled = true schedules one daily")
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("the explanation is not one line:\n%s", out)
	}
	actContains(t, missesRun(t, f, "--all"), "no misses recorded yet")
}

func TestMissesTermWidthIsZeroOffATerminal(t *testing.T) {
	if got := missesTermWidth(&strings.Builder{}); got != 0 {
		t.Errorf("a buffer has width %d", got)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	t.Setenv("COLUMNS", "99")
	if got := missesTermWidth(null); got != 0 {
		t.Errorf("/dev/null has width %d", got)
	}
}

func TestMissesAndRetroCompleteClosedPRsAndClasses(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, nil)
	inspSeedPR(t, st, "talkable/talkable", 11000, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	inspSeedPR(t, st, "zhuravel/app", 3, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHClosed) })
	st.Close()

	for _, cmd := range []string{"misses", "retro"} {
		got := strings.Join(complete(t, f.Ctx, cmd, ""), "\n")
		if !strings.Contains(got, "talkable/talkable#11000\tPR xxx") || !strings.Contains(got, "zhuravel/app#3\t") || strings.Contains(got, "11920") {
			t.Errorf("%s completions = %q (want the closed PRs only)", cmd, got)
		}
	}
	if got := strings.Join(complete(t, f.Ctx, "misses", "110"), "\n"); !strings.Contains(got, "11000\tPR xxx") {
		t.Errorf("a number gets the bare form: %q", got)
	}
	// retro takes any number of PRs; misses takes one.
	if got := complete(t, f.Ctx, "retro", "talkable#5", ""); len(got) == 0 {
		t.Error("retro's second reference completed nothing")
	}
	if got := complete(t, f.Ctx, "misses", "talkable#5", ""); len(got) != 0 {
		t.Errorf("misses' second argument completed %q", got)
	}
	var classes []string
	for _, c := range complete(t, f.Ctx, "misses", "--class", "") {
		name, _, _ := strings.Cut(c, "\t")
		classes = append(classes, name)
	}
	if want := []string{"miss", "not_issue", "style", "outside", "unclassified"}; !slices.Equal(classes, want) {
		t.Errorf("--class completions = %v, want %v", classes, want)
	}
}

func TestMissesIsRegisteredInTheInspectGroup(t *testing.T) {
	c, out, _ := bareContext(t)
	cmd, _, err := newRoot(c).Find([]string{"misses"})
	if err != nil || cmd.GroupID != groupInspect {
		t.Fatalf("misses group = %q (%v)", cmd.GroupID, err)
	}
	if code := execute(c, []string{"misses", "--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, out.String(), "magnum misses [<ref>] [--all] [--class miss|not_issue|style|outside|unclassified] [--json]", "RAISED", "--all")
}
