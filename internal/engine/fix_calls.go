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
// finding the fixer did not fix asks for the author's decision. A conflict
// of the base merge that the fixer aborted ("conflict: <paths>: …", no id)
// asks the author to merge the base by hand on the PR's page and decide it
// (attention.ConflictCall). Each is a
// call with the thread's link (attention.FixCall), and the message names the
// calls one by one with the report last (needsYouText). An entry magnum
// cannot read so (no such words, the description finding, another
// reviewer's review body or comment, a thread the files cannot place) keeps
// the older words. The fixer's reasons are never read: they stay in the
// report. Before the settle and the loop's end raise the calls, magnum's own
// threads are read again (freshNeedsYou): a resolved one asks nothing, and
// an open one with the answer its call asks for asks only for the
// resolution (attention.ResolveCall).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/textx"
)

// needsYouEntry is what magnum reads of a "## Needs you" entry
// (needsYouEntryRe): the finding's comment id (the review's for the
// description finding), its priority ("" when it has none), its place
// (path:line, or "description"), and what the fixer did: fixedIn is the
// commit it says fixed it ("fixed in <sha>."), notFixed says it did not
// ("not fixed."); neither when the entry does not say. A conflict of the
// base merge the fixer aborted has no id: its place is conflictPlace and
// paths are the paths it names.
type needsYouEntry struct {
	id, priority, place string
	fixedIn             string
	notFixed            bool
	paths               []string
}

// readNeedsYouEntry reads entry; ok is false when magnum cannot read its id
// and place, or a conflict's paths.
func readNeedsYouEntry(entry string) (needsYouEntry, bool) {
	m := needsYouEntryRe.FindStringSubmatch(strings.TrimSpace(entry))
	switch {
	case m == nil:
		return needsYouEntry{}, false
	case m[4] != "":
		var paths []string
		for p := range strings.SplitSeq(m[4], ",") {
			paths = append(paths, strings.TrimSpace(p))
		}
		return needsYouEntry{place: conflictPlace, paths: paths}, true
	}
	return needsYouEntry{id: m[1], priority: m[2], place: m[3], fixedIn: strings.ToLower(m[5]), notFixed: m[6] != ""}, true
}

// name is the entry in the older words: "2101 P1 app/models/offer.rb:42",
// "conflict in app/a.rb, db/schema.rb".
func (n needsYouEntry) name() string {
	switch {
	case n.place == conflictPlace:
		return "conflict in " + strings.Join(n.paths, ", ")
	case n.priority == "":
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

// asks reports whether n says what the fixer did in a thread: fixed in a
// commit or not fixed, of a finding with a thread (not the description, nor
// another reviewer's review body or comment, nor a conflict of the base
// merge).
func (n needsYouEntry) asks() bool {
	switch n.place {
	case "description", remarkReview, remarkComment, conflictPlace:
		return false
	}
	return n.fixedIn != "" || n.notFixed
}

// call is n's call (ok false when magnum cannot read one: the entry does
// not say what the fixer did, names the description or another reviewer's
// review body or comment, which have no thread, or its thread cannot be
// placed): the thread's link on the PR's page prURL, and `magnum fix ref`
// to send a declined finding back to the fixer. answered (one of magnum's
// threads that shows its answer, freshNeedsYou) asks only to resolve it. A
// conflict of the base merge asks the author to merge the base on the PR's
// page and decide the conflict (attention.ConflictCall).
func (n needsYouEntry) call(o threadOwners, prURL, ref string, answered bool) (attention.FixCall, bool) {
	if n.place == conflictPlace && prURL != "" {
		return attention.ConflictCall(n.paths, prURL), true
	}
	if !n.asks() || prURL == "" {
		return attention.FixCall{}, false
	}
	magnums, known := o.owner(n)
	if !known {
		return attention.FixCall{}, false
	}
	url := prURL + "#discussion_r" + n.id
	switch {
	case magnums && answered:
		return attention.ResolveCall(n.id, url), true
	case n.fixedIn != "":
		return attention.FixedCall(n.id, url, n.fixedIn, magnums), true
	}
	return attention.DeclinedCall(n.id, url, n.priority, n.place, ref, magnums), true
}

// needsYouCalls are the calls of the entries magnum can read so (call),
// with the files beside report and the threads answered names by comment id
// (freshNeedsYou; nil for none), and the older words for the rest ("2 more
// findings need your call (2101 P1 app/models/offer.rb:42, 1 entry magnum
// could not read)"; "" when none is left).
func needsYouCalls(entries []string, answered map[string]bool, report, prURL, ref string) ([]attention.FixCall, string) {
	owners := readThreadOwners(filepath.Dir(report))
	var calls []attention.FixCall
	var left []string
	for _, entry := range entries {
		n, ok := readNeedsYouEntry(entry)
		if ok {
			if c, ok := n.call(owners, prURL, ref, answered[n.id]); ok {
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
// author (entries, its report's "## Needs you", as freshNeedsYou left them,
// with the threads it found answered) ask of them: a call per finding
// magnum can read so (needsYouCalls), the rest in the older words, and the
// report last ("details: <report>"), joined by "; "
// (attention.FixCallsText); prURL is the PR's page and ref how `magnum fix`
// names it. When magnum can read no call, the older words alone: each
// finding named (needsYourCall) when named, else counted ("2 findings need
// your call: <report>", the settle's; a conflict counts as one,
// needYourCall).
func needsYouText(entries []string, answered map[string]bool, report, prURL, ref string, named bool) string {
	calls, more := needsYouCalls(entries, answered, report, prURL, ref)
	switch {
	case len(calls) > 0:
		return attention.FixCallsText(calls, more, report)
	case named:
		return needsYourCall(entries, report)
	}
	return needYourCall(entries) + ": " + report
}

// FixCalls are the calls of the findings the fixer of the loop st left to
// the author when it pushed (FixState.NeedsYou, kept while the loop waits
// for the re-review of its push), as the loop's end names them: the files
// beside its report place their threads, prURL is the PR's page and ref how
// `magnum fix` names it, and a thread the settle found answered
// (FixState.Answered) asks only for the resolution, as the settle's toast
// said: no thread is read again here. None for entries magnum cannot read
// so.
func FixCalls(st FixState, prURL, ref string) []attention.FixCall {
	calls, _ := needsYouCalls(st.NeedsYou, st.answered(), st.Report, prURL, ref)
	return calls
}

// answeredBy reports whether a thread whose answer has class (answerOf)
// answers what n's call asks: "Fixed in" a fix, "Won't fix" or "Not a bug"
// a decline.
func (n needsYouEntry) answeredBy(class string) bool {
	switch class {
	case pipeline.ReplyFixed:
		return n.fixedIn != ""
	case pipeline.ReplyWontFix, pipeline.ReplyNotABug:
		return n.notFixed
	}
	return false
}

// threadsReader reads the review threads of the job's PR from GitHub once,
// however often it is called, so the settle's looks at them (the author's
// replies without the fix marker, nameUnmarkedReplies; the fixer's answers
// in other reviewers' threads, dropFixerAnswered; magnum's own threads,
// freshNeedsYou) share one read. A job without a reader or a read that
// fails (logged once) gives an error: the first look then names nothing,
// and the other two keep their entries.
func (e *Engine) threadsReader(ctx context.Context, job *fixJob) func() ([]github.Thread, error) {
	return sync.OnceValues(func() ([]github.Thread, error) {
		_, rd, ok := e.fixReader(job)
		if !ok {
			return nil, fmt.Errorf("no GitHub client that reads review threads for poll identity %q", job.watch.PollIdentity)
		}
		threads, _, err := rd.AllReviewThreads(ctx, job.repo.Owner, job.repo.Name, job.pr.Number) // a thread past the list keeps its entry
		if err != nil {
			e.log.Warn("fix: the PR's review threads, for the fixer's replies and what it left to the author", "subject", job.subject(), "err", err)
		}
		return threads, err
	})
}

// threadNow is one of magnum's threads as freshNeedsYou reads it: whether
// it is resolved, and the class of its answer (answerOf; "" for none).
type threadNow struct {
	resolved bool
	answer   string
}

// answerOf is the class a thread's answer carries (pipeline.ReplyFixed,
// ReplyNotABug, ReplyWontFix or ReplyOther; "" for none), from its replies
// oldest first: the latest reply that is not magnum's own, unless magnum
// replied after it with anything but an acknowledgement (a rebuttal or an
// answer argues the thread again).
func answerOf(replies []agents.ThreadReply) string {
	for _, r := range slices.Backward(replies) {
		switch {
		case !r.Own:
			return r.Class
		case r.Kind != postreview.ReplyAck:
			return ""
		}
	}
	return ""
}

// freshNeedsYou is entries, the "## Needs you" of the fixer's report (the
// settle's, or the loop's copy at its end), as magnum's own threads show
// them now, and the threads among them (by comment id) whose call asks only
// for the resolution. An entry with a call in one of magnum's threads (the
// files beside the job's report place it, threadOwners) whose thread is
// resolved asks nothing and is left out; one whose thread is open with the
// answer its call asks for (answeredBy the class answerOf reads) asks only
// to resolve it (attention.ResolveCall). Every other entry keeps its call,
// those in other reviewers' threads among them: magnum asks for a reply
// there and never for a resolution, and dropFixerAnswered has looked at
// them. The threads come from the threads file in dir when a re-review wrote
// it after the fixer's report, else from read, GitHub's once (threadsNow);
// nothing to look at, a read that fails, or a thread the read does not list
// keeps the entries as they are, and known, the threads found answered
// before (the loop's row at its end; nil at the settle), stays what is
// known when no read was made or it failed. A fix.calls_settled event
// counts what changed.
func (e *Engine) freshNeedsYou(ctx context.Context, job *fixJob, entries []string, known map[string]bool, dir string,
	read func() ([]github.Thread, error)) ([]string, map[string]bool) {
	report := job.state.Report
	owners := readThreadOwners(filepath.Dir(report))
	magnums := func(entry string) (needsYouEntry, int64, bool) {
		n, ok := readNeedsYouEntry(entry)
		if !ok || !n.asks() {
			return n, 0, false
		}
		id, err := strconv.ParseInt(n.id, 10, 64)
		mine, _ := owners.owner(n)
		return n, id, err == nil && mine
	}
	if !slices.ContainsFunc(entries, func(entry string) bool { _, _, ok := magnums(entry); return ok }) {
		return entries, known
	}
	threads, from, ok := e.threadsNow(ctx, job, dir, read)
	if !ok {
		return entries, known
	}
	kept, answered, resolved := []string{}, map[string]bool{}, 0
	for _, entry := range entries {
		n, id, mine := magnums(entry)
		t, found := threads[id]
		switch {
		case !mine || !found:
		case t.resolved:
			resolved++
			continue
		case n.answeredBy(t.answer):
			answered[n.id] = true
		}
		kept = append(kept, entry)
	}
	if resolved == 0 && len(answered) == 0 {
		return entries, nil
	}
	var said []string
	if resolved > 0 {
		said = append(said, fmt.Sprintf("%s left to you %s resolved in magnum's %s and %s nothing", textx.Count(resolved, "finding", "findings"),
			textx.Plural(resolved, "is", "are"), textx.Plural(resolved, "thread", "threads"), textx.Plural(resolved, "asks", "ask")))
	}
	if n := len(answered); n > 0 {
		said = append(said, fmt.Sprintf("%s left to you %s answered in magnum's %s and %s only for the resolution", textx.Count(n, "finding", "findings"),
			textx.Plural(n, "is", "are"), textx.Plural(n, "thread", "threads"), textx.Plural(n, "asks", "ask")))
	}
	data := job.loopData()
	data["resolved"], data["answered"], data["read"] = resolved, len(answered), from
	e.event(ctx, "info", job.subject(), "fix.calls_settled", strings.Join(said, "; "), data)
	return kept, answered
}

// threadsNow reads magnum's threads on the job's PR as they are now, by the
// id of each one's first comment, and says where from ("threads file",
// "github"): the threads file in dir, a re-review's, when it was written
// after the fixer's report (threadsFileNow), else read's threads. Their
// replies are magnum's own by the rule a round's threads file marks them
// with (pipeline.OwnHistory): the login of the PR's identity, an App's when
// that identity is one, and the PR's former logins. The login that posted
// the review the fixer fixed (fixJob.login, unset at the loop's end) is not
// added: it is the PR's identity's then, or a former one since. ok is false
// when neither can be read.
func (e *Engine) threadsNow(ctx context.Context, job *fixJob, dir string, read func() ([]github.Thread, error)) (map[int64]threadNow, string, bool) {
	if threads, ok := threadsFileNow(dir, job.state.Report); ok {
		return threads, "threads file", true
	}
	threads, err := read()
	if err != nil {
		return nil, "", false
	}
	name := cmp.Or(job.pr.Identity, job.watch.Identity)
	reviewer, former := e.reviewerLogin(name), e.formerLogins(ctx, job.pr)
	bot := strings.HasSuffix(strings.ToLower(reviewer), "[bot]")
	if id := e.cfg.IdentityByName(name); id != nil && id.Kind == "app" {
		bot = true // as the round's expectBot
	}
	out := make(map[int64]threadNow, len(threads))
	for _, t := range threads {
		all := t.AllComments()
		if len(all) == 0 {
			continue
		}
		replies := make([]agents.ThreadReply, 0, len(all)-1)
		for _, c := range all[1:] {
			r := agents.ThreadReply{Own: pipeline.OwnHistory(c.AuthorLogin, c.AuthorType, reviewer, bot, former)}
			if r.Own {
				_, r.Kind, _ = postreview.ParseReplyMarker(c.Body)
			} else {
				r.Class = pipeline.ClassifyReply(c.Body)
			}
			replies = append(replies, r)
		}
		out[all[0].ID] = threadNow{resolved: t.Resolved, answer: answerOf(replies)}
	}
	return out, "github", true
}

// threadsFileNow reads magnum's threads from the threads file in dir, a
// re-review's, when it was written after report (the fixer's, so after its
// call). The replies that round's judge wrote to its replies file came
// after the file's read, so each is magnum's last word in its thread. ok is
// false for no dir, no file, one written before the report, or one that
// cannot be read.
func threadsFileNow(dir, report string) (map[int64]threadNow, bool) {
	if dir == "" {
		return nil, false
	}
	path := filepath.Join(dir, pipeline.ThreadsFile)
	file, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if rep, err := os.Stat(report); err != nil || !file.ModTime().After(rep.ModTime()) {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var threads []agents.ReviewThread
	if json.Unmarshal(b, &threads) != nil {
		return nil, false
	}
	var judge postreview.RepliesFile
	if b, err := os.ReadFile(filepath.Join(dir, agents.PostRepliesFile)); err == nil && json.Unmarshal(b, &judge) != nil {
		judge = postreview.RepliesFile{}
	}
	out := make(map[int64]threadNow, len(threads))
	for _, t := range threads {
		replies := slices.Clone(t.Replies)
		for _, r := range judge.Replies {
			if r.CommentID == t.CommentID {
				replies = append(replies, agents.ThreadReply{Own: true, Kind: r.Kind})
			}
		}
		out[t.CommentID] = threadNow{resolved: t.Resolved, answer: answerOf(replies)}
	}
	return out, true
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
