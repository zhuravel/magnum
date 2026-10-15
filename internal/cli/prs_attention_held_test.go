package cli

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// boardRowsOf reads the board's rows from h's registry, by PR number.
func boardRowsOf(t *testing.T, h *actHarness) map[int]tui.PRBoardRow {
	t.Helper()
	rows, err := prsSource(h.st, h.c.Config, store.BoardFilter{}, prsSelfLogins(h.c.Config), h.c.Layout)(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		out[r.Number] = r
	}
	return out
}

// boardLine is PR n's line on the board rendered from rows, 200 columns wide.
func boardLine(t *testing.T, rows map[int]tui.PRBoardRow, n int) string {
	t.Helper()
	var list []tui.PRBoardRow
	for _, r := range rows {
		list = append(list, r)
	}
	out := ansi.Strip(tui.RenderPRBoard(list, 200, tui.PRBoardOptions{Now: func() time.Time { return roundsNow }}))
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "#"+strconv.Itoa(n)) {
			return l
		}
	}
	t.Fatalf("no line of #%d:\n%s", n, out)
	return ""
}

// The board's row of a PR in needs_attention carries the reason kind the
// engine recorded (engine.KVPRAttention), else the one attention.Explain
// infers from the stored error, and its state cell names the cause; a PR
// that left needs_attention carries none, whatever kind is left behind.
// `magnum tui --json` mirrors it as attention_kind.
func TestTheBoardRowCarriesTheAttentionKind(t *testing.T) {
	h := newActHarness(t)
	fix := h.seedPR("talkable/talkable", 12501, store.PRNeedsAttention)
	h.setPR(fix.ID, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "fix: 2 findings need your call: /tmp/fix/claude-fix.md")
	})
	failed := h.seedPR("talkable/talkable", 12502, store.PRNeedsAttention)
	h.setPR(failed.ID, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", "3 attempts on abc1234def5678: deps bundle install: exited 1: could not find gem")
	})
	moved := h.seedPR("talkable/talkable", 12503, store.PRReviewed)
	for id, kind := range map[int64]string{fix.ID: attention.KindFix, moved.ID: attention.KindBlocked} {
		if err := h.st.SetKV(h.ctx, engine.KVPRAttention(id), kind); err != nil {
			t.Fatal(err)
		}
	}

	rows := boardRowsOf(t, h)
	for n, want := range map[int]string{12501: attention.KindFix, 12502: attention.KindFailed, 12503: ""} {
		if got := rows[n].AttentionKind; got != want {
			t.Errorf("#%d: AttentionKind = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{12501: "attention · fix", 12502: "attention · failed"} {
		if line := boardLine(t, rows, n); !strings.Contains(line, want) {
			t.Errorf("#%d: board line %q lacks %q", n, line, want)
		}
	}
	for _, r := range marshalPRsJSON(t, rows[12501], rows[12503]) {
		want := map[float64]string{12501: "fix", 12503: ""}[r["number"].(float64)]
		if got, ok := r["attention_kind"]; !ok || got != want {
			t.Errorf("#%v: attention_kind = %v (present %v), want %q", r["number"], got, ok, want)
		}
	}
}

// FINDINGS reads "✔ held" for the review the approval hold names
// (engine.KVPRApprovalHold) and for no other: a later review of the PR (the
// engine drops the record then, and one left behind names an older
// review) and a clean verdict posted as a comment with no record (a
// comment-only repository posts every clean verdict so) read "✔ clean".
// `magnum tui --json` mirrors it as findings.held.
func TestFindingsReadHeldOnlyForTheHeldReview(t *testing.T) {
	h := newActHarness(t)
	held := h.seedPR("talkable/talkable", 12601, store.PRReviewed)
	later := h.seedPR("talkable/talkable", 12602, store.PRReviewed)
	commentOnly := h.seedPR("talkable/talkable", 12603, store.PRReviewed)
	at := h.now.Add(-3 * time.Hour)
	run := func(id string, pr store.PR, reviewID int64, created time.Time) {
		t.Helper()
		if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, outcome, review_id, prompt_text, result_json, created_at)
			VALUES (?, ?, 1, 'codex-judge', 'rereview', ?, 'talkable-app', 'talkable[bot]', 'verified', 'posted', ?, 'p', '{"event":"COMMENT","verdict":"clean","findings":{}}', ?)`,
			id, pr.ID, pr.HeadSHA, reviewID, store.FormatTime(created)); err != nil {
			t.Fatal(err)
		}
	}
	run("r-held", held, 501, at)
	run("r-old", later, 601, at)
	run("r-new", later, 602, at.Add(time.Hour))
	run("r-comment", commentOnly, 701, at)
	for id, review := range map[int64]int64{held.ID: 501, later.ID: 601} {
		v := fmt.Sprintf(`{"review_id":%d,"run":"r","round":4,"target":"abc1234","why":"2 re-reviews since the first review (approve_after_full_pass 2)"}`, review)
		if err := h.st.SetKV(h.ctx, engine.KVPRApprovalHold(id), v); err != nil {
			t.Fatal(err)
		}
	}

	rows := boardRowsOf(t, h)
	for n, want := range map[int]bool{12601: true, 12602: false, 12603: false} {
		f := rows[n].Findings
		if f == nil || f.Verdict != store.VerdictClean {
			t.Fatalf("#%d: findings %+v", n, f)
		}
		if f.Held != want {
			t.Errorf("#%d: Held = %v, want %v", n, f.Held, want)
		}
		word := map[bool]string{true: "✔ held", false: "✔ clean"}[want]
		if line := boardLine(t, rows, n); !strings.Contains(line, word) {
			t.Errorf("#%d: board line %q lacks %q", n, line, word)
		}
	}
	for _, r := range marshalPRsJSON(t, rows[12601], rows[12602]) {
		want := r["number"].(float64) == 12601
		if got := r["findings"].(map[string]any)["held"]; got != want {
			t.Errorf("#%v: findings.held = %v, want %v", r["number"], got, want)
		}
	}
}
