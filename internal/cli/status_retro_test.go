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

// statusRetroSet records the last retro's summary the way the daemon does.
func statusRetroSet(t *testing.T, st *store.Store, sum engine.RetroSummary) {
	t.Helper()
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVRetroLast, string(b)); err != nil {
		t.Fatal(err)
	}
}

// statusRetroSeed adds new misses of class miss (two), a used one and a style one.
func statusRetroSeed(t *testing.T, st *store.Store) {
	t.Helper()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11800, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	for i, m := range []store.Miss{
		{Class: store.MissMiss}, {Class: store.MissMiss}, {Class: store.MissMiss, State: store.MissUsed}, {Class: store.MissStyle},
	} {
		m.SourceURL = "https://example.com/c/" + string(rune('a'+i))
		m.Title = "t"
		missSeed(t, st, pr, m)
	}
}

func statusRetroOut(t *testing.T, d statusDeps) (statusReport, string) {
	t.Helper()
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRenderHeader(&b, r)
	return r, b.String()
}

func TestStatusRetroLineIsAbsentWhenLearnIsOffAndNoRetroRan(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	statusRetroSeed(t, st) // misses alone do not show the line
	r, out := statusRetroOut(t, d)
	if r.Retro != nil || strings.Contains(out, "retro:") {
		t.Errorf("retro = %+v\n%s", r.Retro, out)
	}
	// An unreadable summary is absence too.
	if err := st.SetKV(context.Background(), engine.KVRetroLast, "{not json"); err != nil {
		t.Fatal(err)
	}
	if r, out = statusRetroOut(t, d); r.Retro != nil || strings.Contains(out, "retro:") {
		t.Errorf("malformed summary: retro = %+v\n%s", r.Retro, out)
	}
}

func TestStatusRetroLineWhenEnabledNeverRan(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Config.Learn.Enabled = true
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    never · 0 new misses\n")
	statusRetroSeed(t, st)
	_, out = statusRetroOut(t, d)
	actContains(t, out, "retro:    never · 2 new misses\n")
}

func TestStatusRetroLineAfterARetroEvenWhenLearnIsOff(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSeed(t, st)
	statusRetroSet(t, st, engine.RetroSummary{Run: "20261003-100000", At: now.Add(-3 * time.Hour), Finished: now.Add(-2 * time.Hour),
		PRs: 3, Classified: 1, Misses: 2, Caught: 4})
	r, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 3 PRs, 1 classified · 2 new misses\n")
	if r.Retro == nil || r.Retro.Enabled || r.Retro.Last == nil || r.Retro.NewMisses == nil || *r.Retro.NewMisses != 2 {
		t.Errorf("retro = %+v", r.Retro)
	}
	// The line sits after the codex line (or the github one) and before the rounds line.
	if i, j := strings.Index(out, "retro:"), strings.Index(out, "rounds:"); i < 0 || j < i || strings.Index(out, "github:") > i {
		t.Errorf("retro line out of place:\n%s", out)
	}
}

func TestStatusRetroLineWordsOneAndFailures(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-90 * time.Minute), Finished: now.Add(-time.Hour), PRs: 1, Classified: 0, Failed: 1})
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    1h ago, 1 PR, 0 classified, 1 failed · 0 new misses\n")
}

func TestStatusRetroLineSaysWhyARetroStoppedAndCleansIt(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-50 * time.Minute), Finished: now.Add(-45 * time.Minute), PRs: 2, Classified: 1,
		Stopped: "claude " + statusNoise})
	_, out := statusRetroOut(t, d)
	statusNoControls(t, "status retro line", out)
	actContains(t, out, "retro:    45m ago, stopped: claude ")
	if strings.Contains(out, "classified ·") {
		t.Errorf("a stopped retro also reports its counts:\n%s", out)
	}
}

func TestStatusJSONCarriesTheRetro(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	statusRetroSeed(t, st)
	statusRetroSet(t, st, engine.RetroSummary{Run: "20261003-100000", At: time.Now().Add(-time.Hour), Finished: time.Now().Add(-50 * time.Minute), PRs: 3, Classified: 2, Misses: 2})
	st.Close()
	if code := f.run("status", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	var r struct {
		Retro struct {
			Enabled bool `json:"enabled"`
			Last    struct {
				Run string `json:"run"`
				PRs int    `json:"prs"`
			} `json:"last"`
			NewMisses *int `json:"new_misses"`
		} `json:"retro"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Retro.Last.Run != "20261003-100000" || r.Retro.Last.PRs != 3 || r.Retro.NewMisses == nil || *r.Retro.NewMisses != 2 || r.Retro.Enabled {
		t.Errorf("retro = %+v", r.Retro)
	}
	if code := f.run("status"); code != 0 || !strings.Contains(f.Out.String(), "retro:    ") || !strings.Contains(f.Out.String(), "3 PRs, 2 classified · 2 new misses") {
		t.Errorf("status text: exit %d\n%s", code, f.Out.String())
	}
}
