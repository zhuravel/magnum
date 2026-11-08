package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fixtureTwoRepos is home of a factory with two repositories, as
// factory.ScreenData words it: tasks and your PRs of both, in every group,
// a published task with its PR, a task started from words with a long
// title, and a PR magnum leaves alone.
func fixtureTwoRepos() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repos: []string{"example/talkable", "example/widget"},
		Rows: []FactoryRow{
			{
				ID: 1, Key: "PR-12320", GitHub: "#4530", Repo: "example/talkable", Title: "Coupon rounding in the cart",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "2 items need you: 1 thread of a person and 1 skipped finding",
					Short: "2 items need you", OnYou: true}},
				Move: "see the task card", MoveShort: "task card",
				Since:     factoryAgo(now, 35*time.Minute),
				Help:      "you: 2 items need you: 1 thread of a person and 1 skipped finding",
				CI:        &CIInfo{State: "passed", Total: 41, Passed: 41},
				Findings:  &FindingsInfo{Counts: [4]int{0, 1, 3, 0}, Verdict: "blocking"},
				Approvals: &FactoryApprovals{Decision: "REVIEW_REQUIRED"},
			},
			{
				ID: -72, Key: "widget#4602", GitHub: "#4602", Jira: "PR-12400", Repo: "example/widget", Title: "Coupon rounding",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "1 item needs you: 1 thread of a person", Short: "1 item needs you", OnYou: true}},
				Move:    "b opens it, f fixes", MoveShort: "b or f",
				Since: factoryAgo(now, 25*time.Minute),
				Help:  "you: 1 item needs you: 1 thread of a person",
				PR:    &FactoryPR{Ref: "example/widget#4602"},
				CI: &CIInfo{State: "failed", Total: 12, Passed: 11, Failed: 1, Required: []CheckState{
					{Name: "Completion", Label: "Completion", State: "failed", Done: 1, Total: 1}, {Name: "rspec*", Label: "rspec", State: "passed", Done: 8, Total: 8}}},
				Findings:  &FindingsInfo{Counts: [4]int{0, 0, 0, 2}, Verdict: "non_blocking"},
				Approvals: &FactoryApprovals{Approved: 1, Decision: "APPROVED"},
			},
			{
				ID: 5, Key: "task 5", Repo: "example/widget",
				Title: "In the offer page, a click on a record's link opens the record of another domain",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "ready to push · 2 files +39 −6", Short: "ready to push", OnYou: true}},
				Move:    "read the diff, then push", MoveShort: "diff, then push",
				Since: factoryAgo(now, 2*time.Hour),
				Help:  "ready to push: 2 files, 0 not in the plan",
			},
			{
				ID: 3, Key: "PR-12321", GitHub: "#4531", Repo: "example/talkable", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn:   []WaitsOn{{Who: "agent", What: "magnum fixes #4531 (cycle 2 of 5)", Short: "magnum fixes #4531"}},
				Since:     factoryAgo(now, 9*time.Minute),
				Help:      "agent: magnum fixes #4531 (cycle 2 of 5)",
				CI:        &CIInfo{State: "pending", Total: 41, Passed: 25, Pending: 16},
				Findings:  &FindingsInfo{Counts: [4]int{1, 0, 2, 0}, Verdict: "blocking"},
				Approvals: &FactoryApprovals{Changes: 1, Decision: "CHANGES_REQUESTED"},
			},
			{
				ID: 4, Key: "PR-12325", GitHub: "#4535", Repo: "example/talkable", Title: "Deploy list on campaigns",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "reviewers", What: "nothing open at a1b2c3d · asked rev-ann 17h ago", Short: "asked rev-ann 17h ago"}},
				Since:   factoryAgo(now, 3*time.Hour),
				Help:    "reviewers: nothing open at a1b2c3d · asked rev-ann 17h ago",
				CI: &CIInfo{State: "passed", Total: 41, Passed: 41, Required: []CheckState{
					{Name: "Completion", Label: "Completion", State: "missing"}}},
				Findings:  &FindingsInfo{Verdict: "clean", Fixed: 3},
				Approvals: &FactoryApprovals{Decision: "REVIEW_REQUIRED"},
			},
			{
				ID: 6, Key: "task 6", Repo: "example/widget", Title: "Add passkey login support",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "review", What: "round 1 of 3 of the local review", Short: "round 1 of 3"}},
				Since:   factoryAgo(now, 15*time.Minute),
				Help:    "review: round 1 of 3 of the local review",
			},
			{
				ID: -81, Key: "talkable#4611", GitHub: "#4611", Jira: "PR-12410", Repo: "example/talkable", Title: "Cart totals in the widget",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "reviewers", What: "all 3 findings fixed at a1b2c3d · asked rev-bob 2h ago",
					Short: "asked rev-bob 2h ago"}},
				Since:     factoryAgo(now, 10*time.Minute),
				Help:      "reviewers: all 3 findings fixed at a1b2c3d · asked rev-bob 2h ago",
				PR:        &FactoryPR{Ref: "example/talkable#4611"},
				CI:        &CIInfo{State: "passed", Total: 41, Passed: 41},
				Findings:  &FindingsInfo{Verdict: "clean", Fixed: 3},
				Approvals: &FactoryApprovals{Approved: 2, Stale: 2, Decision: "REVIEW_REQUIRED"},
			},
			{
				ID: -76, Key: "widget#4606", GitHub: "#4606", Repo: "example/widget", Title: "Old sync job cleanup",
				Group: GroupYours, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: WaitsOnYou, What: "#4606 is yours to follow: it was open before magnum first synced the repository",
					Short: "#4606 is yours to follow", OnYou: true}},
				Since: factoryAgo(now, 26*time.Hour),
				Help:  "you: #4606 is yours to follow: it was open before magnum first synced the repository",
				PR:    &FactoryPR{Ref: "example/widget#4606", Off: "#4606 is yours to follow: it was open before magnum first synced the repository"},
			},
			{
				ID: 7, Key: "PR-12302", GitHub: "#4519", Repo: "example/talkable", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20"}},
				Help:    "nothing: merged 13:20",
			},
		},
		Blocking:     []string{"memory full: new tasks wait"},
		BlockingLong: []string{"memory full: new tasks wait (swap 91%)"},
		Decisions:    3,
		Hints:        []hint{{"enter", "task card"}, {"a", "oldest ask"}, {"n", "new task"}, {"?", "more"}},
	}
}

// factoryReposHeight is fixtureTwoRepos' lines: the top, four groups and the
// footer.
const factoryReposHeight = 22

// factoryRowLines are the frame's row lines, by the key each starts with
// after its mark, in the order of rows.
func factoryRowLines(t *testing.T, frame string, rows []FactoryRow) []string {
	t.Helper()
	lines := strings.Split(ansi.Strip(frame), "\n")
	var out []string
	for _, l := range lines[3:] {
		if !strings.HasPrefix(l, "──") && strings.ContainsAny(l[:min(len(l), 8)], "●◷⣷✓›") {
			out = append(out, l)
		}
	}
	if len(out) != len(rows) {
		t.Fatalf("%d row lines for %d rows:\n%s", len(out), len(rows), ansi.Strip(frame))
	}
	return out
}

// The two repositories' frames at 100, 120, 140 and 200 columns: every row
// names its repository, one line each, and the title names both.
func TestFactoryFramesOfTwoRepositories(t *testing.T) {
	for _, w := range []int{100, 120, 140, 200} {
		frame, _ := factoryFrameOf(fixtureTwoRepos(), w, factoryReposHeight, true, IconsUnicode, factoryNow)
		checkFactoryFrame(t, fmt.Sprintf("repos-%d.txt", w), ansi.Strip(frame), w)
	}
}

// With two repositories every row says which one it is of, a task's and
// your PR's alike, in a column of its own (the name without the owner they
// share), at every width from 100; each row stays one line.
func TestFactoryRowsNameTheirRepository(t *testing.T) {
	f := fixtureTwoRepos()
	for _, w := range []int{100, 120, 140, 200} {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		at := -1
		for i, line := range factoryRowLines(t, frame, f.Rows) {
			_, name, _ := strings.Cut(f.Rows[i].Repo, "/")
			x := strings.Index(line, name[:min(len(name), 5)])
			if x >= 0 {
				x = ansi.StringWidth(line[:x])
			}
			switch {
			case x < 0:
				t.Errorf("%d columns: the row of %s does not name %s: %q", w, f.Rows[i].Key, name, line)
			case at >= 0 && x != at:
				t.Errorf("%d columns: %s's repository at %d, the others' at %d: %q", w, f.Rows[i].Key, x, at, line)
			default:
				at = x
			}
			if strings.Contains(line, "example/") {
				t.Errorf("%d columns: one owner, yet the row names it: %q", w, line)
			}
		}
	}
}

// Rows of several owners name a repository by its name alone when no other
// repository has that name, and with its owner beside a name two owners
// share; the title names them so too. Before, every row of several owners
// read owner/name, which the narrow layouts cut to its owner.
func TestFactoryRowsOfSeveralOwnersNameTheOwnerOnlyOfASharedName(t *testing.T) {
	f := fixtureTwoRepos()
	f.Repos = []string{"example/talkable", "example-org/widget", "example/widget"}
	for i := range f.Rows {
		if strings.HasSuffix(f.Rows[i].Repo, "/widget") && i != 2 {
			f.Rows[i].Repo = "example-org/widget"
		}
	}
	frame, _ := factoryFrameOf(f, 200, factoryReposHeight, true, IconsUnicode, factoryNow)
	lines := factoryRowLines(t, frame, f.Rows)
	mustContain(t, lines[1], "example-org/widget")
	mustContain(t, lines[2], "example/widget")
	if strings.Contains(lines[0], "example/talkable") {
		t.Errorf("talkable is the only repository of its name, yet its row names the owner: %q", lines[0])
	}
	mustContain(t, strings.Split(ansi.Strip(frame), "\n")[0], "magnum factory · talkable, example-org/widget, example/widget")
}

// The REPO cells tell the factory's repositories apart at every width from
// 100 columns, with repositories of two owners where one's name starts
// another's: no two cells read alike, and no cut cell's text before its
// ellipsis starts another repository's name. Before, two owners made every
// cell owner/name, cut to 6 or 11 cells (13 at 140), so talkable and
// talkable-widget both read "talka…", "talkable/…" or "talkable/ta…" below
// 200 columns.
func TestFactoryRepoCellsTellTheRepositoriesApart(t *testing.T) {
	repos := []string{"example/talkable", "example-org/example-org", "example/talkable-widget"}
	names := map[string]string{repos[0]: "talkable", repos[1]: "example-org", repos[2]: "talkable-widget"}
	f := fixtureTwoRepos()
	f.Repos = repos
	for i := range f.Rows {
		f.Rows[i].Repo = repos[i%len(repos)]
	}
	for w := 100; w <= 200; w++ {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		lines := factoryRowLines(t, frame, f.Rows)
		cells := map[string]string{}
		for i, line := range lines {
			c := strings.TrimSpace(string([]rune(line)[factoryMarkW:]))
			c, _, _ = strings.Cut(c, "  ")
			if ansi.StringWidth(line) > w {
				t.Errorf("%d columns: a row is wider than the screen: %q", w, line)
			}
			repo := f.Rows[i].Repo
			if prev, ok := cells[repo]; ok && prev != c {
				t.Errorf("%d columns: %s reads %q and %q", w, repo, prev, c)
			}
			cells[repo] = c
		}
		for a, ca := range cells {
			for b, cb := range cells {
				if a == b {
					continue
				}
				if ca == cb {
					t.Errorf("%d columns: %s and %s both read %q", w, a, b, ca)
				}
				if shown, cut := strings.CutSuffix(ca, "…"); cut && strings.HasPrefix(names[b], shown) {
					t.Errorf("%d columns: %s's cell %q starts %s's name %q", w, a, ca, b, names[b])
				}
			}
		}
	}
}

// The title names every repository the factory has, never the first
// task's alone; when the names do not fit beside the blocker line it
// counts them, before it gives up the blocker line.
func TestFactoryTitleNamesEveryRepositoryOrCountsThem(t *testing.T) {
	title := func(f factoryFrame, w int) string {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		return strings.Split(ansi.Strip(frame), "\n")[0]
	}
	f := fixtureTwoRepos()
	mustContain(t, title(f, 140), "magnum factory · talkable, widget", "memory full: new tasks wait")
	f.Repos = []string{"example/talkable", "example/talkable-integration", "example/talkable-helm", "example/widget"}
	got := title(f, 100)
	mustContain(t, got, "magnum factory · 4 repos", "memory full: new tasks wait")
	if strings.Contains(got, "talkable") {
		t.Errorf("a title of 4 repos that does not fit names one: %q", got)
	}
	mustContain(t, title(f, 200), "talkable, talkable-integration, talkable-helm, widget")
}

// A factory of one repository whose rows are all of it draws no REPO
// column: the title names it. A row of another repository (your PR in a
// repository a watch covers) brings the column back.
func TestFactoryOneRepositoryDrawsNoRepoColumn(t *testing.T) {
	f := fixtureYours()
	for i := range f.Rows {
		f.Rows[i].Repo = "example/talkable"
	}
	frame, _ := factoryFrameOf(f, 140, 22, true, IconsUnicode, factoryNow)
	for _, line := range factoryRowLines(t, frame, f.Rows) {
		if strings.Contains(line, "talkable") {
			t.Errorf("one repository, yet a row names it: %q", line)
		}
	}
	f.Rows[4].Repo = "example/helm"
	frame, _ = factoryFrameOf(f, 140, 22, true, IconsUnicode, factoryNow)
	lines := factoryRowLines(t, frame, f.Rows)
	mustContain(t, lines[0], "talkable")
	mustContain(t, lines[4], "helm")
}

// Rows with a PR draw its CI, magnum's findings and the people's approvals
// in columns of their own after WAITS ON, in the board's own cells on a
// wide screen (the worst required check or the counts, the findings by
// priority) and in their short forms on a narrower one (the CI's glyph,
// the findings' verdict and worst priority); past the spec's steps they
// outlast YOUR MOVE, whose step the help line then says, and the CI's glyph
// outlasts the others. A task without a PR leaves them empty, and every row
// stays one line. Before, home drew none of them.
func TestFactoryPRColumnsDrawTheBoardsCells(t *testing.T) {
	f := fixtureTwoRepos()
	lines := func(w int) []string {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		return strings.Split(ansi.Strip(frame), "\n")
	}
	row := func(w, i int) string {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		return factoryRowLines(t, frame, f.Rows)[i]
	}
	mustContain(t, lines(200)[3], "CI", "FINDINGS", "APPR")
	mustContain(t, row(200, 0), "✓ 41/41", "✗ P1 P2×3", "—")
	mustContain(t, row(200, 1), "✗ Completion +1", "● P3×2", "✔ 1")
	mustContain(t, row(200, 3), "◌ 25/41", "✗ P0 P2×2", "✗ 1")
	mustContain(t, row(200, 4), "– Completion not run", "✔ clean")
	mustContain(t, row(200, 6), "✔ 2 ⟳")
	for _, w := range []int{100, 120, 140} {
		mustContain(t, lines(w)[3], "CI", "FINDINGS", "APPR")
		mustContain(t, row(w, 0), "✓", "✗ P1", "—")
		mustContain(t, row(w, 1), "✗", "● P3", "✔ 1")
		if r := row(w, 0); strings.Contains(r, "41/41") || strings.Contains(r, "P2×3") {
			t.Errorf("%d columns: the CI and the findings keep their short forms: %q", w, r)
		}
	}
	mustContain(t, lines(140)[3], "YOUR MOVE")
	for _, w := range []int{100, 120} {
		if strings.Contains(lines(w)[3], "YOUR MOVE") {
			t.Errorf("%d columns: YOUR MOVE gives way to the PR's columns: %q", w, lines(w)[3])
		}
		mustContain(t, lines(w)[factoryReposHeight-2], "your move: task card")
	}
	if l := lines(90); !strings.Contains(l[3], "CI") || strings.Contains(l[3], "FINDINGS") || strings.Contains(l[3], "APPR") {
		t.Errorf("90 columns: the CI's glyph outlasts FINDINGS and APPR: %q", l[3])
	}
	for _, w := range []int{100, 120, 140, 200} {
		if task := row(w, 2); strings.ContainsAny(task, "✓◌✔—") {
			t.Errorf("%d columns: a task without a PR draws a PR cell: %q", w, task)
		}
	}
}

// A CI of an older commit than the head carries the board's stale mark in
// every form, the short one too: its glyph alone would say the head's
// checks passed when nobody has read them. Before, only the 200-column cell
// said so ("✓ 41/41 ⟳"), and 100 to 140 columns drew a bare green ✓.
func TestFactoryCIOfAnOlderCommitIsMarkedStaleInEveryForm(t *testing.T) {
	f := fixtureTwoRepos()
	f.Rows[0].CI = &CIInfo{State: "passed", Total: 41, Passed: 41, Stale: true}
	f.Rows[1].CI = &CIInfo{State: "passed", Total: 12, Passed: 12, Stale: true, Required: []CheckState{
		{Name: "Completion", Label: "Completion", State: "passed", Done: 1, Total: 1}}}
	for _, w := range []int{100, 120, 140, 200} {
		frame, _ := factoryFrameOf(f, w, factoryReposHeight, true, IconsUnicode, factoryNow)
		lines := strings.Split(ansi.Strip(frame), "\n")
		at := strings.Index(lines[3], "  CI  ")
		if at < 0 {
			t.Fatalf("%d columns: no CI column: %q", w, lines[3])
		}
		x := ansi.StringWidth(lines[3][:at]) + 2
		for i, line := range factoryRowLines(t, frame, f.Rows)[:2] {
			cell, _, _ := strings.Cut(ansi.Cut(line, x, w), "  ")
			if !strings.Contains(cell, "⟳") {
				t.Errorf("%d columns: %s's CI of an older commit reads %q, without ⟳: %q", w, f.Rows[i].Key, cell, line)
			}
		}
	}
}
