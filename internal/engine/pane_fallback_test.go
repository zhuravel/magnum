package engine

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// fableLimitScreen is what Claude Code prints when the Fable model is capped
// while the account still has usage left.
const fableLimitScreen = "  ⎿  You've reached your Fable limit. /model to switch models.\n"

// paneCuratorHarness is a harness whose curator is the pane agent over ag,
// with the live registry beside its scratch one, and notes past their size
// limit holding a probe.
func paneCuratorHarness(t *testing.T, ag *fakePaneAgents, mods ...func(*harness)) (*harness, notes.Repo) {
	t.Helper()
	mods = append([]func(*harness){func(h *harness) {
		h.cfg.Notes.MaxBytes = 64
		h.d.Curator = paneCurators(paneDeps{
			Config: h.cfg, Layout: h.layout, Logger: h.d.Logger, Herdr: h.hd, Store: h.st, Now: h.clock.Now,
			Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
			ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: h.hd.WorkspaceClose,
			Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
				ag.fakeAgents = &fakeAgents{st: st}
				ag.scratch, ag.cfg = layout.Scratch, cfg
				return ag
			},
		})
	}}, mods...)
	h := newHarness(t, mods...)
	nr := notesOf(t, h)
	writeNotes(t, nr, "# Notes for talkable/talkable\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes #11920\n",
		map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	knownRepo(t, h)
	return h, nr
}

// latestScratch is the newest curation's scratch directory of nr.
func latestScratch(nr notes.Repo) notes.Scratch {
	dirs, _ := filepath.Glob(filepath.Join(nr.Curate(), "2*"))
	if len(dirs) == 0 {
		return notes.Scratch{}
	}
	return notes.Scratch{Dir: dirs[len(dirs)-1]}
}

// with keeps the calls that start with prefix.
func with(calls []string, prefix string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, prefix) })
}

// liveLimit is when the live registry says kind's model is limited until.
func liveLimit(t *testing.T, h *harness, kind, model string) (time.Time, bool) {
	t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, agents.KVModelLimited(kind, model))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return time.Time{}, false
	}
	until, err := store.ParseTime(v)
	if err != nil {
		t.Fatal(err)
	}
	return until, true
}

// A pane job whose model hits its own limit goes on inside the job: the limit
// is recorded in the live registry (rounds and the next job read it) and in
// the job's own, the agent is released, and a fresh session (never a resume
// of the parked one) starts on the role's first free fallback with the same
// first prompt. The answer comes from that model, and the proposal records
// it. The kind is not paused.
func TestAPaneJobOnALimitedModelContinuesOnItsFallbackInsideTheJob(t *testing.T) {
	ag := &fakePaneAgents{}
	h, nr := paneCuratorHarness(t, ag, func(h *harness) {
		h.cfg.Notes.Model, h.cfg.Notes.FallbackModels = "fable", []string{"opus"}
	})
	ag.reply = func(n int, _ string) bool {
		switch n {
		case 1: // fable is capped: nothing is written
			ag.setScreen(fableLimitScreen)
		case 2: // the fresh session on opus answers
			ag.setScreen("")
			goodProposal(latestScratch(nr))
		}
		return true
	}
	limitAt := h.clock.Now()
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()

	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].State != store.ProposalPending || ps[0].Model != "opus" {
		t.Fatalf("proposals = %+v, want one pending, its model opus", ps)
	}
	sent := ag.sent()
	if len(sent) != 2 || sent[1] != sent[0] {
		t.Fatalf("prompts = %q, want the first prompt again for the new session (no nudge)", sent)
	}
	calls := ag.all()
	starts := with(calls, "start:")
	if len(starts) != 2 || !strings.HasSuffix(starts[0], ":notes:") || !strings.HasSuffix(starts[1], ":notes:") {
		t.Fatalf("starts = %q, want two fresh starts (no conversation resumed)", starts)
	}
	if got := with(calls, "start_model:"); !slices.Equal(got, []string{"start_model:fable", "start_model:opus"}) {
		t.Fatalf("start models = %q, want fable, then opus", got)
	}
	first := slices.Index(calls, starts[0])
	second := first + 1 + slices.Index(calls[first+1:], starts[1])
	if len(with(calls[first:second], "park:")) != 1 {
		t.Fatalf("calls = %q, want the fable session parked before the opus one starts", calls)
	}
	if got := with(calls, "note_limit:"); !slices.Equal(got, []string{"note_limit:fable"}) {
		t.Fatalf("scratch records = %q, want fable's limit", got)
	}

	// The live registry holds the limit: what rounds and the next job read.
	until, ok := liveLimit(t, h, "claude", "fable")
	if want := limitAt.Add(h.cfg.Daemon.ModelLimitCooldown.Duration); !ok || !until.Equal(want) {
		t.Fatalf("live fable limit = %s %v, want %s (no reset named: the cooldown)", until, ok, want)
	}
	if got := agents.ModelLimits(h.ctx, h.st, h.cfg, "claude", h.clock.Now()); len(got) != 1 || got[0].Model != "fable" {
		t.Fatalf("live limits = %+v, want fable", got)
	}
	if evs := h.events(agents.EventModelLimited); len(evs) != 1 || !strings.Contains(evs[0].Message, "fable") {
		t.Fatalf("live %s events = %+v, want one naming fable", agents.EventModelLimited, evs)
	}
	if reason := h.e.kindPauseReason(h.ctx, []string{"claude"}); reason != "" {
		t.Fatalf("claude paused: %s", reason)
	}
}

// The next pane job knows a limit a round or an earlier job recorded in the
// live registry: it starts on the fallback at once, and its answer carries
// that model.
func TestTheNextPaneJobStartsOnTheFallbackOfALimitRecordedBefore(t *testing.T) {
	p := newPaneHarness(t, func(h *harness) { h.cfg.Learn.Model = "fable" })
	if _, err := agents.RecordModelLimit(p.ctx, p.st, "claude", "fable", nil, time.Hour, p.clock.Now()); err != nil {
		t.Fatal(err)
	}
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.reply = func(int, string) bool { validAnswer(t, job); return true }
	cl := p.classifier(run)
	res, err := cl.Classify(p.ctx, job)
	if err != nil || res.Pause != nil || res.Model != "opus" {
		t.Fatalf("Classify = %+v, %v; want an answer from opus", res, err)
	}
	if got := with(p.ag.all(), "start_model:"); !slices.Equal(got, []string{"start_model:opus"}) {
		t.Fatalf("start models = %q, want opus alone", got)
	}
	_ = cl.Close(p.ctx)
}

// With no fallback left (the role's list is opus, and opus is limited) the
// job stops as any curation stop does: uncounted, retried later, nothing
// stored, and the kind is never paused: rounds still have their models.
func TestAPaneJobWithNoFallbackLeftStopsAndNeverPausesTheKind(t *testing.T) {
	ag := &fakePaneAgents{}
	h, _ := paneCuratorHarness(t, ag, func(h *harness) {
		h.cfg.Notes.Model, h.cfg.Notes.FallbackModels = "fable", []string{"opus"}
	})
	if _, err := agents.RecordModelLimit(h.ctx, h.st, "claude", "opus", nil, time.Hour, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	ag.reply = func(int, string) bool { ag.setScreen(fableLimitScreen); return true }
	if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
		t.Fatal(err)
	}
	h.settle()

	if ps := proposals(t, h); len(ps) != 0 {
		t.Fatalf("proposals = %+v, want none", ps)
	}
	if got := with(ag.all(), "start_model:"); !slices.Equal(got, []string{"start_model:fable"}) {
		t.Fatalf("start models = %q, want one start on fable", got)
	}
	if n := len(ag.sent()); n != 1 {
		t.Fatalf("prompts = %d, want one: a limit is not nudged", n)
	}
	evs := h.events("notes.curate_stopped")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "claude: model_limit") {
		t.Fatalf("stops = %+v, want one for claude's model limit", evs)
	}
	h.e.curateMu.Lock()
	tried := len(h.e.curateTried)
	h.e.curateMu.Unlock()
	if tried != 1 {
		t.Fatal("the stop is not remembered: the curation would start again at once")
	}
	if _, ok := liveLimit(t, h, "claude", "fable"); !ok {
		t.Fatal("fable's limit is not in the live registry")
	}
	if reason := h.e.kindPauseReason(h.ctx, []string{"claude"}); reason != "" {
		t.Fatalf("claude paused: %s", reason)
	}
}

// One restart per job: a fallback that hits its own limit too stops the job,
// and both limits are in the live registry.
func TestASecondModelLimitInOnePaneJobStopsIt(t *testing.T) {
	p := newPaneHarness(t, func(h *harness) { h.cfg.Learn.Model = "fable" }) // the kind's list: opus, sonnet
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.reply = func(n int, _ string) bool {
		switch n {
		case 1:
			p.ag.setScreen(job.OutputPath + "\n" + fableLimitScreen)
		case 2:
			p.ag.setScreen(job.OutputPath + "\n  ⎿  You've hit your Opus limit · resets 3:45pm\n")
		}
		return true
	}
	cl := p.classifier(run)
	res, err := cl.Classify(p.ctx, job)
	if err == nil || res.Pause == nil || res.Pause.Kind != string(agents.HealthModelLimit) || res.Pause.Tool != "claude" {
		t.Fatalf("Classify = %+v, %v; want a model-limit stop", res.Pause, err)
	}
	if got := with(p.ag.all(), "start_model:"); !slices.Equal(got, []string{"start_model:fable", "start_model:opus"}) {
		t.Fatalf("start models = %q, want fable, opus and no third", got)
	}
	if n := len(p.ag.sent()); n != 2 {
		t.Fatalf("prompts = %d, want one per session", n)
	}
	for _, model := range []string{"fable", "opus"} {
		if _, ok := liveLimit(t, p.harness, "claude", model); !ok {
			t.Errorf("%s's limit is not in the live registry", model)
		}
	}
	_ = cl.Close(p.ctx)
}

// `magnum resume --tool` (ClearModelLimits) forgets a kind's limits and what
// they were: a model limited again after it is no repeat, so its cooldown
// is not doubled.
func TestClearModelLimitsForgetsTheRepeatsToo(t *testing.T) {
	h := newHarness(t)
	t0, cooldown := h.clock.Now(), 5*time.Hour
	if _, err := agents.RecordModelLimit(h.ctx, h.st, "claude", "fable", nil, cooldown, t0); err != nil {
		t.Fatal(err)
	}
	if models, err := ClearModelLimits(h.ctx, h.st, "claude"); err != nil || !slices.Equal(models, []string{"fable"}) {
		t.Fatalf("ClearModelLimits = %q, %v", models, err)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, agents.KVModelLimitedAt("claude", "fable")); ok {
		t.Fatal("the repeat record of fable stays")
	}
	again := t0.Add(6 * time.Hour)
	if until, err := agents.RecordModelLimit(h.ctx, h.st, "claude", "fable", nil, cooldown, again); err != nil || !until.Equal(again.Add(cooldown)) {
		t.Fatalf("a limit after the clear ends %s, %v; want the plain %s", until, err, again.Add(cooldown))
	}
}

// blockSession shows the scratch registry's live session of the agent as
// herdr's observation would while a dialog waits: agent status blocked, since
// now.
func (p *paneHarness) blockSession(ctx context.Context) {
	ss, err := p.ag.st.LiveSessions(ctx)
	if err != nil || len(ss) != 1 {
		p.t.Errorf("live scratch sessions = %+v, %v", ss, err)
		return
	}
	now := p.clock.Now()
	if err := p.ag.st.TransitionSession(ctx, ss[0].ID, []string{store.SessionLive}, store.SessionLive, func(u *store.SessionUpdate) {
		u.Set("agent_status", "blocked")
		u.Set("agent_status_at", now)
	}); err != nil {
		p.t.Error(err)
	}
}

// unblockSessions shows the scratch registry's blocked sessions idle, as
// the observation does once escape closed the dialog.
func (p *paneHarness) unblockSessions() {
	ctx := context.Background()
	ss, err := p.ag.st.LiveSessions(ctx)
	if err != nil {
		p.t.Error(err)
		return
	}
	for _, s := range ss {
		if store.Deref(s.AgentStatus) != "blocked" {
			continue
		}
		if err := p.ag.st.TransitionSession(ctx, s.ID, []string{store.SessionLive}, store.SessionLive, func(u *store.SessionUpdate) {
			u.Set("agent_status", "idle")
			u.Set("agent_status_at", p.clock.Now())
		}); err != nil {
			p.t.Error(err)
		}
	}
}

// A turn whose session waits on a dialog magnum does not answer ends after
// paneBlockedWait, long before the role's timeout: escape goes to the agent,
// the run is abandoned and the screen read as it stood. An unknown dialog is
// then nudged; Claude Code's consent before it bills Fable to usage credits
// is Fable's limit, so the job goes on on the fallback.
func TestATurnBlockedOnADialogEndsAfterPaneBlockedWait(t *testing.T) {
	for name, tc := range map[string]struct {
		screen    string
		restarted bool
	}{
		"unknown dialog":    {"Try the new editor?\n❯ 1. Yes\n  2. Not now\n", false},
		"the Fable consent": {"Fable limit reached · continuing on Fable 5.1 uses usage credits\n❯ 1. Yes, continue\n  2. No, keep my current model\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPaneHarness(t, func(h *harness) { h.cfg.Learn.Model = "fable" })
			p.step = 10 * time.Second
			run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
			job := p.job(run, 7)
			p.ag.reply = func(n int, _ string) bool {
				if n == 1 { // the agent waits on the dialog; its turn never ends by itself
					p.ag.setScreen(job.OutputPath + "\n" + tc.screen)
					p.blockSession(context.Background())
					return false
				}
				p.ag.setScreen("")
				validAnswer(t, job)
				return true
			}
			start := p.clock.Now()
			cl := p.classifier(run)
			res, err := cl.Classify(p.ctx, job)
			if err != nil || res.Pause != nil {
				t.Fatalf("Classify = %+v, %v", res, err)
			}
			if elapsed := p.clock.Now().Sub(start); elapsed < paneBlockedWait || elapsed >= p.cfg.Learn.Timeout.Duration {
				t.Fatalf("the blocked turn ended after %s, want past %s and well before the %s timeout", elapsed, paneBlockedWait,
					p.cfg.Learn.Timeout.Duration)
			}
			if len(p.keys) != 1 || !strings.HasSuffix(p.keys[0], ":esc") {
				t.Fatalf("keys = %q, want one escape", p.keys)
			}
			if r, err := p.ag.st.RunByID(p.ctx, "r-learn-1"); err != nil || r.State != store.RunAbandoned || !strings.Contains(store.Deref(r.Error), "dialog") {
				t.Fatalf("the blocked run = %+v, %v; want it abandoned for the dialog", r, err)
			}
			sent := p.ag.sent()
			models := with(p.ag.all(), "start_model:")
			switch {
			case tc.restarted && (!slices.Equal(models, []string{"start_model:fable", "start_model:opus"}) || len(sent) != 2 ||
				sent[1] != sent[0] || res.Model != "opus"):
				t.Fatalf("start models %q, %d prompts, model %q; want a restart on opus with the first prompt", models, len(sent), res.Model)
			case !tc.restarted && (len(models) != 1 || len(sent) != 2 || !strings.Contains(sent[1], "was not written") || res.Model != "fable"):
				t.Fatalf("start models %q, prompts %q, model %q; want one session nudged", models, sent, res.Model)
			}
			_ = cl.Close(p.ctx)
		})
	}
}
