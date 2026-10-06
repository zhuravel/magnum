package cli

// `magnum notes <repo>`: the repository notes the review roles read first and
// the judge rewrites after a round that taught it something durable
// (engine.NotesPath), with their sizes against the [notes] curation triggers.
// Reading never needs the daemon, nor a registry (the unused harness files
// and a waiting proposal come from it when there is one); --edit opens the
// file in the user's editor on the terminal and records the result as a
// version. The history (--log, --diff, --restore) and the curation (--curate,
// --review) are in notes_history.go and notes_review.go.

import (
	"context"
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
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

const notesUsage = "<repo> [--edit | --log | --diff [N] | --curate | --review [--reason <text>] | --restore <version> [--reason <text>]] [--json]"

// notesMaxShown caps what `magnum notes` prints: notes larger than this are
// garbage not worth flooding the terminal with.
const notesMaxShown = 1 << 20

// notesFlags are the parsed `magnum notes` flags.
type notesFlags struct {
	edit, log, curate, review, json bool
	diff                            string // --diff [N]: "" = not asked
	restore                         int64
	reason                          string
}

func newNotesCmd(c *Context) *cobra.Command {
	var f notesFlags
	cmd := newCommand(groupInspect, "notes "+notesUsage, "the repository notes reviewers read first, their history and curation",
		"Print the notes file magnum keeps for a repository, ~/.local/share/magnum/notes/<owner>/<repo>.md: what the "+
			"repository is, how to run its tests and lint, how to QA a change, known pitfalls. Every review role reads it "+
			"first and the judge rewrites it after a round that taught it something durable; the directory next to it "+
			"(the same name without .md) holds the QA scripts the notes name, the harness. <repo> is owner/name, or a "+
			"name in daemon.default_repo's owner. A repository without notes prints a note on stderr and exits 0. Below "+
			"the notes, on stderr: their sizes against the [notes] curation triggers, the harness files no round used "+
			"in 20 rounds or more, and a curation proposal waiting for review.\n\n"+
			"--edit opens the file in $VISUAL, else $EDITOR, else vi, on the terminal (the harness directory is created "+
			"first) and records the result as a version.\n\n"+
			"Every version of the notes and the harness is kept in the registry: --log lists them (time, source, PR, "+
			"size, harness changes), --diff [N] shows what changed between the version N back (default 1) and the notes "+
			"now, and --restore <version> proposes an earlier version, reviewed like a curation.\n\n"+
			"--curate asks the daemon for a curation now: an agent proposes new notes and harness in a scratch copy, "+
			"keeping what helps future reviews of the repository; the live notes do not change. --review shows a waiting "+
			"proposal (the notes as a diff, the harness changes with the curator's reasons, the sizes before and after) "+
			"and asks y/N: y applies it under the notes lock unless the notes changed since, n rejects it (--reason says "+
			"why, and the next curation reads it), anything else leaves it for later. --review asks on a terminal only; "+
			"--json prints the data instead.",
		func(pos []string) int { return runNotes(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.edit, "edit", false, "open the notes in $VISUAL, else $EDITOR, else vi")
	fs.BoolVar(&f.log, "log", false, "list the recorded versions of the notes")
	fs.StringVar(&f.diff, "diff", "", "show the changes since the version `N` back (default 1)")
	fs.Lookup("diff").NoOptDefVal = "1"
	fs.BoolVar(&f.curate, "curate", false, "ask the daemon for a curation of the notes now")
	fs.BoolVar(&f.review, "review", false, "review the waiting curation proposal and apply (y) or reject (n) it")
	fs.Int64Var(&f.restore, "restore", 0, "propose the recorded `version` back, reviewed like a curation")
	fs.StringVar(&f.reason, "reason", "", "with --review or --restore: why the proposal is rejected (the next curation reads it)")
	fs.BoolVar(&f.json, "json", false, "print JSON (the notes' sizes, --log, --curate, --review, --restore)")
	cmd.ValidArgsFunction = completeFirst(c.completeNotesRepos)
	return cmd
}

func runNotes(c *Context, f notesFlags, pos []string) int {
	// --diff takes an optional value, which pflag reads only as --diff=N:
	// `--diff 2` leaves the 2 among the arguments.
	if f.diff != "" && len(pos) == 2 {
		_, err0 := parsePositive(pos[0])
		_, err1 := parsePositive(pos[1])
		switch {
		case err0 != nil && err1 == nil:
			f.diff, pos = pos[1], pos[:1]
		case err0 == nil && err1 != nil:
			f.diff, pos = pos[0], pos[1:]
		}
	}
	if len(pos) != 1 {
		return inspUsage(c, "notes", "want exactly one repository (owner/name or name)", notesUsage)
	}
	actions := 0
	for _, on := range []bool{f.edit, f.log, f.diff != "", f.curate, f.review, f.restore != 0} {
		if on {
			actions++
		}
	}
	switch {
	case actions > 1:
		return inspUsage(c, "notes", "--edit, --log, --diff, --curate, --review and --restore go one at a time", notesUsage)
	case f.reason != "" && !f.review && f.restore == 0:
		return inspUsage(c, "notes", "--reason goes with --review or --restore", notesUsage)
	case f.json && (f.edit || f.diff != ""):
		return inspUsage(c, "notes", "--json does not go with --edit or --diff", notesUsage)
	case f.restore < 0:
		return inspUsage(c, "notes", "--restore wants a version id (`--log` lists them)", notesUsage)
	}
	back := 0
	if f.diff != "" {
		n, err := parsePositive(f.diff)
		if err != nil {
			return inspUsage(c, "notes", fmt.Sprintf("--diff %q: want a positive number of versions back", f.diff), notesUsage)
		}
		back = n
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
	full = strings.ToLower(full)
	nr, _ := notes.RepoOf(path)
	switch {
	case f.edit:
		return notesEdit(c, full, nr)
	case f.log, back > 0, f.curate, f.review, f.restore != 0:
		return notesRegistryAction(c, f, back, full, nr)
	}
	return notesShow(c, f, full, nr)
}

// parsePositive reads a positive decimal number.
func parsePositive(s string) (int, error) {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' || n > 1<<20 {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, errors.New("not positive")
	}
	return n, nil
}

// notesOpenStore opens the registry for the notes command, nil when there is
// none yet: reading and editing the notes never needs one. A variable so
// tests can replace it.
var notesOpenStore = func(c *Context) (*store.Store, error) {
	if !fsx.Exists(c.Layout.DB()) {
		return nil, nil
	}
	return store.OpenWith(c.Layout.DB(), store.Options{BeforeMigrate: migrateGuard(c.Layout)})
}

// notesLimits are the [notes] curation triggers of the loaded config, else
// the built-in ones.
func notesLimits(c *Context) (notes.Limits, config.Notes) {
	n := config.DefaultNotes()
	if c.Config != nil || c.LoadConfig() == nil {
		n = c.Config.Notes
	}
	return notes.Limits{MaxBytes: n.MaxBytes, MaxLine: n.MaxLine, MaxHarnessFiles: n.MaxHarnessFiles, MaxHarnessBytes: n.MaxHarnessBytes}, n
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

// notesView is what `magnum notes <repo>` says besides the notes text (its
// --json form).
type notesView struct {
	Repo     string                `json:"repo"`
	Path     string                `json:"path"`
	Size     notes.Size            `json:"size"`
	Limits   notes.Limits          `json:"limits"`
	Over     []string              `json:"over"`
	Curate   config.CurateTriggers `json:"curate"`
	Unused   []string              `json:"unused"`
	Proposal *store.NotesProposal  `json:"proposal,omitempty"` // the newest waiting for review
	Registry string                `json:"registry_error,omitempty"`
}

// notesShow prints the notes file, with control characters other than
// newline and tab replaced (an agent wrote the file: it must not drive the
// terminal), then on stderr its sizes against the curation triggers, the
// unused harness files and a waiting proposal. --json prints only those.
func notesShow(c *Context, nf notesFlags, full string, nr notes.Repo) int {
	path := nr.Notes()
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !nf.json:
		fmt.Fprintf(c.Stderr, "no notes for %s yet (%s)\n", full, path)
		return 0
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return cmdFail(c, "notes", err)
	}
	if f != nil {
		defer f.Close()
	}
	view := notesGather(c, full, nr)
	if nf.json {
		if err := writeJSON(c.Stdout, view); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
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
	notesSummary(c.Stderr, view)
	return 0
}

// notesGather measures the notes and reads what the registry says of them
// (best effort: a registry that cannot be read is named, not fatal).
func notesGather(c *Context, full string, nr notes.Repo) notesView {
	l, cfg := notesLimits(c)
	v := notesView{Repo: full, Path: nr.Notes(), Limits: l, Curate: cfg.Curate, Unused: []string{}}
	size, _, err := notes.Measure(nr, l)
	if err != nil {
		v.Registry = err.Error()
	}
	v.Size, v.Over = size, size.Over(l)
	if v.Over == nil {
		v.Over = []string{}
	}
	st, err := notesOpenStore(c)
	if err != nil || st == nil {
		if err != nil {
			v.Registry = err.Error()
		}
		return v
	}
	defer st.Close()
	ctx, cancel := signalContext()
	defer cancel()
	repo, err := st.RepoByFullName(ctx, full)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			v.Registry = err.Error()
		}
		return v
	}
	if uses, err := st.NotesFileUses(ctx, repo.ID); err == nil {
		for _, u := range uses {
			if u.Unused() {
				v.Unused = append(v.Unused, u.File)
			}
		}
	}
	if ps, err := st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalPending}, Limit: 1}); err == nil && len(ps) > 0 {
		v.Proposal = &ps[0]
	}
	return v
}

// notesSummary writes the sizes against the triggers, the unused harness
// files and a waiting proposal.
func notesSummary(w io.Writer, v notesView) {
	past := func(limit string) string {
		for _, o := range v.Over {
			if o == limit {
				return ": past"
			}
		}
		return ""
	}
	s, l := v.Size, v.Limits
	fmt.Fprintf(w, "notes: %d bytes (max_bytes %d%s), %d lines, %d longer than %d characters (max_line%s)\n",
		s.Bytes, l.MaxBytes, past(notes.LimitBytes), s.Lines, s.LongLines, l.MaxLine, past(notes.LimitLine))
	fmt.Fprintf(w, "harness: %d files (max_harness_files %d%s), %d bytes (max_harness_bytes %d%s)\n",
		s.HarnessFiles, l.MaxHarnessFiles, past(notes.LimitHarnessFiles), s.HarnessBytes, l.MaxHarnessBytes, past(notes.LimitHarnessBytes))
	if len(v.Over) > 0 {
		how := "the daemon curates them once they change"
		if !v.Curate.Has(config.CurateOverLimit) {
			how = "[notes] curate = " + v.Curate.String() + " leaves over_limit out"
		}
		fmt.Fprintf(w, "past %s, curation triggers, not caps: %s; `magnum notes %s --curate` asks for one now\n",
			strings.Join(v.Over, ", "), how, v.Repo)
	}
	if len(v.Unused) > 0 {
		fmt.Fprintf(w, "unused harness files (%d rounds or more, no recorded use): %s\n", store.NotesUnusedRounds, strings.Join(v.Unused, ", "))
	}
	if p := v.Proposal; p != nil {
		fmt.Fprintf(w, "proposal %d (%s) waits for review since %s: `magnum notes %s --review`\n", p.ID, p.Kind,
			p.CreatedAt.Local().Format("2006-01-02 15:04"), v.Repo)
	}
	if v.Registry != "" {
		fmt.Fprintf(w, "registry: %s\n", v.Registry)
	}
}

// notesEdit creates the harness directory, runs the editor on the notes and
// records what it left as a version (source human), when the registry knows
// the repository; otherwise the daemon imports the change later.
func notesEdit(c *Context, full string, nr notes.Repo) int {
	if err := os.MkdirAll(nr.Harness(), 0o700); err != nil {
		return cmdFail(c, "notes", err)
	}
	editor := notesEditor()
	if err := notesRunEditor(c, editor, nr.Notes()); err != nil {
		return cmdFail(c, "notes", fmt.Errorf("editing the notes of %s: %s: %w", full, editor, err))
	}
	ctx, cancel := signalContext()
	defer cancel()
	v, err := notesRecordHuman(ctx, c, full, nr)
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum notes: the edit is not recorded as a version (%v); the daemon imports it at its next reconcile\n", err)
	case v != nil:
		fmt.Fprintf(c.Stderr, "recorded as version %d of the notes of %s\n", v.ID, full)
	}
	return 0
}

// notesRecordHuman records the notes as they are now as a version of source
// human; nil when nothing changed or there is no registry to record in.
func notesRecordHuman(ctx context.Context, c *Context, full string, nr notes.Repo) (*store.NotesVersion, error) {
	st, err := notesOpenStore(c)
	if err != nil || st == nil {
		return nil, err
	}
	defer st.Close()
	repo, err := st.RepoByFullName(ctx, full)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s is not in the registry", full)
	}
	if err != nil {
		return nil, err
	}
	state, err := notes.ReadState(nr)
	if err != nil {
		return nil, err
	}
	v, added, err := st.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: store.NotesFromHuman,
		Content: engine.ContentOf(state), Dedupe: true})
	if err != nil || !added {
		return nil, err
	}
	return &v, nil
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
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") { // .curate holds curations, no owner
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
