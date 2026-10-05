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
