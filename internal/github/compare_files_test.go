package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// compareFilesClient answers every `gh api` call with body.
func compareFilesClient(body string) (*Client, *execx.Fake) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: []byte(body)}}}}
	return &Client{Run: f}, f
}

// listingOf builds a compare response with n files, each carrying a patch.
func listingOf(n int) string {
	var files []string
	for i := 0; i < n; i++ {
		files = append(files, fmt.Sprintf(`{"filename":"app/f%d.rb","status":"modified","patch":"@@ -1 +1 @@\n-a\n+b"}`, i))
	}
	return `{"files":[` + strings.Join(files, ",") + `]}`
}

func TestCompareFilesReadsPatchesRenamesAndBinaries(t *testing.T) {
	c, f := compareFilesClient(`{"total_commits":2,"files":[
		{"filename":"config/app.yml","status":"modified","additions":1,"deletions":1,"patch":"@@ -1,2 +1,2 @@\n key: a\n-old: 1\n+new: 2"},
		{"filename":"lib/new_name.rb","previous_filename":"lib/old_name.rb","status":"renamed","patch":"@@ -3 +3 @@\n-x\n+y"},
		{"filename":"public/logo.png","status":"added","additions":0,"deletions":0}
	]}`)
	got, err := c.CompareFiles(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileDelta{
		{Path: "config/app.yml", Status: "modified", Patch: "@@ -1,2 +1,2 @@\n key: a\n-old: 1\n+new: 2"},
		{Path: "lib/new_name.rb", PreviousPath: "lib/old_name.rb", Status: "renamed", Patch: "@@ -3 +3 @@\n-x\n+y"},
		{Path: "public/logo.png", Status: "added", Truncated: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deltas = %+v\nwant     %+v", got, want)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	call := f.Calls[0]
	wantArgs := []string{"api", "repos/talkable/talkable/compare/" + cmpBase + "..." + cmpHead + "?per_page=1", "--hostname", "github.com"}
	if !reflect.DeepEqual(call.Args, wantArgs) || call.Mutates {
		t.Errorf("call = %v mutates=%v", call.Args, call.Mutates)
	}
}

func TestCompareFilesAtTheListingLimitMarksEveryFileTruncated(t *testing.T) {
	c, _ := compareFilesClient(listingOf(CompareFileLimit))
	got, err := c.CompareFiles(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != CompareFileLimit {
		t.Fatalf("files = %d, want %d", len(got), CompareFileLimit)
	}
	for i, d := range got {
		if !d.Truncated {
			t.Fatalf("file %d (%s) not marked Truncated although the listing is at the limit", i, d.Path)
		}
		if d.Patch == "" {
			t.Fatalf("file %d (%s) lost its patch", i, d.Path)
		}
	}
}

func TestCompareFilesBelowTheListingLimitKeepsPatchedFilesComplete(t *testing.T) {
	c, _ := compareFilesClient(listingOf(CompareFileLimit - 1))
	got, err := c.CompareFiles(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != CompareFileLimit-1 {
		t.Fatalf("files = %d, want %d", len(got), CompareFileLimit-1)
	}
	for i, d := range got {
		if d.Truncated {
			t.Fatalf("file %d (%s) marked Truncated below the limit", i, d.Path)
		}
	}
}

func TestCompareFilesNotFound(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return failResult(c, fixture(t, "compare_404.json"), fixture(t, "compare_404.stderr"))
	}}}}
	got, err := (&Client{Run: f}).CompareFiles(context.Background(), "talkable", "talkable", "0000000000000000000000000000000000000001", cmpHead)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Errorf("deltas = %+v, want none", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Message != "Not Found" {
		t.Errorf("api error = %+v", apiErr)
	}
}

func TestCompareFilesRefusesBadRefsAndReposBeforeAnyCall(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	for _, tc := range [][2]string{{"a..b", cmpHead}, {cmpBase, "a..b"}, {"x y", cmpHead}, {cmpBase, "x y"}, {"", cmpHead}, {cmpBase, ""}, {cmpBase, "-rf"}, {"x?y", cmpHead}} {
		if got, err := c.CompareFiles(context.Background(), "talkable", "talkable", tc[0], tc[1]); err == nil || got != nil {
			t.Errorf("refs %q accepted: %+v, %v", tc, got, err)
		}
	}
	for _, tc := range [][2]string{{"talk able", "talkable"}, {"talkable", "tal/kable"}, {"", "talkable"}, {"talkable", ""}} {
		if got, err := c.CompareFiles(context.Background(), tc[0], tc[1], cmpBase, cmpHead); err == nil || got != nil {
			t.Errorf("repo %q accepted: %+v, %v", tc, got, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.Calls))
	}
}

func TestCompareFilesEmptyPatchIsCompleteNotTruncated(t *testing.T) {
	// GitHub sends "patch": "" for some mode-only changes: the diff is empty,
	// not missing.
	c, _ := compareFilesClient(`{"files":[{"filename":"bin/run","status":"modified","patch":""}]}`)
	got, err := c.CompareFiles(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileDelta{{Path: "bin/run", Status: "modified", Patch: "", Truncated: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deltas = %+v, want %+v", got, want)
	}
}

// TestCompareFilesStatusSaysHowHeadRelatesToBase: the same one call also
// returns GitHub's status of the comparison ("ahead": head descends from
// base; "behind", "diverged", "identical"), which tells whether a later
// commit is a descendant of an earlier one.
func TestCompareFilesStatusSaysHowHeadRelatesToBase(t *testing.T) {
	for _, status := range []string{"ahead", "behind", "diverged", "identical"} {
		c, f := compareFilesClient(`{"status":"` + status + `","files":[{"filename":"a.rb","status":"modified","patch":"@@ -1 +1 @@\n-a\n+b"}]}`)
		got, files, err := c.CompareFilesStatus(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
		if err != nil || got != status || len(files) != 1 || files[0].Path != "a.rb" || len(f.Calls) != 1 {
			t.Fatalf("%s: status %q, files %+v, %v, calls %d", status, got, files, err, len(f.Calls))
		}
	}
}

// ComparePush tells a push that merged the base branch (a commit with two
// parents) from a plain one, and cannot rule a merge out when GitHub listed
// fewer commits than the range has; it keeps the SHAs of the commits listed.
func TestComparePushFindsAMergeCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		merge   bool
		commits int
		shas    []string
	}{
		{"plain push", `{"status":"ahead","total_commits":2,"commits":[{"sha":"c1","parents":[{"sha":"p0"}]},{"sha":"c2","parents":[{"sha":"c1"}]}],"files":[]}`, false, 2, []string{"c1", "c2"}},
		{"base merged", `{"status":"ahead","total_commits":3,"commits":[{"sha":"m1","parents":[{"sha":"m0"}]},{"sha":"m2","parents":[{"sha":"m1"}]},{"sha":"mc","parents":[{"sha":"p0"},{"sha":"m2"}]}],"files":[]}`, true, 3, []string{"m1", "m2", "mc"}},
		{"list cut short", `{"status":"ahead","total_commits":250,"commits":[{"sha":"c1","parents":[{"sha":"p0"}]}],"files":[]}`, true, 250, []string{"c1"}},
		{"rebased", `{"status":"diverged","total_commits":1,"commits":[{"sha":"c1","parents":[{"sha":"m9"}]}],"files":[]}`, false, 1, []string{"c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := compareFilesClient(tc.body)
			got, err := c.ComparePush(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
			if err != nil {
				t.Fatal(err)
			}
			if got.Merge != tc.merge || got.Commits != tc.commits || !reflect.DeepEqual(got.SHAs, tc.shas) {
				t.Errorf("merge %v commits %d shas %q, want %v %d %q", got.Merge, got.Commits, got.SHAs, tc.merge, tc.commits, tc.shas)
			}
			wantArgs := []string{"api", "repos/talkable/talkable/compare/" + cmpBase + "..." + cmpHead + "?per_page=100", "--hostname", "github.com"}
			if len(f.Calls) != 1 || !reflect.DeepEqual(f.Calls[0].Args, wantArgs) {
				t.Errorf("calls = %+v", f.Calls)
			}
		})
	}
}

func TestComparePushReadsStatusAndPatches(t *testing.T) {
	c, _ := compareFilesClient(`{"status":"diverged","total_commits":1,"commits":[{"sha":"c1","parents":[{"sha":"p0"}]}],"files":[
		{"filename":"app/a.rb","status":"modified","patch":"@@ -1 +1 @@\n-a\n+b"},
		{"filename":"public/logo.png","status":"added"}
	]}`)
	got, err := c.ComparePush(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileDelta{{Path: "app/a.rb", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"}, {Path: "public/logo.png", Status: "added", Truncated: true}}
	if got.Status != "diverged" || !reflect.DeepEqual(got.Files, want) {
		t.Errorf("comparison = %+v", got)
	}
	if _, err := c.ComparePush(context.Background(), "talkable", "talkable", "a..b", cmpHead); err == nil {
		t.Error("a bad ref was accepted")
	}
}

// A push comparison carries what Compare reads of the same range, so one
// call answers both (the engine's comparisons of a tick).
func TestComparePushCarriesTheCompareStats(t *testing.T) {
	body := `{"status":"ahead","total_commits":3,"commits":[{"sha":"c1","parents":[{"sha":"p0"}]}],"files":[
		{"filename":"app/a.rb","status":"modified","additions":4,"deletions":1,"patch":"@@ -1 +1 @@\n-a\n+b"},
		{"filename":"public/logo.png","status":"added","additions":0,"deletions":0}
	]}`
	c, _ := compareFilesClient(body)
	got, err := c.ComparePush(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := c.Compare(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CompareStats{Commits: 3, Files: 2, Additions: 4, Deletions: 1}); stats != want || got.Stats != want {
		t.Errorf("Compare %+v, ComparePush's stats %+v, want %+v", stats, got.Stats, want)
	}
	c, _ = compareFilesClient(listingOf(CompareFileLimit))
	if got, err := c.ComparePush(context.Background(), "talkable", "talkable", cmpBase, cmpHead); err != nil || got.Stats.Files != -1 {
		t.Errorf("a listing at the file cap: stats %+v (%v), want Files -1", got.Stats, err)
	}
}
