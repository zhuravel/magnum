package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// scriptedCritic is a Critic whose Critique runs script, so a test can make
// it fail, hit a limit or see its context end.
type scriptedCritic struct {
	mu     sync.Mutex
	calls  int
	closed int
	script func(ctx context.Context, job CritiqueJob) (CritiqueResult, error)
}

func (c *scriptedCritic) Critique(ctx context.Context, job CritiqueJob) (CritiqueResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.script(ctx, job)
}

func (c *scriptedCritic) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *scriptedCritic) count() (calls, closed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.closed
}

// scriptedCriticHarness is a critic harness whose critic runs script.
func scriptedCriticHarness(t *testing.T, script func(context.Context, CritiqueJob) (CritiqueResult, error), mods ...func(*harness)) (*harness, *scriptedCritic) {
	t.Helper()
	sc := &scriptedCritic{script: script}
	mods = append([]func(*harness){func(h *harness) {
		h.d.Critic = func(context.Context, CriticRun) (Critic, error) { return sc, nil }
	}}, mods...)
	h, _, _ := criticHarness(t, decides(notes.DecisionApply, ""), mods...)
	return h, sc
}

// stoppedCritique checks that proposal p has n critiques, the newest ended
// stopped with detail containing why, and no attempt counted: a stop is the
// daemon's or the agent's, not the critic's answer.
func stoppedCritique(t *testing.T, h *harness, p store.NotesProposal, n int, why string) {
	t.Helper()
	cs := critiques(t, h, p.ID)
	if len(cs) != n {
		t.Fatalf("critiques = %+v, want %d", cs, n)
	}
	if got := cs[0]; got.Outcome != store.CritiqueStopped || !strings.Contains(got.Detail, why) {
		t.Fatalf("the newest critique = %+v, want it stopped: %s", got, why)
	}
	if attempts, _ := CriticAttempts(h.ctx, h.st, p.ID); attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
	if got := proposalNow(t, h, p.ID); got.State != store.ProposalPending {
		t.Errorf("the proposal = %+v, want it pending", got)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesCritic); v != "" {
		t.Errorf("the critique's mark stays: %q", v)
	}
}

// A critic that returns an error (its pane went away) stops the critique:
// recorded stopped with the error, a warning, no attempt, the agent closed,
// and the proposal is critiqued again only after an hour.
func TestACriticThatReturnsAnErrorStopsTheCritiqueAndIsTriedAgainAfterAnHour(t *testing.T) {
	h, sc := scriptedCriticHarness(t, func(context.Context, CritiqueJob) (CritiqueResult, error) {
		return CritiqueResult{}, errors.New("the critic's pane went away")
	})
	p := curatedProposal(t, h)
	h.tick()
	stoppedCritique(t, h, p, 1, "the critic's pane went away")
	if evs := notesEvents(t, h, "notes.critic_stopped"); len(evs) != 1 || evs[0].Level != "warn" {
		t.Errorf("stopped events = %+v, want one warning", evs)
	}
	if calls, closed := sc.count(); calls != 1 || closed != 1 {
		t.Errorf("critiques = %d, closed = %d", calls, closed)
	}
	h.advance(30 * time.Minute)
	h.tick()
	if calls, _ := sc.count(); calls != 1 {
		t.Fatalf("critiques = %d within the hour, want 1", calls)
	}
	h.advance(31 * time.Minute)
	h.tick()
	if calls, _ := sc.count(); calls != 2 {
		t.Fatalf("critiques = %d after the hour, want 2", calls)
	}
	stoppedCritique(t, h, p, 2, "the critic's pane went away")
}

// A critique whose context ends (the daemon stops) is stopped as the
// daemon's, not as an answer: no attempt, whatever error the agent
// returned with it.
func TestACritiqueWhoseContextEndsIsStoppedByTheDaemonNotCounted(t *testing.T) {
	var h *harness
	h, sc := scriptedCriticHarness(t, func(ctx context.Context, _ CritiqueJob) (CritiqueResult, error) {
		h.e.stopCritic() // the shutdown cancels the critique's context
		<-ctx.Done()
		return CritiqueResult{}, errors.Join(ErrCriticDown, ctx.Err())
	})
	p := curatedProposal(t, h)
	h.tick()
	stoppedCritique(t, h, p, 1, "the daemon is stopping")
	if _, closed := sc.count(); closed != 1 {
		t.Errorf("closed = %d, want the agent closed once", closed)
	}
}

// A critic that hits its CLI's usage limit stops the critique and pauses
// the kind: no critique starts while the pause lasts, and one does when it
// is over.
func TestACritiqueThatHitsAUsageLimitPausesTheCriticsKind(t *testing.T) {
	var h *harness
	h, sc := scriptedCriticHarness(t, func(context.Context, CritiqueJob) (CritiqueResult, error) {
		return CritiqueResult{Pause: &pipeline.Pause{Tool: h.cfg.CriticRole().AgentKind(), Kind: string(agents.HealthUsageLimit),
			Until: h.clock.Now().Add(3 * time.Hour)}}, errors.New("the notes critic stopped: usage_limit")
	})
	p := curatedProposal(t, h)
	h.tick()
	stoppedCritique(t, h, p, 1, "usage_limit")
	if why := h.e.criticToolPause(h.ctx); why == "" {
		t.Fatal("the critic's kind is not paused")
	}
	h.advance(61 * time.Minute) // the retry spacing is over; the pause is not
	h.tick()
	if calls, _ := sc.count(); calls != 1 {
		t.Fatalf("critiques = %d under the pause, want 1", calls)
	}
	h.advance(2 * time.Hour)
	h.tick()
	if calls, _ := sc.count(); calls != 2 {
		t.Fatalf("critiques = %d after the pause, want 2", calls)
	}
}

// An agent that cannot be set up stops the critique too: the error names
// why, and nothing is counted.
func TestACriticThatCannotStartStopsTheCritique(t *testing.T) {
	h, _, _ := criticHarness(t, decides(notes.DecisionApply, ""), func(h *harness) {
		h.d.Critic = func(context.Context, CriticRun) (Critic, error) { return nil, errors.New("herdr is down") }
	})
	p := curatedProposal(t, h)
	h.tick()
	stoppedCritique(t, h, p, 1, "the critic cannot start: herdr is down")
}

// A critique whose inputs cannot be built (its prompt does not read) stops
// before an agent is asked, with the reason.
func TestACritiqueWhoseInputsCannotBeBuiltStopsBeforeTheAgentIsAsked(t *testing.T) {
	h, sc := scriptedCriticHarness(t, func(context.Context, CritiqueJob) (CritiqueResult, error) {
		return CritiqueResult{}, errors.New("the agent must not be asked")
	}, func(h *harness) { h.cfg.Notes.CriticPrompt = "no-such-prompt.md" })
	p := curatedProposal(t, h)
	h.tick()
	stoppedCritique(t, h, p, 1, "its inputs: the critic's prompt cannot be read")
	if calls, _ := sc.count(); calls != 0 {
		t.Errorf("the agent was asked %d times", calls)
	}
}

// A rejection or a re-curation starts a new curation only where one can
// run: with [notes] curate off, or no curator, no mark is set and the
// event and the toast name `magnum notes <repo> --curate`, which asks for
// one; with curate set the mark is set and they say a new curation runs.
func TestACriticRejectionPromisesACurationOnlyWhereOneCanRun(t *testing.T) {
	const command = "`magnum notes talkable/talkable --curate`"
	for _, decision := range []string{notes.DecisionReject, notes.DecisionRecurate} {
		for _, c := range []struct {
			name   string
			setup  func(h *harness)
			curate bool
		}{
			{"curate set", func(*harness) {}, true},
			{"curate off", func(h *harness) { h.cfg.Notes.Curate = nil }, false},
			{"no curator", func(h *harness) { h.e.d.Curator = nil }, false},
		} {
			t.Run(decision+"/"+c.name, func(t *testing.T) {
				h, _, nr := criticHarness(t, decides(decision, "keep the QA steps for the admin"))
				var p store.NotesProposal
				if decision == notes.DecisionRecurate { // valid only for a proposal that cannot be applied
					p = lossyProposal(t, h, nr)
				} else {
					p = curatedProposal(t, h) // asked for: it runs whatever [notes] curate says
				}
				c.setup(h)
				h.tick()
				if got := proposalNow(t, h, p.ID); got.DecidedBy != store.DecidedByCritic || got.State == store.ProposalPending {
					t.Fatalf("the proposal = %+v, want the critic's decision", got)
				}
				mark, _ := h.e.getKV(h.ctx, KVNotesRecurate("talkable/talkable"))
				msg, toasts := decidedEvent(t, h), toastText(h)
				if c.curate {
					if mark != itoa(p.ID) || !strings.Contains(msg, "A new curation runs.") || strings.Contains(msg+toasts, command) {
						t.Errorf("mark %q, event %q, toasts %q: want the mark and a new curation", mark, msg, toasts)
					}
					return
				}
				if mark != "" {
					t.Errorf("the mark %q is set where no curation can run", mark)
				}
				if !strings.Contains(msg, command) || strings.Contains(msg, "A new curation runs") {
					t.Errorf("event %q: want it to name %s and not promise a curation", msg, command)
				}
				if !strings.Contains(toasts, command) || strings.Contains(toasts, "A new curation runs") {
					t.Errorf("toasts %q: want them to name %s and not promise a curation", toasts, command)
				}
				h.advance(11 * time.Minute)
				h.tick()
				if ps := proposals(t, h); len(ps) != 1 {
					t.Errorf("proposals = %+v: a curation started by itself", ps)
				}
			})
		}
	}
}
