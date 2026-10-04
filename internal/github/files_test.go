package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// filesPages serves GET repos/o/r/pulls/N/files?per_page=100&page=P from pages
// (page 1 first); a page beyond the list is empty.
func filesPages(t *testing.T, pages ...[]map[string]string) *execx.Fake {
	t.Helper()
	return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		var page int
		if _, err := fmt.Sscanf(c.Args[1][strings.Index(c.Args[1], "&page=")+1:], "page=%d", &page); err != nil {
			t.Errorf("no page in %q", c.Args[1])
		}
		body := []map[string]string{}
		if page <= len(pages) {
			body = pages[page-1]
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return okResult(b)
	}}}}
}

// filePage is n entries named <prefix>0..<prefix>n-1.
func filePage(prefix string, n int) []map[string]string {
	out := make([]map[string]string, n)
	for i := range out {
		out[i] = map[string]string{"filename": fmt.Sprintf("%s%d", prefix, i), "status": "modified"}
	}
	return out
}

func TestListFilesSinglePage(t *testing.T) {
	f := filesPages(t, []map[string]string{{"filename": "docs/a.md"}, {"filename": "src/b.go"}})
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !reflect.DeepEqual(files, []string{"docs/a.md", "src/b.go"}) {
		t.Errorf("files = %q complete = %v", files, complete)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	want := []string{"api", "repos/talkable/talkable/pulls/42/files?per_page=100&page=1", "--hostname", "github.com"}
	if c := f.Calls[0]; !reflect.DeepEqual(c.Args, want) || c.Mutates {
		t.Errorf("call = %v mutates=%v", c.Args, c.Mutates)
	}
}

func TestListFilesNoFiles(t *testing.T) {
	files, complete, err := (&Client{Run: filesPages(t)}).ListFiles(context.Background(), "talkable", "talkable", 1)
	if err != nil || !complete || len(files) != 0 {
		t.Errorf("files = %q complete = %v err = %v", files, complete, err)
	}
}

func TestListFilesStopsAtShortPage(t *testing.T) {
	f := filesPages(t, filePage("a", 100), filePage("b", 7), filePage("c", 100))
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || len(files) != 107 || files[0] != "a0" || files[106] != "b6" {
		t.Errorf("len = %d complete = %v", len(files), complete)
	}
	if len(f.Calls) != 2 {
		t.Errorf("calls = %d, want 2 (no third request after a short page)", len(f.Calls))
	}
}

func TestListFilesThreeFullPagesIsIncomplete(t *testing.T) {
	f := filesPages(t, filePage("a", 100), filePage("b", 100), filePage("c", 100), filePage("d", 100))
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Error("complete = true after three full pages")
	}
	if len(files) != 300 || files[299] != "c99" {
		t.Errorf("len = %d", len(files))
	}
	if len(f.Calls) != 3 {
		t.Fatalf("calls = %d, want 3 (pages 1..3 only)", len(f.Calls))
	}
	for i, c := range f.Calls {
		if want := fmt.Sprintf("repos/talkable/talkable/pulls/5/files?per_page=100&page=%d", i+1); c.Args[1] != want {
			t.Errorf("call %d = %q, want %q", i, c.Args[1], want)
		}
	}
}

func TestListFilesExactlyThreePagesThenShortStillStops(t *testing.T) {
	// 250 files: two full pages and a short third.
	f := filesPages(t, filePage("a", 100), filePage("b", 100), filePage("c", 50))
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 5)
	if err != nil || !complete || len(files) != 250 {
		t.Errorf("len = %d complete = %v err = %v", len(files), complete, err)
	}
}

func TestListFilesRenameIncludesPreviousName(t *testing.T) {
	f := filesPages(t, []map[string]string{
		{"filename": "docs/main.go", "previous_filename": "src/main.go", "status": "renamed"},
		{"filename": "README.md", "previous_filename": "README.md", "status": "renamed"},
		{"filename": "new.txt", "status": "added"},
	})
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 9)
	if err != nil || !complete {
		t.Fatalf("complete = %v err = %v", complete, err)
	}
	want := []string{"docs/main.go", "src/main.go", "README.md", "new.txt"}
	if !slices.Equal(files, want) {
		t.Errorf("files = %q, want %q", files, want)
	}
}

func TestListFilesAPIErrorPropagates(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return failResult(c, []byte(`{"message":"Not Found","status":"404"}`), []byte("gh: Not Found (HTTP 404)\n"))
	}}}}
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 9)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if files != nil || complete {
		t.Errorf("files = %q complete = %v on error", files, complete)
	}
}

func TestListFilesErrorOnLaterPageDropsPartialList(t *testing.T) {
	calls := 0
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		calls++
		if calls == 2 {
			return failResult(c, []byte(`{"message":"Server Error"}`), []byte("gh: Server Error (HTTP 502)\n"))
		}
		b, _ := json.Marshal(filePage("a", 100))
		return okResult(b)
	}}}}
	files, complete, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 9)
	if err == nil || files != nil || complete {
		t.Errorf("files = %d complete = %v err = %v", len(files), complete, err)
	}
}

func TestListFilesBadResponse(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: []byte(`{"message":"not a list"}`)}}}}
	if _, _, err := (&Client{Run: f}).ListFiles(context.Background(), "talkable", "talkable", 9); err == nil {
		t.Error("an object instead of a list was accepted")
	}
}

func TestListFilesRejectsBadArguments(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	if _, _, err := c.ListFiles(context.Background(), "talk able", "talkable", 1); err == nil {
		t.Error("bad owner accepted")
	}
	if _, _, err := c.ListFiles(context.Background(), "talkable", "..", 1); err == nil {
		t.Error("bad repo accepted")
	}
	if _, _, err := c.ListFiles(context.Background(), "talkable", "talkable", 0); err == nil {
		t.Error("number 0 accepted")
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d", len(f.Calls))
	}
}
