package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
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
	if r := byNum[11920]; r.Fixing != "fixing 1/3 · claude-fix" || !r.FixLoop || r.FixOff {
		t.Errorf("the fixing row: Fixing %q, FixLoop %v, FixOff %v", r.Fixing, r.FixLoop, r.FixOff)
	}
	if r := byNum[11930]; r.Fixing != "" || r.FixLoop || !r.FixOff {
		t.Errorf("the fix-off row: Fixing %q, FixLoop %v, FixOff %v", r.Fixing, r.FixLoop, r.FixOff)
	}
	if r := byNum[11931]; r.Fixing != "" || !r.FixLoop {
		t.Errorf("a loop waiting for its re-review: Fixing %q, FixLoop %v", r.Fixing, r.FixLoop)
	}
	if s := prsStateCell(byNum[11920], now); !strings.Contains(s, ",fixing") {
		t.Errorf("STATE of the fixing row = %q", s)
	}
	if s := prsStateCell(byNum[11930], now); !strings.Contains(s, ",fix-off") {
		t.Errorf("STATE of the fix-off row = %q", s)
	}
	for n, want := range map[int]string{11920: `"fixing": "fixing 1/3 · claude-fix"`, 11930: `"fix_off": true`, 11931: `"fix_loop": true`} {
		var js bytes.Buffer
		if err := writeJSON(&js, prsJSONOf(byNum[n])); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(js.String(), want) {
			t.Errorf("tui --json of %d lacks %s:\n%s", n, want, js.String())
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

// The card's fix line names what the loop's last cycle did to the fixer's
// conversation, after the role: a compaction with its figures, or a fresh
// session with why, as the loop's row records it; `--json` carries it as
// "context".
func TestTheStatusCardNamesTheFixersCompaction(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(-30 * time.Minute)
	for _, tc := range []struct {
		n, cycle      int
		context, want string
	}{
		{11920, 2, "compacted 82% → 31%", "  fix:       cycle 2 of 3 · claude-fix · compacted 82% → 31% · started by magnum fix "},
		{11931, 3, "fresh session (82% after /compact)", "  fix:       cycle 3 of 3 · claude-fix · fresh session (82% after /compact) · started by magnum fix "},
	} {
		pr, err := st.PRByRepoNumber(ctx, repo.ID, tc.n)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(engine.FixState{Cycle: tc.cycle, Cap: 3, Phase: engine.FixPhaseReviewing, By: "magnum fix", StartedAt: started,
			Role: "claude-fix", Context: tc.context})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, engine.KVPRFix(pr.ID), string(b)); err != nil {
			t.Fatal(err)
		}
		r, err := statusGather(ctx, d, statusOptions{Ref: strconv.Itoa(tc.n)})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		statusRender(&out, r)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("card of %d lacks %q:\n%s", tc.n, tc.want, out.String())
		}
		var js bytes.Buffer
		if err := writeJSON(&js, r); err != nil {
			t.Fatal(err)
		}
		if want, _ := json.Marshal(tc.context); !strings.Contains(js.String(), `"context": `+string(want)) {
			t.Errorf("JSON of %d lacks the context %s:\n%s", tc.n, want, js.String())
		}
	}
}

// A fix loop that ended needing you because its fixer left findings says on
// the card what to do about each, one per line under the fix line with its
// thread's link, and the report last ("details: <path>"); `--json` gives the
// calls as fix.needs_you (thread, url, action). A loop still waiting for the
// re-review of its push names the same calls under its own fix line, read
// from the entries its row keeps, whose words (the fixer's) never reach the
// card or the JSON.
func TestTheStatusCardSaysWhatToDoPerThread(t *testing.T) {
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
	const report = "/state/reviews/talkable/talkable/11950/abcdef012345/claude-fix.md"
	human, magnums := "https://github.com/talkable/talkable/pull/11950#discussion_r4302616576", "https://github.com/talkable/talkable/pull/11950#discussion_r4302866759"
	calls := []attention.FixCall{
		attention.FixedCall("4302616576", human, "b2b2b2b", false),
		attention.DeclinedCall("4302866759", magnums, "P2", "src/integration.js:1706", "talkable/talkable#11950", true),
	}
	parked := prOf(11950)
	if err := st.TransitionPR(ctx, parked.ID, []string{store.PRNeedsAttention}, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "fix: "+attention.FixCallsText(calls, "", report))
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVPRAttention(parked.ID), attention.KindFix); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(ctx, d, statusOptions{Ref: "11950"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	want := "  fix:       " + calls[0].Action + "\n             " + calls[1].Action + "\n             details: " + report + "\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("card lacks\n%s\n%s", want, out.String())
	}
	var doc struct {
		Detail struct {
			Fix struct {
				NeedsYou []attention.FixCall `json:"needs_you"`
			} `json:"fix"`
		} `json:"detail"`
	}
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil || !slices.Equal(doc.Detail.Fix.NeedsYou, calls) {
		t.Errorf("fix.needs_you = %+v (%v), want %+v:\n%s", doc.Detail.Fix.NeedsYou, err, calls, js.String())
	}

	reviewing := prOf(11931)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "review-threads.json"), []byte(`[{"id":"PRRT_x","comment_id":4302866759}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	loopReport := filepath.Join(dir, "claude-fix.md")
	b, err := json.Marshal(engine.FixState{Cycle: 1, Cap: 3, Phase: engine.FixPhaseReviewing, By: "magnum fix", Report: loopReport,
		StartedAt: now.Add(-time.Hour), Role: "claude-fix",
		NeedsYou: []string{"4302616576 P2 src/integration.js:135: fixed in b2b2b2b. A human started this thread."}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVPRFix(reviewing.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	r, err = statusGather(ctx, d, statusOptions{Ref: "11931"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	statusRender(&out, r)
	call := attention.FixedCall("4302616576", "https://github.com/talkable/talkable/pull/11931#discussion_r4302616576", "b2b2b2b", false)
	if want := " · report " + loopReport + "\n             " + call.Action + "\n"; !strings.Contains(out.String(), want) {
		t.Errorf("card of the loop waiting for its re-review lacks\n%s\n%s", want, out.String())
	}
	js.Reset()
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	doc.Detail.Fix.NeedsYou = nil
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil || !slices.Equal(doc.Detail.Fix.NeedsYou, []attention.FixCall{call}) ||
		!strings.Contains(js.String(), `"phase": "reviewing"`) {
		t.Errorf("JSON of the loop waiting for its re-review:\n%s", js.String())
	}
	if strings.Contains(out.String()+js.String(), "A human started") {
		t.Errorf("the fixer's words reach the card or the JSON:\n%s\n%s", out.String(), js.String())
	}
}
