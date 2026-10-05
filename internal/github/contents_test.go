package github

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// fileAtClient answers every `gh api` call with body.
func fileAtClient(body []byte) (*Client, *execx.Fake) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: body}}}}
	return &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app"}}, f
}

func TestFileAtReturnsTheRawBytesVerbatim(t *testing.T) {
	// Not JSON, not UTF-8, CRLF and a missing final newline: nothing is
	// decoded, trimmed or normalised.
	content := []byte("class Order\r\n  # caf\xe9 \"quoted\" {\"a\":1}\r\nend")
	c, f := fileAtClient(content)
	got, err := c.FileAt(context.Background(), "talkable", "talkable", "app/models/order.rb", cmpHead)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	call := f.Calls[0]
	wantArgs := []string{"api", "repos/talkable/talkable/contents/app/models/order.rb?ref=" + cmpHead,
		"--hostname", "github.com", "-H", "Accept: application/vnd.github.raw"}
	if !reflect.DeepEqual(call.Args, wantArgs) {
		t.Errorf("args = %q\nwant   %q", call.Args, wantArgs)
	}
	if call.Mutates || call.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" {
		t.Errorf("call = %+v", call)
	}
}

func TestFileAtEmptyFileIsEmptyNotNil(t *testing.T) {
	c, _ := fileAtClient(nil)
	got, err := c.FileAt(context.Background(), "talkable", "talkable", "app/.keep", "main")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty file = %#v, %v (want an empty, non-nil slice)", got, err)
	}
}

func TestFileAtEscapesEachPathSegmentAndTheRef(t *testing.T) {
	for _, tc := range []struct{ path, ref, wantPath, wantRef string }{
		{"app/my file.rb", "main", "app/my%20file.rb", "main"},
		{"docs/a#1?.md", "main", "docs/a%231%3F.md", "main"},
		{"docs/100%.md", "main", "docs/100%25.md", "main"},
		{"docs/café.md", "main", "docs/caf%C3%A9.md", "main"},
		{"lib/{owner}.rb", "main", "lib/%7Bowner%7D.rb", "main"},
		// gh would turn :owner, :repo and :branch into the current repository's
		// values, so a colon never reaches it unescaped.
		{"docs/a:owner.md", "main", "docs/a%3Aowner.md", "main"},
		{"docs/.hidden/x..y", "main", "docs/.hidden/x..y", "main"},
		{"app/a.rb", "feature/x-1", "app/a.rb", "feature%2Fx-1"},
		{"app/a.rb", "v1.2.3", "app/a.rb", "v1.2.3"},
	} {
		c, f := fileAtClient([]byte("x"))
		if _, err := c.FileAt(context.Background(), "talkable", "talkable", tc.path, tc.ref); err != nil {
			t.Errorf("%q @ %q: %v", tc.path, tc.ref, err)
			continue
		}
		want := "repos/talkable/talkable/contents/" + tc.wantPath + "?ref=" + tc.wantRef
		if got := f.Calls[0].Args[1]; got != want {
			t.Errorf("%q @ %q: endpoint = %q, want %q", tc.path, tc.ref, got, want)
		}
	}
}

func TestFileAtNotFound(t *testing.T) {
	// gh prints the REST error body on stdout and the status on stderr.
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return failResult(c,
			[]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/contents#get-repository-content","status":"404"}`),
			[]byte("gh: Not Found (HTTP 404)\n"))
	}}}}
	got, err := (&Client{Run: f}).FileAt(context.Background(), "talkable", "talkable", "app/gone.rb", cmpHead)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrFileTooLarge) {
		t.Error("a missing file is not a too large one")
	}
	if got != nil {
		t.Errorf("content = %q, want none", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Message != "Not Found" {
		t.Errorf("api error = %+v", apiErr)
	}
}

func TestFileAtRefusesAFileAboveTheLimit(t *testing.T) {
	c, _ := fileAtClient(bytes.Repeat([]byte("a"), FileAtLimit))
	got, err := c.FileAt(context.Background(), "talkable", "talkable", "db/schema.rb", cmpHead)
	if err != nil || len(got) != FileAtLimit {
		t.Fatalf("a file of exactly FileAtLimit bytes: len %d, err %v", len(got), err)
	}

	c, _ = fileAtClient(bytes.Repeat([]byte("a"), FileAtLimit+1))
	got, err = c.FileAt(context.Background(), "talkable", "talkable", "db/schema.rb", cmpHead)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a too large file is not a missing one")
	}
	if got != nil {
		t.Errorf("a too large file came back with %d bytes", len(got))
	}
	if strings.Contains(err.Error(), "aaaa") {
		t.Errorf("error carries file content: %.80s", err)
	}
}

func TestFileAtRefusesBadInputBeforeAnyCall(t *testing.T) {
	f := &execx.Fake{}
	c := &Client{Run: f}
	ctx := context.Background()
	for _, path := range []string{"", "/etc/passwd", "app/", "app//a.rb", "./a.rb", "app/./a.rb", "../a.rb", "app/../a.rb", "app/..", ".", "..",
		`app\a.rb`, "app/a\x00.rb"} {
		if got, err := c.FileAt(ctx, "talkable", "talkable", path, cmpHead); err == nil || got != nil {
			t.Errorf("path %q accepted: %q, %v", path, got, err)
		}
	}
	for _, ref := range []string{"", "a..b", "x y", "-rf", "x?y", "x#y", "/main", "x:y", "x%2Fy"} {
		if got, err := c.FileAt(ctx, "talkable", "talkable", "app/a.rb", ref); err == nil || got != nil {
			t.Errorf("ref %q accepted: %q, %v", ref, got, err)
		}
	}
	for _, tc := range [][2]string{{"talk able", "talkable"}, {"talkable", "tal/kable"}, {"", "talkable"}, {"talkable", ""}, {"talkable", ".."}} {
		if got, err := c.FileAt(ctx, tc[0], tc[1], "app/a.rb", cmpHead); err == nil || got != nil {
			t.Errorf("repo %q accepted: %q, %v", tc, got, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("gh ran for invalid input: %d calls", len(f.Calls))
	}
}

func TestFileAtReauthRetriesOnce(t *testing.T) {
	n, failures := 0, 1
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		n++
		if n <= failures {
			return failResult(c, fixture(t, "gql_401.json"), fixture(t, "gql_401.stderr"))
		}
		return okResult([]byte("package x\n"))
	}}}}
	reauths := 0
	c := &Client{Run: f, Reauth: func(context.Context) error { reauths++; return nil }}
	got, err := c.FileAt(context.Background(), "talkable", "talkable", "x.go", cmpHead)
	if err != nil || string(got) != "package x\n" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if reauths != 1 || len(f.Calls) != 2 || !reflect.DeepEqual(f.Calls[0].Args, f.Calls[1].Args) {
		t.Errorf("reauths = %d, gh calls = %d; the retry must repeat the same request", reauths, len(f.Calls))
	}

	// Still 401 after the retry: ErrUnauthorized, no third call.
	n, failures, reauths = 0, 5, 0
	f.Calls = nil
	if _, err := c.FileAt(context.Background(), "talkable", "talkable", "x.go", cmpHead); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if reauths != 1 || len(f.Calls) != 2 {
		t.Errorf("reauths = %d, gh calls = %d, want 1 and 2", reauths, len(f.Calls))
	}
}
