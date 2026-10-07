package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// setWatchPoll stores a watch's radar record as the daemon writes it.
func setWatchPoll(t *testing.T, st *store.Store, owner string, p engine.WatchPoll) {
	t.Helper()
	if err := st.SetKV(context.Background(), store.KVWatchPoll(owner), p.Value()); err != nil {
		t.Fatal(err)
	}
}

// TestScreenFactsNameTheWatchesWhosePollsFail: the titles name a watch
// whose radar calls have failed for 10 minutes or more, with the cause, and
// leave out one failing for 5; several go oldest failure first, and a watch
// whose radar answers again leaves them.
func TestScreenFactsNameTheWatchesWhosePollsFail(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	if f := screenFacts(ctx, d); f.PollsFailing != nil {
		t.Fatalf("no record, yet failing: %+v", f.PollsFailing)
	}
	setWatchPoll(t, st, "talkable", engine.WatchPoll{LastOK: now.Add(-13 * time.Minute), FailingSince: now.Add(-12 * time.Minute), Error: "HTTP 502"})
	setWatchPoll(t, st, "zhuravel", engine.WatchPoll{LastOK: now.Add(-6 * time.Minute), FailingSince: now.Add(-5 * time.Minute), Error: "HTTP 504"})
	talkable := tui.WatchFailing{Watch: "talkable", Since: now.Add(-12 * time.Minute), Error: "HTTP 502"}
	if got := screenFacts(ctx, d).PollsFailing; !reflect.DeepEqual(got, []tui.WatchFailing{talkable}) {
		t.Fatalf("failing watches %+v, want talkable's alone", got)
	}

	setWatchPoll(t, st, "zhuravel", engine.WatchPoll{FailingSince: now.Add(-30 * time.Minute)})
	zhuravel := tui.WatchFailing{Watch: "zhuravel", Since: now.Add(-30 * time.Minute)}
	if got := screenFacts(ctx, d).PollsFailing; !reflect.DeepEqual(got, []tui.WatchFailing{zhuravel, talkable}) {
		t.Fatalf("failing watches %+v, want the oldest failure first", got)
	}

	setWatchPoll(t, st, "talkable", engine.WatchPoll{LastOK: now})
	if got := screenFacts(ctx, d).PollsFailing; !reflect.DeepEqual(got, []tui.WatchFailing{zhuravel}) {
		t.Fatalf("failing watches %+v after talkable answered", got)
	}
}

// TestStatusNamesTheWatchesWhosePollsFail: `magnum status` says on its
// activity line, next to the daemon's last poll, which watch's radar calls
// fail and since when; the dashboard's data carries the same, and --json
// the failing watches with their last good poll. A watch failing for less
// than 10 minutes is left out.
func TestStatusNamesTheWatchesWhosePollsFail(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js.String(), "polls_failing") {
		t.Fatalf("no watch fails, yet the json names some:\n%s", js.String())
	}

	setWatchPoll(t, st, "talkable", engine.WatchPoll{LastOK: now.Add(-13 * time.Minute), FailingSince: now.Add(-12 * time.Minute), Error: "HTTP 502"})
	setWatchPoll(t, st, "zhuravel", engine.WatchPoll{FailingSince: now.Add(-5 * time.Minute), Error: "HTTP 504"})
	if r, err = statusGather(ctx, d, statusOptions{}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if out := b.String(); !strings.Contains(out, "activity: last poll 40s ago, talkable polls failing 12m (HTTP 502), last tick never") ||
		strings.Contains(out, "zhuravel polls failing") {
		t.Errorf("status output:\n%s", out)
	}

	data := statusDashData(r, "talkable/talkable")
	want := []tui.WatchFailing{{Watch: "talkable", Since: now.Add(-12 * time.Minute), Error: "HTTP 502"}}
	if !reflect.DeepEqual(data.Activity.PollsFailing, want) {
		t.Errorf("dashboard activity %+v", data.Activity)
	}

	js.Reset()
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Daemon struct {
			PollsFailing []map[string]any `json:"polls_failing"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	wantJSON := []map[string]any{{"watch": "talkable", "since": now.Add(-12 * time.Minute).Format(time.RFC3339), "error": "HTTP 502",
		"last_ok": now.Add(-13 * time.Minute).Format(time.RFC3339)}}
	if !reflect.DeepEqual(back.Daemon.PollsFailing, wantJSON) {
		t.Errorf("json polls_failing = %v\nwant %v", back.Daemon.PollsFailing, wantJSON)
	}
}
