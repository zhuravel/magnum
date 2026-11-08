package agents

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// staleTrustDirFor points the stale trust records at a temp dir for the test,
// and back at none after it.
func staleTrustDirFor(t *testing.T) string {
	t.Helper()
	storetest.Serial(t) // swaps the records' directory
	dir := filepath.Join(t.TempDir(), "codex-stale-trust")
	SetStaleCodexTrustDir(dir)
	t.Cleanup(func() { SetStaleCodexTrustDir("") })
	return dir
}

// recordCount is how many records dir holds.
func recordCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// magnumHooksJSON has magnum's Codex Stop entry at stop:1:0, after another
// tool's.
const magnumHooksJSON = `{"hooks":{"Stop":[
  {"hooks":[{"type":"command","command":"/opt/homebrew/bin/moshi-hook codex-hook"}]},
  {"hooks":[{"type":"command","command":"sh '/c/magnum-agent-hook.sh' codex stop","async":true,"timeout":5}]}]}}`

// After `magnum hooks install` rewrites one of magnum's Codex hooks in its
// place, Codex's [hooks.state] entry there still has a trusted_hash, the
// old hook's, which Codex no longer accepts: the place reads untrusted (so
// status, doctor and the daemon's hooks hold see the review Codex will
// show) while the entry keeps the hash the install recorded, and the
// record goes once the entry's hash differs (Trust all) or is gone.
func TestARewrittenCodexHooksOldTrustReadsUntrustedUntilItChanges(t *testing.T) {
	dir := staleTrustDirFor(t)
	trusted := "[hooks.state.\"{hooks}:stop:0:0\"]\ntrusted_hash = \"sha256:aa\"\n[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:old\"\n"
	home := writeHomeHooks(t, magnumHooksJSON, trusted)
	if tr, ok, err := ReadCodexHooksTrust(home); err != nil || !ok || len(tr.Untrusted) != 0 {
		t.Fatalf("before the rewrite = %+v %v %v, want all trusted", tr, ok, err)
	}
	if err := RecordStaleCodexTrust(home, []string{"stop:1:0", "stop:7:0"}); err != nil {
		t.Fatal(err)
	}
	if n := recordCount(t, dir); n != 1 {
		t.Fatalf("records = %d, want one: a place without a trust entry needs none", n)
	}
	tr, ok, err := ReadCodexHooksTrust(home)
	if err != nil || !ok || !slices.Equal(places(tr.Untrusted), []string{"stop:1:0"}) || !tr.Untrusted[0].Stale {
		t.Fatalf("after the rewrite = %+v %v %v, want stop:1:0 untrusted as stale", tr, ok, err)
	}
	if recordCount(t, dir) != 1 {
		t.Fatal("a read removed the record of an entry that still has the old hash")
	}

	// The operator chose Trust all: Codex wrote the new hook's hash.
	newTrust := writeHomeHooksIn(t, home, magnumHooksJSON, "[hooks.state.\"{hooks}:stop:0:0\"]\ntrusted_hash = \"sha256:aa\"\n[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:new\"\n")
	if tr, ok, err := ReadCodexHooksTrust(newTrust); err != nil || !ok || len(tr.Untrusted) != 0 {
		t.Fatalf("after a new trust = %+v %v %v, want all trusted", tr, ok, err)
	}
	if n := recordCount(t, dir); n != 0 {
		t.Fatalf("records after a new trust = %d, want none", n)
	}

	// A record whose entry is gone goes too.
	if err := writeConfig(home, trusted); err != nil {
		t.Fatal(err)
	}
	if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err != nil || recordCount(t, dir) != 1 {
		t.Fatalf("record again = %v, %d records", err, recordCount(t, dir))
	}
	if err := writeConfig(home, "[hooks.state.\"{hooks}:stop:0:0\"]\ntrusted_hash = \"sha256:aa\"\n"); err != nil {
		t.Fatal(err)
	}
	if tr, _, err := ReadCodexHooksTrust(home); err != nil || !slices.Equal(places(tr.Untrusted), []string{"stop:1:0"}) || tr.Untrusted[0].Stale {
		t.Fatalf("entry gone = %+v %v, want stop:1:0 untrusted with no trust entry", tr, err)
	}
	if n := recordCount(t, dir); n != 0 {
		t.Fatalf("records after the entry went = %d, want none", n)
	}
}

// Without a records directory (a test binary's default, a process that set
// none) nothing is recorded and nothing read: recording says why, and the
// reader reads Codex's files alone.
func TestStaleCodexTrustNeedsItsDirectory(t *testing.T) {
	storetest.Serial(t)
	SetStaleCodexTrustDir("")
	home := writeHomeHooks(t, magnumHooksJSON, "[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:old\"\n")
	if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err == nil {
		t.Fatal("recorded with no directory")
	}
	if tr, ok, err := ReadCodexHooksTrust(home); err != nil || !ok || !slices.Equal(places(tr.Untrusted), []string{"stop:0:0"}) {
		t.Fatalf("read = %+v %v %v", tr, ok, err)
	}
}

// writeHomeHooksIn rewrites home's hooks.json and config.toml as
// writeHomeHooks writes them, and returns home.
func writeHomeHooksIn(t *testing.T, home, hooks, state string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(home, state); err != nil {
		t.Fatal(err)
	}
	return home
}

// writeConfig writes home's config.toml, {hooks} standing for its hooks.json.
func writeConfig(home, state string) error {
	file := filepath.Join(home, "hooks.json")
	return os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.ReplaceAll(state, "{hooks}", file)), 0o600)
}

// ForgetStaleCodexTrust removes every record of a home's hooks.json, and
// only that home's: `magnum hooks install codex --forget-stale` runs it
// when Codex asks nothing for a place magnum recorded.
func TestForgetStaleCodexTrustRemovesOnlyThatHomesRecords(t *testing.T) {
	dir := staleTrustDirFor(t)
	state := "[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:old\"\n"
	one, other := writeHomeHooks(t, magnumHooksJSON, state), writeHomeHooks(t, magnumHooksJSON, state)
	for _, home := range []string{one, other} {
		if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ForgetStaleCodexTrust(one)
	if err != nil || n != 1 || recordCount(t, dir) != 1 {
		t.Fatalf("forget = %d %v, %d records left, want 1 forgotten and 1 left", n, err, recordCount(t, dir))
	}
	if tr, _, _ := ReadCodexHooksTrust(one); len(tr.Untrusted) != 1 || tr.Untrusted[0].Stale {
		t.Fatalf("the forgotten home = %+v, want stop:0:0 alone untrusted (no entry)", tr.Untrusted)
	}
	if tr, _, _ := ReadCodexHooksTrust(other); !slices.ContainsFunc(tr.Untrusted, func(h CodexHookAt) bool { return h.Stale }) {
		t.Fatalf("the other home lost its record: %+v", tr.Untrusted)
	}
}

// A reader never removes a record newer than the one it read: a second
// rewrite that records the place again while the reader decides to remove
// the first record keeps its own.
func TestAReaderNeverRemovesARecordNewerThanTheOneItRead(t *testing.T) {
	dir := staleTrustDirFor(t)
	home := writeHomeHooks(t, magnumHooksJSON, "[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:h1\"\n")
	if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(home, "[hooks.state.\"{hooks}:stop:1:0\"]\ntrusted_hash = \"sha256:h2\"\n"); err != nil { // Trust all
		t.Fatal(err)
	}
	staleTrustBeforeRemove = func() { // install rewrites the place again, meanwhile
		if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { staleTrustBeforeRemove = nil })
	if _, _, err := ReadCodexHooksTrust(home); err != nil {
		t.Fatal(err)
	}
	staleTrustBeforeRemove = nil
	tr, _, err := ReadCodexHooksTrust(home)
	if err != nil || !slices.ContainsFunc(tr.Untrusted, func(h CodexHookAt) bool { return h.Place == "stop:1:0" && h.Stale }) {
		t.Fatalf("after the race = %+v %v, want the newer record kept (%d records)", tr.Untrusted, err, recordCount(t, dir))
	}
}

// The sentence the hold's event and pause carry says what a stale place
// has, and the way out when Codex asks nothing.
func TestTheUntrustedSentenceTellsAStaleTrustEntryApart(t *testing.T) {
	cases := []struct {
		trust CodexHooksTrust
		want  string
	}{
		{CodexHooksTrust{Untrusted: []CodexHookAt{{Place: "stop:0:0"}}}, "1 hook in F has no trust entry"},
		{CodexHooksTrust{Untrusted: []CodexHookAt{{Place: "stop:0:0", Stale: true}}},
			"1 hook in F has a trust entry Codex no longer accepts (if `codex` asks nothing, run `magnum hooks install codex --forget-stale`)"},
		{CodexHooksTrust{Untrusted: []CodexHookAt{{Place: "a:0:0"}, {Place: "stop:0:0", Stale: true}}},
			"2 hooks in F have no trust entry or one Codex no longer accepts (if `codex` asks nothing, run `magnum hooks install codex --forget-stale`)"},
	}
	for _, c := range cases {
		if got := c.trust.UntrustedSentence("F"); got != c.want {
			t.Errorf("sentence = %q, want %q", got, c.want)
		}
	}
}

// stampRecords sets the modification time of every record in dir to at:
// when the install wrote it.
func stampRecords(t *testing.T, dir string, at time.Time) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if err := os.Chtimes(filepath.Join(dir, de.Name()), at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// staleHomeOf writes the test env's Codex home with magnum's rewritten Stop
// at stop:1:0, trusted with the old hash, and its record, and returns the
// records' directory.
func staleHomeOf(t *testing.T, e *env) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "codex-stale-trust")
	SetStaleCodexTrustDir(dir)
	t.Cleanup(func() { SetStaleCodexTrustDir("") })
	home := filepath.Dir(e.codexConfig)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(magnumHooksJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(e.codexConfig) // the folder trust magnum wrote, if any
	cfg = append(cfg, "\n[hooks.state.\""+filepath.Join(home, "hooks.json")+":stop:1:0\"]\ntrusted_hash = \"sha256:old\"\n"...)
	if err := os.WriteFile(e.codexConfig, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordStaleCodexTrust(home, []string{"stop:1:0"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A Codex session proves that Codex trusts every hook of its home as it is
// only when it read hooks.json after the install recorded the old trust,
// and magnum answered no hooks review in it: its composer then settles with
// no review, and the records written before its launch go. A record newer
// than the session's launch stays (that Codex read the file before the
// rewrite), and so does every record after a review the composer's wait
// answered.
func TestAQuietCodexStartForgetsOnlyTheStaleTrustRecordedBeforeItsLaunch(t *testing.T) {
	storetest.Serial(t) // swaps the records' directory
	for _, c := range []struct {
		name   string
		before bool // the record was written before the session's launch
		review bool // the hooks review showed before the first prompt
		want   int
	}{
		{"a true quiet start", true, false, 0},
		{"a session older than the record", false, false, 1},
		{"a review the first prompt's wait answered", true, true, 1},
	} {
		e, name := codexFirstPrompt(t)
		dir := staleHomeOf(t, e)
		launched := e.session(RoleJudge).StartedAt
		at := launched.Add(time.Minute)
		if c.before {
			at = launched.Add(-time.Minute)
		}
		stampRecords(t, dir, at)
		if c.review {
			e.h.mu.Lock()
			e.h.reads[name] = codexHooksScreen
			e.h.mu.Unlock()
		}
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("%s: Prompt: %v", c.name, err)
		}
		if n := recordCount(t, dir); n != c.want {
			t.Errorf("%s: records = %d, want %d", c.name, n, c.want)
		}
	}
}

// A hooks review magnum answered when it launched the agent (herdr showed
// it blocked at the review, and StartAgent declined it, as for a checkout
// with Codex hooks of its own, or trusted it) leaves the first prompt's
// screen quiet, but proves nothing for the hooks it declined: the records
// stay, whichever the answer.
func TestAReviewAnsweredAtTheLaunchKeepsTheStaleTrust(t *testing.T) {
	storetest.Serial(t) // swaps the records' directory
	for _, declare := range []bool{true, false} {
		e := newEnv(t)
		e.clockedSleep()
		dir := staleHomeOf(t, e)
		stampRecords(t, dir, t0.Add(-time.Hour)) // before the launch
		checkout := t.TempDir()
		files := map[string]string{}
		if declare {
			files[".codex/hooks.json"] = "{}"
		}
		writeFiles(t, checkout, files)
		if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, checkout, map[string]string{"WT_BRANCH": "review1"}, "talkable#11920", e.coreRoles())
		if err != nil {
			t.Fatalf("EnsureWorkspace: %v", err)
		}
		name := "mg-11920-codex-judge-5d01cf"
		e.h.mu.Lock()
		e.h.startStatus = herdr.StatusBlocked
		e.h.reads[name] = codexHooksScreen
		e.h.onKeys = func(f *fakeHerdr, target string, keys []string) {
			for _, k := range keys {
				d, ok := detectHooksDialog(f.reads[target])
				switch {
				case !ok:
				case k == "down":
					f.reads[target] = hooksCursorAt(d.cursor + 1)
				case k == "enter":
					f.reads[target] = codexIdleScreen
					for i := range f.agents {
						f.agents[i].AgentStatus = herdr.StatusIdle
					}
				}
			}
		}
		e.h.mu.Unlock()
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatalf("StartAgent: %v", err)
		}
		want := EventHooksTrusted
		if declare {
			want = EventHooksDeclined
		}
		if evs := hooksEvents(t, e); len(evs) != 1 || evs[0].Kind != want {
			t.Fatalf("declare %v: hooks events at the launch = %+v, want %s", declare, evs, want)
		}
		if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if n := recordCount(t, dir); n != 1 {
			t.Errorf("declare %v: records after the quiet first prompt = %d, want 1", declare, n)
		}
	}
}

// A session row with no CODEX_HOME in its env (StartAgent's fallback row)
// names its home by the roles: with one Codex home configured that one, and
// with more than one none, so the clear never reaches another home's
// records by the default. A row with no launch time clears nothing.
func TestAQuietStartOfARowWithoutItsHomeClearsOnlyTheOneConfiguredHome(t *testing.T) {
	storetest.Serial(t) // swaps the records' directory
	row := store.Session{ID: 99, Role: string(RoleJudge), StartedAt: t0}

	e := newEnv(t)
	dir := staleHomeOf(t, e)
	stampRecords(t, dir, t0.Add(-time.Hour))
	noLaunch := row
	noLaunch.StartedAt = time.Time{}
	if e.m.forgetStaleTrustOf(noLaunch); recordCount(t, dir) != 1 {
		t.Fatal("a row with no launch time cleared the records")
	}
	if e.m.forgetStaleTrustOf(row); recordCount(t, dir) != 0 {
		t.Fatalf("one Codex home: records = %d, want 0", recordCount(t, dir))
	}

	e = newEnv(t)
	dir = staleHomeOf(t, e)
	stampRecords(t, dir, t0.Add(-time.Hour))
	e.cfg.Roles = append(e.cfg.AllRoles(), config.Role{Name: "codex-alt", Kind: KindCodex, Env: map[string]string{"CODEX_HOME": t.TempDir()}})
	e.cfg.Normalize()
	if e.m.forgetStaleTrustOf(row); recordCount(t, dir) != 1 {
		t.Fatalf("two Codex homes and a row with no home: records = %d, want 1", recordCount(t, dir))
	}
	row.Env = map[string]string{"CODEX_HOME": filepath.Dir(e.codexConfig)}
	if e.m.forgetStaleTrustOf(row); recordCount(t, dir) != 0 {
		t.Fatalf("a row that names its home: records = %d, want 0", recordCount(t, dir))
	}
}
