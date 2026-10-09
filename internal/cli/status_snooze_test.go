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
)

// `magnum status` says what the board says of a waiting PR: its snooze and
// the earlier findings still open, on its queue line, in its JSON and on its
// card.
func TestStatusShowsTheSnoozeAndTheOpenFindings(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, 11930)
	if err != nil {
		t.Fatal(err)
	}
	until := now.Add(6 * time.Hour)
	b, _ := json.Marshal(engine.Snooze{Until: until, At: now.Add(-time.Minute), By: "the board"})
	if err := st.SetKV(ctx, engine.KVPRSnooze(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: "abcdef0123456789", State: store.RunVerified, Outcome: new("posted"), ReviewID: new(int64(7)),
		ResultJSON: new(`{"verdict":"blocking","findings":{},"previous_findings":{"open":3}}`),
		Identity:   "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "x"}); err != nil {
		t.Fatal(err)
	}
	clock := inspClock(now, until)

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var line *statusPRLine
	for i := range r.Queue {
		if r.Queue[i].Number == 11930 {
			line = &r.Queue[i]
		}
	}
	if line == nil || line.SnoozedUntil == nil || !line.SnoozedUntil.Equal(until) || line.Open != 3 {
		t.Fatalf("queue line = %+v", line)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	if want := " · snoozed → " + clock + " · 3 open"; !strings.Contains(out.String(), want) {
		t.Errorf("queue lacks %q:\n%s", want, out.String())
	}
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"snoozed_until"`) || !strings.Contains(js.String(), `"open": 3`) {
		t.Errorf("JSON lacks the snooze or the open count:\n%s", js.String())
	}

	r, err = statusGather(ctx, d, statusOptions{Ref: "11930"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	statusRender(&out, r)
	if want := "state:     queued (GitHub OPEN), snoozed until " + clock + " by the board"; !strings.Contains(out.String(), want) {
		t.Errorf("card lacks %q:\n%s", want, out.String())
	}
	if want := "; earlier: 0 fixed, 3 open, 0 answered"; !strings.Contains(out.String(), want) {
		t.Errorf("card lacks %q:\n%s", want, out.String())
	}
}
