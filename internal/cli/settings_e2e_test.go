package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// e2eConfig is a synthetic operator config: dated comments, aligned
// columns, a [[watch]] list, inline tables in [spend.*]; placeholder names
// only.
const e2eConfig = `# A synthetic operator config for the settings screen's end-to-end test.

[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"

# The main watch.
[[watch]]
owner = "talkable"
include = ["web", "api"]
identity = "zhuravel"
auto_approve_authors = ["alice"]   # 2026-10-17: alice only for now

[pipeline]
spend = "high"                     # 2026-10-14: medium -> high

[usage]
codex_soft = 90                    # 2026-10-16: 80 -> 90
codex_hard = 98

[spend.high]
claude-review = { model = "opus" }   # 2026-10-20: fable -> opus

[spend.burn]
claude-review = { model = "opus", effort = "xhigh" }
`

// e2eScreen is the settings screen over the real settings core: the user
// config of a temporary layout, its registry, and the screen's model driven
// by key messages as a terminal would.
type e2eScreen struct {
	t      *testing.T
	m      tea.Model
	user   string
	layout paths.Layout
	st     *store.Store
	base   string // the --config file under the user's ("" = the built-in defaults)
	quit   bool
}

func newE2EScreen(t *testing.T, config ...string) *e2eScreen {
	t.Helper()
	src := e2eConfig
	if len(config) > 0 {
		src = config[0]
	}
	storetest.Parallel(t)
	home := t.TempDir()
	dir := filepath.Join(home, "xdg", "magnum")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(user, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home, UserConfig: user}
	st := storetest.Open(t, layout.DB())
	s := &e2eScreen{t: t, user: user, layout: layout, st: st}
	s.open(tui.SettingsOptions{})
	return s
}

// e2eSource is the real settings core's source over s's layout and
// registry, with no daemon running.
func (s *e2eScreen) e2eSource() *settingsSource {
	st := s.st
	return &settingsSource{layout: s.layout, base: s.base, registry: func() (*store.Store, error) { return st, nil },
		daemon: func(context.Context) (bool, bool) { return false, false },
		now:    func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local) }}
}

// open opens the screen (again) over the file as it is on disk.
func (s *e2eScreen) open(opts tui.SettingsOptions) {
	s.t.Helper()
	s.m, s.quit = tui.NewSettings(s.t.Context(), s.e2eSource(), nil, opts), false
	s.feed(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.run(s.m.Init())
}

// e2eBlink is the code of a cursor's blink command (it waits half a
// second): dropped like a timer.
var e2eBlink = func() uintptr {
	c := cursor.New()
	return reflect.ValueOf(c.Blink()).Pointer()
}()

// run runs cmd and feeds what it answers back to the screen, until nothing
// is left; batches and sequences are expanded, timers and terminal queries
// dropped.
func (s *e2eScreen) run(cmd tea.Cmd) {
	s.t.Helper()
	if cmd == nil || reflect.ValueOf(cmd).Pointer() == e2eBlink {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(10 * time.Second):
		s.t.Fatal("a command of the screen still runs after 10s")
	}
	if reflect.TypeOf(msg) == reflect.TypeOf(tea.RequestBackgroundColor()) {
		return
	}
	switch m := msg.(type) {
	case nil, spinner.TickMsg, cursor.BlinkMsg:
		return
	case tea.QuitMsg:
		s.quit = true
		return
	case tea.BatchMsg:
		for _, c := range m {
			s.run(c)
		}
		return
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		for i := range v.Len() {
			s.run(v.Index(i).Interface().(tea.Cmd))
		}
		return
	}
	s.feed(msg)
}

// feed gives the screen one message and runs what it returns.
func (s *e2eScreen) feed(msg tea.Msg) {
	s.t.Helper()
	var cmd tea.Cmd
	s.m, cmd = s.m.Update(msg)
	s.run(cmd)
}

// keys presses keys by Bubble Tea's names, one after the other.
func (s *e2eScreen) keys(names ...string) {
	s.t.Helper()
	named := map[string]rune{"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "backspace": tea.KeyBackspace}
	for _, n := range names {
		switch {
		case n == "shift+tab":
			s.feed(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		case strings.HasPrefix(n, "ctrl+"):
			s.feed(tea.KeyPressMsg{Code: rune(n[len("ctrl+")]), Mod: tea.ModCtrl})
		case named[n] != 0:
			s.feed(tea.KeyPressMsg{Code: named[n]})
		default:
			r, _ := utf8.DecodeRuneInString(n)
			s.feed(tea.KeyPressMsg{Code: r, Text: n})
		}
	}
}

// typeIn types text one key at a time.
func (s *e2eScreen) typeIn(text string) {
	s.t.Helper()
	for _, r := range text {
		s.keys(string(r))
	}
}

// view is the screen's frame as text.
func (s *e2eScreen) view() string { return ansi.Strip(s.m.View().Content) }

func (s *e2eScreen) mustShow(want ...string) {
	s.t.Helper()
	v := s.view()
	for _, w := range want {
		if !strings.Contains(v, w) {
			s.t.Errorf("the screen lacks %q:\n%s", w, v)
		}
	}
}

func (s *e2eScreen) file() string {
	s.t.Helper()
	b, err := os.ReadFile(s.user)
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

// The settings screen over the real settings core and a synthetic config:
// a spend change and a list change staged with the screen's own keys, saved
// with y, change exactly their two lines of the file (the dated note first,
// the old comment kept), keep the file they replace in the history, and ask
// the daemon to restart on it. A value its field refuses, and one the
// loader refuses, show their reasons and write nothing.
func TestTheSettingsScreenSavesThroughTheSettingsCore(t *testing.T) {
	s := newE2EScreen(t)
	s.mustShow("magnum · settings · Global", "SPEND & MODELS", "spend level", "high")

	// The spend level: high (saved) → burn, the next choice.
	s.keys("enter")
	s.mustShow("high (saved)", "burn")
	s.keys("j", "enter")
	s.mustShow("1 unsaved", "● unsaved")

	// The watch's allowlist: rev-ann after alice.
	s.keys("/")
	s.typeIn("auto_approve_authors")
	s.keys("enter")
	s.mustShow("watch 1 · auto-approve authors", "alice")
	s.keys("enter", "a")
	s.typeIn("rev-ann")
	s.keys("enter", "esc")
	s.mustShow("2 unsaved", "rev-ann")

	// s shows the diff of the real editor; only its own s asks, only y writes.
	before := s.file()
	s.keys("s")
	s.mustShow("SAVE · 2 changes", "[pipeline]", `+ spend = "burn"`, `"rev-ann"`, "2026-10-20: was")
	s.keys("s")
	s.mustShow("Save 2 changes to ", " y/N")
	if s.file() != before {
		t.Fatal("the file changed before the y")
	}
	s.keys("y")
	s.mustShow("Restart the daemon when it is idle to apply? y/N")
	s.keys("y")
	s.mustShow("no daemon is running: it reads the saved file when it starts")

	after := s.file()
	was, now := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(was) != len(now) {
		t.Fatalf("the save changed the number of lines: %d → %d\n%s", len(was), len(now), after)
	}
	var changed []string
	for i := range was {
		if was[i] != now[i] {
			changed = append(changed, now[i])
		}
	}
	if len(changed) != 2 {
		t.Fatalf("want exactly the two edited lines changed, got %d:\n%s", len(changed), strings.Join(changed, "\n"))
	}
	spend, authors := changed[1], changed[0] // the watch comes first in the file
	if !strings.HasPrefix(spend, `spend = "burn"`) || !strings.Contains(spend, `# 2026-10-20: was "high"`) ||
		!strings.Contains(spend, "2026-10-14: medium -> high") {
		t.Errorf("the spend line: %q, want burn with the dated note first and the old comment kept", spend)
	}
	if !strings.HasPrefix(authors, `auto_approve_authors = ["alice", "rev-ann"]`) || !strings.Contains(authors, "# 2026-10-20: was") ||
		!strings.Contains(authors, "2026-10-17: alice only for now") {
		t.Errorf("the authors line: %q, want the list with the dated note first and the old comment kept", authors)
	}

	kept, err := os.ReadDir(settingsHistoryDir(s.layout))
	if err != nil || len(kept) != 1 {
		t.Fatalf("the history holds %v (%v), want one file", kept, err)
	}
	if b, err := os.ReadFile(filepath.Join(settingsHistoryDir(s.layout), kept[0].Name())); err != nil || string(b) != before {
		t.Errorf("the history file is not the replaced file (%v):\n%s", err, b)
	}
	if v, ok, err := s.st.GetKV(t.Context(), engine.KVConfigRestart); err != nil || !ok || v == "" {
		t.Errorf("the restart mark: %q %v %v", v, ok, err)
	}

	// A value the field refuses: the popup shows why and stays.
	s.keys("esc", "tab") // out of the search, Spend & models → Usage caps
	s.mustShow("USAGE CAPS", "Codex soft cap")
	s.keys("enter", "backspace", "backspace")
	s.typeIn("150")
	s.keys("enter")
	s.mustShow("must be a percentage from 0 (off) to 100")
	// One the loader refuses (soft over hard): marked, and the save says why.
	s.keys("backspace", "backspace", "backspace")
	s.typeIn("99")
	s.keys("enter")
	s.mustShow("✗ invalid", "must be below usage.codex_hard")
	s.keys("s")
	s.mustShow("not saved:", "must be below usage.codex_hard")
	if s.file() != after {
		t.Error("a refused save wrote the file")
	}
	if kept, _ := os.ReadDir(settingsHistoryDir(s.layout)); len(kept) != 1 {
		t.Errorf("a refused save kept a version: %d files", len(kept))
	}
	if s.quit {
		t.Error("the screen quit on its own")
	}
}

// --history lists the versions the saves kept, newest first, numbered as
// --restore takes them.
func TestSettingsHistoryListsTheKeptVersions(t *testing.T) {
	s := newE2EScreen(t)
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	c := &Context{Version: "test", Layout: s.layout, Stdout: out, Stderr: errb}
	if err := settingsHistory(c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no kept versions yet") {
		t.Errorf("before a save: %q", out.String())
	}
	s.keys("enter", "j", "enter", "s", "s", "y", "esc")
	out.Reset()
	if err := settingsHistory(c); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "1  ") || !strings.Contains(lines[0], "config-history") ||
		!strings.Contains(lines[1], "--restore N") {
		t.Errorf("history after one save:\n%s%s", out.String(), errb.String())
	}
	if slices.Contains(lines, "") {
		t.Errorf("blank lines in %q", out.String())
	}
}

// A restart the daemon refused on the file as it is now shows on the
// screen, so the operator knows the saved file is not running.
func TestTheSettingsScreenSaysTheDaemonRefusedToRestartOnTheFile(t *testing.T) {
	s := newE2EScreen(t)
	sum := sha256.Sum256([]byte(e2eConfig))
	b, err := json.Marshal(engine.ConfigRestartRefusal{SHA256: hex.EncodeToString(sum[:]), At: time.Date(2026, 10, 20, 9, 30, 0, 0, time.Local)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetKV(t.Context(), engine.KVConfigRestartRefused, string(b)); err != nil {
		t.Fatal(err)
	}
	s.keys("ctrl+r")
	s.mustShow("warning: the daemon refused to restart on this file at Oct 20 09:30")
}

// A role two [[role]] blocks of the file name is not edited by the screen:
// its detail says why, and a save of an edit to it is refused with the same
// words, the file untouched.
func TestTheSettingsScreenLeavesARoleOfTwoBlocksToTheFile(t *testing.T) {
	s := newE2EScreen(t, e2eConfig+`
[[role]]
name = "claude-fix"
model = "opus"

[[role]]
name = "claude-fix"
effort = "high"
`)
	before := s.file()
	s.keys("G", "k") // the roles follow the settings; claude-fix is next to last
	s.mustShow("2 [[role]] blocks of your file name claude-fix (the last one's keys win): edit them by hand")
	s.keys("enter", "j", "enter", "g", "j", "enter")
	s.keys("s")
	s.mustShow("[[role]] blocks of your config name claude-fix: edit them by hand")
	if s.file() != before {
		t.Error("a refused edit wrote the file")
	}
}

// A hand edit that breaks the file leaves the screen nothing to load, and
// the history mends it: --history still lists the kept versions, and
// --restore 1 opens the save that puts the newest back, written with y.
func TestTheSettingsScreenRestoresAKeptVersionOverABrokenFile(t *testing.T) {
	s := newE2EScreen(t)
	s.keys("enter", "j", "enter", "s", "s", "y", "esc") // one save: the history keeps the original file
	broken := "[pipeline\nspend = \"burn\"\n"
	if err := os.WriteFile(s.user, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	if err := settingsHistory(&Context{Version: "test", Layout: s.layout, Stdout: out, Stderr: &bytes.Buffer{}}); err != nil {
		t.Fatalf("--history over a broken file: %v", err)
	}
	if !strings.Contains(out.String(), "  1  ") {
		t.Errorf("--history lists nothing: %q", out.String())
	}
	s.open(tui.SettingsOptions{Restore: 1})
	s.mustShow("cannot read the config", "SAVE · ", "puts back kept version 1")
	s.keys("s", "y")
	if got := s.file(); got != e2eConfig {
		t.Errorf("the restore wrote:\n%s\nwant the kept version:\n%s", got, e2eConfig)
	}
	if kept, _ := os.ReadDir(settingsHistoryDir(s.layout)); len(kept) != 2 {
		t.Errorf("the broken file was not kept: %d versions", len(kept))
	}
}

// A file edited on disk after the screen read it refuses the save; the
// screen offers to read it again keeping the staged edit, and the save then
// keeps the hand edit too.
func TestTheSettingsScreenReadsAChangedFileAgainKeepingTheDraft(t *testing.T) {
	s := newE2EScreen(t)
	s.keys("enter", "j", "enter") // spend burn, staged
	edited := s.file() + "# 2026-10-20: a hand edit while the screen was open\n"
	if err := os.WriteFile(s.user, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	s.keys("s")
	s.mustShow("The file changed on disk. Read it again and keep the 1 staged edit? y/N")
	s.keys("y")
	s.mustShow("read the file again: kept 1 staged edit")
	s.keys("s", "s", "y")
	got := s.file()
	if !strings.Contains(got, "a hand edit while the screen was open") || !strings.Contains(got, `spend = "burn"`) {
		t.Errorf("the save after the re-read lost the hand edit or the staged one:\n%s", got)
	}
}

// The restart answer says what happens: a daemon launchd runs restarts by
// itself, one it does not run needs a command, and no daemon reads the
// file when it starts.
func TestTheSettingsRestartAnswerSaysWhatWillHappen(t *testing.T) {
	s := newE2EScreen(t)
	src := s.e2eSource()
	if _, err := src.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, err := src.Plan(t.Context(), tui.SettingsDraft{"pipeline.spend": {Text: "burn"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := src.Commit(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		running, supervised bool
		want                string
	}{
		{false, false, "no daemon is running: it reads the saved file when it starts"},
		{true, false, "run `magnum daemon-restart --when-idle`"},
		{true, true, "the daemon restarts on the saved file at its first moment with nothing in flight"},
	} {
		src.daemon = func(context.Context) (bool, bool) { return c.running, c.supervised }
		text, err := src.RequestRestart(t.Context(), saved)
		if err != nil || !strings.Contains(text, c.want) {
			t.Errorf("running %v, launchd %v: %q %v, want %q", c.running, c.supervised, text, err, c.want)
		}
	}
}

// A kind's spend a base config sets ([kinds.codex] spend = "low") is its
// own: staging another [pipeline] spend leaves its row at low, while a
// kind no layer sets follows the staged level.
func TestTheSettingsScreenLeavesABaseLayersKindSpendItsOwn(t *testing.T) {
	defaults, err := os.ReadFile(filepath.Join("..", "..", "config.defaults.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := newE2EScreen(t)
	s.base = filepath.Join(t.TempDir(), "base.toml")
	if err := os.WriteFile(s.base, append(defaults, "\n[kinds.codex]\nspend = \"low\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	s.open(tui.SettingsOptions{})
	s.mustShow("codex spend        low", "claude spend       inherit (high)")
	s.keys("enter", "j", "enter") // [pipeline] spend: high → burn
	s.mustShow("codex spend        low", "claude spend       inherit (burn)")
	if v := s.view(); strings.Contains(v, "codex spend        inherit (burn)") {
		t.Errorf("the base config's codex spend reads as inherited:\n%s", v)
	}
}
