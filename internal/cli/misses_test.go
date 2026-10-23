package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
		for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
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
	actContains(t, out.String(), "magnum misses [<ref>] [--all] [--class miss|not_issue|style|outside|unclassified] [--declines] [--json]",
		"RAISED", "--all", "--declines", "VERDICT")
}

// declineSeed stores d on pr (CommentID from the URL's tail, ReviewedSHA
// the usual one) at the store's clock and returns the stored row.
func declineSeed(t *testing.T, st *store.Store, pr store.PR, id int64, d store.Decline) store.Decline {
	t.Helper()
	d.PRID, d.CommentID = pr.ID, id
	d.SourceURL = pr.URL + "#discussion_r" + strconv.FormatInt(id, 10)
	d.ReviewedSHA = "abcdef0123456789abcdef0123456789abcdef01"
	got, err := st.UpsertDecline(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMissesDeclinesListsAnsweredFindings: --declines lists how authors
// answered magnum's findings, newest first: by default the findings they
// declined or argued that no lesson was drawn from yet, --all every
// answer in every state, a <ref> one PR's; --json prints the rows, and
// --class has no meaning there.
func TestMissesDeclinesListsAnsweredFindings(t *testing.T) {
	f := missesFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.PRByRepoNumber(ctx, repo.ID, 101)
	if err != nil {
		t.Fatal(err)
	}
	_, b := inspSeedPR(t, st, "example/widgets", 8, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	clock := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	st.Clock = func() time.Time { clock = clock.Add(time.Minute); return clock }
	declineSeed(t, st, a, 1, store.Decline{Path: "app/models/gift.rb", Line: 12, Title: "Gift cards skip the balance check", Priority: "P2",
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Scope: store.MissScopeGeneral, Match: []string{"balance"},
		Lesson: "When the caller holds the row lock, do not report a read after the write as a race."})
	declineSeed(t, st, a, 2, store.Decline{Path: "app/a.rb", Line: 3, Title: "Fixed one", Priority: "P1", Outcome: store.DeclineFixed})
	used := declineSeed(t, st, b, 3, store.Decline{Path: "lib/b.rb", Line: 9, Title: "Used one", Priority: "P3", Outcome: store.DeclineArgued,
		Verdict: store.VerdictOverRanked, Scope: store.MissScopeGeneral, Lesson: "A used lesson.", Match: []string{"x"}})
	declineSeed(t, st, b, 4, store.Decline{Path: "lib/c.rb", Title: "Ranked \x1b[31mright", Outcome: store.DeclineArgued, Verdict: store.VerdictRight})
	if _, err := st.SetDeclineState(ctx, []int64{used.ID}, []string{store.MissNew}, store.MissUsed); err != nil {
		t.Fatal(err)
	}
	st.Close()

	titles := func(out string) []string {
		var got []string
		for _, title := range []string{"Gift cards skip the balance check", "Fixed one", "Used one", "Ranked"} {
			if strings.Contains(out, title) {
				got = append(got, title)
			}
		}
		return got
	}
	out := missesRun(t, f, "--declines")
	statusNoControls(t, "misses --declines", out)
	if got := titles(out); !slices.Equal(got, []string{"Gift cards skip the balance check", "Ranked"}) {
		t.Fatalf("default titles = %v:\n%s", got, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if len(lines) != 3 || norm(lines[0]) != "PR PRIO WHERE OUTCOME VERDICT SCOPE STATE TITLE LESSON" {
		t.Fatalf("table:\n%s", out)
	}
	if got, want := norm(lines[2]), "talkable#101 P2 app/models/gift.rb:12 declined wrong general new Gift cards skip the balance check "+
		"When the caller holds the row lock, do not report a read after the write as a race."; got != want {
		t.Errorf("row = %q\nwant  %q", got, want)
	}
	if got, want := norm(lines[1]), "example/widgets#8 - lib/c.rb argued right - new Ranked [31mright -"; got != want {
		t.Errorf("row = %q\nwant  %q", got, want)
	}
	if got := titles(missesRun(t, f, "--declines", "--all")); len(got) != 4 {
		t.Errorf("--all titles = %v", got)
	}
	if got := titles(missesRun(t, f, "--declines", "--all", "example/widgets#8")); !slices.Equal(got, []string{"Used one", "Ranked"}) {
		t.Errorf("--all example/widgets#8 = %v", got)
	}
	actContains(t, missesRun(t, f, "--declines", "example/widgets#7"), "no new declined or argued findings yet")

	var ds []store.Decline
	if err := json.Unmarshal([]byte(missesRun(t, f, "--declines", "--json")), &ds); err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[1].Title != "Gift cards skip the balance check" || ds[1].Repo != "talkable/talkable" || ds[1].Number != 101 ||
		ds[1].Verdict != store.VerdictWrong || !slices.Equal(ds[1].Match, []string{"balance"}) {
		t.Fatalf("--json = %+v", ds)
	}
	if code := f.run("misses", "--declines", "--class", "miss"); code != 2 || !strings.Contains(f.Err.String(), "--class") {
		t.Errorf("--declines --class: exit %d: %s", code, f.Err.String())
	}
}

// A miss or a decline a skill gate retired says why in STATE (with --all)
// and in the JSON: the operator sees what the loop dismissed and which
// proposal did.
func TestMissesSayWhyASkillGateDismissedAnInput(t *testing.T) {
	f := missesFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, 101)
	if err != nil {
		t.Fatal(err)
	}
	m := missSeed(t, st, pr, store.Miss{SourceURL: "https://example.com/c/9", Path: "app/x.rb", Line: 4, Class: store.MissMiss, Severity: "P1",
		Scope: store.MissScopeGeneral, Title: "Covered race", Lesson: "When two writers share a row, check the lock."})
	d := declineSeed(t, st, pr, 9, store.Decline{Path: "app/y.rb", Line: 5, Title: "Covered decline", Priority: "P2",
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Scope: store.MissScopeGeneral, Match: []string{"y"},
		Lesson: "When the caller checks it, do not ask for a guard."})
	p, err := st.CreateLearnProposal(ctx, store.LearnProposalInput{Trigger: store.LearnTriggerEvidence, BaseSHA: "b", Branch: "learn/x",
		State: store.LearnGating, Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: m.ID, Outcome: store.MissNoted, Where: "s4"},
			{Kind: store.LearnInputDecline, InputID: d.ID, Outcome: store.MissNoted, Where: "s5"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GateLearnProposal(ctx, p.ID, store.LearnFailed, "nothing to improve", time.Time{}, nil, []store.LearnTarget{
		{Kind: store.LearnInputMiss, InputID: m.ID, Gate: store.LearnTargetBaseline},
		{Kind: store.LearnInputDecline, InputID: d.ID, Gate: store.LearnTargetBaseline}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	want := "dismissed: found at the baseline (skill proposal " + strconv.FormatInt(p.ID, 10) + ")"
	if out := missesRun(t, f, "--all"); !strings.Contains(out, want) {
		t.Fatalf("misses --all lacks %q:\n%s", want, out)
	}
	want = "dismissed: not reported at the baseline (skill proposal " + strconv.FormatInt(p.ID, 10) + ")"
	if out := missesRun(t, f, "--declines", "--all"); !strings.Contains(out, want) {
		t.Fatalf("misses --declines --all lacks %q:\n%s", want, out)
	}
	var ms []store.Miss
	if err := json.Unmarshal([]byte(missesRun(t, f, "--all", "--json")), &ms); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(x store.Miss) bool { return x.ID == m.ID })
	if i < 0 || ms[i].Dismissed != store.LearnDismissedFound || ms[i].DismissedBy == nil || *ms[i].DismissedBy != p.ID {
		t.Fatalf("--json = %+v", ms)
	}
}

// TestMissesAllSaysWhyACommentIsOutside: with --all (or --class outside)
// an outside comment's CLASS says why it is outside, as RAISED says why
// the judge rejected a point, and --json carries the reason and the commit
// the comment was made on; a row stored before the reason existed says
// only outside.
func TestMissesAllSaysWhyACommentIsOutside(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 101, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	missSeed(t, st, pr, store.Miss{SourceURL: "https://example.com/c/1", Path: "a.rb", Line: 4, Class: store.MissOutside, Title: "Old code",
		CommentSHA: "0123456789abcdef0123456789abcdef01234567", OutsideReason: store.MissOutsideBeforeReview})
	missSeed(t, st, pr, store.Miss{SourceURL: "https://example.com/c/2", Path: "b.rb", Line: 9, Class: store.MissOutside, Title: "Unknown why"})
	st.Close()
	for _, args := range [][]string{{"--all"}, {"--class", "outside"}} {
		out := missesRun(t, f, args...)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 3 {
			t.Fatalf("misses %v:\n%s", args, out)
		}
		if got := strings.Fields(lines[2])[3]; got != "outside:before_review" {
			t.Errorf("misses %v: CLASS = %q, want outside:before_review\n%s", args, got, out)
		}
		if got := strings.Fields(lines[1])[3]; got != "outside" {
			t.Errorf("misses %v: CLASS of a row without a reason = %q\n%s", args, got, out)
		}
	}
	var ms []map[string]any
	if err := json.Unmarshal([]byte(missesRun(t, f, "--json", "--all")), &ms); err != nil || len(ms) != 2 {
		t.Fatalf("--json: %v, %v", ms, err)
	}
	if ms[1]["outside_reason"] != store.MissOutsideBeforeReview || ms[1]["comment_sha"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("--json row = %v", ms[1])
	}
	if _, ok := ms[0]["outside_reason"]; ok {
		t.Errorf("a row without a reason names one: %v", ms[0])
	}
}
