package factory

// The factory's own reading of a local round (spec 6.5): what blocks, what
// is review unavailable, and how a finding is followed across rounds. It is
// magnum's rule (internal/engine/autoapprove.go's roundBlocks) in a
// function of the factory's, and it never reads the planned event: a dry
// run's event is the identity's, not a verdict.

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// Blocks reads local round r: blocks when a P0, P1 or P2 finding is new
// (Counts) or still open (OpenCounts), or the judge's verdict is
// "blocking"; unavailable when the round did not finish as a dry run
// (needs_attention, blocked, error, timeout, a refusal, a stop, no outcome
// at all) or its result does not parse (no verdict): review unavailable,
// never clean, whatever the result says (Review Focus 1). why says which,
// in plain words that carry no finding's text ("" when nothing blocks).
func Blocks(r evalrun.LocalResult) (blocks, unavailable bool, why string) {
	switch {
	case r.Outcome != pipeline.OutcomeDryRun:
		return false, true, "review unavailable (" + cmp.Or(r.Outcome, "no outcome") + ")"
	case r.Verdict == "":
		return false, true, "review unavailable (no result that parses)"
	}
	n := r.Counts[0] + r.Counts[1] + r.Counts[2]
	if r.OpenCounts != nil {
		n += r.OpenCounts[0] + r.OpenCounts[1] + r.OpenCounts[2]
	}
	switch {
	case n > 0:
		return true, false, plural(n, "blocking finding")
	case r.Verdict == store.VerdictBlocking:
		return true, false, "the judge's verdict is blocking"
	}
	return false, false, ""
}

// blockingSeverity reports whether a finding of severity sev blocks: P0 to
// P2.
func blockingSeverity(sev string) bool { return sev == "P0" || sev == "P1" || sev == "P2" }

// fingerprintWindow is the window of lines a finding's line is rounded to.
const fingerprintWindow = 10

// titleNoise is what a finding's title carries that is not the finding: its
// priority, its bold, and the suffixes the skill adds.
var (
	titlePriority = regexp.MustCompile(`^\[?p[0-3]\]?:?\s*`)
	titleSuffix   = regexp.MustCompile(`\s*\((optional, no reply needed|missed earlier|in the fix for [^)]*)\)\s*$`)
)

// optionalSuffix is the suffix the judge adds to a P3: "(optional, no reply
// needed)".
var optionalSuffix = regexp.MustCompile(`\s*\(optional, no reply needed\)\s*$`)

// normTitle is a finding's title as its fingerprint hashes it: lowercase,
// without bold, priority and the skill's suffixes, its spaces collapsed.
func normTitle(title string) string {
	s := strings.ToLower(strings.ReplaceAll(title, "*", ""))
	s = strings.Join(strings.Fields(s), " ")
	s = titlePriority.ReplaceAllString(s, "")
	for {
		t := titleSuffix.ReplaceAllString(s, "")
		if t == s {
			return strings.TrimSpace(s)
		}
		s = t
	}
}

// titleHash is the hash of a finding's title without its priority (12 hex
// digits of its sha256).
func titleHash(title string) string {
	sum := sha256.Sum256([]byte(normTitle(title)))
	return hex.EncodeToString(sum[:])[:12]
}

// Fingerprint is how the factory follows a finding across rounds: its path,
// its line rounded down to a window of 10, and a hash of its title without
// its priority ("app/credit.rb:40:1a2b3c4d5e6f"). A finding re-ranked, or
// moved within its window, is the same one.
func Fingerprint(f evalrun.LocalFinding) string {
	return fmt.Sprintf("%s:%d:%s", f.Path, f.Line/fingerprintWindow*fingerprintWindow, titleHash(f.Title))
}
