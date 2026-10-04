package eval

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

const runFile = "run.json"

// CaseRun is one case of a replay: what became of the review round and, when it left a result,
// the score.
type CaseRun struct {
	Case       string    `json:"case"`
	PR         string    `json:"pr"`
	Head       string    `json:"head"`
	Outcome    string    `json:"outcome"` // dry_run, error, needs_attention, usage_limit, skipped, ...
	Error      string    `json:"error,omitempty"`
	ResultFile string    `json:"result_file,omitempty"` // absolute path of the judge result
	ReportDir  string    `json:"report_dir,omitempty"`
	Started    time.Time `json:"started,omitzero"`
	Finished   time.Time `json:"finished,omitzero"`
	Score      *Score    `json:"score,omitempty"` // nil when there was no result to score
}

// Run is one replay of a corpus.
type Run struct {
	ID       string            `json:"id"`               // sortable, e.g. 20261004-153000
	Label    string            `json:"label"`            // free text
	Commit   string            `json:"commit"`           // magnum's git HEAD (short) when the run started, "" unknown
	Dirty    bool              `json:"dirty"`            // magnum's checkout had uncommitted changes
	Models   map[string]string `json:"models,omitempty"` // role name -> model, "" = the kind's default
	Corpus   string            `json:"corpus"`           // corpus path
	Started  time.Time         `json:"started,omitzero"`
	Finished time.Time         `json:"finished,omitzero"`
	Cases    []CaseRun         `json:"cases"`
}

// Totals sums the scored cases of a run.
type Totals struct {
	Cases           int // all cases of the run
	Scored          int // cases that have a score
	Found           int
	Total           int
	Noise           int
	Simplifications int
	SeverityOK      int
	Recall          float64 // Found/Total, 0 when Total is 0
}

// Totals adds up the scores of the run. Cases without a score count in Cases only.
func (r Run) Totals() Totals {
	t := Totals{Cases: len(r.Cases)}
	for _, c := range r.Cases {
		if c.Score == nil {
			continue
		}
		t.Scored++
		t.Found += c.Score.Found
		t.Total += c.Score.Total
		t.Noise += c.Score.Noise
		t.Simplifications += c.Score.Simplifications
		t.SeverityOK += c.Score.SeverityOK()
	}
	if t.Total > 0 {
		t.Recall = float64(t.Found) / float64(t.Total)
	}
	return t
}

// SaveRun writes dir/run.json atomically (a temporary file in dir, then a rename), mode 0600; dir is
// created with mode 0700.
func SaveRun(dir string, r Run) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: save run %s: %w", r.ID, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("eval: save run %s: %w", r.ID, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("eval: save run %s: %w", r.ID, err)
	}
	defer root.Close()
	tmp := "." + runFile + ".tmp"
	if err := writeFile(root, tmp, append(data, '\n')); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("eval: save run %s: %w", r.ID, err)
	}
	if err := root.Rename(tmp, runFile); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("eval: save run %s: %w", r.ID, err)
	}
	return nil
}

func writeFile(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// LoadRun reads dir/run.json.
func LoadRun(dir string) (Run, error) {
	data, err := os.ReadFile(filepath.Join(dir, runFile))
	if err != nil {
		return Run{}, fmt.Errorf("eval: load run: %w", err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return Run{}, fmt.Errorf("eval: load run %s: %w", dir, err)
	}
	if r.ID == "" {
		r.ID = filepath.Base(dir)
	}
	return r, nil
}

// ListRuns loads every root/*/run.json, oldest ID first. A directory without a readable run.json is
// skipped; a missing root has no runs.
func ListRuns(root string) ([]Run, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eval: list runs: %w", err)
	}
	var runs []Run
	for _, e := range entries {
		if r, err := LoadRun(filepath.Join(root, e.Name())); err == nil {
			runs = append(runs, r)
		}
	}
	slices.SortStableFunc(runs, func(a, b Run) int { return cmp.Compare(a.ID, b.ID) })
	return runs, nil
}

// Rescore recomputes the score of every case of r from its saved result file against the current
// defects of the corpus, so a corrected match expression takes effect without running the agents
// again. read loads a file (os.ReadFile when nil). The run it returns is a copy; r is not changed.
//
// A case the corpus no longer has, or that has no result file, keeps its score. A result that cannot
// be read or parsed keeps the old score too and, when there was one, sets Error to say so.
func Rescore(r Run, c *Corpus, read func(path string) ([]byte, error)) Run {
	if read == nil {
		read = os.ReadFile
	}
	out := r
	out.Cases = slices.Clone(r.Cases)
	out.Models = maps.Clone(r.Models)
	if c == nil {
		return out
	}
	byName := make(map[string]Case, len(c.Cases))
	for _, cs := range c.Cases {
		byName[cs.Name] = cs
	}
	for i := range out.Cases {
		cr := &out.Cases[i]
		cs, ok := byName[cr.Case]
		if !ok || cr.ResultFile == "" {
			continue
		}
		data, err := read(cr.ResultFile)
		var res Result
		if err == nil {
			res, err = ParseResult(data)
		}
		if err != nil {
			if cr.Score != nil {
				cr.Error = "rescore: " + err.Error()
			}
			continue
		}
		sc := ScoreCase(cs, res)
		cr.Score = &sc
	}
	return out
}

// WriteText writes the human report of r: a table with one row per case and a total, then, when prev
// is not nil, how the totals moved since it, then one line per missed defect and per case error.
func WriteText(w io.Writer, r Run, prev *Run) {
	fmt.Fprintln(w, runHeader(r))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tPR\tFOUND\tSEV\tNOISE\tSIMPL\tOUTCOME")
	for _, c := range r.Cases {
		if c.Score == nil {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t%s\n", c.Case, c.PR, c.Outcome)
			continue
		}
		s := c.Score
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%d/%d\t%d\t%d\t%s\n",
			c.Case, c.PR, s.Found, s.Total, s.SeverityOK(), s.Found, s.Noise, s.Simplifications, c.Outcome)
	}
	t := r.Totals()
	fmt.Fprintf(tw, "total\t\t%s\t%d/%d\t%d\t%d\n", foundWithRecall(t), t.SeverityOK, t.Found, t.Noise, t.Simplifications)
	tw.Flush()
	if prev != nil {
		p := prev.Totals()
		fmt.Fprintf(w, "vs %s: recall %d%% → %d%% (%+d pts), noise %d → %d\n",
			prev.ID, percent(p.Recall), percent(t.Recall), percent(t.Recall)-percent(p.Recall), p.Noise, t.Noise)
	}
	for _, c := range r.Cases {
		if c.Score == nil {
			continue
		}
		for _, d := range c.Score.Defects {
			if !d.Found {
				fmt.Fprintf(w, "missed: %s/%s %q\n", c.Case, d.ID, d.Title)
			}
		}
	}
	for _, c := range r.Cases {
		if c.Error != "" {
			fmt.Fprintf(w, "error: %s: %s\n", c.Case, firstLine(c.Error, 100))
		}
	}
}

// WriteMarkdown writes the report of r as markdown (report.md): a summary table, then per case each
// defect as found or missed with its severity, and the findings that matched no defect.
func WriteMarkdown(w io.Writer, r Run) {
	fmt.Fprintf(w, "# Eval run %s\n\n", r.ID)
	if r.Label != "" {
		fmt.Fprintf(w, "- Label: %s\n", r.Label)
	}
	if m := magnumVersion(r); m != "" {
		fmt.Fprintf(w, "- Magnum: %s\n", m)
	}
	if r.Corpus != "" {
		fmt.Fprintf(w, "- Corpus: `%s`\n", r.Corpus)
	}
	if len(r.Models) > 0 {
		var roles []string
		for _, role := range slices.Sorted(maps.Keys(r.Models)) {
			model := r.Models[role]
			if model == "" {
				model = "default"
			}
			roles = append(roles, role+"="+model)
		}
		fmt.Fprintf(w, "- Models: %s\n", strings.Join(roles, ", "))
	}
	if !r.Started.IsZero() {
		fmt.Fprintf(w, "- Started: %s\n", r.Started.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "\n## Summary\n\n| Case | PR | Found | Severity | Noise | Simplifications | Outcome |\n|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cases {
		if c.Score == nil {
			fmt.Fprintf(w, "| %s | %s | - | - | - | - | %s |\n", c.Case, c.PR, c.Outcome)
			continue
		}
		s := c.Score
		fmt.Fprintf(w, "| %s | %s | %d/%d | %d/%d | %d | %d | %s |\n",
			c.Case, c.PR, s.Found, s.Total, s.SeverityOK(), s.Found, s.Noise, s.Simplifications, c.Outcome)
	}
	t := r.Totals()
	fmt.Fprintf(w, "| **total** | | %s | %d/%d | %d | %d | |\n", foundWithRecall(t), t.SeverityOK, t.Found, t.Noise, t.Simplifications)
	for _, c := range r.Cases {
		fmt.Fprintf(w, "\n## %s\n\n%s", c.Case, c.PR)
		if c.Head != "" {
			fmt.Fprintf(w, " at `%s`", head(c.Head, 12))
		}
		fmt.Fprintf(w, ", outcome %s\n", c.Outcome)
		if c.Error != "" {
			fmt.Fprintf(w, "\nError: %s\n", firstLine(c.Error, 100))
		}
		if c.Score == nil {
			fmt.Fprintf(w, "\nNo result to score.\n")
			continue
		}
		fmt.Fprintln(w)
		for _, d := range c.Score.Defects {
			fmt.Fprintf(w, "- %s `%s` %s%s\n", defectVerdict(d), d.ID, d.Title, defectSeverity(d))
		}
		if len(c.Score.Unmatched) > 0 {
			fmt.Fprintf(w, "\nUnmatched findings:\n\n")
			for _, f := range c.Score.Unmatched {
				fmt.Fprintf(w, "- `%s` %s\n", findingPlace(f), firstLine(f.Body, 100))
			}
		}
	}
}

func runHeader(r Run) string {
	h := "run " + r.ID
	if r.Label != "" {
		h += fmt.Sprintf("  %q", r.Label)
	}
	if m := magnumVersion(r); m != "" {
		h += "  " + m
	}
	return h
}

// magnumVersion is "magnum 6824a5e (dirty)", or "" when neither the commit nor a dirty tree is known.
func magnumVersion(r Run) string {
	var parts []string
	if r.Commit != "" {
		parts = append(parts, r.Commit)
	}
	if r.Dirty {
		parts = append(parts, "(dirty)")
	}
	if len(parts) == 0 {
		return ""
	}
	return "magnum " + strings.Join(parts, " ")
}

func foundWithRecall(t Totals) string {
	return fmt.Sprintf("%d/%d (%d%%)", t.Found, t.Total, percent(t.Recall))
}

func percent(f float64) int { return int(math.Round(f * 100)) }

func defectVerdict(d DefectResult) string {
	if d.Found {
		return "found"
	}
	return "missed"
}

// defectSeverity describes the severity of a defect for the markdown report.
func defectSeverity(d DefectResult) string {
	switch {
	case !d.Found && d.Want != "":
		return " (wanted " + d.Want + ")"
	case !d.Found:
		return ""
	}
	got := "no severity"
	if d.Severity != "" {
		got = d.Severity
	}
	switch {
	case d.Want == "":
		return " (" + got + ")"
	case d.SeverityOK:
		return " (" + got + ", wanted " + d.Want + ")"
	default:
		return " (" + got + ", below the wanted " + d.Want + ")"
	}
}

func findingPlace(f Finding) string {
	switch {
	case f.Path == "":
		return "(no path)"
	case f.Line <= 0:
		return f.Path
	default:
		return fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
}

// firstLine returns the first line of s, cut at n runes with an ellipsis.
func firstLine(s string, n int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimSpace(line)
	if cut := head(line, n); cut != line {
		return cut + "…"
	}
	return line
}
