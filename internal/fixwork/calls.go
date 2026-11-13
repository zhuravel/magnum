package fixwork

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/textx"
)

// needsYouEntryRe reads a "## Needs you" entry's comment id, priority and
// place ("2101 P1 app/models/offer.rb:42: <reason>"; "5485002407
// description: <reason>" for the review body's "Description: ✗" finding,
// which has the review's id and no priority; "4100 review: <reason>" and
// "4100 comment: <reason>" for another reviewer's review body or comment of
// the external file, fix_external.go), the place in plain path characters
// only, so no control character or other text of the fixer's reaches a
// message, and the words the fix skill starts the reason with: "fixed in
// <sha>." (group 5: the commit) or "not fixed." (group 6), as fix_calls.go
// reads them (ReadNeedsYouEntry). A conflict of the base merge that the
// fixer aborted has no comment id: "conflict: app/a.rb, db/schema.rb:
// <the decision it needs>" (group 4: the paths, plain path characters
// joined by ", ", up to the colon). After "not fixed." the skill's fixed
// words "The author's call" (group 7) mark a finding whose Fix left the
// choice to the author. A CI fix's job it could not fix has the job's id
// and the place `ci` ("129341926468 ci: not fixed. flaky: <reason>"), and
// after "not fixed." its class word (group 8: flaky, infra, secret, config,
// base or unfixed; skills/magnum-fix/SKILL.md, section 8). The reason is
// the fixer's own words about the PR, so it is never read.
var needsYouEntryRe = regexp.MustCompile(`^(?:(\d{1,20}) +(?:(P[0-3]) +)?([A-Za-z0-9._/@+-]+:\d{1,9}|description|review|comment|` + CIPlace + `)(?::|\s|$)` +
	`|` + ConflictPlace + `: +([A-Za-z0-9._/@+-]+(?:, *[A-Za-z0-9._/@+-]+)*)(?::|$))` +
	`(?:\s*(?i:fixed in ([0-9a-f]{7,40})|(not fixed))(?:[.,;:]|\s|$)` +
	`(?:\s*(?:(?i:(the author's call))|(flaky|infra|secret|config|base|unfixed))(?:[.,;:]|\s|$))?)?`)

// CIPlace is the place of a "## Needs you" entry for a CI fix's job
// (needsYouEntryRe).
const CIPlace = "ci"

// ConflictPlace is the place of a "## Needs you" entry for a conflict of the
// base merge (needsYouEntryRe), which has no comment id.
const ConflictPlace = "conflict"

// NeedsYouEntry is what magnum reads of a "## Needs you" entry
// (needsYouEntryRe): the finding's comment id (the review's for the
// description finding), its priority ("" when it has none), its place
// (path:line, or "description"), and what the fixer did: FixedIn is the
// commit it says fixed it ("fixed in <sha>."), NotFixed says it did not
// ("not fixed."); neither when the entry does not say. AuthorsCall: a
// finding it did not fix because its Fix leaves the choice to the author,
// or another reviewer's item that asks for a product or business-logic
// choice ("not fixed. The author's call"). A conflict of the base merge the
// fixer aborted has no id: its Place is ConflictPlace and Paths are the
// paths it names.
type NeedsYouEntry struct {
	ID, Priority, Place string
	FixedIn             string
	NotFixed            bool
	AuthorsCall         bool
	Paths               []string
}

// ReadNeedsYouEntry reads entry; ok is false when magnum cannot read its id
// and place, or a conflict's paths.
func ReadNeedsYouEntry(entry string) (NeedsYouEntry, bool) {
	m := needsYouEntryRe.FindStringSubmatch(strings.TrimSpace(entry))
	switch {
	case m == nil:
		return NeedsYouEntry{}, false
	case m[4] != "":
		var paths []string
		for p := range strings.SplitSeq(m[4], ",") {
			paths = append(paths, strings.TrimSpace(p))
		}
		return NeedsYouEntry{Place: ConflictPlace, Paths: paths}, true
	}
	return NeedsYouEntry{ID: m[1], Priority: m[2], Place: m[3], FixedIn: strings.ToLower(m[5]), NotFixed: m[6] != "", AuthorsCall: m[6] != "" && m[7] != ""}, true
}

// Name is the entry in the older words: "2101 P1 app/models/offer.rb:42",
// "conflict in app/a.rb, db/schema.rb".
func (n NeedsYouEntry) Name() string {
	switch {
	case n.Place == ConflictPlace:
		return "conflict in " + strings.Join(n.Paths, ", ")
	case n.Priority == "":
		return n.ID + " " + n.Place
	}
	return n.ID + " " + n.Priority + " " + n.Place
}

// Asks reports whether n says what the fixer did in a thread: fixed in a
// commit or not fixed, of a finding with a thread (not the description, nor
// another reviewer's review body or comment, nor a conflict of the base
// merge).
func (n NeedsYouEntry) Asks() bool {
	switch n.Place {
	case "description", ItemReview, ItemComment, ConflictPlace:
		return false
	}
	return n.FixedIn != "" || n.NotFixed
}

// fixCountRe reads the count of a fix job's message that only counts what
// the fixer left the author (needYourCall, the settle's "2 findings need
// your call: <report>"): findings or conflicts (group 1), and the conflicts
// after findings (group 2). A message that names them has "(" after "your
// call", and the older words after calls say "N more", so neither matches.
var fixCountRe = regexp.MustCompile(`\b(\d+) (?:findings?|conflicts?)(?: and (\d+) conflicts?)? needs? your call: `)

// FixNeedsAsked keeps of entries, the "## Needs you" of the fixer's report
// that msg (a fix job's message, a PR's last_error) names, those that msg
// still asks the author for. The settle and the loop's end leave out the
// entries whose magnum thread is resolved or that the fixer answered
// (freshNeedsYou, dropFixerAnswered) before they write the message, and its
// first line names each one left: by its call's link or its comment id in
// the older words (needsYouNames), a conflict by its paths, and the entries
// magnum cannot read by their count, whatever the loop's own words before
// them. A message that only counts them keeps all of entries when it counts
// them all and none otherwise, since it cannot say which went; one that
// asks for no call keeps none. The factory screen draws what it keeps.
func FixNeedsAsked(msg string, entries []string) []string {
	line := textx.FirstLine(msg)
	if !attention.FixAsks(line) {
		return nil
	}
	if m := fixCountRe.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[1])
		k, _ := strconv.Atoi(cmp.Or(m[2], "0"))
		if n+k != len(entries) {
			return nil
		}
		return slices.Clone(entries)
	}
	var kept []string
	for _, entry := range entries {
		n, ok := ReadNeedsYouEntry(entry)
		var named bool
		switch {
		case !ok:
			named = strings.Contains(line, " magnum could not read")
		case n.Place == ConflictPlace:
			named = strings.Contains(line, "conflict in "+strings.Join(n.Paths[:min(len(n.Paths), maxNamedConflictPaths)], ", "))
		default:
			named = namesNeedsYouID(line, n.ID)
		}
		if named {
			kept = append(kept, entry)
		}
	}
	return kept
}

// maxNamedConflictPaths is how many paths of a conflict both its call
// (attention.ConflictCall) and the older words name at least.
const maxNamedConflictPaths = 20

// namesNeedsYouID reports whether line, a fix job's message, names the
// entry whose comment id is id: in a call's link ("#discussion_r<id>", not
// followed by a digit) or in the older words, where an entry's name starts
// a list or follows a comma ("(<id> P1 …", ", <id> description").
func namesNeedsYouID(line, id string) bool {
	for _, before := range []string{"#discussion_r", "(", ", "} {
		for rest := line; ; {
			i := strings.Index(rest, before+id)
			if i < 0 {
				break
			}
			rest = rest[i+len(before)+len(id):]
			switch {
			case before == "#discussion_r" && (rest == "" || rest[0] < '0' || rest[0] > '9'):
				return true
			case before != "#discussion_r" && strings.HasPrefix(rest, " "):
				return true
			}
		}
	}
	return false
}

// CICannotOf reads the fixer's ci entries under Needs you (needsYouEntryRe,
// place `ci`) apart from the rest: the class word of the first (unfixed for
// one without a class) and the other entries.
func CICannotOf(needs []string) (class string, ci bool, rest []string) {
	for _, n := range needs {
		m := needsYouEntryRe.FindStringSubmatch(n)
		if m == nil || m[3] != CIPlace {
			rest = append(rest, n)
			continue
		}
		if !ci {
			class = cmp.Or(m[8], "unfixed")
		}
		ci = true
	}
	return class, ci, rest
}
