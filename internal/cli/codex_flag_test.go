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

// flag records PR pr's Codex flag as a refused round leaves it.
func flagPR(t *testing.T, st *store.Store, prID int64, at time.Time) engine.CodexFlag {
	t.Helper()
	f := engine.CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-own-1", Head: "abc1234def5678", At: at,
		Detail: "■ This content was flagged for possible cybersecurity risk.", By: "round 3"}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVPRCodexFlag(prID), string(b)); err != nil {
		t.Fatal(err)
	}
	return f
}

// codexFlagRequests is the codex-flag payloads queued so far.
func (h *actHarness) codexFlagRequests() []engine.CodexFlagPayload {
	h.t.Helper()
	var out []engine.CodexFlagPayload
	for _, r := range h.requests() {
		if r.Kind == engine.ReqCodexFlag {
			out = append(out, actDecode[engine.CodexFlagPayload](h.t, r.Payload))
		}
	}
	return out
}

// `magnum codex-flag set <ref> <reason>` hands the daemon the PR and the
// reason; `clear` asks y/N on a terminal first, naming when Codex flagged it
// and the account risk, and sends nothing on no, off a terminal, or for a
// PR that is not flagged.
func TestCodexFlagSetAndClearAskBeforeLifting(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 10
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "done") }
	if code := h.cmd("codex-flag", "set", "talkable#5", "Codex", "warned", "about", "abuse"); code != 0 {
		t.Fatalf("set: exit %d: %s", code, h.errb.String())
	}
	got := h.codexFlagRequests()
	if len(got) != 1 || got[0].Number != 5 || got[0].Clear || got[0].Reason != "Codex warned about abuse" || got[0].By != "magnum codex-flag" {
		t.Fatalf("set requests = %+v", got)
	}

	h.out.Reset()
	if code := h.cmd("codex-flag", "clear", "talkable#5"); code != 0 || !strings.Contains(h.out.String(), "is not flagged") {
		t.Fatalf("clear without a flag: exit %d, %s", code, h.out.String())
	}
	flagPR(t, h.st, pr.ID, h.now.Add(-time.Hour))
	if code := h.cmd("codex-flag", "clear", "talkable#5"); code != 1 || !strings.Contains(h.errb.String(), "asks y/N on a terminal") {
		t.Fatalf("clear off a terminal: exit %d, %s", code, h.errb.String())
	}
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("n\n")
	h.out.Reset()
	if code := h.cmd("codex-flag", "clear", "talkable#5"); code != 1 {
		t.Fatalf("clear, n: exit %d", code)
	}
	actContains(t, h.out.String(), "Codex flagged this PR as a possible cybersecurity risk", "(codex-judge, run r-own-1, head abc1234)",
		"Clear the flag of talkable#5? Codex may block the account it runs under", "[y/N]")
	if n := len(h.codexFlagRequests()); n != 1 {
		t.Fatalf("n sent a request: %d", n)
	}
	h.stdin("y\n")
	if code := h.cmd("codex-flag", "clear", "talkable#5"); code != 0 {
		t.Fatalf("clear, y: exit %d: %s", code, h.errb.String())
	}
	if got := h.codexFlagRequests(); len(got) != 2 || !got[1].Clear || got[1].Number != 5 {
		t.Fatalf("clear requests = %+v", got)
	}
	for _, args := range [][]string{{}, {"flag", "talkable#5"}, {"set"}, {"clear", "talkable#5", "talkable#6"}} {
		if code := h.cmd("codex-flag", args...); code != 2 {
			t.Errorf("codex-flag %v: exit %d, want 2", args, code)
		}
	}
}

// `magnum status` lists a flagged PR among what needs the operator, with
// the command that lifts the flag, and its card says what the flag is in
// place of "skipped … `magnum review` forces a round".
func TestStatusShowsAFlaggedPR(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11990, store.PRIneligible, func(u *store.PRUpdate) {
		u.Set("skip_reason", "Codex flagged it as a possible cybersecurity risk: never reviewed again")
	})
	flagPR(t, st, pr.ID, now.Add(-time.Hour))
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var row *statusAttention
	for i := range r.Attention {
		if strings.Contains(r.Attention[i].Subject, "#11990") {
			row = &r.Attention[i]
		}
	}
	if row == nil || !strings.HasPrefix(row.Message, "Codex flagged · never reviewed again: ") ||
		!strings.Contains(row.Fix, "magnum codex-flag clear") {
		t.Fatalf("attention = %+v", r.Attention)
	}

	r, err = statusGather(ctx, d, statusOptions{Ref: "11990"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	if !strings.Contains(out.String(), "Codex flagged this PR as a possible cybersecurity risk") || strings.Contains(out.String(), "forces a round") {
		t.Errorf("card:\n%s", out.String())
	}
}

// `magnum stats` counts a round Codex refused as refused, apart from the
// rounds whose judge failed (needs_attention), whether the round.end event
// or the judge run's outcome says it.
func TestStatsCountARefusedRoundAsRefused(t *testing.T) {
	t0 := statsOct(3, 10, 0)
	refused := statsRR("j1", 1, 5, 1, store.RoleJudge, store.RunFailed, t0)
	refused.Outcome = store.Ptr("refused")
	noEvent := statsRR("j2", 2, 6, 1, store.RoleJudge, store.RunFailed, t0)
	noEvent.Outcome = store.Ptr("refused")
	failed := statsRR("j3", 3, 7, 1, store.RoleJudge, store.RunFailed, t0)
	failed.Outcome = store.Ptr("needs_attention")
	events := []store.Event{
		statsEnd(t0.Add(time.Minute), "pr:talkable/talkable#5", "round 1 ended: refused: Codex refused the review", `{"outcome":"refused"}`),
		statsEnd(t0.Add(time.Minute), "pr:talkable/talkable#7", "round 1 ended: needs_attention", `{"outcome":"needs_attention"}`),
	}
	r := statsCompute([]store.RoundRun{refused, noEvent, failed}, nil, events, func(role string) bool { return role == store.RoleJudge },
		t0.Add(-time.Hour), t0.Add(time.Hour), "")
	if got := r.Total.Rounds.Outcomes; got["refused"] != 2 || got["needs_attention"] != 1 {
		t.Fatalf("outcomes = %v, want 2 refused and 1 needs_attention", got)
	}
}

// `magnum review` (and the board's r and R, which run it) refuses a flagged
// PR with the reason and queues nothing.
func TestReviewRefusesAFlaggedPR(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRIneligible)
	h.pid = 10
	flagPR(t, h.st, pr.ID, h.now.Add(-time.Hour))
	if code := h.cmd("review", "talkable#5"); code == 0 {
		t.Fatalf("review of a flagged PR: exit 0: %s", h.out.String())
	}
	actContains(t, h.errb.String(), "Codex flagged this PR", "never reviews it again", "magnum codex-flag clear talkable#5")
	if n := len(h.requests()); n != 0 {
		t.Fatalf("requests queued: %d", n)
	}
}

// The board's rows carry the flag (its cell and its sentence) and the mute's
// reason from the latest pr.muted event; the printed STATE says
// codex-flagged and the JSON carries both.
func TestPRsRowsCarryTheFlagAndTheMuteReason(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "RF", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	var ids [2]int64
	for i, n := range []int{11990, 11991} {
		res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PF" + string(rune('0'+i)), Number: n, URL: "u", HeadSHA: "abc",
			GHState: store.GHOpen, InitialState: store.PRIneligible, Identity: "talkable-app"})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = res.PR.ID
		if err := st.UpdatePR(ctx, res.PR.ID, func(u *store.PRUpdate) { u.Set("muted", true) }); err != nil {
			t.Fatal(err)
		}
	}
	flagPR(t, st, ids[0], time.Now().Add(-time.Hour))
	for _, ev := range []store.Event{
		{Level: "info", Subject: store.Ptr("pr:talkable/talkable#11990"), Kind: engine.EvPRMuted, Message: "muted: older", Data: json.RawMessage(`{"reason":"older"}`)},
		{Level: "info", Subject: store.Ptr("pr:talkable/talkable#11990"), Kind: engine.EvPRMuted, Message: "muted: Codex warned", Data: json.RawMessage(`{"reason":"Codex warned"}`)},
		{Level: "info", Subject: store.Ptr("pr:talkable/talkable#11991"), Kind: engine.EvPRMuted, Message: "muted", Data: json.RawMessage(`{}`)},
	} {
		if _, err := st.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := prsSource(st, nil, store.BoardFilter{}, nil, f.Ctx.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		byNum[r.Number] = r
	}
	flagged, plain := byNum[11990], byNum[11991]
	if flagged.CodexFlag != "Codex flagged · never reviewed again" || !strings.Contains(flagged.CodexFlagSentence, "talkable/talkable#11990") ||
		flagged.MuteReason != "Codex warned" {
		t.Fatalf("flagged row = flag %q, sentence %q, mute reason %q", flagged.CodexFlag, flagged.CodexFlagSentence, flagged.MuteReason)
	}
	if plain.CodexFlag != "" || plain.MuteReason != "" {
		t.Fatalf("plain row = flag %q, mute reason %q", plain.CodexFlag, plain.MuteReason)
	}
	if s := prsStateCell(flagged, time.Now()); !strings.Contains(s, "codex-flagged") {
		t.Errorf("STATE = %q", s)
	}
	var js bytes.Buffer
	if err := writeJSON(&js, prsJSONOf(flagged)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"codex_flag": "Codex flagged · never reviewed again"`, `"codex_flag_detail": "Codex flagged this PR`, `"mute_reason": "Codex warned"`} {
		if !strings.Contains(js.String(), want) {
			t.Errorf("JSON lacks %s:\n%s", want, js.String())
		}
	}
}
