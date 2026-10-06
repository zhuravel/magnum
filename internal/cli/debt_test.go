package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// debtNearby is a pre-existing problem the judge proved next to a PR's
// changes, as the provenance of its round recorded it.
func debtNearby(id, sev, path string, line int, title string, at time.Time) store.Finding {
	return store.Finding{FindingID: id, Severity: sev, Path: path, Line: line, Title: title, Nearby: true,
		Sources: []string{"claude-review"}, Verdict: store.FindingRejected, ReasonCode: "pre_existing", CreatedAt: at}
}

// debtFixture records the provenance of rounds on four PRs: the same
// problem found next door twice (the newer in talkable#102), a titled
// pre-existing problem the judge did not mark nearby, two untitled ones on
// one path from before titles, a nearby one in example/widgets, and
// findings debt never lists (a P3, a posted finding, another reason).
func debtFixture(t *testing.T) *inspFixture {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	defer st.Close()
	ctx := context.Background()
	record := func(fullName string, number int, at time.Time, fs ...store.Finding) {
		t.Helper()
		_, pr := inspSeedPR(t, st, fullName, number, store.PRReviewed, nil)
		run := statsMkRun(t, st, statsRunSpec{pr: pr, round: 1, role: store.RoleJudge, state: store.RunVerified, outcome: "posted", created: at})
		if err := st.RecordFindings(ctx, run.ID, pr.ID, 1, fs); err != nil {
			t.Fatal(err)
		}
	}
	record("talkable/talkable", 101, statsOct(2, 12, 0),
		debtNearby("A", "P2", "app/exports/emails.rb", 31, "Export skips site check", statsOct(2, 12, 0)),
		store.Finding{FindingID: "D", Severity: "P2", Path: "lib/legacy.rb", Line: 3, Sources: []string{"claude-review"},
			Verdict: store.FindingRejected, ReasonCode: "pre_existing", CreatedAt: statsOct(2, 12, 0)},
		debtNearby("P", "P3", "lib/minor.rb", 4, "Log level too loud", statsOct(2, 12, 0)),
		store.Finding{FindingID: "Q", Severity: "P1", Path: "lib/q.rb", Title: "Posted, the PR's own", Sources: []string{"judge"},
			Verdict: store.FindingPosted, CreatedAt: statsOct(2, 12, 0)},
		store.Finding{FindingID: "R", Severity: "P1", Path: "lib/r.rb", Title: "Not reproduced", Sources: []string{"judge"},
			Verdict: store.FindingRejected, ReasonCode: "not_reproducible", CreatedAt: statsOct(2, 12, 0)})
	record("talkable/talkable", 102, statsOct(3, 12, 0),
		debtNearby("B", "P1", "app/exports/emails.rb", 33, "export skips site check", statsOct(3, 12, 0)),
		store.Finding{FindingID: "C", Severity: "P2", Path: "app/far_away.rb", Line: 8, Title: "Not next to this PR", Sources: []string{"judge"},
			Verdict: store.FindingRejected, ReasonCode: "pre_existing", CreatedAt: statsOct(3, 12, 0)})
	record("talkable/talkable", 103, statsOct(1, 12, 0),
		store.Finding{FindingID: "E", Severity: "P1", Path: "lib/legacy.rb", Line: 9, Sources: []string{"codex-review"},
			Verdict: store.FindingRejected, ReasonCode: "pre_existing", CreatedAt: statsOct(1, 12, 0)})
	record("example/widgets", 7, statsOct(3, 14, 0),
		debtNearby("F", "P2", "lib/cache.rb", 12, "Cache key ignores locale", statsOct(3, 14, 0)))
	return f
}

// The judge proves and drops problems a PR did not bring (63 so far, 1 P1
// and 26 P2) and nobody heard of them. `magnum debt` lists the nearby ones
// across PRs, newest first, once per path and title (the newest find), and
// the untitled ones recorded before titles by path and reason.
func TestDebtListsProvenProblemsNextDoorNewestFirst(t *testing.T) {
	f := debtFixture(t)
	if code := f.run("debt"); code != 0 {
		t.Fatalf("debt: exit %d: %s", code, f.Err.String())
	}
	out := f.Out.String()
	statusNoControls(t, "debt", out)
	day := func(d int) string { return store.DayKey(statsOct(d, 12, 0)) }
	want := []string{
		"PRI WHERE PROBLEM PR FOUND",
		"P2 lib/cache.rb:12 Cache key ignores locale example/widgets#7 " + day(3),
		"P1 app/exports/emails.rb:33 export skips site check talkable/talkable#102 " + day(3),
		"P2 lib/legacy.rb pre_existing talkable/talkable#101 " + day(2),
	}
	if got := slices.DeleteFunc(statsLines(out), func(l string) bool { return l == "" }); !slices.Equal(got, want) {
		t.Fatalf("debt =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A repository argument keeps one (a bare name takes daemon.default_repo's owner).
	if code := f.run("debt", "example/widgets"); code != 0 || strings.Contains(f.Out.String(), "talkable/talkable") ||
		!strings.Contains(f.Out.String(), "Cache key ignores locale") {
		t.Fatalf("debt example/widgets: exit %d\n%s%s", code, f.Out.String(), f.Err.String())
	}
	if code := f.run("debt", "talkable"); code != 0 || strings.Contains(f.Out.String(), "example/widgets") ||
		strings.Count(f.Out.String(), "talkable/talkable#") != 2 {
		t.Fatalf("debt talkable: exit %d\n%s%s", code, f.Out.String(), f.Err.String())
	}
}

func TestDebtJSON(t *testing.T) {
	f := debtFixture(t)
	if code := f.run("debt", "--json"); code != 0 {
		t.Fatalf("debt --json: exit %d: %s", code, f.Err.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &items); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if len(items) != 3 {
		t.Fatalf("%d items:\n%s", len(items), f.Out.String())
	}
	first := items[1] // the export problem, newest find
	if got := statsKeys(first); !slices.Equal(got, []string{"found_at", "line", "nearby", "number", "path", "reason_code", "repo", "severity", "title"}) {
		t.Errorf("keys = %v", got)
	}
	if first["severity"] != "P1" || first["path"] != "app/exports/emails.rb" || first["line"] != 33.0 || first["title"] != "export skips site check" ||
		first["nearby"] != true || first["repo"] != "talkable/talkable" || first["number"] != 102.0 || first["reason_code"] != "pre_existing" {
		t.Errorf("item = %v", first)
	}
	if at, err := time.Parse(time.RFC3339, first["found_at"].(string)); err != nil || !at.Equal(statsOct(3, 12, 0)) {
		t.Errorf("found_at = %v (%v)", first["found_at"], err)
	}
	if legacy := items[2]; legacy["title"] != nil || legacy["nearby"] != false || legacy["path"] != "lib/legacy.rb" || legacy["number"] != 101.0 {
		t.Errorf("untitled item = %v", legacy)
	}
}

func TestDebtWithNothingRecorded(t *testing.T) {
	f := newInspFixture(t)
	f.store().Close()
	if code := f.run("debt"); code != 0 || f.Out.String() != "no proven pre-existing problems recorded\n" {
		t.Fatalf("debt: exit %d, %q", code, f.Out.String())
	}
	if code := f.run("debt", "--json"); code != 0 || f.Out.String() != "[]\n" {
		t.Fatalf("debt --json: exit %d, %q", code, f.Out.String())
	}
}

func TestDebtUsageErrors(t *testing.T) {
	f := debtFixture(t)
	for name, args := range map[string][]string{
		"two repositories":  {"talkable/talkable", "example/widgets"},
		"repo with slashes": {"a/b/c"},
	} {
		if code := f.run("debt", args...); code != 2 || !strings.Contains(f.Err.String(), "usage: magnum debt ") || f.Out.Len() != 0 {
			t.Errorf("%s: exit %d, stderr %q, stdout %q", name, code, f.Err.String(), f.Out.String())
		}
	}
}
