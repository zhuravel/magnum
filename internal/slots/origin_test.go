package slots

import (
	"errors"
	"testing"
)

// A per-PR worktree is made only from a clone whose origin is the PR's
// repository on github.com: the clone step once took any URL whose path
// named owner/name (any host), or that held the name anywhere (a local
// path), for the repository's clone.
func TestCreatePRWorktreeRefusesAnOriginOffGitHub(t *testing.T) {
	for _, url := range []string{
		"https://gitlab.com/zhuravel/widget.git",
		"git@gitlab.com:zhuravel/widget.git",
		"https://evil-github.com/zhuravel/widget.git",
		"git@evil-github.com:zhuravel/widget.git",
		"/srv/mirrors/zhuravel/widget.git",
	} {
		t.Run(url, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			fakeOrigins(h, map[string]string{f.main: url})
			_, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
			if !errors.Is(err, ErrOriginMismatch) {
				t.Fatalf("err = %v, want ErrOriginMismatch", err)
			}
			if n := len(h.run.gitCalls("worktree", "add")); n != 0 {
				t.Fatal("worktree added from a clone whose origin is not on github.com")
			}
		})
	}
}

// The check is gitx's: any form of the github.com URL, in any case, passes.
func TestCreatePRWorktreeTakesAnyFormOfTheGitHubOrigin(t *testing.T) {
	for _, url := range []string{
		"https://github.com/zhuravel/widget.git",
		"git@github.com:Zhuravel/Widget.git",
		"ssh://git@github.com/zhuravel/widget",
	} {
		t.Run(url, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			fakeOrigins(h, map[string]string{f.main: url})
			if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
				t.Fatalf("CreatePRWorktree with origin %s: %v", url, err)
			}
		})
	}
}
