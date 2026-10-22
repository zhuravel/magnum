package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// The critic is a pane job like the curator: on its built-in fable it hits
// Fable's own limit, starts again inside the critique on its fallback
// (critic_fallback_models, opus) with the same prompt, and its decision
// comes from opus: the proposal is applied by the critic and the registry's
// critique names opus.
func TestACriticOnFableContinuesOnOpusAtAModelLimit(t *testing.T) {
	ag := &fakePaneAgents{}
	cur := &fakeCurator{write: goodProposal}
	var nr notes.Repo
	h := newHarness(t, func(h *harness) {
		h.cfg.Notes.MaxBytes = 64
		h.cfg.Notes.Critic = true
		h.d.Curator = func(context.Context, CurateRun) (Curator, error) { return cur, nil }
		h.d.Critic = paneCritics(paneDeps{
			Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: h.hd, Store: h.st, Now: h.clock.Now,
			Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
			ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: h.hd.WorkspaceClose,
			Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
				ag.fakeAgents = &fakeAgents{st: st}
				ag.scratch, ag.cfg = layout.Scratch, cfg
				return ag
			},
		})
	})
	if h.cfg.Notes.CriticModel != "fable" || !slices.Equal(h.cfg.Notes.CriticFallbackModels, []string{"opus"}) {
		t.Fatalf("the critic's built-in models = %q, %q", h.cfg.Notes.CriticModel, h.cfg.Notes.CriticFallbackModels)
	}
	nr = notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n",
		map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	knownRepo(t, h)
	ag.reply = func(n int, text string) bool {
		switch n {
		case 1: // fable is capped: nothing is written
			ag.setScreen(fableLimitScreen)
		case 2: // the fresh session on opus decides
			ag.setScreen("")
			dirs, _ := filepath.Glob(filepath.Join(nr.Critic(), "2*"))
			m := promptTarget.FindStringSubmatch(text)
			if len(dirs) == 0 || m == nil {
				t.Errorf("no critique directory or target: %v, %q", dirs, text)
				return true
			}
			b, _ := json.Marshal(notes.Decision{Decision: notes.DecisionApply, Target: m[1], Reason: "keeps every decision",
				Checked: []string{"the diff"}})
			_ = os.WriteFile(notes.CriticRun{Dir: dirs[len(dirs)-1]}.Decision(), b, 0o600)
		}
		return true
	}
	p := curatedProposal(t, h)
	h.tick()
	h.settle()

	got := proposalNow(t, h, p.ID)
	if got.State != store.ProposalApplied || got.DecidedBy != store.DecidedByCritic {
		t.Fatalf("the proposal = %+v, want applied by the critic", got)
	}
	cs := critiques(t, h, p.ID)
	if len(cs) != 1 || cs[0].Outcome != store.CritiqueApplied || cs[0].Model != "opus" {
		t.Fatalf("critiques = %+v, want one applied on opus", cs)
	}
	if sent := ag.sent(); len(sent) != 2 || sent[1] != sent[0] {
		t.Fatalf("prompts = %d, want the first prompt again for the opus session", len(sent))
	}
	if got := with(ag.all(), "start_model:"); !slices.Equal(got, []string{"start_model:fable", "start_model:opus"}) {
		t.Fatalf("start models = %q, want fable, then opus", got)
	}
	if _, ok := liveLimit(t, h, "claude", "fable"); !ok {
		t.Error("the live registry does not hold fable's limit")
	}
}
