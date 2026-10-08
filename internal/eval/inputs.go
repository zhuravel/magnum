package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// HashNotes is the SHA-256 (hex) of each notes file a replay gives the judge: the repository's
// notes file, keyed by its base name ("" when there is none), and every regular file under its
// harness directory dir, keyed by the directory's base name and the path below it ("web/qa.sh").
// Two runs whose maps differ for a case gave its judge other notes (WriteText says so).
func HashNotes(file, dir string) (map[string]string, error) {
	out := map[string]string{filepath.Base(file): ""}
	switch b, err := os.ReadFile(file); {
	case err == nil:
		out[filepath.Base(file)] = hashHex(b)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("eval: hash notes: %w", err)
	}
	err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case !de.Type().IsRegular():
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(filepath.Join(filepath.Base(dir), rel))] = hashHex(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("eval: hash notes: %w", err)
	}
	return out, nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// notesDiff says how the notes the judge of each case of r read differ from what the same case of
// prev read, one line per case that differs ("geo (web.md changed, web/qa.sh new)"). A case only
// one run has, or whose notes a run did not record (runs before HashNotes), is not compared; a run
// that withheld the notes (--no-notes) differs from one whose judge read any.
func notesDiff(r, prev Run) []string {
	var out []string
	for _, c := range r.Cases {
		i := slices.IndexFunc(prev.Cases, func(p CaseRun) bool { return p.Case == c.Case })
		if i < 0 {
			continue
		}
		cur, curOK := caseNotes(r, c)
		old, oldOK := caseNotes(prev, prev.Cases[i])
		if !curOK || !oldOK {
			continue
		}
		var what []string
		switch {
		case r.NoNotes && prev.NoNotes:
		case r.NoNotes || prev.NoNotes:
			if len(nonEmpty(cur))+len(nonEmpty(old)) > 0 {
				what = append(what, "withheld in "+cmpID(r, prev))
			}
		default:
			for _, name := range slices.Sorted(maps.Keys(union(cur, old))) {
				a, b := old[name], cur[name]
				switch {
				case a == b:
				case a == "":
					what = append(what, name+" new")
				case b == "":
					what = append(what, name+" gone")
				default:
					what = append(what, name+" changed")
				}
			}
		}
		if len(what) > 0 {
			out = append(out, fmt.Sprintf("%s (%s)", c.Case, strings.Join(what, ", ")))
		}
	}
	return out
}

// caseNotes is what notes c of r read: none when r withheld them; ok is false when r did not record them.
func caseNotes(r Run, c CaseRun) (map[string]string, bool) {
	if r.NoNotes {
		return nil, true
	}
	return c.Notes, c.Notes != nil
}

// cmpID names the run of the two that withheld the notes.
func cmpID(r, prev Run) string {
	if r.NoNotes {
		return r.ID
	}
	return prev.ID
}

func nonEmpty(m map[string]string) map[string]string {
	return maps.Collect(func(yield func(string, string) bool) {
		for k, v := range m {
			if v != "" && !yield(k, v) {
				return
			}
		}
	})
}

func union(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

// Baseline is a stored replay of a case that a run with the same inputs need not repeat.
type Baseline struct {
	Run    string  // the stored run's ID
	Commit string  // magnum's commit that ran it
	Case   CaseRun // the case as that run recorded it
}

// Baselines lists, newest run first, the stored replays of c that a run with inputs (Run.Inputs:
// the judge skill, the roles' prompts and the role config) may reuse instead of replaying c: runs
// that recorded the same inputs from a clean checkout, whose case of c's name reviewed c's head
// and has a score. Unknown inputs ("") reuse nothing. The inputs leave magnum's code out, so the
// caller decides whether the code of Commit reviews as its own does (eval-at.sh compares the Go
// files of the two commits).
func Baselines(runs []Run, inputs string, c Case) []Baseline {
	var out []Baseline
	if inputs == "" {
		return nil
	}
	for _, r := range slices.Backward(runs) {
		if r.Inputs != inputs || r.Dirty {
			continue
		}
		for _, cr := range r.Cases {
			if cr.Case == c.Name && cr.Head == c.Head && cr.Score != nil {
				out = append(out, Baseline{Run: r.ID, Commit: r.Commit, Case: cr})
				break
			}
		}
	}
	return out
}
