package factory

// The ready card's FLAGS line (spec 6.6): what in the change needs a
// second look before you push it. The migrations the branch adds with
// their flags (ScanMigrations), the files [factory] sensitive names
// (Gemfile.lock, the workflows and the config files by default), the files
// the plan did not name, the guards the change touches that are older than
// 6 months with the reason the history agent found, the CI jobs whose
// commands the precheck did not run, and the conflicts with a fresh
// origin/<base> (git merge-tree, which changes no work tree). Every flag is
// read from the copy at the task's head and the task's files; none of it
// changes the copy.

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// Flags are what the ready card's FLAGS line names: the migrations the
// branch adds (with their flags), the sensitive files it changes, the files
// it changes that the plan did not name, the old guards it touches, the CI
// jobs whose commands did not run locally ("<workflow>: <job>"), the
// paths merging origin/<base> into it would conflict in, and what the
// precheck that passed at the head did not finish (its cap's note, from
// precheck.json; "" for nothing).
type Flags struct {
	Migrations                         []MigrationFlag
	Sensitive, NotInPlan, CIJobsNotRun []string
	OldGuards                          []OldGuard
	Conflicts                          []string
	PrecheckUnfinished                 string
}

// OldGuard is a guard the change touches (a condition, a rescue, a validation,
// an early return): its place in the base, how long ago the line was
// written, and why it was added, as the history agent found it.
type OldGuard struct {
	Path string
	Line int
	Age  time.Duration
	Why  string
}

// oldGuardMonths is the age from which a changed guard is flagged.
const oldGuardMonths = 6

// flags reads task t's change in copy c (its branch at its head) for the
// ready card: see Flags. The conflicts come from a fetch of origin/<base>
// (its main clone's) and `git merge-tree` against it.
func (e *Engine) flags(ctx context.Context, t registry.Task, c Copy) (Flags, error) {
	var f Flags
	repo, err := e.factoryRepo(t)
	if err != nil {
		return f, err
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	if err != nil {
		return f, err
	}
	if bh.Problem != "" {
		return f, fmt.Errorf("the flags of %s: %s", t.Branch, bh.Problem)
	}
	git := e.d.Git
	changes, err := git.ChangedFiles(ctx, c.Path, bh.Base, bh.Head)
	if err != nil {
		return f, err
	}
	var paths, added []string
	for _, ch := range changes {
		paths = append(paths, ch.Path)
		if ch.Status == 'A' && strings.HasPrefix(ch.Path, "db/migrate/") && strings.HasSuffix(ch.Path, ".rb") {
			added = append(added, ch.Path)
		}
	}
	if f.Migrations, err = ScanMigrations(c.Path, added, repo); err != nil {
		return f, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	f.Sensitive = matching(paths, e.cfg.Factory.Sensitive)
	f.NotInPlan = notInPlan(paths, readPlanFiles(dir))
	if f.OldGuards, err = e.oldGuards(ctx, c, bh, dir, paths); err != nil {
		return f, err
	}
	if f.CIJobsNotRun, err = ciJobsNotRun(c.Path, repo.Precheck); err != nil {
		return f, err
	}
	f.PrecheckUnfinished = precheckUnfinishedAt(dir, bh.Head)
	base := baseOf(repo)
	if err := git.FetchBranch(ctx, cmp.Or(repo.MainClone, c.Path), base); err != nil {
		return f, err
	}
	if f.Conflicts, err = git.MergeConflicts(ctx, c.Path, bh.Head, "origin/"+base); err != nil {
		return f, err
	}
	return f, nil
}

// Lines are the flags as the card's FLAGS line says them, one each; none
// when nothing needs a second look.
func (f Flags) Lines() []string {
	var out []string
	for _, m := range f.Migrations {
		out = append(out, "migration "+m.String())
	}
	if len(f.Sensitive) > 0 {
		out = append(out, "sensitive: "+strings.Join(f.Sensitive, ", "))
	}
	if len(f.NotInPlan) > 0 {
		out = append(out, "not in the plan: "+strings.Join(f.NotInPlan, ", "))
	}
	for _, g := range f.OldGuards {
		out = append(out, fmt.Sprintf("an old guard at %s:%d (%s): %s", g.Path, g.Line, ageWords(g.Age), cmp.Or(g.Why, "the history names no reason")))
	}
	if len(f.CIJobsNotRun) > 0 {
		out = append(out, "CI jobs not run here: "+strings.Join(f.CIJobsNotRun, ", "))
	}
	if f.PrecheckUnfinished != "" {
		out = append(out, "precheck unfinished: "+f.PrecheckUnfinished)
	}
	if len(f.Conflicts) > 0 {
		out = append(out, conflictsFlag+strings.Join(f.Conflicts, ", "))
	}
	return out
}

// conflictsFlag starts the FLAGS line of the conflicts with the base: the
// screen offers no push on a card that has it.
const conflictsFlag = "conflicts with the base: "

// ageWords is a guard's age in months or years ("8 months", "2 years").
func ageWords(d time.Duration) string {
	const month = 30 * 24 * time.Hour
	switch months := int(d / month); {
	case months >= 24:
		return strconv.Itoa(months/12) + " years"
	case months >= 12:
		return "a year"
	default:
		return plural(max(months, 1), "month")
	}
}

// matching are the paths one of globs matches (config.MatchPath), in order.
func matching(paths, globs []string) []string {
	var out []string
	for _, p := range paths {
		if slices.ContainsFunc(globs, func(g string) bool { return config.MatchPath(g, p) }) {
			out = append(out, p)
		}
	}
	return out
}

// readPlanFiles are the files report.md in dir names in its Files section.
func readPlanFiles(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, reportFile))
	if err != nil {
		return nil
	}
	return parseReport(string(b)).files
}

// notInPlan are the paths the plan's files do not name: a file of its own,
// a directory that holds it ("app/services/") or a glob that matches it.
func notInPlan(paths, plan []string) []string {
	var out []string
	for _, p := range paths {
		if !slices.ContainsFunc(plan, func(f string) bool {
			f = strings.TrimPrefix(f, "./")
			return f == p || (strings.HasSuffix(f, "/") && strings.HasPrefix(p, f)) || config.MatchPath(f, p)
		}) {
			out = append(out, p)
		}
	}
	return out
}

// guardEntryRe is a guard as the history agent lists it in its findings: a
// list item that starts with its place, `path:line`.
var guardEntryRe = regexp.MustCompile("^\\s*(?:[-*+]|\\d+[.)])\\s+`?([^\\s`:]+):(\\d+)`?(.*)$")

// guardWhyRe is the reason of a guard entry: what follows "why:", up to
// "breaks:".
var guardWhyRe = regexp.MustCompile(`(?i)\bwhy:\s*(.*?)\s*(?:[;,.·—-]*\s*\bbreaks:.*)?$`)

// historyGuard is a guard a findings file names: its place and why.
type historyGuard struct {
	path string
	line int
	why  string
}

// historyPrompt is the history agent's prompt, which asks for the guards
// as list items with their place and their why.
const historyPrompt = "factory-history.md"

// historyFindings are the findings files of the history agents: the
// factory roles whose prompt is historyPrompt (fx-history's
// findings-fx-history.md by default). No other agent's findings name a
// guard.
func (e *Engine) historyFindings() []string {
	var out []string
	for _, r := range e.cfg.Factory.Roles {
		if r.Prompt == historyPrompt {
			out = append(out, findingsFile(r.Name))
		}
	}
	return out
}

// historyGuards are the guards that files (the history agents' findings,
// historyFindings) in dir name, each a list item with its place and its
// why, in the order they come, each place once.
func historyGuards(dir string, files []string) []historyGuard {
	var out []historyGuard
	seen := map[string]bool{}
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		for l := range strings.SplitSeq(string(b), "\n") {
			m := guardEntryRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			w := guardWhyRe.FindStringSubmatch(m[3])
			n, err := strconv.Atoi(m[2])
			if w == nil || err != nil || n < 1 || seen[m[1]+":"+m[2]] {
				continue
			}
			seen[m[1]+":"+m[2]] = true
			out = append(out, historyGuard{path: m[1], line: n, why: textx.Clip(strings.TrimSpace(w[1]), 200)})
		}
	}
	return out
}

// oldGuards are the guards of the history findings in dir that the change
// from bh.Base to bh.Head (changing paths) removes or rewrites, whose line
// the base had for longer than oldGuardMonths.
func (e *Engine) oldGuards(ctx context.Context, c Copy, bh branchHead, dir string, paths []string) ([]OldGuard, error) {
	var out []OldGuard
	cutoff := e.now().AddDate(0, -oldGuardMonths, 0)
	touched := map[string]map[int]bool{}
	for _, g := range historyGuards(dir, e.historyFindings()) {
		if !slices.Contains(paths, g.path) {
			continue
		}
		lines, ok := touched[g.path]
		if !ok {
			diff, err := e.d.Git.FileDiff(ctx, c.Path, bh.Base, bh.Head, g.path)
			if err != nil {
				return nil, err
			}
			lines = removedLines(diff)
			touched[g.path] = lines
		}
		if !lines[g.line] {
			continue
		}
		_, at, err := e.d.Git.BlameLine(ctx, c.Path, bh.Base, g.path, g.line)
		if err != nil {
			return nil, err
		}
		if at.Before(cutoff) {
			out = append(out, OldGuard{Path: g.path, Line: g.line, Age: e.now().Sub(at), Why: g.why})
		}
	}
	return out, nil
}

// removedLines are the lines of the base's file that a unified diff of one
// file removes (a rewritten line is removed and added), by their number in
// the base.
func removedLines(diff string) map[int]bool {
	out := map[int]bool{}
	base := 0
	in := false
	for l := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(l, "@@ ") {
			f := strings.Fields(l)
			if len(f) < 3 {
				in = false
				continue
			}
			start, _, _ := strings.Cut(strings.TrimPrefix(f[1], "-"), ",")
			n, err := strconv.Atoi(start)
			base, in = n, err == nil
			continue
		}
		if !in || l == "" {
			continue
		}
		switch l[0] {
		case ' ':
			base++
		case '-':
			out[base] = true
			base++
		case '+', '\\':
		default:
			in = false // the next file's header
		}
	}
	return out
}

// ciJobsNotRun are the jobs of the repository's GitHub workflows in checkout
// (.github/workflows/*.yml and *.yaml) that run on a pull request and whose
// text holds none of the precheck's commands (each up to its first
// placeholder: `bundle exec rubocop {files}` is `bundle exec rubocop`), as
// "<workflow>: <job>": what CI checks that the precheck did not. A
// repository without workflows has none.
func ciJobsNotRun(checkout string, pc config.FactoryPrecheck) ([]string, error) {
	var ran []string
	for _, cmd := range slices.Concat(pc.Lint, []string{pc.Specs, pc.Jest}, pc.Migrations) {
		if p, _, _ := strings.Cut(cmd, "{"); strings.TrimSpace(p) != "" {
			ran = append(ran, strings.TrimSpace(p))
		}
	}
	dir := filepath.Join(checkout, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, en := range entries {
		ext := filepath.Ext(en.Name())
		if en.IsDir() || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, en.Name()))
		if err != nil {
			return nil, err
		}
		wf := readWorkflow(string(b))
		if !wf.onPR {
			continue
		}
		for _, j := range wf.jobs {
			if !slices.ContainsFunc(ran, func(cmd string) bool { return strings.Contains(j.text, cmd) }) {
				out = append(out, strings.TrimSuffix(en.Name(), ext)+": "+j.name)
			}
		}
	}
	return out, nil
}

// workflow is a GitHub workflow file as ciJobsNotRun reads it: whether it
// runs on a pull request, and its jobs, each with its name and its text.
type workflow struct {
	onPR bool
	jobs []workflowJob
}

type workflowJob struct{ name, text string }

// topKeyRe is a key at the top of a YAML document; jobKeyRe a key one level
// in.
var (
	topKeyRe = regexp.MustCompile(`^["']?([A-Za-z_][\w-]*)["']?\s*:(.*)$`)
	jobKeyRe = regexp.MustCompile(`^(\s+)["']?([\w-]+)["']?\s*:\s*(?:#.*)?$`)
	nameRe   = regexp.MustCompile(`^\s*name\s*:\s*["']?(.*?)["']?\s*$`)
)

// readWorkflow reads a workflow by its indentation, as far as ciJobsNotRun
// needs: its `on` section (a pull_request or pull_request_target trigger)
// and the jobs of its `jobs` section, each named by its own `name` (else
// its id), with every line of it as its text. It is no YAML parser: what a
// workflow writes in flow style past the top keys is read as text.
func readWorkflow(text string) workflow {
	var wf workflow
	section := ""
	jobIndent := -1
	var job *workflowJob
	keyIndent := -1
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		if indent == 0 {
			if m := topKeyRe.FindStringSubmatch(l); m != nil {
				section = m[1]
				if (section == "on" || section == "true") && strings.Contains(m[2], "pull_request") {
					wf.onPR = true
				}
			}
			continue
		}
		switch section {
		case "on", "true":
			if strings.Contains(trimmed, "pull_request") {
				wf.onPR = true
			}
		case "jobs":
			if jobIndent < 0 {
				jobIndent = indent
			}
			if m := jobKeyRe.FindStringSubmatch(l); m != nil && len(m[1]) == jobIndent {
				wf.jobs = append(wf.jobs, workflowJob{name: m[2]})
				job, keyIndent = &wf.jobs[len(wf.jobs)-1], -1
				continue
			}
			if job == nil || indent <= jobIndent {
				continue
			}
			if keyIndent < 0 {
				keyIndent = indent
			}
			if m := nameRe.FindStringSubmatch(l); m != nil && indent == keyIndent && m[1] != "" {
				job.name = m[1]
			}
			job.text += trimmed + "\n"
		}
	}
	return wf
}
