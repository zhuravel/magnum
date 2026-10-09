package tui

// The PR board's views (v cycles them), the filter's qualifiers and the
// last round's stage timings the card shows.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/textx"
)

// PRView is a preset subset of the board's rows.
type PRView string

// The board's views, in the order v cycles through them.
const (
	ViewAll    PRView = "all"    // every row
	ViewMagnum PRView = "magnum" // rows magnum reviewed or is reviewing (state not baseline or ineligible), or whose review is requested from a self login
	ViewMine   PRView = "mine"   // assigned to one of the self logins, or their review is requested
	ViewReady  PRView = "ready"  // open, not a draft, approved on the head, nothing blocking, required checks passed
)

var prViewOrder = []PRView{ViewAll, ViewMagnum, ViewMine, ViewReady}

// PRViews lists the views in the order the v key cycles through them.
func PRViews() []PRView { return slices.Clone(prViewOrder) }

// ParsePRView reads a view name ("all", "magnum", "mine", "ready"; case
// does not matter); empty means ViewAll.
func ParsePRView(s string) (PRView, error) {
	n := PRView(strings.ToLower(strings.TrimSpace(s)))
	if n == "" {
		return ViewAll, nil
	}
	if n.valid() {
		return n, nil
	}
	names := make([]string, len(prViewOrder))
	for i, v := range prViewOrder {
		names[i] = string(v)
	}
	return "", fmt.Errorf("unknown view %q (want %s)", s, strings.Join(names, ", "))
}

func (v PRView) valid() bool { return slices.Contains(prViewOrder, v) }

func (v PRView) next() PRView {
	i := slices.Index(prViewOrder, v)
	return prViewOrder[(i+1)%len(prViewOrder)]
}

// has reports whether r belongs to the view; self holds the normalized
// self logins (normLogin).
func (v PRView) has(r PRBoardRow, self map[string]bool) bool {
	switch v {
	case ViewMagnum:
		switch normState(r.State) {
		case "", "baseline", "ineligible":
			return r.CodexFlag != "" || reviewRequestedFrom(r, self)
		}
		return true
	case ViewMine:
		return slices.ContainsFunc(r.Assignees, func(a string) bool { return self[textx.FoldLogin(a)] }) || reviewRequestedFrom(r, self)
	case ViewReady:
		return readyToMerge(r)
	}
	return true
}

// readyToMerge reports whether r looks ready to merge: open and not a
// draft; approved by someone on the current head (an approval of an older
// commit does not count) and nobody's latest verdict is changes requested
// (stale or not: GitHub keeps blocking on it); magnum's latest review is
// not blocking; and every required check passed. A skipped required check
// does not count as passed although GitHub accepts it (a skipped
// aggregator hides a cancelled or failed dependency), nor does one that
// never ran. Without required checks CI does not decide: optional checks
// are often noisy (advisory audits, statuses stuck pending).
func readyToMerge(r PRBoardRow) bool {
	if r.Draft || !isOpen(r) {
		return false
	}
	approved := false
	for _, rv := range r.Reviewers {
		switch normVerdict(rv.Verdict) {
		case "changes_requested":
			return false
		case "approved":
			approved = approved || !rv.Stale
		}
	}
	if !approved || (r.Findings != nil && r.Findings.Verdict == "blocking") {
		return false
	}
	return r.CI == nil || !slices.ContainsFunc(r.CI.Required, func(c CheckState) bool { return normCI(c.State) != "passed" })
}

// reviewRequestedFrom reports whether a review of r is requested from one
// of the self logins.
func reviewRequestedFrom(r PRBoardRow, self map[string]bool) bool {
	return slices.ContainsFunc(r.Reviewers, func(rv ReviewerInfo) bool { return rv.Requested && (rv.Mine || self[textx.FoldLogin(rv.Login)]) })
}

// FilterPRBoard returns the rows of rows in view v; selfLogins are the
// logins that count as "me" (PRBoardOptions.SelfLogins).
func FilterPRBoard(rows []PRBoardRow, v PRView, selfLogins []string) []PRBoardRow {
	if v == ViewAll || !v.valid() {
		return slices.Clone(rows)
	}
	self := selfSet(selfLogins)
	out := make([]PRBoardRow, 0, len(rows))
	for _, r := range rows {
		if v.has(r, self) {
			out = append(out, r)
		}
	}
	return out
}

// prQuery is a parsed filter: free-text words that must each fuzzily match
// one of the row's texts, and qualifiers that must each hold.
type prQuery struct {
	words []string
	quals []func(PRBoardRow, map[string]bool) bool
}

// prQualKeys are the filter's qualifiers; any other "key:value" word is
// free text (titles often carry a colon).
var prQualKeys = []string{"state", "assignee", "author", "review"}

// parsePRQuery splits q into words and qualifiers: state:<s> (a state or
// its label, a prefix is enough), assignee:<login> and author:<login> (a
// part of the login, @me for the self logins) and review:requested (a
// review is requested from a self login). A qualifier's value may list
// alternatives separated by commas; an empty value matches every row.
func parsePRQuery(q string) prQuery {
	var out prQuery
	for w := range strings.FieldsSeq(strings.ToLower(q)) {
		key, val, ok := strings.Cut(w, ":")
		if !ok || !slices.Contains(prQualKeys, key) {
			out.words = append(out.words, w)
			continue
		}
		var alts []string
		for a := range strings.SplitSeq(val, ",") {
			if a = strings.TrimSpace(a); a != "" {
				alts = append(alts, a)
			}
		}
		if len(alts) == 0 {
			continue
		}
		var one func(PRBoardRow, map[string]bool, string) bool
		switch key {
		case "state":
			one = func(r PRBoardRow, _ map[string]bool, a string) bool {
				a = normState(a)
				return strings.HasPrefix(normState(r.State), a) || strings.HasPrefix(normState(stateLabel(r.State)), a)
			}
		case "assignee":
			one = func(r PRBoardRow, self map[string]bool, a string) bool {
				return slices.ContainsFunc(r.Assignees, func(l string) bool { return loginMatches(l, a, self) })
			}
		case "author":
			one = func(r PRBoardRow, self map[string]bool, a string) bool { return loginMatches(r.Author, a, self) }
		case "review":
			one = func(r PRBoardRow, self map[string]bool, a string) bool {
				return strings.HasPrefix("requested", a) && reviewRequestedFrom(r, self)
			}
		}
		out.quals = append(out.quals, func(r PRBoardRow, self map[string]bool) bool {
			return slices.ContainsFunc(alts, func(a string) bool { return one(r, self, a) })
		})
	}
	return out
}

// loginMatches reports whether login contains want ("@me": is one of the
// self logins); case, "@" and "[bot]" do not matter.
func loginMatches(login, want string, self map[string]bool) bool {
	n := textx.FoldLogin(login)
	if want == "@me" {
		return n != "" && self[n]
	}
	w := textx.FoldLogin(want)
	return n != "" && w != "" && strings.Contains(n, w)
}

// match reports whether r satisfies every qualifier and every word fuzzily
// matches (its letters in order, close together) the row's ref, title,
// author, a reviewer, a label or a badge (its label or its text).
func (q prQuery) match(r PRBoardRow, self map[string]bool) bool {
	for _, f := range q.quals {
		if !f(r, self) {
			return false
		}
	}
	if len(q.words) == 0 {
		return true
	}
	fields := []string{prRef(r), r.Title, r.Author}
	if owner, repo, n := prRefParts(r); repo != "" {
		fields = append(fields, fmt.Sprintf("%s/%s#%d", owner, repo, n))
	}
	for _, v := range r.Reviewers {
		fields = append(fields, v.Login)
	}
	fields = append(fields, r.Labels...)
	for _, b := range r.Badges {
		fields = append(fields, b.Label, b.Text)
	}
	for i := range fields {
		fields[i] = strings.ToLower(fields[i])
	}
	for _, t := range q.words {
		if !slices.ContainsFunc(fields, func(f string) bool { return fuzzyContains(f, t) }) {
			return false
		}
	}
	return true
}

// RoundTimings is how long each stage of a PR's last review round took,
// from the registry's runs and step events.
type RoundTimings struct {
	Round   int
	Kind    string        // the round's kind: initial, rereview, continue, recovery
	Stages  []StageTiming // checkout, then each role in the order it started, then verify
	Total   time.Duration // the first stage's start to the last one's end (to now while running)
	Running bool          // the round is still in flight
}

// StageTiming is one stage of a round.
type StageTiming struct {
	Name     string        // "fetch/checkout", a role's name, the judge's own pass ("codex-judge own pass"), "verify"
	Duration time.Duration // to now while Running
	Running  bool
	Failed   bool
}

// StageDuration renders a stage's length: "12s", "18m04s", "1h02m".
func StageDuration(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// TimingsText is t on one line: "fetch/checkout 12s · claude-review 18m04s
// · … · total 34m10s", a running stage marked "(running)", a failed one
// "(failed)".
func TimingsText(t RoundTimings) string {
	parts := make([]string, 0, len(t.Stages)+1)
	for _, s := range t.Stages {
		parts = append(parts, s.Name+" "+StageDuration(s.Duration)+stageNote(s.Running, s.Failed))
	}
	parts = append(parts, "total "+StageDuration(t.Total)+stageNote(t.Running, false))
	return strings.Join(parts, " · ")
}

func stageNote(running, failed bool) string {
	switch {
	case running:
		return " (running)"
	case failed:
		return " (failed)"
	}
	return ""
}
