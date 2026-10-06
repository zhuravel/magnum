package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// The curator is a pane agent like the retro's classifier: its own tagged
// agent in a "learn notes" workspace, working in the repository's curation
// directory, prompted with paths only. A first answer that is not valid gets
// one nudge naming the problems; the corrected proposal is stored for
// review, and the agent is parked and its scratch registry removed.
func TestThePaneCuratorProducesAProposalAfterANudge(t *testing.T) {
	ag := &fakePaneAgents{}
	var nr notes.Repo
	h := newHarness(t, func(h *harness) {
		h.cfg.Notes.MaxBytes = 64
		h.d.Curator = paneCurators(paneDeps{
			Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: h.hd, Now: h.clock.Now,
			Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
			ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: h.hd.WorkspaceClose,
			Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
				if len(cfg.Roles) != 1 || cfg.Roles[0].Name != config.NotesRoleName {
					t.Errorf("the manager's roles = %v, want the [notes] role alone", cfg.Roles)
				}
				ag.fakeAgents = &fakeAgents{st: st}
				ag.scratch = layout.Scratch
				return ag
			},
		})
	})
	nr = notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n",
		map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	knownRepo(t, h)
	ag.reply = func(n int, _ string) bool {
		dirs, _ := filepath.Glob(filepath.Join(nr.Curate(), "2*"))
		s := notes.Scratch{Dir: dirs[len(dirs)-1]}
		goodProposal(s)
		if n == 1 { // the first answer forgets a reason
			b, _ := os.ReadFile(s.Changes())
			_ = os.WriteFile(s.Changes(), []byte(strings.Replace(string(b), `"reason":"every review runs specs"`, `"reason":""`, 1)), 0o600)
		}
		return true
	}
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()

	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending {
		t.Fatalf("proposals = %+v", ps)
	}
	sent := ag.sent()
	if len(sent) != 2 || !strings.Contains(sent[1], `section "Tests" in changes.json has no reason`) || !strings.Contains(sent[0], ps[0].Scratch) {
		t.Fatalf("prompts = %q", sent)
	}
	if strings.Contains(sent[0], "#11920") || strings.Contains(sent[0], "campaign_snapshot") {
		t.Errorf("the prompt carries notes text:\n%s", sent[0])
	}
	calls := ag.all()
	ws := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "ensure_workspace:") })
	if len(ws) != 1 || !strings.Contains(ws[0], ":"+nr.Curate()+":learn notes:") || !strings.HasSuffix(ws[0], ":notes") {
		t.Fatalf("agent setup = %v", calls)
	}
	if ag.count("park:") != 1 {
		t.Errorf("park calls: %v", calls)
	}
	if _, err := os.Stat(ag.scratch); !os.IsNotExist(err) {
		t.Errorf("scratch registry %s: %v", ag.scratch, err)
	}
}
