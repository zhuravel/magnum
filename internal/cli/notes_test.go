package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// notesWrite writes the notes file of owner/name under the fixture's home.
func notesWrite(t *testing.T, l paths.Layout, owner, name, text string) string {
	t.Helper()
	path := engine.NotesPath(l, owner, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// notesEditorEnv points $VISUAL and $EDITOR at values (the test never
// inherits the user's editor).
func notesEditorEnv(t *testing.T, visual, editor string) {
	t.Helper()
	t.Setenv("VISUAL", visual)
	t.Setenv("EDITOR", editor)
}

// notesFakeEditor writes a script that records its arguments, one per line,
// in <script>.args and appends text to the file named by its last argument.
func notesFakeEditor(t *testing.T, name, text string, exit int) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), name)
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$0.args\"\n" +
		"for last; do :; done\n" +
		"printf '%s\\n' '" + text + "' >> \"$last\"\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestNotesPrintsTheFile(t *testing.T) {
	f := newInspFixture(t)
	text := "# Notes for talkable/talkable (updated 2026-10-04)\n\n- run specs with bin/rspec\n\tindented\n"
	notesWrite(t, f.Ctx.Layout, "talkable", "talkable", text)

	for _, arg := range []string{"talkable/talkable", "Talkable/TALKABLE", "talkable"} { // the last: daemon.default_repo's owner
		if code := f.run("notes", arg); code != 0 || f.Out.String() != text || f.Err.Len() != 0 {
			t.Errorf("notes %s: exit %d\nstdout %q\nstderr %q", arg, code, f.Out, f.Err)
		}
	}
}

func TestNotesPrintsNoTerminalControls(t *testing.T) {
	f := newInspFixture(t)
	notesWrite(t, f.Ctx.Layout, "talkable", "talkable", "ok\x1b]0;pwned\a \x1b[31mred\x1b[0m\r\nnext\n")
	if code := f.run("notes", "talkable/talkable"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err)
	}
	statusNoControls(t, "notes", f.Out.String())
	if !strings.Contains(f.Out.String(), "ok") || !strings.Contains(f.Out.String(), "\nnext\n") {
		t.Errorf("the text is gone: %q", f.Out)
	}
}

func TestNotesMissingFileIsNotAnError(t *testing.T) {
	c, out, errb := bareContext(t) // no config.toml: owner/name needs none
	want := filepath.Join(c.Layout.Home, "state", "notes", "example", "tools.md")
	if code := execute(c, []string{"notes", "example/tools"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if out.Len() != 0 || errb.String() != "no notes for example/tools yet ("+want+")\n" {
		t.Errorf("stdout %q stderr %q", out, errb)
	}
	if _, err := os.Stat(filepath.Dir(want)); !os.IsNotExist(err) {
		t.Errorf("reading created %s: %v", filepath.Dir(want), err)
	}
}

func TestNotesUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"notes"}, "want exactly one repository"},
		{[]string{"notes", "a/b", "c/d"}, "want exactly one repository"},
		{[]string{"notes", "a/b/c"}, "want owner/name or name"},
		{[]string{"notes", "/b"}, "want owner/name or name"},
		{[]string{"notes", "a/"}, "want owner/name or name"},
		{[]string{"notes", "example/.."}, "is not a repository name"},
		{[]string{"notes", "--bogus", "a/b"}, "unknown flag"},
	} {
		c, _, errb := bareContext(t)
		if code := execute(c, tc.args); code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %s)", tc.args, code, errb)
		}
		if !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%v: stderr lacks %q:\n%s", tc.args, tc.want, errb)
		}
	}
	// A bare name needs daemon.default_repo for its owner.
	f := newInspFixture(t)
	cfg := strings.Replace(inspTestConfig, `default_repo = "talkable/talkable"`, "", 1)
	if err := os.WriteFile(filepath.Join(f.Home, "config.toml"), []byte(strings.ReplaceAll(cfg, "HOME", f.Home)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("notes", "talkable"); code != 2 || !strings.Contains(f.Err.String(), "no daemon.default_repo") {
		t.Errorf("bare name without default_repo: exit %d\n%s", code, f.Err)
	}
}

func TestNotesEditRunsTheEditor(t *testing.T) {
	f := newInspFixture(t)
	path := notesWrite(t, f.Ctx.Layout, "talkable", "talkable", "old\n")
	visual := notesFakeEditor(t, "visual", "from-visual", 0)
	editor := notesFakeEditor(t, "editor", "from-editor", 0)

	notesEditorEnv(t, "", editor) // $EDITOR when $VISUAL is empty
	if code := f.run("notes", "talkable/talkable", "--edit"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old\nfrom-editor\n" {
		t.Errorf("notes after $EDITOR = %q", got)
	}
	if got, _ := os.ReadFile(editor + ".args"); string(got) != path+"\n" {
		t.Errorf("editor arguments = %q, want the notes path alone", got)
	}
	if f.Out.Len() != 0 {
		t.Errorf("--edit printed %q", f.Out)
	}

	notesEditorEnv(t, visual, editor) // $VISUAL wins
	if code := f.run("notes", "--edit", "talkable"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old\nfrom-editor\nfrom-visual\n" {
		t.Errorf("notes after $VISUAL = %q", got)
	}
}

func TestNotesEditCreatesTheHarnessDirectoryAndPassesEditorArguments(t *testing.T) {
	// A home with a space: the file name must reach the editor as one word.
	home := filepath.Join(t.TempDir(), "my home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	c := &Context{Layout: paths.Layout{Home: home}, Stdout: out, Stderr: errb}
	editor := notesFakeEditor(t, "ed itor", "new", 0)
	notesEditorEnv(t, "", "'"+editor+"' --wait")

	if code := execute(c, []string{"notes", "example/Tools", "--edit"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	path := filepath.Join(home, "state", "notes", "example", "tools.md")
	if fi, err := os.Stat(strings.TrimSuffix(path, ".md")); err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("harness directory: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Errorf("notes = %q", got)
	}
	if got, _ := os.ReadFile(editor + ".args"); string(got) != "--wait\n"+path+"\n" {
		t.Errorf("editor arguments = %q", got)
	}
}

func TestNotesEditFailureAndDefaultEditor(t *testing.T) {
	f := newInspFixture(t)
	notesEditorEnv(t, "", notesFakeEditor(t, "broken", "x", 3))
	if code := f.run("notes", "talkable/talkable", "--edit"); code != 1 || !strings.Contains(f.Err.String(), "editing the notes of talkable/talkable") {
		t.Errorf("failing editor: exit %d\n%s", code, f.Err)
	}

	// Neither variable: vi (replaced here, nothing interactive runs).
	notesEditorEnv(t, "  ", "")
	var gotEditor, gotPath string
	prev := notesRunEditor
	notesRunEditor = func(_ *Context, editor, path string) error { gotEditor, gotPath = editor, path; return nil }
	t.Cleanup(func() { notesRunEditor = prev })
	if code := f.run("notes", "talkable/talkable", "--edit"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err)
	}
	if gotEditor != "vi" || gotPath != engine.NotesPath(f.Ctx.Layout, "talkable", "talkable") {
		t.Errorf("editor %q on %q", gotEditor, gotPath)
	}
}

func TestNotesCompletion(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 1, store.PRQueued, nil)
	inspSeedPR(t, st, "zhuravel/app", 2, store.PRQueued, nil)
	st.Close()
	notesWrite(t, f.Ctx.Layout, "talkable", "talkable", "x")                                         // known to the registry too: listed once
	notesWrite(t, f.Ctx.Layout, "example", "archived", "x")                                          // only a notes file
	if err := os.MkdirAll(engine.NotesDir(f.Ctx.Layout, "example", "archived"), 0o700); err != nil { // the harness directory is no repository
		t.Fatal(err)
	}

	var names []string
	for _, c := range complete(t, f.Ctx, "notes", "") {
		name, _, _ := strings.Cut(c, "\t")
		names = append(names, name)
	}
	if got := strings.Join(names, ","); got != "talkable/talkable,talkable,zhuravel/app,example/archived" {
		t.Errorf("notes completions = %q", got)
	}
	if got := complete(t, f.Ctx, "notes", "talkable/talkable", ""); len(got) != 0 {
		t.Errorf("second argument completed %q", got)
	}
}

func TestNotesCompletionWithoutRegistryOrNotes(t *testing.T) {
	c, _, _ := bareContext(t)
	if got := complete(t, c, "notes", ""); len(got) != 0 {
		t.Errorf("completions without anything: %q", got)
	}
}

func TestStatusCardShowsWhetherTheRepositoryHasNotes(t *testing.T) {
	f, _, d, _ := statusFixture(t)
	card := func() string {
		t.Helper()
		r, err := statusGather(context.Background(), d, statusOptions{Ref: "11920"})
		if err != nil || r.Detail == nil {
			t.Fatalf("gather: %v %+v", err, r.Detail)
		}
		var b bytes.Buffer
		statusRenderDetail(&b, *r.Detail, r.GeneratedAt)
		return b.String()
	}
	if out := card(); !strings.Contains(out, "\n  notes:     no\n") {
		t.Errorf("card without notes:\n%s", out)
	}
	// Another repository's notes do not count.
	notesWrite(t, f.Ctx.Layout, "zhuravel", "app", "x")
	if out := card(); !strings.Contains(out, "\n  notes:     no\n") {
		t.Errorf("card with another repository's notes:\n%s", out)
	}
	notesWrite(t, f.Ctx.Layout, "talkable", "talkable", "x")
	if out := card(); !strings.Contains(out, "\n  notes:     yes\n") {
		t.Errorf("card with notes:\n%s", out)
	}

	// status <ref> --json carries it too.
	if code := f.run("status", "11920", "--json"); code != 0 {
		t.Fatalf("status --json: exit %d: %s", code, f.Err)
	}
	var rep struct {
		Detail struct {
			Notes bool `json:"notes"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rep); err != nil || !rep.Detail.Notes {
		t.Errorf("status --json notes: %v %v\n%s", rep.Detail.Notes, err, f.Out)
	}
}
