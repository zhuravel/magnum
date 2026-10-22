package agents

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// setRoleFallbacks gives the configured role r its own fallback_models.
func (e *env) setRoleFallbacks(r Role, models ...string) {
	e.t.Helper()
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == string(r) {
			e.cfg.Roles[i].FallbackModels = models
			return
		}
	}
	e.t.Fatalf("no role %s", r)
}

// A role's own fallback_models replace its kind's in every path that picks
// a fallback (a fresh launch, the switch before a prompt, the switch after a
// limit, the model NoteModelLimit says is in use, LaunchModel); the two lists
// never merge. ModelLimits has no role and names the kind's list.
func TestARoleFallbackListWinsOverTheKindsInEveryPath(t *testing.T) {
	e := newEnv(t)
	e.setRoleFallbacks(RoleClaude, "sonnet") // the kind's list is opus, sonnet
	e.limitFable(e.clock.Now().Add(time.Hour))

	// startModel: the launch skips the kind's opus.
	ws := e.started()
	var claudeArgs []string
	for _, st := range e.h.starts {
		if st.Kind == KindClaude {
			claudeArgs = st.Args
		}
	}
	if i := slices.Index(claudeArgs, "--model"); i < 0 || i+1 >= len(claudeArgs) || claudeArgs[i+1] != "sonnet" {
		t.Fatalf("claude args = %q, want --model sonnet (the role's list)", claudeArgs)
	}
	s := e.session(RoleClaude)
	if v, _ := e.kv(KVSessionModel(s.ID)); v != "sonnet" {
		t.Fatalf("session model = %q, want sonnet", v)
	}

	// ensureModel: a session on the limited fable switches to sonnet before
	// its prompt.
	if err := e.st.DeleteKV(e.ctx, KVSessionModel(s.ID)); err != nil {
		t.Fatal(err)
	}
	e.claudeScreen(false, "")
	e.clock.Add(time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunContinue, "go on"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.h.runs, paneRunCall{ws.Panes[RoleClaude], "/model sonnet"}) {
		t.Fatalf("pane runs = %+v, want /model sonnet", e.h.runs)
	}

	// FallbackModel: on sonnet, the role has nothing left; the kind's opus is
	// not the role's to use.
	if m, ok := e.m.FallbackModel(e.ctx, s, nil); ok {
		t.Fatalf("FallbackModel on sonnet = %q, want none: the role lists sonnet alone", m)
	}
	if err := e.st.DeleteKV(e.ctx, KVSessionModel(s.ID)); err != nil {
		t.Fatal(err)
	}
	if m, ok := e.m.FallbackModel(e.ctx, s, nil); !ok || m != "sonnet" {
		t.Fatalf("FallbackModel on fable = %q %v, want sonnet", m, ok)
	}

	// NoteModelLimit's Using names the role's list.
	l, err := e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable"})
	if err != nil || l.Using != "sonnet" {
		t.Fatalf("NoteModelLimit = %+v, %v; want sonnet in use", l, err)
	}

	// LaunchModel, the pane jobs' question, answers alike.
	if m, fb, none := LaunchModel(e.ctx, e.st, e.cfg, e.spec(RoleClaude), e.clock.Now()); m != "sonnet" || !fb || none {
		t.Fatalf("LaunchModel = %q %v %v, want sonnet on a fallback", m, fb, none)
	}

	// ModelLimits has no role: the kind's own list.
	if got := ModelLimits(e.ctx, e.st, e.cfg, KindClaude, e.clock.Now()); len(got) != 1 || got[0].Using != "opus" {
		t.Fatalf("ModelLimits = %+v, want fable using the kind's opus", got)
	}
}

// A role with fallback models of its own still needs a kind that can pick a
// model: it has no other way to launch on one.
func TestARoleFallbackListNeedsTheKindsSwitchModel(t *testing.T) {
	e := newEnv(t)
	e.setRoleFallbacks(RoleClaude, "sonnet")
	e.setKind(KindClaude, func(k *config.Kind) { k.SwitchModel = "" })
	e.limitFable(e.clock.Now().Add(time.Hour))
	if m, fb, none := LaunchModel(e.ctx, e.st, e.cfg, e.spec(RoleClaude), e.clock.Now()); m != "" || fb || !none {
		t.Fatalf("LaunchModel = %q %v %v, want none: switch_model = \"\" turns switching off", m, fb, none)
	}
}

// A pane job's scratch registry knows the limits the live one holds once
// CopyModelLimits seeded it: LaunchModel over the scratch registry skips the
// model rounds found limited, and one whose limit ended is not copied.
func TestCopyModelLimitsSeedsAnotherRegistry(t *testing.T) {
	e := newEnv(t)
	now := e.clock.Now()
	e.limitFable(now.Add(time.Hour))
	if _, err := RecordModelLimit(e.ctx, e.st, KindClaude, "haiku", nil, time.Minute, now.Add(-time.Hour)); err != nil {
		t.Fatal(err) // ended at -59m
	}
	scratch := storetest.Open(t, filepath.Join(t.TempDir(), "scratch", "magnum.db"))
	role := config.Role{Name: "notes", Kind: KindClaude, Model: "fable"}
	if m, fb, none := LaunchModel(e.ctx, scratch, e.cfg, role, now); m != "fable" || fb || none {
		t.Fatalf("LaunchModel before the copy = %q %v %v, want fable", m, fb, none)
	}
	if err := CopyModelLimits(e.ctx, e.st, scratch, KindClaude, now); err != nil {
		t.Fatal(err)
	}
	if m, fb, none := LaunchModel(e.ctx, scratch, e.cfg, role, now); m != "opus" || !fb || none {
		t.Fatalf("LaunchModel after the copy = %q %v %v, want opus on a fallback", m, fb, none)
	}
	got := ModelLimits(e.ctx, scratch, e.cfg, KindClaude, now)
	if len(got) != 1 || got[0].Model != "fable" || !got[0].Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("scratch limits = %+v, want fable alone, until the live end", got)
	}
	// A role without a model runs the CLI's default, which the copy names.
	role.Model = ""
	if m, fb, _ := LaunchModel(e.ctx, scratch, e.cfg, role, now); m != "opus" || !fb {
		t.Fatalf("LaunchModel for a role on the CLI default = %q %v, want opus", m, fb)
	}
	// Every fallback limited: none to launch on.
	e.recordLimits(now.Add(time.Hour), "fable", "opus", "sonnet")
	if err := CopyModelLimits(e.ctx, e.st, scratch, KindClaude, now); err != nil {
		t.Fatal(err)
	}
	if _, fb, none := LaunchModel(e.ctx, scratch, e.cfg, config.Role{Name: "notes", Kind: KindClaude, Model: "fable"}, now); fb || !none {
		t.Fatalf("LaunchModel with every model limited: fallback %v none %v, want none", fb, none)
	}
}

// A model that hits its limit again less than a day after its last limit
// ended is a longer window being probed: without a reset time named, each
// such limit lasts twice the one before, up to MaxReset (24h), so a weekly
// window costs at most one probe of the model a day. A report while the
// limit lasts is no repeat, and a day without a hit starts over.
func TestALimitHitAgainWithinADayDoublesItsCooldownUpTo24Hours(t *testing.T) {
	e := newEnv(t)
	t0 := e.clock.Now()
	const cooldown = 3 * time.Hour
	h := func(n int) time.Duration { return time.Duration(n) * time.Hour }
	record := func(at time.Duration, model string) time.Time {
		t.Helper()
		got, err := RecordModelLimit(e.ctx, e.st, KindClaude, model, nil, cooldown, t0.Add(at))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got, want := record(0, "fable"), t0.Add(cooldown); !got.Equal(want) {
		t.Fatalf("first limit ends %s, want %s", got, want)
	}
	// Another session reports it an hour in: its own cooldown, not doubled.
	if got, want := record(h(1), "fable"), t0.Add(h(1)+cooldown); !got.Equal(want) {
		t.Fatalf("a second report while the limit lasts ends %s, want %s", got, want)
	}
	record(h(4)+30*time.Minute, "sonnet") // drops fable's ended row (4h, a 4h span); the memory stays
	if _, ok := e.kv(KVModelLimited(KindClaude, "fable")); ok {
		t.Fatal("fable's ended limit row stays")
	}
	for _, step := range []struct {
		at, span time.Duration
		why      string
	}{
		{h(5), h(8), "an hour after the 4h limit ended: twice 4h"},
		{h(14), h(16), "an hour after that ended: twice 8h"},
		{h(31), MaxReset, "twice 16h is past MaxReset"},
		{h(56), MaxReset, "stays at MaxReset while the window lasts"},
	} {
		if got, want := record(step.at, "fable"), t0.Add(step.at+step.span); !got.Equal(want) {
			t.Fatalf("limit at %s ends %s, want %s: %s", step.at, got, want, step.why)
		}
	}
	// The last ended at 80h; 25h without a hit: a new window.
	if got, want := record(h(105), "fable"), t0.Add(h(105)+cooldown); !got.Equal(want) {
		t.Fatalf("a limit a day after the last ended ends %s, want the plain %s", got, want)
	}
	if v, _ := e.kv(KVModelLimits(KindClaude)); v != "fable" {
		t.Fatalf("index = %q, want the active fable alone", v)
	}

	// A cooldown longer than MaxReset is the operator's: a repeat keeps it.
	long := 48 * time.Hour
	if _, err := RecordModelLimit(e.ctx, e.st, KindClaude, "haiku", nil, long, t0); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(49 * time.Hour)
	if got, err := RecordModelLimit(e.ctx, e.st, KindClaude, "haiku", nil, long, at); err != nil || !got.Equal(at.Add(long)) {
		t.Fatalf("a repeat of a 48h cooldown = %s, %v; want %s", got, err, at.Add(long))
	}
}

// A limit message that names its reset keeps it on a repeat: the CLI knows
// the end better than the doubled guess. NoteModelLimit, the rounds' record,
// doubles a repeat without one as RecordModelLimit does.
func TestARepeatedLimitKeepsANamedResetAndDoublesWithoutOne(t *testing.T) {
	e := newEnv(t)
	e.started()
	s := e.session(RoleClaude)
	if _, err := e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(DefaultModelLimitCooldown + time.Hour) // ended an hour ago
	reset := e.clock.Now().Add(2 * time.Hour)
	if l, err := e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable", ResetAt: &reset}); err != nil || !l.Until.Equal(reset) {
		t.Fatalf("a repeat naming its reset = %+v, %v; want until %s", l, err, reset)
	}
	e.clock.Add(3 * time.Hour) // that one ended an hour ago, after a 2h span
	l, err := e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable"})
	if want := e.clock.Now().Add(DefaultModelLimitCooldown); err != nil || !l.Until.Equal(want) {
		t.Fatalf("a repeat after a 2h named span = %+v, %v; want the plain cooldown %s (twice 2h is shorter)", l, err, want)
	}
	e.clock.Add(DefaultModelLimitCooldown + time.Hour)
	l, err = e.m.NoteModelLimit(e.ctx, s, Health{Kind: HealthModelLimit, Model: "fable"})
	if want := e.clock.Now().Add(2 * DefaultModelLimitCooldown); err != nil || !l.Until.Equal(want) {
		t.Fatalf("a repeat without a reset = %+v, %v; want %s", l, err, want)
	}
}
