package cli

// `magnum notes <repo> [--edit]`: the repository notes the review roles read
// first and the judge rewrites after a round that taught it something durable
// (engine.NotesPath). Reading never needs a registry or the daemon; --edit
// opens the file in the user's editor on the terminal.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

const notesUsage = "<repo> [--edit]"

// notesMaxShown caps what `magnum notes` prints: the judge keeps the file to
// about 80 lines, so anything bigger is garbage not worth flooding the
// terminal with.
const notesMaxShown = 1 << 20

func newNotesCmd(c *Context) *cobra.Command {
	var edit bool
	cmd := newCommand(groupInspect, "notes "+notesUsage, "the repository notes reviewers read first (--edit: open them in $EDITOR)",
		"Print the notes file magnum keeps for a repository, state/notes/<owner>/<repo>.md: what the repository is, "+
			"how to run its tests and lint, how to QA a change, known pitfalls. Every review role reads it first and "+
			"the judge rewrites it after a round that taught it something durable. <repo> is owner/name, or a name "+
			"in daemon.default_repo's owner. A repository without notes prints a note on stderr and exits 0.\n\n"+
			"--edit opens the file in $VISUAL, else $EDITOR, else vi, on the terminal; the directory next to the file "+
			"(the same name without .md), where the judge saves QA scripts, is created first.",
		func(pos []string) int { return runNotes(c, edit, pos) })
	cmd.Flags().BoolVar(&edit, "edit", false, "open the notes in $VISUAL, else $EDITOR, else vi")
	cmd.ValidArgsFunction = completeFirst(c.completeNotesRepos)
	return cmd
}

func runNotes(c *Context, edit bool, pos []string) int {
	if len(pos) != 1 {
		return inspUsage(c, "notes", "want exactly one repository (owner/name or name)", notesUsage)
	}
	arg := strings.TrimSpace(pos[0])
	cfg := &config.Config{}
	if !strings.Contains(arg, "/") { // a bare name needs daemon.default_repo
		if err := c.LoadConfig(); err != nil {
			return cmdFail(c, "notes", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
		}
		cfg = c.Config
	}
	full, err := rolesRepo(cfg, arg)
	if err != nil {
		return inspUsage(c, "notes", strings.TrimPrefix(err.Error(), "--repo "), notesUsage)
	}
	owner, name, _ := strings.Cut(full, "/")
	path := engine.NotesPath(c.Layout, owner, name)
	if path == "" {
		return inspUsage(c, "notes", fmt.Sprintf("%q is not a repository name", full), notesUsage)
	}
	if edit {
		return notesEdit(c, full, path, engine.NotesDir(c.Layout, owner, name))
	}
	return notesShow(c, full, path)
}

// notesExist reports whether the repository has a notes file (the card of
// `magnum status <ref>` shows it).
func notesExist(l paths.Layout, owner, name string) bool {
	path := engine.NotesPath(l, owner, name)
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// notesShow prints the notes file, with control characters other than
// newline and tab replaced (an agent wrote the file: it must not drive the
// terminal).
func notesShow(c *Context, full, path string) int {
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(c.Stderr, "no notes for %s yet (%s)\n", full, path)
		return 0
	case err != nil:
		return cmdFail(c, "notes", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, notesMaxShown+1))
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	truncated := len(b) > notesMaxShown
	if truncated {
		b = b[:notesMaxShown]
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, string(b))
	if _, err := io.WriteString(c.Stdout, text); err != nil {
		return cmdFail(c, "notes", err)
	}
	if truncated {
		fmt.Fprintf(c.Stderr, "magnum notes: %s is larger than %d bytes; showing the start (open it with --edit)\n", path, notesMaxShown)
	}
	return 0
}

// notesEdit creates the harness directory and runs the editor on path.
func notesEdit(c *Context, full, path, dir string) int {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cmdFail(c, "notes", err)
	}
	editor := notesEditor()
	if err := notesRunEditor(c, editor, path); err != nil {
		return cmdFail(c, "notes", fmt.Errorf("editing the notes of %s: %s: %w", full, editor, err))
	}
	return 0
}

// notesEditor is the editor command: $VISUAL, else $EDITOR, else vi.
func notesEditor() string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if e := strings.TrimSpace(os.Getenv(k)); e != "" {
			return e
		}
	}
	return "vi"
}

// notesRunEditor runs editor on path with the terminal attached. Like git it
// goes through the shell, so an editor setting with arguments ("code -w",
// "emacsclient -t") works; the file name is a positional parameter, never
// part of the shell text. magnum ignores ctrl+c meanwhile: the editor owns
// the terminal and must not be orphaned by a signal meant for it. A variable
// so tests can replace it.
var notesRunEditor = func(c *Context, editor, path string) error {
	cmd := exec.Command("sh", "-c", editor+` "$@"`, "sh", path)
	cmd.Stdin = inspStdin
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	return cmd.Run()
}

// completeNotesRepos offers the repositories completeRepos knows, then the
// ones that have a notes file but are not in the registry.
func (c *Context) completeNotesRepos(toComplete string) []cobra.Completion {
	out := c.completeRepos(toComplete)
	seen := map[string]bool{}
	for _, cand := range out {
		name, _, _ := strings.Cut(cand, "\t")
		seen[strings.ToLower(name)] = true
	}
	root := engine.NotesRoot(c.Layout)
	if root == "" {
		return out
	}
	owners, _ := os.ReadDir(root)
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, o.Name()))
		for _, f := range files {
			repo, ok := strings.CutSuffix(f.Name(), ".md")
			full := o.Name() + "/" + repo
			if !ok || f.IsDir() || seen[full] {
				continue
			}
			seen[full] = true
			out = append(out, cobra.CompletionWithDesc(full, "repository with notes"))
		}
	}
	return out
}
