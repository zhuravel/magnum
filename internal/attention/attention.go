// Package attention explains why a PR needs the user. The engine stores the
// error that sent a PR to needs_attention as it came: often a chain of
// contexts ending in a command's whole output (a 300-line Ruby build log, a
// git transcript), whose first line says nothing useful. Explain turns that
// text into a stage, the one line of the output that names the cause, a
// one-line summary and the next step, so every screen can say why in a line
// and what to do in another. It is pure text work, so rows written before it
// existed are explained too.
package attention

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/textx"
)

// Kinds: why the engine parked the PR (engine needsAttention's why).
const (
	KindFailed        = "failed"          // setup failed maxAttempts times on one head
	KindOverloaded    = "overloaded"      // the agent API stayed overloaded
	KindBlocked       = "blocked"         // the judge reported a blocker or waits on a dialog
	KindNoReview      = "needs_attention" // the judge posted nothing after a nudge, or its result was inconsistent
	KindIdentityError = "identity_error"  // the judge's identity check failed
	KindIdentityLeak  = "identity_leak"   // a review with the run's marker came from another login
)

// Stages: where the failure happened.
const (
	StageFetch        = "fetch"
	StageWorktree     = "worktree"
	StageCheckout     = "checkout"
	StageDependencies = "dependencies"
	StageAgents       = "agents"
	StageJudge        = "judge"
	StageIdentity     = "identity"
	StageReview       = "review"
)

// SummaryMax bounds Summary and Cause (runes).
const SummaryMax = 160

// Reason is why a PR needs the user, in screen-sized parts.
type Reason struct {
	Kind     string `json:"kind,omitempty"`
	Stage    string `json:"stage"`
	Attempts int    `json:"attempts,omitempty"`
	Head     string `json:"head,omitempty"` // the short sha the attempts were on
	// Step is the configured step that failed (a dependency command), "" when
	// not one.
	Step string `json:"step,omitempty"`
	// Cause is the line of the error that names what went wrong.
	Cause string `json:"cause"`
	// Summary is one line: the stage, the attempts and the cause.
	Summary string `json:"summary"`
	// Fix is the next step, with the PR's reference filled in.
	Fix string `json:"fix"`
	// Tail is the end of the failing command's output, cleaned (at most
	// TailLines lines), for a detail view; empty when there was none.
	Tail []string `json:"tail,omitempty"`
}

// TailLines bounds Reason.Tail.
const TailLines = 8

var (
	attemptsRe = regexp.MustCompile(`^(\d+) attempts on ([0-9a-f]{7,40}): `)
	exitedRe   = regexp.MustCompile(` exited (\d+): `)
	depsStepRe = regexp.MustCompile(`deps [^:]+: (.+?): (?:mise -C|/bin/sh|env )`)
	ansiRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	// errorLineRe: "configure: error: …", "fatal: …", "ERROR: …", "Error: …".
	errorLineRe = regexp.MustCompile(`(?i)(^|[\s:])(fatal|error)\s*:\s*\S`)
	// failLineRe: lines that name a failure without the error: label.
	failLineRe = regexp.MustCompile(`(?i)\b(denied|not found|cannot|can't|could not|couldn't|failed|refused|timed out|no such|unable to|missing)\b`)
	// noiseRe: lines that are never the cause.
	noiseRe    = regexp.MustCompile(`(?i)(run with --verbose|mise_verbose|^mise error version:|you can inspect the build directory|^please make sure you have the correct access|and the repository exists\.?$|^external command failed|[░█]{3,}|^\s*\d+\s+[\d.]+[kmg]?\s+\d+\s|% total\s+% received)`)
	toolRe     = regexp.MustCompile(`Failed to install (?:core:)?([A-Za-z0-9_-]+)@`)
	toolVerRe  = regexp.MustCompile(`(?m)^mise ([A-Za-z0-9_-]+)@([0-9][0-9A-Za-z.\-]*) `)
	jemallocRe = regexp.MustCompile(`jemalloc requested but not found`)
	sshKeyRe   = regexp.MustCompile(`Permission denied \(publickey\)`)
)

// Explain reads why a PR needs the user from the stored error msg (the PR's
// last_error) and kind (the engine's why, "" when unknown: then inferred).
// ref is how the user names the PR (talkable#9992), used in Fix.
func Explain(kind, msg, ref string) Reason {
	text := strings.ReplaceAll(strings.ReplaceAll(msg, "\r\n", "\n"), "\r", "\n")
	text = ansiRe.ReplaceAllString(text, "")
	r := Reason{Kind: kind}
	if m := attemptsRe.FindStringSubmatch(text); m != nil {
		r.Attempts, _ = strconv.Atoi(m[1])
		r.Head = m[2][:min(7, len(m[2]))]
		text = text[len(m[0]):]
		if r.Kind == "" {
			r.Kind = KindFailed
		}
	}
	// "identity_error: …": the engine's event form, and older rows, lead with the kind.
	for _, k := range []string{KindFailed, KindOverloaded, KindBlocked, KindNoReview, KindIdentityError, KindIdentityLeak} {
		if rest, ok := strings.CutPrefix(text, k+": "); ok {
			text = rest
			if r.Kind == "" {
				r.Kind = k
			}
			break
		}
	}
	chain, output := text, ""
	if loc := exitedRe.FindStringIndex(text); loc != nil {
		chain, output = text[:loc[0]], text[loc[1]:]
	} else if i := strings.IndexByte(text, '\n'); i >= 0 {
		chain, output = text[:i], text[i+1:]
	}
	if r.Kind == "" {
		r.Kind = inferKind(chain)
	}
	r.Stage = stageOf(r.Kind, chain)
	if m := depsStepRe.FindStringSubmatch(chain); m != nil {
		r.Step = m[1]
	}
	lines := cleanLines(output)
	r.Tail = lines[max(0, len(lines)-TailLines):]
	r.Cause, r.Fix = cause(r, chain, output, lines, ref)
	r.Cause = clip(r.Cause)
	r.Summary = clip(summary(r))
	return r
}

// inferKind guesses the engine's why from the message of an older row.
func inferKind(chain string) string {
	l := strings.ToLower(chain)
	switch {
	case strings.Contains(l, "identity check failed"):
		return KindIdentityError
	case strings.Contains(l, "carrying the run's marker was posted as"):
		return KindIdentityLeak
	case strings.Contains(l, "blocked"):
		return KindBlocked
	case strings.Contains(l, "overloaded"), strings.Contains(l, "rate limit"):
		return KindOverloaded
	case strings.Contains(l, "checkout in"), strings.Contains(l, "per-pr worktree"), strings.Contains(l, "gitx:"),
		strings.Contains(l, "slots:"), strings.Contains(l, "deps "), strings.Contains(l, "workspace:"):
		return KindFailed
	}
	return KindNoReview
}

// stageOf places the failure.
func stageOf(kind, chain string) string {
	switch kind {
	case KindIdentityError, KindIdentityLeak:
		return StageIdentity
	case KindBlocked:
		return StageJudge
	case KindNoReview, KindOverloaded:
		return StageReview
	}
	l := strings.ToLower(chain)
	switch {
	case strings.Contains(l, "deps "), strings.Contains(l, "bundle install"), strings.Contains(l, "pnpm install"),
		strings.Contains(l, "yarn install"), strings.Contains(l, "npm install"):
		return StageDependencies
	case strings.Contains(l, "fetch pr"), strings.Contains(l, "git fetch"), strings.Contains(l, " fetch --no-tags"):
		return StageFetch
	case strings.Contains(l, "per-pr worktree"), strings.Contains(l, "worktree add"):
		return StageWorktree
	case strings.Contains(l, "workspace"), strings.Contains(l, "agents:"), strings.Contains(l, "preflight"):
		return StageAgents
	}
	return StageCheckout
}

// cause finds the line that names what went wrong and the fix for it.
func cause(r Reason, chain, output string, lines []string, ref string) (string, string) {
	retry := fmt.Sprintf("`magnum review %s`", ref)
	generic := "fix the cause, then " + retry + " (or `magnum ignore " + ref + "` to stop reviewing it)"
	switch r.Kind {
	case KindBlocked, KindNoReview, KindOverloaded:
		c := strings.TrimSpace(strings.TrimPrefix(textx.FirstLine(chain), "judge blocked:"))
		fix := "`magnum open " + ref + "` shows the judge's pane; fix what it reports, then " + retry
		if r.Kind == KindOverloaded {
			fix = "the agent's API stayed overloaded; " + retry + " retries"
		}
		return c, fix
	case KindIdentityError:
		return strings.TrimSpace(strings.TrimPrefix(textx.FirstLine(chain), "judge identity check failed:")),
			"`magnum identities check` shows what fails; then " + retry
	case KindIdentityLeak:
		return textx.FirstLine(chain), "check that review on GitHub and the identity's token; automation for the watch is paused until `magnum resume`"
	}

	all := chain + "\n" + output
	if sshKeyRe.MatchString(all) {
		return "GitHub refused the SSH key (Permission denied (publickey)): the SSH agent served no key",
			"magnum fetches over HTTPS through gh now; " + retry + " retries"
	}
	if m := toolRe.FindStringSubmatch(output); m != nil {
		tool := m[1]
		ver := tool
		if v := toolVerRe.FindStringSubmatch(output); v != nil && v[1] == tool {
			ver = tool + "@" + v[2]
		}
		why := salient(lines)
		c := "mise could not install " + ver + ", which the PR pins"
		if why != "" {
			c += ": " + why
		}
		fix := "make `mise install " + ver + "` work (in any slot), then " + retry + "; `magnum ignore " + ref + "` if the PR is stale"
		if jemallocRe.MatchString(output) {
			fix = "`brew install jemalloc` (mise builds Ruby with jemalloc), then " + retry + "; `magnum ignore " + ref + "` if the PR is stale"
		}
		return c, fix
	}
	if c := salient(lines); c != "" {
		return c, generic
	}
	return lastSegment(chain), generic
}

// salient is the line of a command's output that names the failure: the first
// "error:"/"fatal:" line, else the first line naming a failure, else the last
// line; noise (progress, hints, version banners) never counts.
func salient(lines []string) string {
	for _, l := range lines {
		if errorLineRe.MatchString(l) {
			return l
		}
	}
	for _, l := range lines {
		if failLineRe.MatchString(l) {
			return l
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return ""
}

// cleanLines splits a command's output into trimmed, non-blank lines with
// the noise removed; a tool's "mise ruby@3.4.11 " prefix is dropped.
func cleanLines(output string) []string {
	var out []string
	for _, l := range strings.Split(output, "\n") {
		l = strings.TrimSpace(l)
		if m := toolVerRe.FindStringSubmatch(l + " "); m != nil && strings.HasPrefix(l+" ", m[0]) {
			l = strings.TrimSpace(l[min(len(m[0]), len(l)):])
		}
		if l == "" || noiseRe.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// summary is the one-line why.
func summary(r Reason) string {
	switch r.Kind {
	case KindBlocked:
		return "judge blocked: " + r.Cause
	case KindNoReview:
		return r.Cause // the pipeline's own sentence
	case KindOverloaded:
		return "agent API overloaded: " + r.Cause
	case KindIdentityError:
		return "identity check failed: " + r.Cause
	case KindIdentityLeak:
		return "identity leak: " + r.Cause
	}
	s := r.Stage + " failed"
	if l := stepLabel(r.Step); l != "" {
		s = r.Stage + " (" + l + ") failed"
	}
	if r.Attempts > 0 {
		s += fmt.Sprintf(" %d× on %s", r.Attempts, r.Head)
	}
	return s + ": " + r.Cause
}

// stepLabel names a step command by its last alternative's first two words:
// "bundle check >/dev/null || bundle install --jobs 4" → "bundle install".
func stepLabel(step string) string {
	parts := strings.FieldsFunc(step, func(r rune) bool { return r == '|' || r == '&' || r == ';' })
	if len(parts) == 0 {
		return ""
	}
	f := strings.Fields(parts[len(parts)-1])
	return strings.Join(f[:min(2, len(f))], " ")
}

// lastSegment is the innermost context of an error chain ("a: b: c" → "c").
func lastSegment(chain string) string {
	chain = textx.FirstLine(chain)
	if i := strings.LastIndex(chain, ": "); i >= 0 && i+2 < len(chain) {
		return chain[i+2:]
	}
	return chain
}

// clip collapses s to one line of single spaces, at most SummaryMax runes.
func clip(s string) string {
	return textx.Clip(strings.Join(strings.Fields(s), " "), SummaryMax)
}
