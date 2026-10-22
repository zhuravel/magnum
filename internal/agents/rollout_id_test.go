package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// metaRollout writes a rollout under the Codex home home as Codex 0.162
// does: named and filed by its start in local time, its first line the
// session_meta with the id, the start, the cwd and the source ("cli" for the
// TUI, "exec" for `codex exec`; a helper's source is an object naming its
// parent), long instructions and a written-at stamp later than the start
// (the file is created at the first prompt).
func metaRollout(t *testing.T, home, id, cwd, source, parent string, started time.Time) string {
	t.Helper()
	local := started.Local()
	dir := filepath.Join(home, "sessions", local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"id": id, "session_id": id, "timestamp": started.UTC().Format("2006-01-02T15:04:05.000Z"),
		"cwd": cwd, "originator": "codex_cli_rs", "cli_version": "0.162.1", "source": source,
		"base_instructions": map[string]any{"text": strings.Repeat("You are Codex. ", 1500)}}
	if parent != "" {
		payload["parent_thread_id"], payload["session_id"] = parent, parent
		payload["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
	}
	line, err := json.Marshal(map[string]any{"timestamp": started.Add(40 * time.Second).UTC().Format("2006-01-02T15:04:05.000Z"),
		"ordinal": 0, "type": "session_meta", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// recoveredIDEvents are the EventSessionID events of the env's PR.
func (e *env) recoveredIDEvents() []store.Event {
	e.t.Helper()
	evs, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == EventSessionID {
			out = append(out, ev)
		}
	}
	return out
}

// codexWithoutID starts the env's agents, the judge's checkout at cwd, and
// returns the judge's session, which herdr reports without an id.
func (e *env) codexWithoutID(cwd string) store.Session {
	e.t.Helper()
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, cwd, map[string]string{"MAGNUM_PR_URL": e.pr.URL}, "talkable#11920", e.coreRoles())
	if err != nil {
		e.t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		e.t.Fatalf("StartAgent: %v", err)
	}
	return e.session(RoleJudge)
}

// On 2026-10-22 herdr's Codex session hook stopped reaching new sessions, and
// judges ran without an id. A Codex session herdr gave no id gets it, a
// minute after its start, from its rollout: the one of Codex's TUI whose
// cwd is the session's checkout (here through a symlink) and whose start is
// the session's. codex-review's `codex exec` in the same checkout, a helper
// agent of the session, a session in another checkout, one that started
// long before and one whose id another session holds are skipped. The id is
// recorded once, with one event naming the source, and herdr's id, when it
// comes, wins.
func TestACodexSessionHerdrGaveNoIDGetsItFromTheRolloutOfItsCheckoutAndStart(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.m.d.CodexHome = home
	checkout := t.TempDir()
	link := filepath.Join(t.TempDir(), "talkable.review1")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	s := e.codexWithoutID(link)
	start := s.StartedAt
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	const (
		judge   = "01a16446-c221-7073-b0fd-8609589b9a77"
		review  = "01a16446-c300-7000-8000-000000000001" // codex-review's codex exec
		helper  = "01a16446-c400-7000-8000-000000000002"
		other   = "01a16446-c500-7000-8000-000000000003" // another checkout
		earlier = "01a16446-c600-7000-8000-000000000004" // started 10 minutes before
		held    = "01a16446-c700-7000-8000-000000000005" // another session's
	)
	metaRollout(t, home, judge, resolved, "cli", "", start.Add(3*time.Second))
	metaRollout(t, home, review, resolved, "exec", "", start.Add(20*time.Second))
	metaRollout(t, home, helper, resolved, "", judge, start.Add(30*time.Second))
	metaRollout(t, home, other, t.TempDir(), "cli", "", start.Add(time.Second))
	metaRollout(t, home, earlier, resolved, "cli", "", start.Add(-10*time.Minute))
	metaRollout(t, home, held, resolved, "cli", "", start.Add(-time.Minute))
	if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: "codex-fix", AgentKind: new(KindCodex),
		SessionID: new(held), State: store.SessionClosed}); err != nil {
		t.Fatal(err)
	}

	e.clock.Add(30 * time.Second)
	e.observe()
	if got := store.Deref(e.session(RoleJudge).SessionID); got != "" {
		t.Fatalf("session id %q half a minute after the start, want none before %s", got, RecoverIDAfter)
	}
	e.clock.Add(40 * time.Second)
	e.observe()
	if got := store.Deref(e.session(RoleJudge).SessionID); got != judge {
		t.Fatalf("session id = %q, want the judge's rollout %s", got, judge)
	}
	e.clock.Add(30 * time.Second)
	e.observe()
	evs := e.recoveredIDEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one %s", evs, EventSessionID)
	}
	if ev := evs[0]; ev.Level != "info" || !strings.Contains(ev.Message, "from its Codex rollout; herdr's session hook gave none") ||
		!strings.Contains(string(ev.Data), `"source":"rollout"`) || strings.Contains(ev.Message, home) {
		t.Fatalf("event = %s %q %s", ev.Level, ev.Message, ev.Data)
	}

	const fromHerdr = "01a16446-c800-7000-8000-000000000006"
	e.h.setAgentSession(store.Deref(s.AgentName), fromHerdr)
	e.observe()
	if got := store.Deref(e.session(RoleJudge).SessionID); got != fromHerdr {
		t.Fatalf("session id = %q, want herdr's %s", got, fromHerdr)
	}
	if recs := e.logs.records("herdr's wins"); len(recs) != 1 {
		t.Fatalf("logs = %q, want herdr's id winning over the rollout's said once", e.logs.all())
	}
}

// Two Codex TUI sessions started in one checkout within the window cannot
// be told apart: neither id is recorded, and that is logged once.
func TestTwoCodexSessionsStartedInOneCheckoutWithinTheWindowRecordNoID(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.m.d.CodexHome = home
	checkout := t.TempDir()
	s := e.codexWithoutID(checkout)
	metaRollout(t, home, "01a16446-c221-7073-b0fd-8609589b9a77", checkout, "cli", "", s.StartedAt.Add(2*time.Second))
	metaRollout(t, home, "01a16446-d000-7000-8000-000000000001", checkout, "cli", "", s.StartedAt.Add(90*time.Second))
	for range 3 {
		e.clock.Add(time.Minute)
		e.observe()
	}
	if got := store.Deref(e.session(RoleJudge).SessionID); got != "" {
		t.Fatalf("session id = %q, want none from two candidates", got)
	}
	if evs := e.recoveredIDEvents(); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
	if recs := e.logs.records("2 rollouts of its Codex home match its checkout and start"); len(recs) != 1 {
		t.Fatalf("logs = %q, want the ambiguity said once", e.logs.all())
	}
}

// The recovery reads the first line of at most limit rollouts, newest first
// by name, and only of those whose names say they started within the
// window: a directory named like an older or a newer rollout, which would
// fail the read, is never opened, nor is the oldest candidate past the
// limit.
func TestTheRolloutRecoveryReadsABoundedNumberOfFirstLines(t *testing.T) {
	home := t.TempDir()
	start := time.Now().Truncate(time.Second)
	var ids []string
	for i := range 6 {
		id := "01a16446-e000-7000-8000-00000000000" + string(rune('0'+i))
		ids = append(ids, id)
		metaRollout(t, home, id, "/w", "cli", "", start.Add(time.Duration(i*10-30)*time.Second))
	}
	unreadable := func(at time.Time, id string) {
		path := metaRollout(t, home, id, "/w", "cli", "", at)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unreadable(start.Add(-RecoverIDWindow-time.Minute), "01a16446-f000-7000-8000-000000000001")
	unreadable(start.Add(RecoverIDWindow+time.Minute), "01a16446-f000-7000-8000-000000000002")
	unreadable(start.Add(-RecoverIDWindow+5*time.Second), "01a16446-f000-7000-8000-000000000003") // the oldest candidate

	heads, err := rolloutHeads(home, start, RecoverIDWindow, 6)
	if err != nil {
		t.Fatalf("rolloutHeads read a file it should not have: %v", err)
	}
	if len(heads) != 6 {
		t.Fatalf("heads = %+v, want the 6 newest", heads)
	}
	for i, h := range heads {
		if want := ids[len(ids)-1-i]; h.id != want || h.cwd != "/w" || h.source != "cli" {
			t.Errorf("head %d = %+v, want %s first", i, h, want)
		}
	}
	if _, err := rolloutHeads(home, start, RecoverIDWindow, 7); err == nil {
		t.Fatal("a limit that takes in the oldest candidate read no directory: the bound is not what decides")
	}
}
