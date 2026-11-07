package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// itemsAt is the base time of the item tests: magnum's review of the head
// posted an hour after it.
var itemsAt = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

// readFor is the read the look makes of PR #2 (its author zhuravel, magnum
// posting as talkable[bot]) from what GitHub lists, with the quiet markers
// and the logins whose resolution of another reviewer's thread does not
// count (resolvers).
func readFor(threads []github.Thread, reviews []github.Review, comments []github.PRComment, markers, resolvers []string) externalRead {
	return externalReadOf(externalInput{Threads: threads, Reviews: reviews, Comments: comments, CommentsComplete: true,
		Author: "zhuravel", Own: []string{"talkable[bot]"}, Markers: markers, Resolvers: resolvers}, fixHead)
}

// itemKeys are the keys of items, sorted.
func itemKeys(items []openItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Key)
	}
	slices.Sort(out)
	return out
}

// cleanSum is magnum's review of the head that found nothing (run r1,
// review 101), posted at itemsAt.
func cleanSum() *store.ReviewSummary {
	return &store.ReviewSummary{RunID: "r1", ReviewID: 101, SHA: fixHead, URL: prPage + "#pullrequestreview-101", At: itemsAt, Verdict: store.VerdictClean}
}

// A review tool's receipt and summary on the conversation (its hidden
// marker configured in quiet_markers) and a notice login's conversation
// comment (quiet_logins) ask nothing: they are never items, so they never
// start a loop. A colleague's comment next to them is one.
func TestCodexReceiptsAndNoticeCommentsNeverStartALoop(t *testing.T) {
	q := quietRules{logins: []string{"example-bot"}, markers: []string{"<!-- review-receipt:v1 -->"}}
	comments := []github.PRComment{
		{ID: 701, URL: prPage + "#issuecomment-701", Body: "Reviewed.\n<!-- review-receipt:v1 -->", CreatedAt: itemsAt.Add(time.Minute),
			AuthorLogin: "review-tool", AuthorType: "Bot"},
		{ID: 702, URL: prPage + "#issuecomment-702", Body: "Lint: 3 offenses on this push", CreatedAt: itemsAt.Add(2 * time.Minute),
			AuthorLogin: "example-bot", AuthorType: "User", AuthorAssociation: "MEMBER"},
		{ID: 703, URL: prPage + "#issuecomment-703", Body: "Could this use the cache?", CreatedAt: itemsAt.Add(3 * time.Minute),
			AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
	}
	read := readFor(nil, nil, comments, q.markers, nil)
	if got := itemKeys(openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead, quiet: q})); !slices.Equal(got, []string{"remark:703"}) {
		t.Errorf("items = %v, want the colleague's comment alone", got)
	}
	if got := itemKeys(openItemsOf(itemsInput{read: readFor(nil, nil, comments, nil, nil), sum: cleanSum(), head: fixHead})); len(got) != 3 {
		t.Errorf("items without quiet rules = %v, want all three", got)
	}
}

// A notice login also posts lint findings as review threads: quiet_logins
// apply to the conversation only, so its thread is a person's item.
func TestAProntoThreadByANoticeLoginStillStartsALoop(t *testing.T) {
	q := quietRules{logins: []string{"example-bot"}}
	th := extThread(4100, "app/a.rb", 3, itemsAt.Add(time.Minute), "Style/StringLiterals: prefer single quotes", "example-bot")
	th.Comments[0].AuthorType, th.Comments[0].AuthorAssociation = "User", "MEMBER"
	items := openItemsOf(itemsInput{read: readFor([]github.Thread{th}, nil, nil, nil, nil), sum: cleanSum(), head: fixHead, quiet: q})
	if len(items) != 1 || items[0].Key != "thread:4100@4100" || items[0].Class != classPerson || items[0].Kind != itemThread {
		t.Errorf("items = %+v, want the notice login's thread as a person's", items)
	}
}

// The quiet marker is looked for in the whole body, not in the 600
// characters the fixer's file keeps of it.
func TestAQuietMarkerPastTheExcerptStillCounts(t *testing.T) {
	q := quietRules{markers: []string{"<!-- review-summary -->"}}
	body := strings.Repeat("x", 900) + "<!-- review-summary -->"
	comments := []github.PRComment{{ID: 701, URL: prPage + "#issuecomment-701", Body: body, CreatedAt: itemsAt.Add(time.Minute),
		AuthorLogin: "review-tool", AuthorType: "Bot"}}
	read := readFor(nil, nil, comments, q.markers, nil)
	if !read.quiet[701] {
		t.Fatal("the marker at character 900 was not seen")
	}
	if got := openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead, quiet: q}); len(got) != 0 {
		t.Errorf("items = %+v, want none", got)
	}
}

// An item's key carries its thread's latest comment: a new reply makes a
// new key, so a thread a loop was handed opens again as new work.
func TestANewReplyInASeenThreadReopensIt(t *testing.T) {
	th := extThread(4100, "app/a.rb", 3, itemsAt.Add(time.Minute), "Rename this", "rev-ann")
	first := itemKeys(openItemsOf(itemsInput{read: readFor([]github.Thread{th}, nil, nil, nil, nil), sum: cleanSum(), head: fixHead}))
	th = extThread(4100, "app/a.rb", 3, itemsAt.Add(time.Minute), "Rename this", "rev-ann", "zhuravel", "rev-ann")
	th.Comments[1].Body = "Renamed.\n\n" + postreview.FixReplyMarker
	again := itemKeys(openItemsOf(itemsInput{read: readFor([]github.Thread{th}, nil, nil, nil, nil), sum: cleanSum(), head: fixHead}))
	if !slices.Equal(first, []string{"thread:4100@4100"}) || !slices.Equal(again, []string{"thread:4100@4102"}) {
		t.Errorf("keys = %v, then %v; want the reviewer's new reply to make a new key", first, again)
	}
}

// magnumThreadWith makes a thread magnum (talkable[bot]) started in review 101,
// its replies by authors with the given bodies.
func magnumThreadWith(id int64, at time.Time, replies ...[2]string) github.Thread {
	authors := []string{"talkable"}
	for _, r := range replies {
		authors = append(authors, r[0])
	}
	th := extThread(id, "app/m.rb", 9, at, "**[P2] Missing nil check**", authors...)
	th.Comments[0].ReviewID = 101
	for i, r := range replies {
		th.Comments[i+1].Body = r[1]
	}
	return th
}

// magnum resolves none of its threads, and it speaks last in most of them
// (an ack, an answer): only its rebuttal made after its latest review asks
// for an answer. The author's reply, or the fixer's fix-marked one, answers
// it; a later rebuttal opens it again under a new key.
func TestAMagnumThreadWaitsOnlyForARebuttalAfterTheLatestReview(t *testing.T) {
	rebuttal := "The nil check is still missing at line 9.\n" + postreview.ReplyMarker("r9", postreview.ReplyRebuttal)
	after := itemsAt.Add(10 * time.Minute) // the thread's comments start here, after the review
	cases := map[string]struct {
		th   github.Thread
		want []string
	}{
		"a finding with no reply": {magnumThreadWith(5000, after), nil},
		"an ack": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Fixed."},
			[2]string{"talkable", "Thanks.\n" + postreview.ReplyMarker("r9", postreview.ReplyAck)}), nil},
		"an answer": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Why?"},
			[2]string{"talkable", "Because.\n" + postreview.ReplyMarker("r9", postreview.ReplyAnswer)}), nil},
		"a rebuttal": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Not a bug."}, [2]string{"talkable", rebuttal}),
			[]string{"mthread:5000@5002"}},
		"the author answered the rebuttal": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Not a bug."}, [2]string{"talkable", rebuttal},
			[2]string{"zhuravel", "See the guard above."}), nil},
		"the fixer answered the rebuttal": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Not a bug."}, [2]string{"talkable", rebuttal},
			[2]string{"zhuravel", "Added it.\n" + postreview.FixReplyMarker}), nil},
		"a later rebuttal again": {magnumThreadWith(5000, after, [2]string{"zhuravel", "Not a bug."}, [2]string{"talkable", rebuttal},
			[2]string{"zhuravel", "See the guard above."}, [2]string{"talkable", rebuttal}), []string{"mthread:5000@5004"}},
		"a rebuttal before the latest review": {magnumThreadWith(5000, itemsAt.Add(-time.Hour), [2]string{"zhuravel", "Not a bug."},
			[2]string{"talkable", rebuttal}), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := openItemsOf(itemsInput{read: readFor([]github.Thread{tc.th}, nil, nil, nil, nil), sum: cleanSum(), head: fixHead})
			if !slices.Equal(itemKeys(got), tc.want) {
				t.Errorf("items = %v, want %v", itemKeys(got), tc.want)
			}
			for _, it := range got {
				if it.Class != classMagnum || it.Kind != itemMagnum {
					t.Errorf("item %+v, want magnum's", it)
				}
			}
		})
	}
}

// A reply of magnum's without a reply marker (one posted before markers
// existed) reads as a rebuttal, as postreview counts it.
func TestAnUnmarkedMagnumReplyCountsAsARebuttal(t *testing.T) {
	th := magnumThreadWith(5000, itemsAt.Add(10*time.Minute), [2]string{"zhuravel", "Not a bug."}, [2]string{"talkable", "It still is."})
	got := itemKeys(openItemsOf(itemsInput{read: readFor([]github.Thread{th}, nil, nil, nil, nil), sum: cleanSum(), head: fixHead}))
	if !slices.Equal(got, []string{"mthread:5000@5002"}) {
		t.Errorf("items = %v, want the unmarked reply as a rebuttal", got)
	}
}

// A person's thread never closes on the fixer's word alone: one that a
// fixer login resolved (the fixer may hold the right to) while its
// reviewer's word is the latest stays a person's item; one its reviewer or
// the author resolved is gone.
func TestAPersonsThreadThatAFixerResolvedStaysOpen(t *testing.T) {
	th := extThread(4100, "app/a.rb", 3, itemsAt.Add(time.Minute), "Rename this", "rev-ann", "zhuravel", "rev-ann")
	th.Comments[1].Body = "Renamed.\n\n" + postreview.FixReplyMarker
	th.Resolved = true
	for by, want := range map[string][]string{"factory-app[bot]": {"thread:4100@4102"}, "rev-ann": nil, "zhuravel": nil} {
		th.ResolvedBy = by
		items := openItemsOf(itemsInput{read: readFor([]github.Thread{th}, nil, nil, nil, []string{"factory-app[bot]"}), sum: cleanSum(), head: fixHead})
		if !slices.Equal(itemKeys(items), want) {
			t.Errorf("resolved by %s: items = %v, want %v", by, itemKeys(items), want)
		}
		for _, it := range items {
			if it.Class != classPerson || it.FixerReplies != 1 {
				t.Errorf("resolved by %s: item %+v, want a person's with one fixer reply", by, it)
			}
		}
	}
}

// Rule A1: a review with only simplifications, or one that keeps an
// optional P3 open, is magnum's work for the fixer, as findings are.
func TestAReviewWithOnlySimplificationsIsAnItem(t *testing.T) {
	sum := cleanSum()
	if got := openItemsOf(itemsInput{read: readFor(nil, nil, nil, nil, nil), sum: sum, head: fixHead}); len(got) != 0 {
		t.Errorf("a clean review gives %+v", got)
	}
	sum.Simplifications = 1
	got := openItemsOf(itemsInput{read: readFor(nil, nil, nil, nil, nil), sum: sum, head: fixHead})
	if len(got) != 1 || got[0].Key != "review:r1" || got[0].Class != classMagnum || got[0].Kind != itemFindings || got[0].URL != sum.URL {
		t.Errorf("a simplification gives %+v, want review:r1", got)
	}
	sum.Simplifications, sum.OpenCounts = 0, &[4]int{0, 0, 0, 1}
	if got := itemKeys(openItemsOf(itemsInput{read: readFor(nil, nil, nil, nil, nil), sum: sum, head: fixHead})); !slices.Equal(got, []string{"review:r1"}) {
		t.Errorf("an open P3 gives %v, want review:r1", got)
	}
}

// Rule A1 again: after a loop that was handed the review, a finding of it
// that the fixer neither resolved nor answered with a fix-marked reply was
// skipped without a word; it is an rthread item, which needs you. A
// resolved thread, or one with the fixer's reply, gives none, and so does a
// review no loop was handed.
func TestAFindingTheFixerSkippedWithoutAWordNeedsYou(t *testing.T) {
	sum := cleanSum()
	sum.Counts[3] = 1
	skipped := magnumThreadWith(5000, itemsAt.Add(-time.Minute))
	replied := magnumThreadWith(5100, itemsAt.Add(-time.Minute), [2]string{"zhuravel", "Won't fix: out of scope.\n" + postreview.FixReplyMarker})
	resolved := magnumThreadWith(5200, itemsAt.Add(-time.Minute))
	resolved.Resolved = true
	other := magnumThreadWith(5300, itemsAt.Add(-time.Minute))
	other.Comments[0].ReviewID = 99 // an earlier review's
	read := readFor([]github.Thread{skipped, replied, resolved, other}, nil, nil, nil, nil)
	got := openItemsOf(itemsInput{read: read, sum: sum, handed: true, head: fixHead})
	if want := []string{"review:r1", "rthread:5000"}; !slices.Equal(itemKeys(got), want) {
		t.Errorf("items = %v, want %v", itemKeys(got), want)
	}
	for _, it := range got {
		if it.Key == "rthread:5000" && (it.Class != classMagnum || it.Kind != itemFinding || it.URL != skipped.Comments[0].URL) {
			t.Errorf("rthread item = %+v", it)
		}
	}
	if got := itemKeys(openItemsOf(itemsInput{read: read, sum: sum, head: fixHead})); !slices.Equal(got, []string{"review:r1"}) {
		t.Errorf("before any loop: items = %v, want the review alone", got)
	}
}

// A conflict of the head with the PR's own base is an item keyed by the
// head, never by the base's tip, which moves several times a day.
func TestAConflictIsAnItemKeyedByTheHead(t *testing.T) {
	c := fixConflict{base: "feature", sha: fixBaseTip, paths: []string{"a.rb"}}
	got := openItemsOf(itemsInput{read: readFor(nil, nil, nil, nil, nil), sum: cleanSum(), head: fixHead, conflict: c})
	if len(got) != 1 || got[0].Key != "conflict:"+fixHead[:12] || got[0].Class != classConflict {
		t.Errorf("items = %+v, want the conflict keyed by the head", got)
	}
}

// A thread a fixer login resolved closes as an unresolved one does: on a
// fix-marked reply of a fixer login, or on the author's reply after the
// resolution. Its reviewer's word after the fixer's keeps it open. Before,
// it stayed open whatever came after, and the fixer's own reply gave it a
// new key that no loop could be handed (the fixer's file leaves resolved
// threads out): work for good, refused as nothing to fix.
func TestAThreadAFixerResolvedClosesOnAnAnswer(t *testing.T) {
	root := github.ThreadComment{ID: 10, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER", Body: "MINOR: rename",
		URL: prPage + "#discussion_r10", CreatedAt: itemsAt}
	fix := github.ThreadComment{ID: 11, AuthorLogin: "factory-app", AuthorType: "Bot", Body: "Fixed in abc1234.\n\n" + postreview.FixReplyMarker,
		CreatedAt: itemsAt.Add(time.Minute)}
	author := github.ThreadComment{ID: 12, AuthorLogin: "zhuravel", AuthorType: "User", AuthorAssociation: "MEMBER", Body: "agreed, thanks",
		CreatedAt: itemsAt.Add(2 * time.Minute)}
	again := github.ThreadComment{ID: 13, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER", Body: "still wrong",
		CreatedAt: itemsAt.Add(3 * time.Minute)}
	logins := []string{"talkable[bot]", "factory-app[bot]"}
	for name, tc := range map[string]struct {
		comments []github.ThreadComment
		want     []string
	}{
		"the fixer's fix-marked reply":            {[]github.ThreadComment{root, fix}, nil},
		"the author's reply after the resolution": {[]github.ThreadComment{root, author}, nil},
		"the reviewer's word after the fixer's":   {[]github.ThreadComment{root, fix, again}, []string{"thread:10@13"}},
		"no word at all":                          {[]github.ThreadComment{root}, []string{"thread:10@10"}},
	} {
		th := github.Thread{Resolved: true, ResolvedBy: "factory-app[bot]", Comments: tc.comments}
		read := externalReadOf(externalInput{Threads: []github.Thread{th}, CommentsComplete: true, Author: "zhuravel", Own: logins, Resolvers: logins}, fixHead)
		if got := itemKeys(othersItemsOf(read, quietRules{})); !slices.Equal(got, tc.want) {
			t.Errorf("%s: items = %v, want %v", name, got, tc.want)
		}
	}
}

// GitHub names no one for a resolution an App makes (a review thread's
// resolvedBy is a User, so an App's reads null). On a watch whose fixer
// acts as a dedicated App, a person's thread resolved by no one GitHub
// names counts as the fixer's resolution and stays open, as one the App
// resolved by name would; a gh fixer's resolution reads as a login, so
// there it closes. A bot's thread resolved so closes either way: a bot
// resolves its own threads once the code changed. Before, the App's
// resolution of a person's thread closed it on the fixer's word alone.
func TestAPersonsThreadResolvedByNoOneNamedStaysOpenUnderAnAppFixer(t *testing.T) {
	person := extThread(4100, "app/a.rb", 3, itemsAt, "Rename this", "rev-ann")
	person.Resolved = true
	bot := extThread(4200, "app/b.rb", 4, itemsAt, "P1: nil check", "lint-bot")
	bot.Resolved = true
	logins := []string{"talkable[bot]", "factory-app[bot]"}
	in := externalInput{Threads: []github.Thread{person, bot}, CommentsComplete: true, Author: "zhuravel", Own: logins, Resolvers: logins, AppFixer: true}
	items := othersItemsOf(externalReadOf(in, fixHead), quietRules{})
	if got := itemKeys(items); !slices.Equal(got, []string{"thread:4100@4100"}) {
		t.Errorf("under an App fixer: items = %v, want the person's thread alone", got)
	}
	for _, it := range items {
		if it.Class != classPerson || !it.Resolved {
			t.Errorf("item %+v, want a person's thread a fixer resolved", it)
		}
	}
	in.AppFixer = false
	if got := itemKeys(othersItemsOf(externalReadOf(in, fixHead), quietRules{})); len(got) != 0 {
		t.Errorf("under a gh fixer: items = %v, want none", got)
	}
}

// A person's approval asks for nothing: the fix skill gives it no verdict
// and no reply, so its body ("No blocking items") is neither work nor a
// leftover, in the look's items and in the fixer's file alike, and a clean
// review with an approval beside it leaves nothing open. A comment review
// and a changes request are items as before, and so is a bot's approval
// body neither.
func TestAnApprovalBodyIsNoItem(t *testing.T) {
	review := func(id int64, state, login, typ, body string) github.Review {
		return github.Review{DatabaseID: id, State: state, Body: body, URL: prPage + "#pullrequestreview-" + itoa(id), SubmittedAt: itemsAt.Add(time.Duration(id-900) * time.Minute),
			AuthorLogin: login, AuthorType: typ, AuthorAssociation: "MEMBER"}
	}
	approval := review(901, "APPROVED", "rev-ann", "User", "Reviewed a1a1a1a. No blocking items; the specs pass.")
	read := readFor(nil, []github.Review{approval}, nil, nil, nil)
	if got := openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead}); len(got) != 0 {
		t.Errorf("items = %+v, want none for an approval", got)
	}
	if _, conversation := read.items.unanswered(); conversation != 0 || len(read.items.Conversation) != 0 {
		t.Errorf("the fixer's file lists %+v, want no approval", read.items.Conversation)
	}
	reviews := []github.Review{approval,
		review(902, "COMMENTED", "rev-ann", "User", "Could this use the cache?"),
		review(903, "CHANGES_REQUESTED", "bob-rev", "User", "Please split this method."),
		review(904, "APPROVED", "review-bot", "Bot", "Automated review: approved, no issues found."),
	}
	read = readFor(nil, reviews, nil, nil, nil)
	if got := itemKeys(openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead})); !slices.Equal(got, []string{"remark:902", "remark:903"}) {
		t.Errorf("items = %v, want the comment review and the changes request alone", got)
	}
}

// A review body or comment edited in place asks anew: a review tool that
// rewrites its guide on each new commit, or a colleague who edits their
// comment, makes it work again after the fixer answered the old text, and
// a bot's note handed to a loop (seen) comes back under a new key. The key
// carries the edit time, and the answer counts only after the edit; the
// fixer's file says the same and gives the edit time. A quiet body stays
// quiet when edited, and an unedited remark keeps its key.
func TestAnEditedRemarkIsNewWorkAgain(t *testing.T) {
	guide := github.PRComment{ID: 701, URL: prPage + "#issuecomment-701", Body: "## Reviewer guide\nConcurrency race in the job", CreatedAt: itemsAt.Add(time.Minute),
		AuthorLogin: "example-bot", AuthorType: "User", AuthorAssociation: "MEMBER"}
	answer := github.PRComment{ID: 702, URL: prPage + "#issuecomment-702", Body: "> ## Reviewer guide\n" + guide.URL + "\n\nNot a bug: the job holds the lock.\n\n" +
		postreview.FixReplyMarker, CreatedAt: itemsAt.Add(5 * time.Minute), AuthorLogin: "zhuravel", AuthorType: "User", AuthorAssociation: "OWNER"}
	note := github.Review{DatabaseID: 801, State: "COMMENTED", Body: "Automated review: 2 suggestions", URL: prPage + "#pullrequestreview-801",
		SubmittedAt: itemsAt.Add(2 * time.Minute), AuthorLogin: "review-tool", AuthorType: "Bot"}
	receipt := github.PRComment{ID: 703, URL: prPage + "#issuecomment-703", Body: "Reviewed.\n<!-- review-receipt:v1 -->", CreatedAt: itemsAt.Add(3 * time.Minute),
		AuthorLogin: "review-tool", AuthorType: "Bot"}
	q := quietRules{markers: []string{"<!-- review-receipt:v1 -->"}}
	items := func(reviews []github.Review, comments ...github.PRComment) (externalRead, []string) {
		read := readFor(nil, reviews, comments, q.markers, nil)
		return read, itemKeys(openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead, quiet: q}))
	}
	if _, got := items([]github.Review{note}, guide, answer, receipt); !slices.Equal(got, []string{"remark:801"}) {
		t.Fatalf("items before the edits = %v, want the bot's note alone (the guide is answered)", got)
	}
	edited := itemsAt.Add(20 * time.Minute)
	guide.Body, guide.EditedAt = "## Reviewer guide\nReview updated until commit b2b2b2b\nMissing pagination", edited
	note.Body, note.EditedAt = "Automated review: 3 suggestions", edited
	receipt.Body, receipt.EditedAt = "Reviewed again.\n<!-- review-receipt:v1 -->", edited
	read, got := items([]github.Review{note}, guide, answer, receipt)
	at := itoa(edited.Unix())
	if want := []string{"remark:701@" + at, "remark:801@" + at}; !slices.Equal(got, want) {
		t.Fatalf("items after the edits = %v, want %v: the edited guide answered before the edit, the bot's note anew; the quiet receipt stays quiet", got, want)
	}
	work, _ := sortOpen(openItemsOf(itemsInput{read: read, sum: cleanSum(), head: fixHead, quiet: q}),
		openLedger{Handed: map[string]time.Time{"remark:801": itemsAt.Add(10 * time.Minute)}})
	if len(work) != 2 {
		t.Errorf("work = %+v, want the guide and the note: the note's handed key was its text before the edit", work)
	}
	for _, r := range read.items.Conversation {
		if r.ID == 701 && (r.Answered || r.EditedAt != rfc3339(edited)) {
			t.Errorf("the fixer's file says %+v, want the guide unanswered, edited at %s", r, rfc3339(edited))
		}
	}
	later := answer
	later.ID, later.URL, later.CreatedAt = 704, prPage+"#issuecomment-704", edited.Add(time.Minute)
	if _, got := items([]github.Review{note}, guide, answer, receipt, later); !slices.Equal(got, []string{"remark:801@" + at}) {
		t.Errorf("items after a reply to the edited guide = %v, want the note alone", got)
	}
}
