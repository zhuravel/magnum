package inventory

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// plantMarker makes path a slug marker of kind: a symlink to a plain file
// outside the checkout naming another slot's slug, a symlink to /dev/zero, a
// FIFO, or a sparse file past the marker's cap.
func plantMarker(t *testing.T, kind, path string) {
	t.Helper()
	var err error
	switch kind {
	case "symlink outside":
		outside := filepath.Join(t.TempDir(), "slug")
		if err = os.WriteFile(outside, []byte("review7\n"), 0o600); err == nil {
			err = os.Symlink(outside, path)
		}
	case "symlink to /dev/zero":
		err = os.Symlink("/dev/zero", path)
	case "fifo":
		err = syscall.Mkfifo(path, 0o600)
	case "over the cap":
		if err = os.WriteFile(path, nil, 0o600); err == nil {
			err = os.Truncate(path, 1<<20)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A worktree's slug marker is written by code the pull request controls.
// One that is a symlink (even to a plain file naming another slot's slug
// outside the checkout), a FIFO, /dev/zero or past its cap is not read: the
// scan warns, names no slug for the worktree and leaves ownership discovery
// incomplete, so no database counts as an orphan on what it could not read.
func TestASlugMarkerThatIsNoSmallRegularFileIsNotRead(t *testing.T) {
	for _, kind := range []string{"symlink outside", "symlink to /dev/zero", "fifo", "over the cap"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.slot("review1", store.SlotFree, true)
			f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
			f.db.add("review1", 1, talkableDBs[0])
			f.db.add("review7", 1, talkableDBs[0])
			plantMarker(t, kind, filepath.Join(f.mkdir("talkable.repo1/tmp"), ".worktree-db-slug"))
			f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "x"})

			done := make(chan struct{})
			var (
				inv Inventory
				err error
			)
			go func() {
				defer close(done)
				inv, err = f.scanner().Scan(context.Background(), Options{})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Scan blocked on the slug marker")
			}
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if inv.OrphansKnown || len(inv.OrphanDBs) != 0 {
				t.Fatalf("orphans computed on a marker that was not read: known=%v %v", inv.OrphansKnown, inv.OrphanDBs)
			}
			if !hasWarning(inv, "fs:", "talkable.repo1") || !hasWarning(inv, "discovery incomplete:", "talkable.repo1") {
				t.Fatalf("warnings = %v", inv.Warnings)
			}
			for _, ev := range inv.External {
				if ev.Path == f.path("talkable.repo1") && ev.Slug != "" {
					t.Errorf("external worktree slug = %q (%s), want none", ev.Slug, ev.SlugSource)
				}
			}
		})
	}
}
