package pipeline

// The paths whose findings come back (DECISIONS "A re-review names the paths
// whose P1 or P2 findings came back in three or more rounds"): the review
// skill's section 6 asks for one structural finding when the fixes of two
// or more findings patched one mechanism and a new case still gets through,
// and that rule posted none from 2026-10-12 to 2026-11-11, since the judge
// sees only its own threads and cannot count the rounds per file. When a
// re-review's or a recovery's judge prompt (its own pass too) goes out, the
// PR's paths with posted P1 or P2 findings in three or more rounds, from
// the registry's findings (store.FindingsByPR, which a posted round
// records), go to repeat-paths.json in the report directory, named as
// `repeat_paths`. Titles are the judge's own words, kept in the file, never
// in a prompt or an event.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// RepeatPathsFile is the PR's repeat paths in the round's report directory.
const RepeatPathsFile = "repeat-paths.json"

// repeatRounds is how many posted rounds with a P0 to P2 finding on one path
// make it a repeat path.
const repeatRounds = 3

// fixForMark starts the judge's title suffix for a finding in code written
// to fix an earlier one: "(in the fix for <earlier title>)" (the skill's
// section 6, store.FindingOrigin).
const fixForMark = "(in the fix for "

// repeatPaths is repeat-paths.json.
type repeatPaths struct {
	PR    string       `json:"pr"` // owner/repo#N
	Paths []repeatPath `json:"paths"`
}

// repeatPath is one path of repeatPaths: the posted rounds with a P0 to P2
// finding on it, ascending, and those findings by round.
type repeatPath struct {
	Path     string          `json:"path"`
	Rounds   []int           `json:"rounds"`
	Findings []repeatFinding `json:"findings"`
}

// repeatFinding is one posted P0 to P2 finding of a repeat path, titled as
// the judge posted it. InTheFixFor is the earlier finding its title's "(in
// the fix for ...)" names, and InTheFixForRound the latest earlier round in
// which the PR's posted findings hold that title (0 when none does).
type repeatFinding struct {
	Round            int    `json:"round"`
	Severity         string `json:"severity"`
	Title            string `json:"title"`
	InTheFixFor      string `json:"in_the_fix_for,omitempty"`
	InTheFixForRound int    `json:"in_the_fix_for_round,omitempty"`
}

// addRepeatPaths writes repeat-paths.json for a re-review's or a recovery's
// judge prompt about to go out and names it in jd when a path of the PR
// qualifies (findRepeatPaths); with none, no repeat-paths.json is there (an
// earlier round's on this head is removed). A first review, a continued
// turn, a blind replay and a local round get none. A failure leaves the
// prompt without it (a warning): the review goes on.
func (rd *round) addRepeatPaths(ctx context.Context, jd *agents.JudgeData) {
	in := rd.in
	if in.Blind || in.Local != nil || rd.r.Store == nil || (in.Kind != KindRereview && in.Kind != KindRecovery) {
		return
	}
	file := filepath.Join(rd.dir, RepeatPathsFile)
	found, err := rd.r.Store.FindingsByPR(ctx, rd.pr.ID)
	var paths []repeatPath
	if err == nil {
		if paths = findRepeatPaths(found, in.SlotPath); len(paths) > 0 {
			err = writeRepeatPaths(file, repeatPaths{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), Paths: paths})
		} else if err = os.Remove(file); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.repeat_paths", fmt.Sprintf("could not list the paths whose findings came back; the judge goes without: %v", err), nil)
		}
		return
	}
	if len(paths) == 0 {
		return
	}
	jd.RepeatPaths = file
	most := 0
	for _, p := range paths {
		most = max(most, len(p.Rounds))
	}
	rd.event(ctx, "info", "round.repeat_paths", fmt.Sprintf("%s: %d path(s) with P1 or P2 findings in %d or more posted rounds (at most %d)",
		RepeatPathsFile, len(paths), repeatRounds, most), map[string]any{"paths": len(paths), "most_rounds": most, "file": file})
}

// writeRepeatPaths writes v to file atomically, indented.
func writeRepeatPaths(file string, v repeatPaths) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(file, append(b, '\n'), 0o600)
}

// findRepeatPaths is the paths of found (one PR's findings, oldest first)
// with a posted P0 to P2 finding in repeatRounds or more rounds, the most
// rounds first, then by path. A path is cleaned and made relative to the
// checkout when the judge wrote it inside checkout.
func findRepeatPaths(found []store.Finding, checkout string) []repeatPath {
	byPath := map[string]*repeatPath{}
	for _, f := range found {
		if f.Verdict != store.FindingPosted || f.Path == "" || !blockingSeverity(f.Severity) {
			continue
		}
		p := path.Clean(f.Path)
		if checkout != "" {
			p = strings.TrimPrefix(p, strings.TrimSuffix(checkout, "/")+"/")
		}
		rp := byPath[p]
		if rp == nil {
			rp = &repeatPath{Path: p}
			byPath[p] = rp
		}
		if !slices.Contains(rp.Rounds, f.Round) {
			rp.Rounds = append(rp.Rounds, f.Round)
		}
		rf := repeatFinding{Round: f.Round, Severity: f.Severity, Title: f.Title}
		if earlier, ok := inTheFixFor(f.Title); ok {
			rf.InTheFixFor, rf.InTheFixForRound = earlier, titledRound(found, earlier, f.Round)
		}
		rp.Findings = append(rp.Findings, rf)
	}
	var out []repeatPath
	for _, rp := range byPath {
		if len(rp.Rounds) < repeatRounds {
			continue
		}
		slices.Sort(rp.Rounds)
		slices.SortStableFunc(rp.Findings, func(a, b repeatFinding) int { return cmp.Compare(a.Round, b.Round) })
		out = append(out, *rp)
	}
	slices.SortFunc(out, func(a, b repeatPath) int {
		return cmp.Or(cmp.Compare(len(b.Rounds), len(a.Rounds)), strings.Compare(a.Path, b.Path))
	})
	return out
}

// blockingSeverity: P0, P1 or P2, as the judge wrote it.
func blockingSeverity(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "P0", "P1", "P2":
		return true
	}
	return false
}

// inTheFixFor is the earlier title that title's "(in the fix for ...)"
// names, up to its closing parenthesis, or to the end of a title the
// registry cut before it.
func inTheFixFor(title string) (string, bool) {
	_, rest, ok := strings.Cut(title, fixForMark)
	if !ok {
		return "", false
	}
	depth, end := 1, len(rest)
	for i, r := range rest {
		if r == '(' {
			depth++
		} else if r == ')' {
			if depth--; depth == 0 {
				end = i
				break
			}
		}
	}
	earlier := strings.TrimSpace(rest[:end])
	return earlier, earlier != ""
}

// titledRound is the latest round before round in which a posted finding of
// found is titled title, 0 when none is.
func titledRound(found []store.Finding, title string, round int) int {
	latest := 0
	for _, f := range found {
		if f.Verdict == store.FindingPosted && f.Round < round && strings.TrimSpace(f.Title) == title {
			latest = max(latest, f.Round)
		}
	}
	return latest
}
