package engine

// What the findings a fixer left to the author ask of them. The fix skill
// starts each "## Needs you" entry's reason with "fixed in <sha>." for a
// finding the fixer fixed and pushed but did not answer, or "not fixed."
// for one it did not fix (skills/magnum-fix/SKILL.md, section 7); the files
// beside its report say whose thread the finding is in: the round's threads
// file lists magnum's threads as the round that posted the review found
// them, the review file that review's own comments, and the external file
// (fix_external.go) the open threads other reviewers started. A fixed
// finding in magnum's thread (its reply failed) asks the author to reply
// and resolve it; one in another reviewer's thread (its reply failed, or
// the thread was answered before, where the fixer does not write) asks for
// the reply alone, since that reviewer resolves it (one GitHub shows the
// fixer's reply in after all is no entry by then: dropFixerAnswered); a
// finding the fixer did not fix asks for the author's decision. Each is a
// call with the thread's link (attention.FixCall), and the message names the
// calls one by one with the report last (needsYouText). An entry magnum
// cannot read so (no such words, the description finding, another
// reviewer's review body or comment, a thread the files cannot place) keeps
// the older words. The fixer's reasons are never read: they stay in the
// report.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/textx"
)

// needsYouEntry is what magnum reads of a "## Needs you" entry
// (needsYouEntryRe): the finding's comment id (the review's for the
// description finding), its priority ("" when it has none), its place
// (path:line, or "description"), and what the fixer did: fixedIn is the
// commit it says fixed it ("fixed in <sha>."), notFixed says it did not
// ("not fixed."); neither when the entry does not say.
type needsYouEntry struct {
	id, priority, place string
	fixedIn             string
	notFixed            bool
}

// readNeedsYouEntry reads entry; ok is false when magnum cannot read its id
// and place.
func readNeedsYouEntry(entry string) (needsYouEntry, bool) {
	m := needsYouEntryRe.FindStringSubmatch(strings.TrimSpace(entry))
	if m == nil {
		return needsYouEntry{}, false
	}
	return needsYouEntry{id: m[1], priority: m[2], place: m[3], fixedIn: strings.ToLower(m[4]), notFixed: m[5] != ""}, true
}

// name is the entry in the older words: "2101 P1 app/models/offer.rb:42".
func (n needsYouEntry) name() string {
	if n.priority == "" {
		return n.id + " " + n.place
	}
	return n.id + " " + n.priority + " " + n.place
}

// threadOwners says whose the threads of the review a fix report answers
// are, from the files beside the report: magnums are the first comments of
// magnum's threads (the threads file), listed whether that file was read
// (then a thread neither in it nor of the review's own comments is another
// reviewer's), comments the review's own comments (the review file), whose
// threads the threads file predates, and others the first comments of the
// open threads other reviewers started (the external file).
type threadOwners struct {
	magnums  map[int64]bool
	listed   bool
	comments []ownComment
	others   map[int64]bool
}

// ownComment is a comment of the review file: a finding of magnum's review.
type ownComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// readThreadOwners reads the threads file and the review file in dir; a
// file that is missing or cannot be read says nothing.
func readThreadOwners(dir string) threadOwners {
	o := threadOwners{magnums: map[int64]bool{}, others: map[int64]bool{}}
	if x, ok := readExternalFile(dir); ok {
		for _, t := range x.Threads {
			o.others[t.CommentID] = true
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, pipeline.ThreadsFile)); err == nil {
		var threads []agents.ReviewThread
		if json.Unmarshal(b, &threads) == nil {
			o.listed = true
			for _, t := range threads {
				o.magnums[t.CommentID] = true
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, agents.PostReviewFile)); err == nil {
		var review struct {
			Comments []ownComment `json:"comments"`
		}
		if json.Unmarshal(b, &review) == nil {
			o.comments = review.Comments
		}
	}
	return o
}

// owner says whether n's finding is in magnum's thread (magnums) and whether
// the files can tell (known): a thread the external file lists is another
// reviewer's; a thread the threads file lists, or a comment of the review
// at n's place whose priority is n's, is magnum's; any other is another
// reviewer's once the threads file was read, and unknown without it.
func (o threadOwners) owner(n needsYouEntry) (magnums, known bool) {
	id, err := strconv.ParseInt(n.id, 10, 64)
	if err == nil && o.others[id] {
		return false, true
	}
	if err == nil && o.magnums[id] {
		return true, true
	}
	if i := strings.LastIndexByte(n.place, ':'); i > 0 {
		path, line := n.place[:i], n.place[i+1:]
		if slices.ContainsFunc(o.comments, func(c ownComment) bool {
			return c.Path == path && strconv.Itoa(c.Line) == line && (n.priority == "" || strings.HasPrefix(c.Body, "**["+n.priority+"]"))
		}) {
			return true, true
		}
	}
	return false, o.listed
}

// call is n's call (ok false when magnum cannot read one: the entry does
// not say what the fixer did, names the description or another reviewer's
// review body or comment, which have no thread, or its thread cannot be
// placed): the thread's link on the PR's page prURL, and `magnum fix ref`
// to send a declined finding back to the fixer.
func (n needsYouEntry) call(o threadOwners, prURL, ref string) (attention.FixCall, bool) {
	switch n.place {
	case "description", remarkReview, remarkComment:
		return attention.FixCall{}, false
	}
	if (n.fixedIn == "" && !n.notFixed) || prURL == "" {
		return attention.FixCall{}, false
	}
	magnums, known := o.owner(n)
	if !known {
		return attention.FixCall{}, false
	}
	url := prURL + "#discussion_r" + n.id
	if n.fixedIn != "" {
		return attention.FixedCall(n.id, url, n.fixedIn, magnums), true
	}
	return attention.DeclinedCall(n.id, url, n.priority, n.place, ref, magnums), true
}

// needsYouCalls are the calls of the entries magnum can read so (call),
// with the files beside report, and the older words for the rest ("2 more
// findings need your call (2101 P1 app/models/offer.rb:42, 1 entry magnum
// could not read)"; "" when none is left).
func needsYouCalls(entries []string, report, prURL, ref string) ([]attention.FixCall, string) {
	owners := readThreadOwners(filepath.Dir(report))
	var calls []attention.FixCall
	var left []string
	for _, entry := range entries {
		n, ok := readNeedsYouEntry(entry)
		if ok {
			if c, ok := n.call(owners, prURL, ref); ok {
				calls = append(calls, c)
				continue
			}
		}
		left = append(left, entry)
	}
	if len(left) == 0 {
		return calls, ""
	}
	return calls, fmt.Sprintf("%d more %s %s your call (%s)", len(left), textx.Plural(len(left), "finding", "findings"),
		textx.Plural(len(left), "needs", "need"), strings.Join(needsYouNames(left), ", "))
}

// needsYouText says, after "fix: ", what the findings a fixer left to the
// author (entries, its report's "## Needs you") ask of them: a call per
// finding magnum can read so (needsYouCalls), the rest in the older words,
// and the report last ("details: <report>"), joined by "; "
// (attention.FixCallsText); prURL is the PR's page and ref how `magnum fix`
// names it. When magnum can read no call, the older words alone: each
// finding named (needsYourCall) when named, else counted ("2 findings need
// your call: <report>", the settle's).
func needsYouText(entries []string, report, prURL, ref string, named bool) string {
	calls, more := needsYouCalls(entries, report, prURL, ref)
	switch {
	case len(calls) > 0:
		return attention.FixCallsText(calls, more, report)
	case named:
		return needsYourCall(entries, report)
	}
	n := len(entries)
	return fmt.Sprintf("%s %s your call: %s", textx.Count(n, "finding", "findings"), textx.Plural(n, "needs", "need"), report)
}

// FixCalls are the calls of the findings the fixer of the loop st left to
// the author when it pushed (FixState.NeedsYou, kept while the loop waits
// for the re-review of its push), as the loop's end names them: the files
// beside its report place their threads, prURL is the PR's page and ref how
// `magnum fix` names it. None for entries magnum cannot read so.
func FixCalls(st FixState, prURL, ref string) []attention.FixCall {
	calls, _ := needsYouCalls(st.NeedsYou, st.Report, prURL, ref)
	return calls
}

// attentionToast is the body of an attention toast for r: its summary, then,
// for a fix job's calls, each call and the findings in the older words on a
// line of its own (Steps without the report, which the card names).
func attentionToast(r attention.Reason) string {
	if len(r.NeedsYou) == 0 {
		return r.Summary
	}
	lines := []string{r.Summary}
	for _, s := range r.Steps {
		if !strings.HasPrefix(s, "details: ") {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}
