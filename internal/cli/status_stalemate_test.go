package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// 24 pr.stalemate warnings on 10 PRs reached only the event log, the board
// row and the card, and flags from 10-29 and 11-04 waited for days: `magnum
// status`, the dashboard and --needs-me left them out. A PR with threads
// magnum stopped arguing in that the operator has not acted on is listed
// in status's ATTENTION, with the card's words and what decides it, in the
// dashboard's ATTENTION section and by --needs-me. One they acted on (a
// review, a verdict, a mute: the flag cleared, its threads seen) is not.
func TestAStalemateNotYetSeenIsListedWhereTheOperatorLooks(t *testing.T) {
	f, st, d, _ := statusFixture(t)
	ctx := context.Background()
	const url = "https://github.com/talkable/talkable/pull/12048#discussion_r4322458360"
	_, flagged := inspSeedPR(t, st, "talkable/talkable", 12048, store.PRReviewed, nil)
	_, seen := inspSeedPR(t, st, "talkable/talkable", 12179, store.PRReviewed, nil)
	for k, v := range map[string]string{
		engine.KVPRStalemate(flagged.ID):             `{"threads":[{"id":"PRRT_1","url":"` + url + `","last_reply":7}]}`,
		fmt.Sprintf("pr.%d.stalemate_seen", seen.ID): `{"PRRT_2":9}`,
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []statusAttention
	for _, a := range r.Attention {
		if strings.Contains(a.Message, "stopped arguing") {
			rows = append(rows, a)
		}
	}
	if len(rows) != 1 || rows[0].Subject != "talkable#12048" ||
		rows[0].Message != "magnum stopped arguing after two rebuttals in 1 thread: "+url {
		t.Fatalf("stalemate attention = %+v, want #12048's alone", rows)
	}
	for _, want := range []string{"decide it", "`magnum review talkable#12048` has the judge re-decide",
		"`magnum approve talkable#12048` or `magnum request-changes talkable#12048` posts your verdict", "r, A/C"} {
		if !strings.Contains(rows[0].Fix, want) {
			t.Errorf("fix %q lacks %q", rows[0].Fix, want)
		}
	}

	dash := statusDashData(r, "talkable/talkable")
	if !slices.ContainsFunc(dash.Attention, func(a tui.AttentionRow) bool {
		return a.Subject == "talkable#12048" && a.Message == rows[0].Message && a.Fix == rows[0].Fix
	}) {
		t.Errorf("the dashboard's ATTENTION lacks the stalemate: %+v", dash.Attention)
	}
	if slices.ContainsFunc(dash.Attention, func(a tui.AttentionRow) bool { return a.Subject == "talkable#12179" }) {
		t.Errorf("the dashboard lists a stalemate the operator saw: %+v", dash.Attention)
	}

	st.Close()
	if code := f.run("tui", "--needs-me", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var prs []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &prs); err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, p := range prs {
		nums = append(nums, p.Number)
	}
	if !slices.Equal(nums, []int{12048}) {
		t.Errorf("--needs-me rows %v, want the unseen stalemate alone", nums)
	}
}
