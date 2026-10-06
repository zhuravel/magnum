package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// UPDATED is a PR's last activity: `magnum prs` shows and sorts by it, and
// its JSON says activity_at with GitHub's own updatedAt as
// github_updated_at. #11940 was last active five days ago though an
// invisible change moved GitHub's updatedAt 11 minutes ago; #11941's
// activity is not read yet, so GitHub's updatedAt stands in for it.
func TestPRsShowTheActivityTime(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	now := time.Now()
	invisible, quiet, unread := now.Add(-11*time.Minute), now.Add(-5*24*time.Hour), now.Add(-2*time.Hour)
	inspSeedPR(t, st, "talkable/talkable", 11940, store.PRQueued, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", invisible)
		u.Set("activity_at", quiet)
	})
	inspSeedPR(t, st, "talkable/talkable", 11941, store.PRQueued, func(u *store.PRUpdate) { u.Set("gh_updated_at", unread) })
	st.Close()

	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	lines := strings.Split(strings.TrimSpace(f.Out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "talkable#11941") || !strings.HasPrefix(lines[2], "talkable#11940") {
		t.Fatalf("the latest activity first:\n%s", f.Out.String())
	}
	if !strings.Contains(lines[2], " 5d ") || strings.Contains(lines[2], " 11m ") || !strings.Contains(lines[1], " 2h ") {
		t.Errorf("UPDATED shows the activity:\n%s", f.Out.String())
	}

	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &raw); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	var rows []prsJSONRow
	_ = json.Unmarshal(f.Out.Bytes(), &rows)
	if len(rows) != 2 || rows[0].Number != 11941 || rows[1].Number != 11940 {
		t.Fatalf("json rows %+v", rows)
	}
	same := func(a, b time.Time) bool { return a.Sub(b).Abs() < time.Microsecond }
	if r := rows[1]; !same(r.ActivityAt, quiet) || !same(r.GitHubUpdatedAt, invisible) {
		t.Errorf("#11940 activity_at %v github_updated_at %v; want %v and %v", r.ActivityAt, r.GitHubUpdatedAt, quiet, invisible)
	}
	if r := rows[0]; !same(r.ActivityAt, unread) || !same(r.GitHubUpdatedAt, unread) {
		t.Errorf("#11941 activity_at %v github_updated_at %v; want GitHub's %v for both", r.ActivityAt, r.GitHubUpdatedAt, unread)
	}
	for _, r := range raw {
		if _, ok := r["updated_at"]; ok {
			t.Errorf("%v: updated_at is github_updated_at now", r["ref"])
		}
	}
}

// The picker lists the PRs by their last activity and shows its age.
func TestPickShowsTheActivityTime(t *testing.T) {
	h := newActHarness(t)
	quiet := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	busy := h.seedPR("talkable/talkable", 6, store.PRQueued)
	h.setPR(quiet.ID, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", h.now.Add(-11*time.Minute))
		u.Set("activity_at", h.now.Add(-5*24*time.Hour))
	})
	h.setPR(busy.ID, store.PRQueued, func(u *store.PRUpdate) { u.Set("gh_updated_at", h.now.Add(-2*time.Hour)) })
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{} })
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var got []string
	for _, e := range (*calls)[0].entries {
		got = append(got, e.Ref+" "+e.Age)
	}
	if want := []string{"talkable/talkable#6 2h", "talkable/talkable#5 5d"}; !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
}

// The dashboard's queue shows each PR's last activity, while the queue
// stays in the dispatcher's order (GitHub's updatedAt).
func TestStatusQueueShowsTheActivityTime(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	gh, quiet := now.Add(-11*time.Minute), now.Add(-5*24*time.Hour)
	older := now.Add(-time.Hour)
	q := []statusPRLine{
		{Repo: "talkable/talkable", Number: 1, State: store.PRQueued, UpdatedAt: &gh, ActivityAt: &quiet},
		{Repo: "talkable/talkable", Number: 2, State: store.PRQueued, UpdatedAt: &older},
	}
	statusSortQueue(q)
	if q[0].Number != 2 {
		t.Fatalf("queue order %d, %d: want the dispatcher's, the oldest GitHub update first", q[0].Number, q[1].Number)
	}
	d := statusDashData(statusReport{GeneratedAt: now, Queue: q}, "talkable/talkable")
	if len(d.Queue) != 2 || d.Queue[0].Age != "1h" || d.Queue[1].Age != "5d" {
		t.Fatalf("queue rows %+v", d.Queue)
	}
}
