package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// operatorOnlyRefusal is what a command that acts for the operator says in
// a review agent's pane.
const operatorOnlyRefusal = "run this from your own terminal: a review agent's shell cannot act for the operator"

// A review agent runs in a pane whose environment carries MAGNUM_REPORT_DIR
// and MAGNUM_PR_URL, and it reads the PR, whose text may steer it. The
// commands that act for the operator (approve --as, unapprove, mute,
// ignore, pause) refuse to run there and queue nothing; the ones the roles
// run (post-review, db-lock) are not refused.
func TestAReviewAgentsShellCannotActForTheOperator(t *testing.T) {
	for _, env := range []string{"MAGNUM_REPORT_DIR", "MAGNUM_PR_URL"} {
		t.Run(env, func(t *testing.T) {
			storetest.Serial(t)
			t.Setenv(env, "/x/reviews/talkable/talkable/5")
			h := newActHarness(t)
			h.pid = 4242
			pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
			h.standingAutoApproval(pr)
			for _, args := range [][]string{
				{"approve", "5", "--as", "zhuravel"},
				{"unapprove", "5", "--yes"},
				{"mute", "5", "reviewed", "by", "hand"},
				{"ignore", "5"},
				{"pause", "--for", "2h"},
			} {
				h.errb.Reset()
				if code := h.cmd(args[0], args[1:]...); code != 1 {
					t.Errorf("%v: exit %d, want 1: %s", args, code, h.errb.String())
				}
				actContains(t, h.errb.String(), "magnum "+args[0]+": "+operatorOnlyRefusal)
			}
			if reqs := h.requests(); len(reqs) != 0 {
				t.Fatalf("queued %+v", reqs)
			}
			if v, _, _ := h.st.GetKV(h.ctx, store.KVDaemonPaused); v != "" {
				t.Fatalf("paused: %q", v)
			}
			for _, args := range [][]string{{"post-review"}, {"db-lock"}} {
				h.errb.Reset()
				h.cmd(args[0], args[1:]...)
				if strings.Contains(h.errb.String(), operatorOnlyRefusal) {
					t.Errorf("%v refused in an agent's pane: %s", args, h.errb.String())
				}
			}
		})
	}
}
