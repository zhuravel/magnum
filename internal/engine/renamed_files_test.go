package engine

import (
	"strings"
	"testing"
	"time"
)

// renamesHarness is an auto-approval harness whose zhuravel client lists a
// PR's files through REST (filesGH), as the paths renames leave are read.
func renamesHarness(t *testing.T) (*harness, *filesGH) {
	t.Helper()
	fg := &filesGH{}
	h, _ := newAutoHarness(t, func(h *harness) {
		fg.fakeGH = h.gh
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return fg
			}
			return nil
		}
	})
	return h, fg
}

// A rename's previous path depends on the base GitHub compares the head
// with: a PR retargeted to another base reads the REST list again, though
// its head and the Details' new paths stay the same, so the path a rename
// leaves under the new base (.claude/hooks/x.rb) is in the list.
func TestARetargetReadsTheRenamesAgainstTheNewBase(t *testing.T) {
	h, fg := renamesHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	fg.set(2, "scripts/x.rb", "lib/x.rb")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", base: "feature", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	fg.set(2, "scripts/x.rb", ".claude/hooks/x.rb")
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID)
	if err != nil || !ok || !strings.Contains(strings.Join(f.Paths, " "), ".claude/hooks/x.rb") {
		t.Fatalf("the retargeted head's list = %+v, %v (%v), after %d REST reads: want the path its rename leaves under the new base",
			f, ok, err, fg.fetches(2))
	}
}
