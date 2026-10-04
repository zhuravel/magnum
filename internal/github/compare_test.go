package github

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// compare*.json were captured on 2026-10-03 with read-only
// `gh api repos/talkable/talkable/compare/<base>...<head>?per_page=1` calls and
// trimmed with jq (patches, file names, URLs and commit bodies removed; the
// fields Compare reads are untouched): compare.json is #11984 (1 file),
// compare_truncated.json is #11939 (311 changed files, GitHub lists 300).

const (
	cmpBase = "e1286c81e31f4ef8634ab5b128564aa4b2f00589"
	cmpHead = "3cdf82c3f06b32e07dcee9396ae32e904ff33ae3"
)

func TestCompareFixture(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: fixture(t, "compare.json")}}}}
	got, err := (&Client{Run: f}).Compare(context.Background(), "talkable", "talkable", cmpBase, cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CompareStats{Commits: 1, Files: 1, Additions: 178, Deletions: 0}); got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
	c := f.Calls[0]
	want := []string{"api", "repos/talkable/talkable/compare/" + cmpBase + "..." + cmpHead + "?per_page=1", "--hostname", "github.com"}
	if !reflect.DeepEqual(c.Args, want) || c.Mutates {
		t.Errorf("call = %v mutates=%v", c.Args, c.Mutates)
	}
}

func TestCompareTruncatedFileList(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: fixture(t, "compare_truncated.json")}}}}
	got, err := (&Client{Run: f}).Compare(context.Background(), "talkable", "talkable",
		"f9e52e9adf64629461446f86fcdcadabfc65da7a", "b077ce2f09c70a1c40583db1848083b6c23c4930")
	if err != nil {
		t.Fatal(err)
	}
	// 99 commits and 311 files on GitHub; the 300 listed files add up to a lower bound.
	if want := (CompareStats{Commits: 99, Files: -1, Additions: 12801, Deletions: 649}); got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}

func TestCompareNotFound(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return failResult(c, fixture(t, "compare_404.json"), fixture(t, "compare_404.stderr"))
	}}}}
	_, err := (&Client{Run: f}).Compare(context.Background(), "talkable", "talkable", "0000000000000000000000000000000000000001", cmpHead)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Message != "Not Found" {
		t.Errorf("api error = %+v", apiErr)
	}
}

func TestCompareRejectsBadRefs(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	for _, tc := range [][2]string{{"", cmpHead}, {cmpBase, "a..b"}, {"x?y", cmpHead}, {cmpBase, "-rf"}, {"a b", cmpHead}} {
		if _, err := c.Compare(context.Background(), "talkable", "talkable", tc[0], tc[1]); err == nil {
			t.Errorf("refs %q accepted", tc)
		}
	}
	if _, err := c.Compare(context.Background(), "talk able", "talkable", cmpBase, cmpHead); err == nil {
		t.Error("bad owner accepted")
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d", len(f.Calls))
	}
}
