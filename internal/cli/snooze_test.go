package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// snoozeRequests is the snooze payloads queued so far, oldest first.
func (h *actHarness) snoozeRequests() []engine.SnoozePayload {
	h.t.Helper()
	var out []engine.SnoozePayload
	for _, r := range h.requests() {
		if r.Kind != engine.ReqSnooze {
			h.t.Fatalf("request %d is %s, want %s", r.ID, r.Kind, engine.ReqSnooze)
		}
		out = append(out, actDecode[engine.SnoozePayload](h.t, r.Payload))
	}
	return out
}

// `magnum snooze <ref>` hands the daemon the PR and the snooze's end: 2h
// from now by default, --for, --until (a local time, tomorrow once past)
// or --off; the daemon's answer is printed.
func TestSnoozeSendsTheDaemonTheEnd(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 10
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "snoozed talkable/talkable#5 until 14:00") }
	var wants []time.Time // each command's now moves on: the wait for the answer sleeps
	for _, tc := range []struct {
		args []string
		end  func(now time.Time) time.Time
	}{
		{[]string{"talkable#5"}, func(now time.Time) time.Time { return now.Add(2 * time.Hour) }},
		{[]string{"talkable#5", "--for", "3h"}, func(now time.Time) time.Time { return now.Add(3 * time.Hour) }},
		{[]string{"talkable#5", "--until", "11:30"}, func(time.Time) time.Time { return time.Date(2026, 10, 4, 11, 30, 0, 0, time.UTC) }},
		{[]string{"--off", "talkable#5"}, func(time.Time) time.Time { return time.Time{} }},
	} {
		wants = append(wants, tc.end(h.now))
		if code := h.cmd("snooze", tc.args...); code != 0 {
			t.Fatalf("snooze %v: exit %d: %s", tc.args, code, h.errb.String())
		}
	}
	got := h.snoozeRequests()
	if len(got) != 4 {
		t.Fatalf("requests %+v", got)
	}
	for i, want := range wants {
		p := got[i]
		if p.Repo != "talkable/talkable" || p.Number != 5 || !p.Until.Equal(want) || p.By != "magnum snooze" || p.Off != want.IsZero() {
			t.Errorf("request %d: %+v, want until %v", i+1, p, want)
		}
	}
	actContains(t, h.out.String(), "snoozed talkable/talkable#5 until 14:00")
}

// Without a daemon the snooze waits in the queue, and says so.
func TestSnoozeWithoutDaemonIsQueued(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("snooze", "talkable#5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "snooze talkable#5 is queued as request 1 and applies when the daemon starts")
}

// A snooze names one PR and one end.
func TestSnoozeUsage(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	for _, args := range [][]string{{}, {"talkable#5", "talkable#6"}, {"talkable#5", "--for", "1h", "--until", "18:00"},
		{"talkable#5", "--off", "--for", "1h"}, {"talkable#5", "--for", "-1h"}, {"talkable#5", "--until", "noon"}} {
		h.errb.Reset()
		if code := h.cmd("snooze", args...); code != 2 {
			t.Errorf("snooze %v: exit %d, want 2 (%s)", args, code, h.errb.String())
		}
	}
	if len(h.requests()) != 0 {
		t.Fatalf("a usage error queued %+v", h.requests())
	}
}

// The board's z snoozes for 2h as "the board" and lifts a snooze the same
// way, through the command's own path.
func TestScreenSnoozeAndUnsnooze(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.held = true
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()
	now := h.now
	if _, err := acts.Snooze(ctx, "talkable#5", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := acts.Unsnooze(ctx, "talkable#5"); err != nil {
		t.Fatal(err)
	}
	got := h.snoozeRequests()
	if len(got) != 2 || !got[0].Until.Equal(now.Add(2*time.Hour)) || got[0].By != "the board" || got[0].Off ||
		!got[1].Off || got[1].By != "the board" {
		t.Fatalf("requests %+v", got)
	}
}

// `prs --json` gains snoozed_until (with when and by whom) for a PR whose
// snooze holds; one that has ended is left out.
func TestPRsJSONSaysTheSnooze(t *testing.T) {
	f := newInspFixture(t)
	prsSeed(t, f)
	st := f.store()
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	for n, s := range map[int]engine.Snooze{
		11920: {Until: until, At: at, By: "the board"},
		11931: {Until: time.Now().Add(-time.Minute).UTC(), At: at, By: "magnum snooze"},
	} {
		repo, err := st.RepoByFullName(context.Background(), "talkable/talkable")
		if err != nil {
			t.Fatal(err)
		}
		pr, err := st.PRByRepoNumber(context.Background(), repo.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		if err := st.SetKV(context.Background(), engine.KVPRSnooze(pr.ID), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	by := map[string]map[string]any{}
	for _, r := range rows {
		by[r["ref"].(string)] = r
	}
	a, b := by["talkable/talkable#11920"], by["talkable/talkable#11931"]
	if a == nil || b == nil {
		t.Fatalf("rows %v", by)
	}
	if a["snoozed_until"] != until.Format(time.RFC3339) || a["snoozed_at"] != at.Format(time.RFC3339) || a["snoozed_by"] != "the board" {
		t.Errorf("#11920: snoozed_until %v at %v by %v", a["snoozed_until"], a["snoozed_at"], a["snoozed_by"])
	}
	if v, ok := b["snoozed_until"]; ok {
		t.Errorf("#11931's snooze has ended: snoozed_until %v", v)
	}
}
