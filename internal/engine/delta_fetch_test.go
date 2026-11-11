package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// headGit is a clone that lacks a PR's newest head until FetchPR brings
// refs/pull/N/head: Tree of a head it lacks fails as git's ls-tree does.
type headGit struct {
	fakeGit
	mu       sync.Mutex
	lacks    map[string]bool // heads not in the clone
	heads    map[int]string  // what refs/pull/N/head holds, by PR number
	fetches  []string        // "<clone>#<N>", each FetchPR
	fetchErr error
}

func (g *headGit) Tree(ctx context.Context, dir, rev string, keep func(string) bool) (gitx.Tree, error) {
	g.mu.Lock()
	lacks := g.lacks[rev]
	g.mu.Unlock()
	if lacks {
		return gitx.Tree{}, fmt.Errorf("gitx: ls-tree -r %s: exit status 128: fatal: not a tree object", rev)
	}
	return g.fakeGit.Tree(ctx, dir, rev, keep)
}

func (g *headGit) FetchPR(_ context.Context, clone string, number int) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetches = append(g.fetches, fmt.Sprintf("%s#%d", clone, number))
	if g.fetchErr != nil {
		return "", g.fetchErr
	}
	delete(g.lacks, g.heads[number])
	return g.heads[number], nil
}

func (g *headGit) fetched() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.fetches)
}

// A trivial push settles with no round, and only a round's checkout used to
// fetch a head: the gates then read the settled head's tree from a clone
// that lacked it, every tick (1,398 "not a tree object" warnings from
// 2026-11-10 on, and two PRs whose needs-you line said for 18 hours that
// magnum cannot tell which files they change). The settle fetches the head
// into the PR's clone (refs/pull/N/head, as a round does) and counts it
// fetched, so the next tick reads its links. A fetch that fails is one
// warning, however many ticks follow, and the push stays settled.
func TestATrivialPushThatSettlesFetchesItsHeadForTheGates(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name     string
		fetchErr error
	}{
		{name: "the fetch brings the head"},
		{name: "the fetch fails", fetchErr: errors.New("fetch PR 2: network down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			g := &headGit{lacks: map[string]bool{"b2": true}, heads: map[int]string{2: "b2"}, fetchErr: tc.fetchErr}
			h := newHarness(t, func(h *harness) {
				h.d.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
				h.d.Git = g
			})
			pr := reviewedThenPushed(t, h, "", docsOnly)
			if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || len(trivialEvents(t, h)) != 1 {
				t.Fatalf("state %s reviewed_sha %q; want the docs push settled", pr.State, deref(pr.ReviewedSHA))
			}
			repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{deref(repo.ClonePath) + "#2"}; deref(repo.ClonePath) == "" || !slices.Equal(g.fetched(), want) {
				t.Fatalf("fetches = %q, want %q: the PR's head into its clone, once", g.fetched(), want)
			}
			h.advance(5 * time.Minute)
			h.tick()
			_, linksErr := h.e.configLinks(h.ctx, h.pr(2))
			if tc.fetchErr == nil {
				if linksErr != nil || strings.Contains(logs.String(), "not a tree object") {
					t.Fatalf("configLinks of the settled head: %v; log:\n%s", linksErr, logs.String())
				}
				return
			}
			if linksErr == nil {
				t.Fatal("configLinks read a head the clone still lacks")
			}
			if n := strings.Count(logs.String(), "trivial delta: cannot fetch the head"); n != 1 {
				t.Fatalf("%d fetch warnings, want 1:\n%s", n, logs.String())
			}
			if n := len(g.fetched()); n != 1 {
				t.Fatalf("%d fetches, want the settle's one", n)
			}
		})
	}
}
