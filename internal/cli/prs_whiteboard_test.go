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

// The board rows and `magnum prs --json` carry a PR's whiteboard as the
// daemon records it (engine.KVPRWhiteboard): the session w opens and the
// head it describes; a PR without a record has none (null), and so has one
// whose record names no session.
func TestBoardRowsAndPRsJSONCarryThePRsWhiteboard(t *testing.T) {
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
	written, broken := prOf(11920), prOf(11930)
	setJSON(engine.KVPRWhiteboard(written.ID), engine.WhiteboardRecord{Session: "wb-11920", Head: written.HeadSHA,
		At: now.Add(-time.Hour), Role: "claude-whiteboard"})
	setJSON(engine.KVPRWhiteboard(broken.ID), engine.WhiteboardRecord{Head: broken.HeadSHA})

	rows, err := prsSource(st, d.Config, store.BoardFilter{}, nil, d.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		byNum[r.Number] = r
	}
	if w := byNum[11920].Whiteboard; w == nil || w.Session != "wb-11920" || w.Head != written.HeadSHA {
		t.Errorf("the written row's whiteboard: %+v, want session wb-11920 at %s", w, written.HeadSHA)
	}
	for _, n := range []int{11930, 11931} {
		if w := byNum[n].Whiteboard; w != nil {
			t.Errorf("%d has a whiteboard %+v, want none", n, w)
		}
	}
	for n, want := range map[int]any{
		11920: map[string]any{"session": "wb-11920", "head": written.HeadSHA},
		11930: nil,
	} {
		var js bytes.Buffer
		if err := writeJSON(&js, prsJSONOf(byNum[n])); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(js.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if got, ok := out["whiteboard"]; !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("prs --json of %d: whiteboard = %#v (present %v), want %#v", n, got, ok, want)
		}
	}
}

// The board's w opens the PR's whiteboard as `magnum whiteboard --open`
// does, without asking again; a PR without one is refused with the
// command's sentence, without its prefix.
func TestScreenOpenWhiteboardRefusesAPRWithoutOne(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	acts := newScreenActions(h.c)
	defer acts.Close()
	_, err := acts.OpenWhiteboard(context.Background(), "talkable#5")
	if err == nil || !strings.HasPrefix(err.Error(), "no whiteboard of talkable#5 yet") {
		t.Fatalf("a PR without a whiteboard: %v, want the command's refusal", err)
	}
}
