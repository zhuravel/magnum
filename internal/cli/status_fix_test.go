package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The board and `magnum status` show a PR's fix loop as the daemon keeps it:
// a fixer at work in the state cell ("fixing 1/3 · claude-fix") and on the
// card's fix line with who started it, when, and the report; a loop waiting
// for its re-review shows the PR's normal state; and a PR switched out of
// autofix carries "fix off" among its flags and says since when and by whom.
func TestBoardRowsAndTheStatusCardCarryTheFixLoop(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	prOf := func(n int) store.PR {
		pr, err := st.PRByRepoNumber(ctx, repo.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}
	setJSON := func(key string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, key, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	started := now.Add(-30 * time.Minute)
	report := "/state/reviews/talkable/talkable/11920/abcdef012345/claude-fix.md"
	fixing, off, reviewing := prOf(11920), prOf(11930), prOf(11931)
	setJSON(engine.KVPRFix(fixing.ID), engine.FixState{Cycle: 1, Cap: 3, Phase: engine.FixPhaseFixing, Since: fixing.HeadSHA,
		Head: fixing.HeadSHA, By: "magnum fix", Report: report, StartedAt: started, Role: "claude-fix"})
	setJSON(engine.KVPRFixOff(off.ID), engine.FixOff{Since: started, By: "magnum fix --off"})
	setJSON(engine.KVPRFix(reviewing.ID), engine.FixState{Cycle: 2, Cap: 3, Phase: engine.FixPhaseReviewing, By: "autofix",
		Report: report, StartedAt: started, Role: "claude-fix"})

	rows, err := prsSource(st, d.Config, store.BoardFilter{}, nil, d.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		byNum[r.Number] = r
	}
	if r := byNum[11920]; r.Fixing != "fixing 1/3 · claude-fix" || r.FixOff {
		t.Errorf("the fixing row: Fixing %q, FixOff %v", r.Fixing, r.FixOff)
	}
	if r := byNum[11930]; r.Fixing != "" || !r.FixOff {
		t.Errorf("the fix-off row: Fixing %q, FixOff %v", r.Fixing, r.FixOff)
	}
	if r := byNum[11931]; r.Fixing != "" {
		t.Errorf("a loop waiting for its re-review: Fixing %q", r.Fixing)
	}
	if s := prsStateCell(byNum[11920], now); !strings.Contains(s, ",fixing") {
		t.Errorf("STATE of the fixing row = %q", s)
	}
	if s := prsStateCell(byNum[11930], now); !strings.Contains(s, ",fix-off") {
		t.Errorf("STATE of the fix-off row = %q", s)
	}
	for n, want := range map[int]string{11920: `"fixing": "fixing 1/3 · claude-fix"`, 11930: `"fix_off": true`} {
		var js bytes.Buffer
		if err := writeJSON(&js, prsJSONOf(byNum[n])); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(js.String(), want) {
			t.Errorf("prs --json of %d lacks %s:\n%s", n, want, js.String())
		}
	}

	clock := inspClock(now, started)
	for _, tc := range []struct {
		ref, want string
	}{
		{ref: "11920", want: "  fix:       cycle 1 of 3 · claude-fix · started by magnum fix " + clock + " · report " + report + "\n"},
		{ref: "11931", want: "  fix:       cycle 2 of 3 · claude-fix · started by autofix " + clock + " · report " + report + "\n"},
		{ref: "11930", want: "  fix:       off since " + clock + " (magnum fix --off)\n"},
	} {
		r, err := statusGather(ctx, d, statusOptions{Ref: tc.ref})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		statusRender(&out, r)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("card of %s lacks %q:\n%s", tc.ref, tc.want, out.String())
		}
	}
	r, err := statusGather(ctx, d, statusOptions{Ref: "11950"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	if strings.Contains(out.String(), "cycle") || strings.Contains(out.String(), "off since") {
		t.Errorf("a PR without a loop or the flag:\n%s", out.String())
	}

	r, err = statusGather(ctx, d, statusOptions{Ref: "11920"})
	if err != nil {
		t.Fatal(err)
	}
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"phase": "fixing"`) || !strings.Contains(js.String(), `"role": "claude-fix"`) {
		t.Errorf("JSON lacks the loop:\n%s", js.String())
	}
}
