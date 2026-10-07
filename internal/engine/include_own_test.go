package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// include_own = false skips a PR by any of the operator's logins (the board's
// "mine", config.SelfLogins), not only by the watch's poll login: a PR by a
// second gh identity of his was reviewed although every other screen called
// it his own. Someone else's PR is still queued.
func TestIncludeOwnFalseSkipsAPRByASecondSelfLogin(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.cfg.Identities = append(h.cfg.Identities, config.Identity{Name: "second", Kind: "gh", Login: "rev-ann"})
		h.cfg.Watches[0].IncludeOwn = new(false)
	})
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", author: "rev-ann"}, prSpec{n: 3, head: "c1", author: "zhuravel"},
		prSpec{n: 4, head: "d1", author: "alice"})
	h.tick()
	for _, n := range []int{2, 3} {
		if pr := h.wantState(n, store.PRIneligible); !strings.Contains(deref(pr.SkipReason), "include_own = false") {
			t.Fatalf("#%d skip reason = %q, want include_own", n, deref(pr.SkipReason))
		}
	}
	h.wantState(4, store.PRQueued)
}
