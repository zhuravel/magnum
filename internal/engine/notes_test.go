package engine

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

func TestNotesPathAndDir(t *testing.T) {
	l := paths.Layout{Home: "/h"}
	if got, want := NotesPath(l, "Talkable", "Magnum"), "/h/state/notes/talkable/magnum.md"; got != want {
		t.Errorf("NotesPath = %q, want %q", got, want)
	}
	if got, want := NotesDir(l, "Talkable", "Magnum"), "/h/state/notes/talkable/magnum"; got != want {
		t.Errorf("NotesDir = %q, want %q", got, want)
	}
	if got := NotesPath(l, "talkable", "magnum"); got != NotesDir(l, "talkable", "magnum")+".md" {
		t.Errorf("NotesPath %q is not NotesDir plus .md", got)
	}
	if got, want := NotesRoot(l), "/h/state/notes"; got != want {
		t.Errorf("NotesRoot = %q, want %q", got, want)
	}
}

func TestNotesPathIsEmptyWithoutHomeOrWithUnsafeNames(t *testing.T) {
	if p, d, r := NotesPath(paths.Layout{}, "talkable", "magnum"), NotesDir(paths.Layout{}, "talkable", "magnum"), NotesRoot(paths.Layout{}); p != "" || d != "" || r != "" {
		t.Errorf("no home: path %q dir %q root %q, want empty", p, d, r)
	}
	l := paths.Layout{Home: "/h"}
	for _, c := range [][2]string{
		{"", "magnum"}, {"talkable", ""}, {"..", "magnum"}, {"talkable", ".."}, {".", "x"},
		{"a/b", "magnum"}, {"talkable", "../../etc"}, {"talkable", `a\b`}, {"talkable", "a\x00b"},
	} {
		if p, d := NotesPath(l, c[0], c[1]), NotesDir(l, c[0], c[1]); p != "" || d != "" {
			t.Errorf("NotesPath(%q, %q) = %q, NotesDir = %q, want empty", c[0], c[1], p, d)
		}
	}
	// Dots inside a name are fine: GitHub names have them.
	if got := NotesPath(l, "talkable", "my.repo"); got != "/h/state/notes/talkable/my.repo.md" {
		t.Errorf("NotesPath with a dot = %q", got)
	}
}

func TestRoundNotesCreatesTheHarnessDirectory(t *testing.T) {
	h := newHarness(t)
	repo := store.Repo{Owner: "Talkable", Name: "Talkable"}
	path := h.e.roundNotes(repo)
	if want := filepath.Join(h.layout.State(), "notes", "talkable", "talkable.md"); path != want {
		t.Fatalf("roundNotes = %q, want %q", path, want)
	}
	dir := strings.TrimSuffix(path, ".md")
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("harness directory %s: %v", dir, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("harness directory mode %v, want private", fi.Mode().Perm())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the notes file must be left to the judge: %v", err)
	}
	if again := h.e.roundNotes(repo); again != path { // idempotent
		t.Errorf("second roundNotes = %q", again)
	}
}

func TestRoundNotesInDryRunCreatesNothing(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.d.DryRun = true })
	path := h.e.roundNotes(store.Repo{Owner: "talkable", Name: "talkable"})
	if want := NotesPath(h.layout, "talkable", "talkable"); path != want {
		t.Fatalf("roundNotes = %q, want %q", path, want)
	}
	if _, err := os.Stat(NotesRoot(h.layout)); !os.IsNotExist(err) {
		t.Fatalf("a dry run created the notes directory: %v", err)
	}
}

func TestRoundNotesLogsAFailureAndStillReturnsThePath(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, func(h *harness) { h.d.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	// state/notes is a file, so no directory can be created below it.
	if err := os.WriteFile(NotesRoot(h.layout), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := h.e.roundNotes(store.Repo{Owner: "talkable", Name: "talkable"})
	if want := NotesPath(h.layout, "talkable", "talkable"); path != want {
		t.Fatalf("roundNotes = %q, want %q", path, want)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "create repository notes directory") {
		t.Fatalf("no warning logged: %q", out)
	}
}

func TestRoundInputCarriesTheNotesPath(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	ins := h.rd.all()
	if len(ins) == 0 {
		t.Fatal("no round ran")
	}
	want := NotesPath(h.layout, "talkable", "talkable")
	for i, in := range ins {
		if in.NotesPath != want {
			t.Errorf("round %d: NotesPath = %q, want %q", i+1, in.NotesPath, want)
		}
	}
	if fi, err := os.Stat(strings.TrimSuffix(want, ".md")); err != nil || !fi.IsDir() {
		t.Errorf("the round did not create the harness directory: %v", err)
	}
}

// The judge derives the harness directory and the lock from the notes file
// it is given (agents.NotesFiles); both must be the engine's.
func TestNotesLockPathAgreesWithTheJudgesDerivation(t *testing.T) {
	l := paths.Layout{Home: "/h"}
	if got, want := NotesLockPath(l, "Talkable", "Magnum"), "/h/state/notes/talkable/magnum.lock"; got != want {
		t.Errorf("NotesLockPath = %q, want %q", got, want)
	}
	if got := NotesLockPath(l, "talkable", ".."); got != "" {
		t.Errorf("NotesLockPath with an unsafe name = %q, want empty", got)
	}
	dir, lock := agents.NotesFiles(NotesPath(l, "talkable", "my.repo"))
	if dir != NotesDir(l, "talkable", "my.repo") || lock != NotesLockPath(l, "talkable", "my.repo") {
		t.Errorf("agents.NotesFiles = %q, %q; engine has %q, %q", dir, lock, NotesDir(l, "talkable", "my.repo"), NotesLockPath(l, "talkable", "my.repo"))
	}
}
