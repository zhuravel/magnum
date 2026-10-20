package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// Two [[role]] blocks of one name are merged by the loader (the last one's
// keys win), so which one a save should edit is ambiguous: the screen says
// so and refuses the edit.
func TestARoleSeveralBlocksNameIsLeftToAHandEdit(t *testing.T) {
	s := newSnapshot(t, fixture(t)+"\n[[role]]\nname = \"claude-review\"\nmodel = \"fable\"\n")
	tg := g("role.claude-review.model")
	if v := s.Value(tg); !strings.Contains(v.Source, "2 [[role]] blocks") || v.Effective != "fable" {
		t.Errorf("Value = %+v", v)
	}
	if err := s.Check(tg, text("sonnet")); err == nil {
		t.Error("Check passed")
	}
	if _, err := s.Plan(Draft{tg: text("sonnet")}, false, testNow); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("Plan = %v", err)
	}
}

// What the user file does not set comes from the resolved layers: a base
// config's kind spend, [[role]] model and level table show as such, not as
// the user layer's inheritance.
func TestInheritedValuesComeFromTheLayersUnderTheUserFile(t *testing.T) {
	storetest.Parallel(t)
	home := t.TempDir()
	base := strings.Replace(string(magnum.DefaultConfig), "name = \"claude-simplify\"\n", "name = \"claude-simplify\"\nmodel = \"base-model\"\n", 1)
	base += "\n[kinds.codex]\nspend = \"high\"\n\n[spend.low]\nclaude-review = { model = \"base-sonnet\" }\n"
	basePath := filepath.Join(home, "base.toml")
	if err := os.WriteFile(basePath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(home, "user.toml")
	if err := os.WriteFile(user, []byte(strings.Replace(fixture(t), "spend = \"high\" ", "spend = \"low\"  ", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(paths.Layout{Home: home, UserConfig: user}, basePath)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[FieldID][2]string{
		"kinds.codex.spend":             {"high", "the base config (base.toml) [kinds.codex] spend"},
		"kinds.claude.spend":            {"inherit (low)", "[pipeline] spend"},
		"role.claude-simplify.model":    {"base-model", ""},
		"spend.low.claude-review.model": {"base-sonnet", "the base config (base.toml) [spend.low] table"},
	} {
		v := s.Value(g(id))
		if v.State != StateDefault || v.Effective != want[0] || (want[1] != "" && v.Source != want[1]) {
			t.Errorf("%s = %+v, want %q from %q", id, v, want[0], want[1])
		}
	}
}

func TestPlanRefusesAFileWithSeveralHardLinks(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	if err := os.Link(s.Path, s.Path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, false, testNow); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("Plan = %v, want the hard links named", err)
	}
}

// Plan runs the prompt check `magnum config` runs: a prompt that does not
// render with this build refuses the save.
func TestPlanRefusesAConfigWhosePromptsDoNotRender(t *testing.T) {
	storetest.Parallel(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "judge-initial.md"), []byte("{{.NoSuchField}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSnapshot(t, strings.Replace(fixture(t), "[pipeline]\n", "[pipeline]\nprompts_dir = \""+dir+"\"\n", 1))
	_, err := s.Plan(Draft{g("pipeline.fix_cycles"): text("4")}, false, testNow)
	if err == nil || !strings.Contains(err.Error(), "prompts do not render") {
		t.Errorf("Plan = %v, want the prompt check's refusal", err)
	}
}

func TestCommitRefusesASymlinkSwappedInAfterThePlan(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	target := s.Path + ".real"
	if err := os.Rename(s.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.Path); err != nil { // the same content, through a link
		t.Fatal(err)
	}
	if _, err := Commit(p); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Commit = %v, want the symlink refused", err)
	}
	if fi, _ := os.Lstat(s.Path); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
}

// Commit checks the file's hash when it starts and again right before the
// write; each check alone refuses a file that changed.
func TestEachOfCommitsHashChecksRefusesAChangedFile(t *testing.T) {
	storetest.Serial(t)
	s := newSnapshot(t, fixture(t))
	edited := fixture(t) + "# edited by hand\n"
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// The first: changed before Commit.
	if err := os.WriteFile(s.Path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(p); !errors.Is(err, ErrChanged) {
		t.Errorf("Commit after a change = %v, want ErrChanged", err)
	}
	// The second: changed while Commit kept the history.
	if err := os.WriteFile(s.Path, []byte(fixture(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	commitHook = func(stage string) {
		if stage == "rename" {
			if err := os.WriteFile(s.Path, []byte(edited), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { commitHook = nil })
	if _, err := Commit(p); !errors.Is(err, ErrChanged) {
		t.Errorf("Commit with a change before the write = %v, want ErrChanged", err)
	}
	if got, _ := os.ReadFile(s.Path); string(got) != edited {
		t.Error("the hand edit was overwritten")
	}
}

// Two saves never interleave: the second waits for the first's lock, then
// finds the file changed.
func TestConcurrentCommitsAreSerializedByTheLock(t *testing.T) {
	storetest.Serial(t)
	s := newSnapshot(t, fixture(t))
	p1, err := s.Plan(Draft{g("pipeline.fix_cycles"): text("7")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Plan(Draft{g("pipeline.fix_cycles"): text("8")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	commitHook = func(stage string) {
		if stage == "rename" {
			once.Do(func() { close(inside); <-release })
		}
	}
	t.Cleanup(func() { commitHook = nil })
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := Commit(p1); first <- err }()
	<-inside
	go func() { _, err := Commit(p2); second <- err }()
	time.Sleep(200 * time.Millisecond) // the second save waits on the lock meanwhile
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first save: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrChanged) {
		t.Errorf("the second save = %v, want ErrChanged", err)
	}
	if got, _ := os.ReadFile(s.Path); string(got) != string(p1.New) {
		t.Error("the file is not the first save's")
	}
}

// Restore reads only a regular file directly in the history directory: a
// symlink there, or a path that leaves it, is refused.
func TestRestoreReadsOnlyTheHistorysOwnFiles(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	dir := s.historyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.toml")
	if err := os.WriteFile(outside, []byte("[pipeline]\nspend = \"low\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "20261020T120000Z-0123456789ab.toml")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, filepath.Join(dir, "..", "x", "20261020T120000Z-0123456789ab.toml"), outside} {
		if _, err := s.Restore(HistoryEntry{Path: p}, testNow); err == nil {
			t.Errorf("Restore(%s) passed", p)
		}
	}
	if hist, _ := s.History(); len(hist) != 0 {
		t.Errorf("History lists a symlink: %+v", hist)
	}
}
