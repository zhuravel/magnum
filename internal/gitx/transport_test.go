package gitx

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCloneWithCredentialHelper(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(okRule("git"))
	url := GitHubHTTPSURL("talkable", "widget")
	if url != "https://github.com/talkable/widget.git" {
		t.Fatalf("GitHubHTTPSURL = %q", url)
	}
	if err := c.CloneWith(ctx, url, "/repos/widget", CloneOptions{CredentialHelper: GHCredentialHelper}); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, 0, true, "git",
		"-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"clone", "--quiet",
		"--config", "credential.helper=", "--config", "credential.helper=!gh auth git-credential",
		"--", url, "/repos/widget")

	for _, bad := range []string{"!gh\nx", "!gh\rx", "!gh\x00"} {
		if err := c.CloneWith(ctx, url, "/repos/x", CloneOptions{CredentialHelper: bad}); err == nil {
			t.Errorf("helper %q accepted", bad)
		}
	}
	if len(f.Calls) != 1 {
		t.Errorf("refused clones reached git: %d calls", len(f.Calls))
	}
}

func TestGitHubSSHURL(t *testing.T) {
	if got := GitHubSSHURL("talkable", "widget"); got != "git@github.com:talkable/widget.git" {
		t.Fatalf("GitHubSSHURL = %q", got)
	}
}

func TestIsSSHURL(t *testing.T) {
	for in, want := range map[string]bool{
		"git@github.com:talkable/widget.git":       true,
		"ssh://git@github.com/talkable/widget.git": true,
		"SSH://git@github.com/talkable/widget":     true,
		"git+ssh://git@github.com/talkable/w.git":  true,
		" git@github.com:talkable/widget.git\n":    true,
		"https://github.com/talkable/widget.git":   false,
		"https://user@github.com/talkable/w.git":   false,
		"git://github.com/talkable/widget.git":     false,
		"/local/path@v1:x":                         false,
		"github.com:talkable/widget.git":           true, // scp-like without a user
		"./dir:x":                                  false,
		"":                                         false,
	} {
		if got := IsSSHURL(in); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestOwnerUsesSSH(t *testing.T) {
	root, rec := cloneRoot(t, map[string]string{
		"widget":            "git@github.com:talkable/widget.git",
		"gadget":            "https://github.com/talkable/gadget.git",
		"other":             "git@github.com:example/other.git",
		"notes":             "",
		"lookalike":         "git@gitlab.com:talkable/lookalike.git",
		"widget__worktrees": "git@github.com:outsider/x.git",
		"spaced":            "ssh://git@github.com/Example2/spaced.git",
		"httpsonly":         "https://github.com/example3/a.git",
	})
	c := New(rec)
	ctx := context.Background()
	for owner, want := range map[string]bool{
		"talkable": true,  // widget is SSH (gadget's https does not matter)
		"TALKABLE": true,  // owners compare case-insensitively
		"example":  true,  // other
		"example2": true,  // ssh:// form
		"example3": false, // https only
		"outsider": false, // __worktrees directories are skipped
		"nobody":   false,
	} {
		got, err := c.OwnerUsesSSH(ctx, root, owner)
		if err != nil || got != want {
			t.Errorf("OwnerUsesSSH(%s) = %v, %v; want %v", owner, got, err, want)
		}
	}
	// Origins are cached: a second sweep asks git nothing.
	before := rec.total()
	if _, err := c.OwnerUsesSSH(ctx, root, "nobody"); err != nil {
		t.Fatal(err)
	}
	if rec.total() != before {
		t.Errorf("second sweep ran git %d times", rec.total()-before)
	}
	if rec.callsIn(filepath.Join(root, "notes")) != 0 {
		t.Errorf("a folder without .git was handed to git")
	}

	if got, err := c.OwnerUsesSSH(ctx, filepath.Join(root, "missing"), "talkable"); err != nil || got {
		t.Errorf("missing root = %v, %v; want false, nil", got, err)
	}
	for _, bad := range [][2]string{{root, ""}, {root, "a/b"}, {"", "talkable"}} {
		if _, err := c.OwnerUsesSSH(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("OwnerUsesSSH(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// The helper really lands in the new clone's local config, and git accepts
// the command line.
func TestRealCloneWithPersistsCredentialHelper(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dest := filepath.Join(filepath.Dir(fx.clone), "https-clone")
	if err := fx.c.CloneWith(ctx, fx.origin, dest, CloneOptions{CredentialHelper: "!true"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range strings.Lines(fx.git(dest, "config", "--local", "--get-regexp", `^credential\.helper$`)) {
		got = append(got, strings.TrimSpace(line))
	}
	if want := []string{"credential.helper", "credential.helper !true"}; !slices.Equal(got, want) {
		t.Fatalf("local credential.helper = %q, want %q", got, want)
	}
}
