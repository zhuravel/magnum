package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

var prsNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

const prsHead = "head000000000000000000000000000000000000"

// prsRichRow is a registry row with every board field set.
func prsRichRow() store.BoardRow {
	at := func(d time.Duration) *time.Time { t := prsNow.Add(-d); return &t }
	return store.BoardRow{
		PRID: 7, Ref: "talkable/talkable#11920", Owner: "talkable", Name: "talkable", Number: 11920,
		Title: "Fix the widget", Author: "ann", URL: "https://github.com/talkable/talkable/pull/11920", Draft: true,
		Labels: []string{"bug"}, Assignees: []string{"bob", "cat"},
		RequestedReviewers: []string{"dan", "team:core", "Cat"},
		LatestReviews: []store.LatestReview{
			{Login: "cat", State: "APPROVED", SubmittedAt: at(2 * time.Hour), CommitSHA: prsHead},
			{Login: "bob", State: "CHANGES_REQUESTED", SubmittedAt: at(26 * time.Hour), CommitSHA: "old"},
			{Login: "eve", State: "COMMENTED", SubmittedAt: at(3 * time.Hour), CommitSHA: ""},
			{Login: "fay", State: "DISMISSED", SubmittedAt: at(4 * time.Hour), CommitSHA: "old"},
			{Login: "gus", State: "PENDING", CommitSHA: "old"},
			{Login: "talkable", State: "COMMENTED", SubmittedAt: at(time.Hour), CommitSHA: "reviewed"},
			{Login: "", State: "APPROVED", SubmittedAt: at(9 * time.Hour), CommitSHA: prsHead},
		},
		// Oldest first: bob was asked three days ago and again an hour ago, the
		// user (zhuravel) twice, a team by someone who left.
		ReviewRequests: []store.ReviewRequest{
			{At: prsNow.Add(-72 * time.Hour), By: "ann", To: "bob"},
			{At: prsNow.Add(-26 * time.Hour), By: "", To: "team:core"},
			{At: prsNow.Add(-4 * time.Hour), By: "ann", To: "zhuravel"},
			{At: prsNow.Add(-2 * time.Hour), By: "ann", To: "zhuravel"},
			{At: prsNow.Add(-time.Hour), By: "ann", To: "bob"},
		},
		SinceReview: &store.SinceReview{Source: store.SinceFromReviewed, Base: "reviewed", Head: prsHead,
			Commits: 2, Files: 3, Additions: 40, Deletions: 5, ComputedAt: prsNow},
		State: store.PRVerifying, GHState: "OPEN", UpdatedAt: prsNow.Add(-30 * time.Minute), HeadSHA: prsHead,
		ReviewedSHA: "reviewed", LastReviewEvent: "COMMENTED", LastReviewAt: prsNow.Add(-time.Hour), LastReviewLogin: "talkable",
		Identity: "talkable-app", Slot: "review1", SlotPath: "/w/talkable.review1", Pinned: true, Muted: true,
		NextEligibleAt: prsNow.Add(time.Hour), LastError: "judge failed", RoundsToday: 2,
	}
}

func TestPRsBoardRowMapsEveryField(t *testing.T) {
	got := prsBoardRow(prsRichRow(), []string{"zhuravel", "talkable[bot]"})
	want := tui.PRBoardRow{
		Ref: "talkable/talkable#11920", Owner: "talkable", Repo: "talkable", Number: 11920,
		Title: "Fix the widget", Author: "ann", URL: "https://github.com/talkable/talkable/pull/11920", Draft: true,
		Labels: []string{"bug"}, Assignees: []string{"bob", "cat"},
		State: store.PRReviewing, GHState: "OPEN", UpdatedAt: prsNow.Add(-30 * time.Minute), HeadSHA: prsHead,
		LastReview: &tui.ReviewInfo{Login: "talkable", Event: "COMMENTED", SubmittedAt: prsNow.Add(-time.Hour),
			CommitSHA: "reviewed", Stale: true, Mine: true},
		Reviewers: []tui.ReviewerInfo{
			{Login: "cat", Verdict: "approved", SubmittedAt: prsNow.Add(-2 * time.Hour), CommitSHA: prsHead, Requested: true},
			{Login: "bob", Verdict: "changes_requested", SubmittedAt: prsNow.Add(-26 * time.Hour), CommitSHA: "old", Stale: true},
			{Login: "eve", Verdict: "commented", SubmittedAt: prsNow.Add(-3 * time.Hour), Stale: true}, // the commit is gone
			{Login: "fay", Verdict: "dismissed", SubmittedAt: prsNow.Add(-4 * time.Hour), CommitSHA: "old", Stale: true},
			{Login: "gus", Verdict: "pending", CommitSHA: "old"}, // a draft review never goes stale
			{Login: "talkable", Verdict: "commented", SubmittedAt: prsNow.Add(-time.Hour), CommitSHA: "reviewed", Stale: true, Mine: true},
			{Login: "ghost", Verdict: "approved", SubmittedAt: prsNow.Add(-9 * time.Hour), CommitSHA: prsHead},
			{Login: "dan", Verdict: "pending", Requested: true},
			{Login: "team:core", Verdict: "pending", Requested: true},
		},
		SinceReview: &tui.ReviewDelta{Base: "reviewed", BaseSHA: "reviewed", Commits: 2, Files: 3, Additions: 40, Deletions: 5},
		Slot:        "review1", Pinned: true, Muted: true, NextEligibleAt: prsNow.Add(time.Hour),
		LastError: "judge failed", RoundsToday: 2,
		// The latest request to each reviewer, newest first; zhuravel is me.
		RequestedToMe: &tui.RequestInfo{To: "zhuravel", By: "ann", At: prsNow.Add(-2 * time.Hour), Mine: true},
		LastRequest:   &tui.RequestInfo{To: "bob", By: "ann", At: prsNow.Add(-time.Hour)},
		Requests: []tui.RequestInfo{
			{To: "bob", By: "ann", At: prsNow.Add(-time.Hour)},
			{To: "zhuravel", By: "ann", At: prsNow.Add(-2 * time.Hour), Mine: true},
			{To: "team:core", At: prsNow.Add(-26 * time.Hour)},
		},
	}
	gj, _ := json.MarshalIndent(got, "", " ")
	wj, _ := json.MarshalIndent(want, "", " ")
	if string(gj) != string(wj) {
		t.Fatalf("mapped row\n%s\nwant\n%s", gj, wj)
	}
}

func TestPRsBoardRowEdgeCases(t *testing.T) {
	cases := map[string]struct {
		edit  func(*store.BoardRow)
		check func(t *testing.T, r tui.PRBoardRow)
	}{
		"never reviewed: no last review, the whole PR against its base": {
			edit: func(b *store.BoardRow) {
				b.ReviewedSHA, b.LastReviewEvent, b.LastReviewAt, b.LastReviewLogin = "", "", time.Time{}, ""
				b.SinceReview = &store.SinceReview{Source: store.SinceFromBase, Base: "basetip", Commits: 9, Files: 4}
			},
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.LastReview != nil {
					t.Errorf("last review %+v, want nil", r.LastReview)
				}
				if d := r.SinceReview; d == nil || d.Base != "base branch" || d.BaseSHA != "basetip" || d.Commits != 9 || d.Truncated {
					t.Errorf("since %+v", d)
				}
			},
		},
		"reviewed head is current": {
			edit: func(b *store.BoardRow) { b.ReviewedSHA = prsHead },
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.LastReview == nil || r.LastReview.Stale {
					t.Errorf("last review %+v, want fresh", r.LastReview)
				}
			},
		},
		"the identity's GitHub review counts as reviewed": {
			edit: func(b *store.BoardRow) {
				b.SinceReview = &store.SinceReview{Source: store.SinceFromReview, Base: "ghreview", Commits: 1}
			},
			check: func(t *testing.T, r tui.PRBoardRow) {
				if d := r.SinceReview; d == nil || d.Base != "reviewed" || d.BaseSHA != "ghreview" {
					t.Errorf("since %+v", d)
				}
			},
		},
		"truncated compare: at least the file limit": {
			edit: func(b *store.BoardRow) {
				b.SinceReview = &store.SinceReview{Source: store.SinceFromReviewed, Base: "x", Files: -1, Additions: 9000}
			},
			check: func(t *testing.T, r tui.PRBoardRow) {
				if d := r.SinceReview; d == nil || !d.Truncated || d.Files != 300 || d.Additions != 9000 {
					t.Errorf("since %+v", d)
				}
			},
		},
		"failed compare: no delta, the error joins LastError": {
			edit: func(b *store.BoardRow) {
				b.SinceReview = &store.SinceReview{Source: store.SinceFromReviewed, Base: "gone", Error: "base commit is gone"}
			},
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.SinceReview != nil || r.LastError != "judge failed; compare since the last review: base commit is gone" {
					t.Errorf("since %+v, last error %q", r.SinceReview, r.LastError)
				}
			},
		},
		"no comparison yet": {
			edit: func(b *store.BoardRow) { b.SinceReview, b.LastError = nil, "" },
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.SinceReview != nil || r.LastError != "" {
					t.Errorf("since %+v, last error %q", r.SinceReview, r.LastError)
				}
			},
		},
		"states: claiming reads as reviewing, releasing as closed": {
			edit: func(b *store.BoardRow) { b.State = store.PRReleasing },
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.State != store.PRClosed {
					t.Errorf("state %q", r.State)
				}
				for in, want := range map[string]string{store.PRClaiming: "reviewing", store.PRQueued: "queued", store.PRReleased: "released"} {
					if got := prsBoardState(in); got != want {
						t.Errorf("state %s = %s, want %s", in, got, want)
					}
				}
			},
		},
		"no ref stored: built from the parts": {
			edit: func(b *store.BoardRow) { b.Ref = "" },
			check: func(t *testing.T, r tui.PRBoardRow) {
				if r.Ref != "talkable/talkable#11920" {
					t.Errorf("ref %q", r.Ref)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := prsRichRow()
			tc.edit(&b)
			tc.check(t, prsBoardRow(b, nil))
		})
	}
	for in, want := range map[string]string{"APPROVED": "approved", "changes_requested": "changes_requested", "COMMENTED": "commented",
		"DISMISSED": "dismissed", "PENDING": "pending", "": "pending", "WEIRD": "weird"} {
		if got := prsVerdict(in); got != want {
			t.Errorf("verdict %q = %q, want %q", in, got, want)
		}
	}
}

// The board row sums up the review requests: the latest to each reviewer
// (newest first), the latest of all and the latest asking me, where "me" is
// the ★ of the board: the user and the App posting as them, whatever the case
// and the "[bot]", while another account of the same name is not.
func TestPRsBoardRowSummarisesReviewRequests(t *testing.T) {
	ask := func(to, by string, ago time.Duration) store.ReviewRequest {
		return store.ReviewRequest{At: prsNow.Add(-ago), By: by, To: to}
	}
	self := []string{"zhuravel", "talkable[bot]"}
	for name, tc := range map[string]struct {
		list        []store.ReviewRequest
		toMe, last  string // "<to> <hours>h", "" = nil
		perReviewer []string
	}{
		"none": {},
		"only others: the latest is shown, nothing is mine": {
			list: []store.ReviewRequest{ask("bob", "ann", 5*time.Hour), ask("cat", "ann", 3*time.Hour)},
			last: "cat 3h", perReviewer: []string{"cat 3h", "bob 5h"},
		},
		"asked again: the later request replaces the earlier of one reviewer": {
			list: []store.ReviewRequest{ask("zhuravel", "ann", 30*time.Hour), ask("bob", "ann", 20*time.Hour), ask("zhuravel", "ann", 10*time.Hour)},
			toMe: "zhuravel 10h", last: "zhuravel 10h", perReviewer: []string{"zhuravel 10h", "bob 20h"},
		},
		"mine is not the latest": {
			list: []store.ReviewRequest{ask("zhuravel", "ann", 30*time.Hour), ask("team:core", "", time.Hour)},
			toMe: "zhuravel 30h", last: "team:core 1h", perReviewer: []string{"team:core 1h", "zhuravel 30h"},
		},
		"the App posting as me and the user's case count as me": {
			list: []store.ReviewRequest{ask("Zhuravel", "ann", 6*time.Hour), ask("talkable[bot]", "ann", 2*time.Hour)},
			toMe: "talkable[bot] 2h", last: "talkable[bot] 2h", perReviewer: []string{"talkable[bot] 2h", "Zhuravel 6h"},
		},
		"another account of my name is another reviewer, and still not mine": {
			list: []store.ReviewRequest{ask("rev-ann", "ann", 4*time.Hour), ask("rev-ann[bot]", "ann", 3*time.Hour)},
			last: "rev-ann[bot] 3h", perReviewer: []string{"rev-ann[bot] 3h", "rev-ann 4h"},
		},
		"a request without a reviewer or a time says nothing": {
			list: []store.ReviewRequest{ask("", "ann", time.Hour), {To: "bob", By: "ann"}, ask("cat", "ann", 2*time.Hour)},
			last: "cat 2h", perReviewer: []string{"cat 2h"},
		},
		"unordered input still ends with the newest per reviewer": {
			list: []store.ReviewRequest{ask("bob", "ann", time.Hour), ask("bob", "ann", 9*time.Hour)},
			last: "bob 1h", perReviewer: []string{"bob 1h"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := prsRichRow()
			b.ReviewRequests = tc.list
			r := prsBoardRow(b, self)
			show := func(q *tui.RequestInfo) string {
				if q == nil {
					return ""
				}
				return q.To + " " + strconv.Itoa(int(prsNow.Sub(q.At).Hours())) + "h"
			}
			if show(r.RequestedToMe) != tc.toMe || show(r.LastRequest) != tc.last {
				t.Errorf("to me %q, last %q; want %q, %q", show(r.RequestedToMe), show(r.LastRequest), tc.toMe, tc.last)
			}
			var per []string
			for _, q := range r.Requests {
				per = append(per, show(&q))
				if want := slices.Contains([]string{"zhuravel", "Zhuravel", "talkable[bot]"}, q.To); q.Mine != want {
					t.Errorf("request to %s: mine = %v, want %v", q.To, q.Mine, want)
				}
			}
			if !slices.Equal(per, tc.perReviewer) {
				t.Errorf("per reviewer %v, want %v", per, tc.perReviewer)
			}
		})
	}
	// No self logins: nothing is mine, whoever was asked.
	if r := prsBoardRow(prsRichRow(), nil); r.RequestedToMe != nil || r.LastRequest == nil || len(r.Requests) != 3 {
		t.Errorf("without self logins: to me %+v, last %+v, %d requests", r.RequestedToMe, r.LastRequest, len(r.Requests))
	}
}

func TestPRsSelfLogins(t *testing.T) {
	cfg := &config.Config{
		Identities: []config.Identity{
			{Name: "me", Kind: "gh", Login: "zhuravel"},
			{Name: "app", Kind: "app", Login: "talkable[bot]"},
			{Name: "other-app", Kind: "app", Login: "unused[bot]"},
			{Name: "me-again", Kind: "gh", Login: "@Zhuravel"},
		},
		Watches: []config.Watch{{Owner: "talkable", Identity: "app"}, {Owner: "x", Identity: "me"}, {Owner: "y", Identity: "missing"}},
	}
	if got := prsSelfLogins(cfg); !slices.Equal(got, []string{"talkable[bot]", "zhuravel"}) {
		t.Fatalf("self logins %v (watch identities, then gh identities; app identities no watch posts as are not you)", got)
	}
	if prsSelfLogins(nil) != nil {
		t.Fatal("no config: no self logins")
	}
}

func TestPRsRenderCells(t *testing.T) {
	r := prsBoardRow(prsRichRow(), nil)
	var b strings.Builder
	prsRender(&b, []tui.PRBoardRow{r}, "talkable/talkable", prsNow)
	out := b.String()
	for _, want := range []string{
		"REF", "TITLE", "AUTHOR", "ASSIGNEE", "UPDATED", "REQUESTED", "STATE", "LAST REVIEW", "SINCE", "REVIEWERS",
		"talkable#11920", "Fix the widget", "ann", "bob,cat", "30m", "reviewing,draft,pinned,muted,error",
		"commented 1h by talkable, stale", "2c 3f +40/-5",
		"cat(approved,re-requested)", "bob(changes_requested,stale)", "dan(requested)", "team:core(requested)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if got := prsSinceCell(&tui.ReviewDelta{Base: "base branch", Commits: 1, Files: 300, Truncated: true}); got != "PR >=1c 300f +0/-0" {
		t.Errorf("since cell %q", got)
	}
	if got := prsLastReviewCell(nil, prsNow); got != "-" {
		t.Errorf("no review cell %q", got)
	}
	// The request the board shows: the latest to me, else the latest to anyone.
	mine := prsBoardRow(prsRichRow(), []string{"zhuravel"})
	for _, tc := range []struct {
		name string
		row  tui.PRBoardRow
		want string
	}{
		{"to me", mine, "me 2h by ann"},
		{"to someone else", prsBoardRow(prsRichRow(), nil), "bob 1h by ann"},
		{"nobody asked", tui.PRBoardRow{}, "-"},
		{"by someone who left", tui.PRBoardRow{LastRequest: &tui.RequestInfo{To: "team:core", At: prsNow.Add(-26 * time.Hour)}}, "team:core 26h"},
	} {
		if got := prsRequestedCell(tc.row, prsNow); got != tc.want {
			t.Errorf("%s: requested cell %q, want %q", tc.name, got, tc.want)
		}
	}
	b.Reset()
	prsRender(&b, nil, "", prsNow)
	if !strings.Contains(b.String(), "no pull requests") {
		t.Errorf("empty table %q", b.String())
	}
}

// prsSeed seeds three PRs with board fields: one reviewed (stale, its review
// requested of the user five hours ago), one queued (its review requested of
// someone else an hour ago) and one merged.
func prsSeed(t *testing.T, f *inspFixture) {
	t.Helper()
	st := f.store()
	ts := func(d time.Duration) string { return store.FormatTime(time.Now().Add(-d)) }
	inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", ts(3*time.Hour))
		u.Set("reviewed_sha", "reviewedsha")
		u.Set("last_review_event", "APPROVED")
		u.Set("reviewed_at", ts(2*time.Hour))
		u.Set("last_review_login", "talkable")
		u.Set("assignees_json", `["bob"]`)
		u.Set("requested_reviewers_json", `["dan","team:core"]`)
		u.Set("latest_reviews_json", `[{"login":"cat","state":"CHANGES_REQUESTED","submitted_at":"`+ts(time.Hour)+`","commit_sha":"reviewedsha"}]`)
		u.Set("since_review_json", `{"source":"reviewed","base":"reviewedsha","head":"x","commits":2,"files":-1,"additions":10,"deletions":1}`)
		u.Set("review_requests_json", `[{"at":"`+ts(5*time.Hour)+`","by":"alice","to":"zhuravel"}]`)
	})
	inspSeedPR(t, st, "talkable/talkable", 11931, store.PRQueued, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", ts(time.Minute))
		u.Set("review_requests_json", `[{"at":"`+ts(time.Hour)+`","by":"alice","to":"bob"}]`)
	})
	inspSeedPR(t, st, "zhuravel/app", 3, store.PRReleased, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", ts(time.Hour))
		u.Set("gh_state", store.GHMerged)
	})
	st.Close()
}

func TestPRsPrintsTableAndJSON(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)

	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "talkable#11931") || !strings.HasPrefix(lines[2], "talkable#11920") {
		t.Fatalf("open PRs, newest update first:\n%s", out)
	}
	for _, want := range []string{"approved 2h by talkable, stale", ">=2c 300f +10/-1", "cat(changes_requested,stale)", "team:core(requested)", "bob", "me 5h by alice"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("row lacks %q:\n%s", want, lines[2])
		}
	}
	if !strings.Contains(lines[0], "REQUESTED") || !strings.Contains(lines[1], "bob 1h by alice") {
		t.Errorf("the REQUESTED column:\n%s", out)
	}

	if code := f.run("prs", "--all", "--sort", "last-review", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, f.Out.String())
	}
	if refs := prsJSONRefs(rows); !slices.Equal(refs, []string{"talkable/talkable#11920", "talkable/talkable#11931", "zhuravel/app#3"}) {
		t.Fatalf("--all --sort last-review: %v (reviewed first, then newest update)", refs)
	}
	if r := rows[0]; r.LastReview == nil || !r.LastReview.Mine || !r.SinceReview.Truncated || len(r.Reviewers) != 3 {
		t.Errorf("mapped row %+v", r)
	}
	if rows[2].GHState != store.GHMerged {
		t.Errorf("merged row %+v", rows[2])
	}
	if q := rows[0].RequestedToMe; q == nil || q.To != "zhuravel" || q.By != "alice" || !q.Mine || rows[0].LastRequest == nil || len(rows[0].Requests) != 1 ||
		rows[1].RequestedToMe != nil || rows[1].LastRequest == nil || rows[1].LastRequest.To != "bob" || rows[2].LastRequest != nil {
		t.Errorf("review requests of the mapped rows: %+v / %+v / %+v", rows[0].RequestedToMe, rows[1].LastRequest, rows[2].LastRequest)
	}

	// The newest request first, whatever it asked; PRs nobody asked come last.
	if code := f.run("prs", "--all", "--sort", "requested", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, f.Out.String())
	}
	if refs := prsJSONRefs(rows); !slices.Equal(refs, []string{"talkable/talkable#11931", "talkable/talkable#11920", "zhuravel/app#3"}) {
		t.Fatalf("--all --sort requested: %v (the hour-old request, the five-hour-old one, then none)", refs)
	}
	if code := f.run("prs", "--all", "--sort", "requested", "--desc=false", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, f.Out.String())
	}
	if refs := prsJSONRefs(rows); !slices.Equal(refs, []string{"talkable/talkable#11920", "talkable/talkable#11931", "zhuravel/app#3"}) {
		t.Fatalf("--all --sort requested --desc=false: %v (oldest request first, none still last)", refs)
	}

	if code := f.run("prs", "--repo", "app", "--all", "--json"); code != 0 || !strings.Contains(f.Out.String(), "zhuravel/app#3") ||
		strings.Contains(f.Out.String(), "talkable#") {
		t.Fatalf("--repo app: code %d\n%s", code, f.Out.String())
	}
	if code := f.run("prs", "--desc=false", "--limit", "1", "--json"); code != 0 {
		t.Fatalf("code %d", code)
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Ref != "talkable/talkable#11920" {
		t.Fatalf("--desc=false --limit 1 must be the oldest update: %v %v", err, prsJSONRefs(rows))
	}
	// The limit cuts after the requested sort, not after the registry's
	// newest-update order.
	if code := f.run("prs", "--all", "--sort", "last-review", "--limit", "1", "--json"); code != 0 {
		t.Fatalf("code %d", code)
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Ref != "talkable/talkable#11920" {
		t.Fatalf("--sort last-review --limit 1: %v %v", err, prsJSONRefs(rows))
	}
	if code := f.run("prs", "--repo", "nope", "--json"); code != 0 || strings.TrimSpace(f.Out.String()) != "[]" {
		t.Fatalf("no rows: code %d %q", code, f.Out.String())
	}
}

func prsJSONRefs(rows []prsJSONRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Ref)
	}
	return out
}

func prsRefs(rows []tui.PRBoardRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Ref)
	}
	return out
}

func TestPRsUsageErrors(t *testing.T) {
	f := newInspFixture(t)
	for _, args := range [][]string{
		{"--sort", "bogus"}, {"--limit", "-1"}, {"--repo", "talkable/"}, {"--repo", "a/b/c"}, {"extra"},
	} {
		if code := f.run("prs", args...); code != 2 {
			t.Errorf("prs %v: code %d, want 2 (stderr %s)", args, code, f.Err.String())
		}
	}
	if code := f.run("prs", "--sort", "bogus"); code != 2 || !strings.Contains(f.Err.String(), "usage: magnum prs") {
		t.Errorf("bad sort message: %s", f.Err.String())
	}
	for _, s := range []string{"last_review", "Reviewer Activity", "Requested", "CHANGES", "state", ""} {
		if code := f.run("prs", "--sort", s, "--json"); code != 0 {
			t.Errorf("--sort %q: code %d %s", s, code, f.Err.String())
		}
	}
}

func TestPRsOpensTheBoard(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	onScreen(t)
	var opts tui.PRBoardOptions
	var rows []tui.PRBoardRow
	var acts tui.DashboardActions
	old := tuiPRBoard
	tuiPRBoard = func(ctx context.Context, src tui.PRBoardSource, act tui.DashboardActions, o tui.PRBoardOptions) error {
		opts, acts = o, act
		var err error
		rows, err = src.Rows(ctx)
		return err
	}
	t.Cleanup(func() { tuiPRBoard = old })

	if code := f.run("prs", "--repo", "talkable/talkable", "--sort", "changes"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if f.Out.Len() != 0 {
		t.Errorf("printed while the screen ran:\n%s", f.Out.String())
	}
	if opts.Repo != "talkable/talkable" || opts.DefaultSort != tui.SortChanges || acts == nil ||
		!slices.Equal(opts.SelfLogins, []string{"talkable[bot]", "zhuravel"}) {
		t.Errorf("options %+v actions %v", opts, acts)
	}
	if opts.NoMouse || opts.MouseToggled == nil || opts.Widths == nil {
		t.Errorf("mouse options: NoMouse %v, toggle hook %v, widths %v; want the mouse on and widths kept", opts.NoMouse, opts.MouseToggled != nil, opts.Widths)
	}
	if refs := prsRefs(rows); len(refs) != 2 || !slices.Contains(refs, "talkable/talkable#11920") {
		t.Errorf("rows %v", refs)
	}
	for _, r := range rows {
		if r.Ref == "talkable/talkable#11920" && (r.RequestedToMe == nil || !r.RequestedToMe.Mine || len(r.Requests) != 1) {
			t.Errorf("the board's row lacks the review request to me: %+v", r.RequestedToMe)
		}
	}
	if code := f.run("prs", "--json"); code != 0 || !strings.HasPrefix(f.Out.String(), "[") {
		t.Errorf("--json on a terminal prints: code %d %q", code, f.Out.String())
	}
}

func TestRunScreensSwitches(t *testing.T) {
	ctx := context.Background()
	var trail []string
	script := func(name string, results ...error) func(context.Context) error {
		return func(context.Context) error {
			trail = append(trail, name)
			err := results[0]
			if len(results) > 1 {
				results = results[1:]
			}
			return err
		}
	}
	dash := script("dashboard", tui.ErrSwitchToBoard, nil)
	board := script("board", tui.ErrSwitchToDashboard, nil)
	if err := runScreens(ctx, screenBoard, dash, board); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(trail, []string{"board", "dashboard", "board"}) {
		t.Fatalf("trail %v", trail)
	}

	trail = nil
	boom := errors.New("boom")
	if err := runScreens(ctx, screenDashboard, script("dashboard", tui.ErrSwitchToBoard), script("board", boom)); !errors.Is(err, boom) {
		t.Fatalf("a screen's error: %v", err)
	}
	if !slices.Equal(trail, []string{"dashboard", "board"}) {
		t.Fatalf("trail %v", trail)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	trail = nil
	if err := runScreens(cctx, screenDashboard, script("dashboard", tui.ErrSwitchToBoard), script("board", nil)); err != nil || len(trail) != 1 {
		t.Fatalf("ended ctx: %v trail %v", err, trail)
	}
}

func TestStatusWatchSwitchesToTheBoardAndBack(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	onScreen(t)
	var trail []string
	var dashActs, boardActs tui.DashboardActions
	var rows []tui.PRBoardRow
	var boardOpts tui.PRBoardOptions
	oldDash, oldBoard := tuiDashboard, tuiPRBoard
	t.Cleanup(func() { tuiDashboard, tuiPRBoard = oldDash, oldBoard })
	var dashMouse []bool
	tuiDashboard = func(ctx context.Context, src tui.DashboardSource, act tui.DashboardActions, o tui.DashboardOptions) error {
		trail = append(trail, "dashboard")
		dashActs = act
		dashMouse = append(dashMouse, !o.NoMouse)
		if len(trail) == 1 {
			o.MouseToggled(false) // m on the dashboard
			return tui.ErrSwitchToBoard
		}
		return nil
	}
	tuiPRBoard = func(ctx context.Context, src tui.PRBoardSource, act tui.DashboardActions, o tui.PRBoardOptions) error {
		trail = append(trail, "board")
		boardActs, boardOpts = act, o
		var err error
		if rows, err = src.Rows(ctx); err != nil {
			return err
		}
		return tui.ErrSwitchToDashboard
	}

	if code := f.run("status", "--watch"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if !slices.Equal(trail, []string{"dashboard", "board", "dashboard"}) {
		t.Fatalf("trail %v", trail)
	}
	if dashActs == nil || dashActs != boardActs {
		t.Errorf("the screens must share one set of actions: %v %v", dashActs, boardActs)
	}
	if len(rows) != 2 || boardOpts.Repo != "" || boardOpts.DefaultSort != tui.SortUpdated {
		t.Errorf("board from status: %d rows, options %+v", len(rows), boardOpts)
	}
	if !boardOpts.NoMouse || !slices.Equal(dashMouse, []bool{true, false}) {
		t.Errorf("m on one screen must carry over: board NoMouse %v, dashboard mouse per run %v", boardOpts.NoMouse, dashMouse)
	}
}

// The screens keep dragged column widths in the registry's kv table, per
// screen, and an empty map forgets them.
func TestScreensKeepColumnWidths(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	w := kvColumnWidths{st: st}
	if got, err := w.LoadWidths(ctx, "board"); err != nil || got != nil {
		t.Fatalf("nothing kept yet: %v, %v", got, err)
	}
	if err := w.SaveWidths(ctx, "board", map[string]int{"title": 40, "author": 9}); err != nil {
		t.Fatal(err)
	}
	if err := w.SaveWidths(ctx, "dashboard", map[string]int{"queue.title": 30}); err != nil {
		t.Fatal(err)
	}
	if got, err := w.LoadWidths(ctx, "board"); err != nil || got["title"] != 40 || got["author"] != 9 || len(got) != 2 {
		t.Fatalf("board widths %v, %v", got, err)
	}
	if v, ok, _ := st.GetKV(ctx, store.KVScreenWidths("dashboard")); !ok || v != `{"queue.title":30}` {
		t.Fatalf("dashboard kv %q %v", v, ok)
	}
	if err := w.SaveWidths(ctx, "board", map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetKV(ctx, store.KVScreenWidths("board")); ok {
		t.Fatal("an empty map must forget the board's widths")
	}
	if err := st.SetKV(ctx, store.KVScreenWidths("board"), "not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.LoadWidths(ctx, "board"); err == nil || !strings.Contains(err.Error(), "column widths of the board") {
		t.Fatalf("a broken value: %v", err)
	}
}

// The board's layout (L) is kept in the registry: the next board, in this
// run after tab or in the next run, opens in it.
func TestBoardKeepsItsLayout(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	onScreen(t)
	var opened []tui.PRLayout
	oldDash, oldBoard := tuiDashboard, tuiPRBoard
	t.Cleanup(func() { tuiDashboard, tuiPRBoard = oldDash, oldBoard })
	tuiDashboard = func(context.Context, tui.DashboardSource, tui.DashboardActions, tui.DashboardOptions) error {
		return tui.ErrSwitchToBoard
	}
	tuiPRBoard = func(_ context.Context, _ tui.PRBoardSource, _ tui.DashboardActions, o tui.PRBoardOptions) error {
		opened = append(opened, o.Layout)
		if len(opened) == 1 {
			o.LayoutChanged(tui.LayoutTwoLines) // L on the board, then tab
			return tui.ErrSwitchToDashboard
		}
		return nil
	}
	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if want := []tui.PRLayout{tui.LayoutAuto, tui.LayoutTwoLines, tui.LayoutTwoLines}; !slices.Equal(opened, want) {
		t.Fatalf("board layouts %v, want %v", opened, want)
	}
	if v, ok, _ := f.store().GetKV(context.Background(), kvBoardLayout); !ok || v != "2-line" {
		t.Fatalf("kept %q %v", v, ok)
	}
}

// [terminal] mouse = false starts both screens with the mouse off.
func TestScreensFollowTheMouseSetting(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	onScreen(t)
	cfg, err := os.ReadFile(filepath.Join(f.Home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Home, "config.toml"), append(cfg, []byte("\n[terminal]\nmouse = false\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var opts tui.PRBoardOptions
	old := tuiPRBoard
	tuiPRBoard = func(ctx context.Context, src tui.PRBoardSource, act tui.DashboardActions, o tui.PRBoardOptions) error {
		opts = o
		return nil
	}
	t.Cleanup(func() { tuiPRBoard = old })
	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if !opts.NoMouse {
		t.Error("[terminal] mouse = false must start the board with the mouse off")
	}
}

func TestPRsCompletion(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	var got []string
	for _, c := range f.Ctx.completeRepos("") {
		got = append(got, strings.SplitN(string(c), "\t", 2)[0])
	}
	if !slices.Equal(got, []string{"talkable/talkable", "talkable", "zhuravel/app"}) {
		t.Errorf("repos %v (owner/name, plus the bare name in the default owner)", got)
	}
	var sorts []string
	for _, c := range completeSorts("") {
		sorts = append(sorts, strings.SplitN(string(c), "\t", 2)[0])
	}
	var want []string
	for _, s := range tui.PRSorts() {
		want = append(want, string(s))
	}
	if !slices.Equal(sorts, want) {
		t.Errorf("sorts %v, want %v", sorts, want)
	}

	if code := f.run(cobra.ShellCompRequestCmd, "prs", "--sort", "la"); code != 0 || !strings.Contains(f.Out.String(), "last-review") {
		t.Errorf("--sort completion: code %d\n%s", code, f.Out.String())
	}
	if code := f.run(cobra.ShellCompRequestCmd, "prs", "--repo", ""); code != 0 || !strings.Contains(f.Out.String(), "zhuravel/app") {
		t.Errorf("--repo completion: code %d\n%s", code, f.Out.String())
	}
}

func TestPRsRowStateShowsIgnored(t *testing.T) {
	for _, tc := range []struct {
		state, skip string
		muted       bool
		want        string
	}{
		{store.PRIneligible, engine.SkipIgnored, true, "ignored"},
		{store.PRReviewing, engine.SkipIgnored, true, "ignored"},
		{store.PRIneligible, "muted", true, "ineligible"},
		{store.PRIneligible, engine.SkipIgnored, false, "ineligible"}, // unmuted: the mark is stale
		{store.PRClosed, engine.SkipIgnored, true, "closed"},
		{store.PRReleasing, engine.SkipIgnored, true, "closed"},
	} {
		b := prsRichRow()
		b.State, b.SkipReason, b.Muted = tc.state, tc.skip, tc.muted
		if got := prsBoardRow(b, nil).State; got != tc.want {
			t.Errorf("%s/%q/muted=%v: %s, want %s", tc.state, tc.skip, tc.muted, got, tc.want)
		}
	}
}

func TestPRsSourceKnowsWhichRepositoriesHaveNotes(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedPR("talkable/talkable", 6, store.PRQueued)
	h.seedPR("zhuravel/magnum", 7, store.PRQueued)
	path := engine.NotesPath(h.c.Layout, "talkable", "talkable")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# Notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := prsSource(h.st, nil, store.BoardFilter{}, nil, h.c.Layout)(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Ref] = r.Notes
	}
	want := map[string]bool{"talkable/talkable#5": true, "talkable/talkable#6": true, "zhuravel/magnum#7": false}
	if !maps.Equal(got, want) {
		t.Fatalf("notes %v, want %v", got, want)
	}
}

// --view picks the printed rows and the view the board opens in; a view
// chosen with v survives a trip to the dashboard and back.
func TestPRsView(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 11000, store.PRBaseline, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", store.FormatTime(time.Now().Add(-48*time.Hour)))
	})
	inspSeedPR(t, st, "talkable/talkable", 11950, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("gh_updated_at", store.FormatTime(time.Now().Add(-5*time.Hour)))
		u.Set("requested_reviewers_json", `["zhuravel"]`)
		u.Set("head_sha", "x") // approved on its current head: not stale, so the ready view keeps it
		u.Set("latest_reviews_json", `[{"login":"ann","state":"APPROVED","submitted_at":"`+store.FormatTime(time.Now())+`","commit_sha":"x"}]`)
	})
	st.Close()

	for view, want := range map[string][]string{
		"all":    {"talkable/talkable#11931", "talkable/talkable#11920", "talkable/talkable#11950", "talkable/talkable#11000"},
		"magnum": {"talkable/talkable#11931", "talkable/talkable#11920", "talkable/talkable#11950"},
		"Mine":   {"talkable/talkable#11950"},
		"ready":  {"talkable/talkable#11950"},
	} {
		if code := f.run("prs", "--view", view, "--json"); code != 0 {
			t.Fatalf("--view %s: code %d err %s", view, code, f.Err.String())
		}
		var rows []prsJSONRow
		if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if got := prsJSONRefs(rows); !slices.Equal(got, want) {
			t.Errorf("--view %s = %v, want %v", view, got, want)
		}
	}
	if code := f.run("prs", "--view", "bogus"); code != 2 || !strings.Contains(f.Err.String(), "all, magnum, mine, ready") {
		t.Errorf("--view bogus: code %d %s", code, f.Err.String())
	}
	var views []string
	for _, c := range completeViews("") {
		views = append(views, strings.SplitN(string(c), "\t", 2)[0])
	}
	if !slices.Equal(views, []string{"all", "magnum", "mine", "ready"}) {
		t.Errorf("view completion %v", views)
	}

	onScreen(t)
	var opened []tui.PRView
	oldDash, oldBoard := tuiDashboard, tuiPRBoard
	t.Cleanup(func() { tuiDashboard, tuiPRBoard = oldDash, oldBoard })
	tuiDashboard = func(context.Context, tui.DashboardSource, tui.DashboardActions, tui.DashboardOptions) error {
		return tui.ErrSwitchToBoard
	}
	tuiPRBoard = func(ctx context.Context, src tui.PRBoardSource, act tui.DashboardActions, o tui.PRBoardOptions) error {
		opened = append(opened, o.DefaultView)
		if len(opened) == 1 {
			o.ViewChanged(tui.ViewReady) // v on the board, then tab
			return tui.ErrSwitchToDashboard
		}
		return nil
	}
	if code := f.run("prs", "--view", "mine"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if !slices.Equal(opened, []tui.PRView{tui.ViewMine, tui.ViewReady}) {
		t.Fatalf("board views %v, want mine then ready", opened)
	}
}

// Board rows carry the engine's wait reason and the trivial-skip note.
func TestPRsSourceCarriesWaitAndTrivialNote(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "RW", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PW", Number: 77, URL: "u", HeadSHA: "abc",
		InitialState: store.PRRereviewPending, Identity: "talkable-app"})
	if err != nil {
		t.Fatal(err)
	}
	w := engine.Wait{Reason: "quiet", Rereview: true, Until: time.Now().Add(30 * time.Minute)}
	b, _ := json.Marshal(w)
	if err := st.SetKV(ctx, engine.KVPRWait(res.PR.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	ts := engine.TrivialSkip{From: "a7b3f8c0", To: "602da9d0", Classes: []string{"comments"}}
	b, _ = json.Marshal(ts)
	if err := st.SetKV(ctx, engine.KVPRTrivial(res.PR.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	rows, err := prsSource(st, nil, store.BoardFilter{}, nil, f.Ctx.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *tui.PRBoardRow
	for i := range rows {
		if rows[i].Number == 77 {
			got = &rows[i]
		}
	}
	if got == nil || got.Wait == "" || got.WaitDetail == "" || !strings.Contains(got.Note, "skipped") {
		t.Fatalf("row = %+v", got)
	}
}

// [terminal] icons reaches both screens; without a configuration they
// draw Unicode symbols.
func TestScreensDrawTheConfiguredIcons(t *testing.T) {
	f := newInspFixture(t)
	path := filepath.Join(f.Home, "config.toml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(cfg, "\n[terminal]\nicons = \"nerd\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	prsSeed(t, f)
	onScreen(t)
	var dash, board tui.IconMode
	oldDash, oldBoard := tuiDashboard, tuiPRBoard
	t.Cleanup(func() { tuiDashboard, tuiPRBoard = oldDash, oldBoard })
	tuiDashboard = func(_ context.Context, _ tui.DashboardSource, _ tui.DashboardActions, o tui.DashboardOptions) error {
		dash = o.Icons
		return tui.ErrSwitchToBoard
	}
	tuiPRBoard = func(_ context.Context, _ tui.PRBoardSource, _ tui.DashboardActions, o tui.PRBoardOptions) error {
		board = o.Icons
		return nil
	}
	if code := f.run("status", "--watch"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if dash != tui.IconsNerd || board != tui.IconsNerd {
		t.Errorf("icons = nerd: dashboard draws %q, board %q", dash, board)
	}
	if got := prsBoardOptions(nil, prsOptions{}).Icons; got != tui.IconsUnicode {
		t.Errorf("no configuration: the board draws %q", got)
	}
}

// The plain table names what magnum's latest review concluded.
func TestPRsTableFindingsCell(t *testing.T) {
	for _, tc := range []struct {
		f    *tui.FindingsInfo
		want string
	}{
		{nil, "-"},
		{&tui.FindingsInfo{Verdict: "clean"}, "clean"},
		{&tui.FindingsInfo{Verdict: "blocking", Counts: [4]int{0, 1, 3, 0}, Simplifications: 4}, "blocking P1:1 P2:3 simplify:4"},
		{&tui.FindingsInfo{Verdict: "non_blocking", Counts: [4]int{0, 0, 0, 2}}, "non-blocking P3:2"},
	} {
		if got := prsFindingsCell(tc.f); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}
