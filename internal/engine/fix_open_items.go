package engine

// The items autofix = "open" looks at on an own PR (fix_open.go): what is
// open on it, each under a key that the look's ledger remembers (the seen
// ledger, fix_open_verdict.go). They come from the read the fix job makes
// of the other reviewers (readExternal, no call of its own), from magnum's
// last review of the head and from a conflict with the PR's base:
//
//   - thread:<root id>@<latest id>: another reviewer's unresolved thread
//     with no answer, or one of theirs with no answer that a fixer login
//     resolved (a person's item never closes on the fixer's word alone; an
//     answer closes it, as it closes an unresolved one). No loop can be
//     handed a resolved one, so it needs you, never work;
//   - remark:<id>, or remark:<id>@<edit> once edited: another reviewer's
//     review body or conversation comment with no answer since its last
//     edit, unless it is quiet ([pipeline] quiet_logins and quiet_markers in
//     its body as it is now; threads never go quiet);
//   - mthread:<root id>@<latest id>: magnum's unresolved thread whose latest
//     comment is magnum's rebuttal (or an unmarked reply of magnum's, which
//     postreview reads as one) made after its latest review: it waits for
//     the author's answer. A finding with no reply, an ack and an answer
//     never make one: magnum resolves none of its threads, and its review
//     summary says which findings are open;
//   - rthread:<root id>: after a loop that was handed magnum's review, a
//     finding of that review the fixer neither resolved nor answered (rule
//     A1: an optional finding counts as open until a fix or a fix-marked
//     reply), unless a fixer left it the operator as an optional call
//     (fix_optional.go);
//   - review:<run id>: magnum's review of the head, when it leaves the fixer
//     work (reviewWork, the operator's open optional calls left out, its
//     `Description: ✗` line counted);
//   - conflict:<head sha12>: the head conflicts with its base. The key names
//     the head, not the base's tip, which moves several times a day.
//
// A key carries the thread's latest comment where it has one, so a new
// reply makes a new key and opens the item again, and a remark's last
// edit, so an edit in place does too (remarkKey). Items carry kinds, links
// and counts only, never PR text.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// Classes of an open item (openItem.Class): whose word it is.
const (
	classMagnum   = "magnum"   // magnum's review, thread or finding
	classPerson   = "person"   // another reviewer who is not a bot
	classBot      = "bot"      // another reviewer that is a bot (github.IsBot)
	classConflict = "conflict" // a conflict of the head with its base
)

// Kinds of an open item (openItem.Kind).
const (
	itemThread   = "thread" // another reviewer's review thread
	itemReview   = remarkReview
	itemComment  = remarkComment
	itemMagnum   = "magnum"   // magnum's thread whose rebuttal waits for an answer
	itemFinding  = "finding"  // a finding of a handed review the fixer skipped without a word
	itemFindings = "findings" // magnum's review with work for the fixer
	itemConflict = "conflict"
)

// openItem is one thing open on a PR: its ledger key, whose word it is, its
// kind and link, and, for another reviewer's thread, how many fix-marked
// replies the fixer gave in it (the thread's first and newest comments
// only: a long thread can undercount, which errs toward work), whether
// a fixer login resolved it (Resolved: the fixer's file leaves resolved
// threads out, so it is never work), and whether its last comment is your
// decision on the fixer's call there (Decision, fix_decide.go: its key
// takes your reply, so it is new work).
type openItem struct {
	Key          string `json:"key"`
	Class        string `json:"class"`
	Kind         string `json:"kind"`
	URL          string `json:"url,omitempty"`
	FixerReplies int    `json:"fixer_replies,omitempty"`
	Resolved     bool   `json:"resolved,omitempty"`
	Decision     bool   `json:"decision,omitempty"`
	// By is another reviewer's item's reviewer (Account form, "ghost" for
	// none; a thread's first comment's author), and Changes says that it is
	// a review body that requests changes: their pushback once one of their
	// items waited for you (openLedger.pushback).
	By      string `json:"by,omitempty"`
	Changes bool   `json:"changes,omitempty"`
	// Accepted says that the reviewer of another reviewer's item approved
	// the PR after its latest word (acceptedBy): one that would wait for
	// you waits for no one (sortOpen).
	Accepted bool `json:"accepted,omitempty"`
}

// quietRules are [pipeline] quiet_logins and quiet_markers: a review body
// or conversation comment by one of the logins, or whose whole body holds
// one of the markers, asks nothing. Threads never go quiet.
type quietRules struct {
	logins, markers []string
}

// login reports whether author (Account form) is a quiet login.
func (q quietRules) login(author string) bool {
	return slices.ContainsFunc(q.logins, func(l string) bool { return github.SameAccount(l, author) })
}

// marked reports whether body holds a quiet marker.
func quietMarked(markers []string, body string) bool {
	return slices.ContainsFunc(markers, func(m string) bool { return m != "" && strings.Contains(body, m) })
}

// magnumThread is an unresolved review thread one of magnum's logins
// started: its first comment's id and link, the review it belongs to, its
// latest comment's id and time, whether that latest comment is magnum's
// rebuttal (a reply of magnum's whose marker says rebuttal or that has
// none; a fixer's reply or a chat session's magnum comment is none), and
// whether it was answered: a reply by the PR author, or a fix-marked reply
// by one of magnum's logins, the latest of them made at AnsweredAt (a
// magnum comment answers none of magnum's own findings).
type magnumThread struct {
	RootID     int64     `json:"root_id"`
	URL        string    `json:"url,omitempty"`
	ReviewID   int64     `json:"review_id,omitempty"`
	LatestID   int64     `json:"latest_id"`
	LatestAt   time.Time `json:"latest_at"`
	Rebuttal   bool      `json:"rebuttal,omitempty"`
	Answered   bool      `json:"answered,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
}

// answeredSince reports whether mt was answered after at (a read recorded
// before AnsweredAt was kept says answered, whenever it was).
func (mt magnumThread) answeredSince(at time.Time) bool {
	return mt.Answered && (mt.AnsweredAt.IsZero() || mt.AnsweredAt.After(at))
}

// resolvedThread is another reviewer's thread that a fixer login resolved:
// still open (a person's item never closes on the fixer's word alone),
// unless its reviewer (By, Account form) approved the PR since (Accepted,
// acceptedBy).
type resolvedThread struct {
	RootID       int64  `json:"root_id"`
	LatestID     int64  `json:"latest_id"`
	URL          string `json:"url,omitempty"`
	Bot          bool   `json:"bot,omitempty"`
	FixerReplies int    `json:"fixer_replies,omitempty"`
	By           string `json:"by,omitempty"`
	Accepted     bool   `json:"accepted,omitempty"`
}

// magnumThreadsOf are the unresolved threads of threads that one of own
// (magnum's logins, Account form) started; author is the PR's.
func magnumThreadsOf(threads []github.Thread, own []string, author string) []magnumThread {
	isOwn := func(login, typ string) bool {
		a := github.Account(login, typ)
		return slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, a) })
	}
	byAuthor := func(login, typ string) bool { return github.SameAccount(github.Account(login, typ), author) }
	var out []magnumThread
	for _, t := range threads {
		all := t.AllComments()
		if t.Resolved || len(all) == 0 || !isOwn(all[0].AuthorLogin, all[0].AuthorType) {
			continue
		}
		root, latest := all[0], all[len(all)-1]
		mt := magnumThread{RootID: root.ID, URL: root.URL, ReviewID: root.ReviewID, LatestID: latest.ID, LatestAt: latest.CreatedAt}
		if len(all) > 1 && isOwn(latest.AuthorLogin, latest.AuthorType) && !postreview.IsOwnAnswer(latest.Body) {
			_, kind, marked := postreview.ParseReplyMarker(latest.Body)
			mt.Rebuttal = !marked || kind == postreview.ReplyRebuttal
		}
		for _, c := range all[1:] {
			if byAuthor(c.AuthorLogin, c.AuthorType) || (postreview.IsFixReply(c.Body) && isOwn(c.AuthorLogin, c.AuthorType)) {
				mt.Answered, mt.AnsweredAt = true, later(mt.AnsweredAt, c.CreatedAt)
			}
		}
		out = append(out, mt)
	}
	return out
}

// resolvedByFixerOf are the resolved threads of threads that another
// reviewer started (a bot, or an owner, member or collaborator: what
// externalOf counts), that one of resolvers resolved (Thread.ResolvedBy):
// the fixer's logins, whose resolution of another reviewer's thread does
// not close it, and that nobody answered. A person's thread that GitHub
// names no resolver of is the fixer's under an App fixer (AppFixer: a
// review thread's resolvedBy is a User, so an App's resolution reads
// null); a bot's is the bot's own, which resolves its threads once the
// code changed. The answer is externalOf's: the latest of the reviewers'
// comments is the author's, or a fix-marked reply or a magnum comment of
// one of magnum's logins; the key's latest comment is that one too, so a
// thread that is resolved and unresolved again keeps its key.
func resolvedByFixerOf(in externalInput) []resolvedThread {
	inList := func(list []string, login, typ string) bool {
		a := github.Account(login, typ)
		return slices.ContainsFunc(list, func(o string) bool { return github.SameAccount(o, a) })
	}
	byAuthor := func(login, typ string) bool { return github.SameAccount(github.Account(login, typ), in.Author) }
	reviewer := func(c github.ThreadComment) bool {
		return github.IsBot(c.AuthorType, c.AuthorLogin) || trustedReplier(c.AuthorAssociation) || byAuthor(c.AuthorLogin, c.AuthorType) ||
			inList(in.Own, c.AuthorLogin, c.AuthorType)
	}
	var out []resolvedThread
	decisions := reviewerDecisions(in.Reviews)
	for _, t := range in.Threads {
		all := t.AllComments()
		if !t.Resolved || len(all) == 0 {
			continue
		}
		root := all[0]
		bot := github.IsBot(root.AuthorType, root.AuthorLogin)
		switch {
		case inList(in.Own, root.AuthorLogin, root.AuthorType) || byAuthor(root.AuthorLogin, root.AuthorType) || !(bot || trustedReplier(root.AuthorAssociation)):
			continue // magnum's or the author's own thread, or an outsider's
		case t.ResolvedBy == "" && (bot || !in.AppFixer):
			continue // resolved by no one GitHub names: the bot itself, or an account gone
		case t.ResolvedBy != "" && !inList(in.Resolvers, t.ResolvedBy, ""):
			continue // its reviewer, the author or another person resolved it
		}
		cs := slices.DeleteFunc(slices.Clone(all), func(c github.ThreadComment) bool { return !reviewer(c) })
		latest := cs[len(cs)-1]
		if len(cs) > 1 && (byAuthor(latest.AuthorLogin, latest.AuthorType) ||
			postreview.IsOwnAnswer(latest.Body) && inList(in.Own, latest.AuthorLogin, latest.AuthorType)) {
			continue // answered, as externalOf answers an unresolved one
		}
		rt := resolvedThread{RootID: root.ID, LatestID: latest.ID, URL: root.URL, Bot: bot, By: cmp.Or(github.Account(root.AuthorLogin, root.AuthorType), "ghost")}
		rt.Accepted = acceptedBy(decisions, rt.By, latest.CreatedAt, cs)
		for _, c := range cs[1:] {
			if postreview.IsFixReply(c.Body) {
				rt.FixerReplies++
			}
		}
		out = append(out, rt)
	}
	return out
}

// quietOf are the ids of the review bodies and conversation comments in in
// whose whole body (not the file's excerpt) a quiet marker stands.
func quietOf(in externalInput) map[int64]bool {
	if len(in.Markers) == 0 {
		return nil
	}
	out := map[int64]bool{}
	for _, r := range in.Reviews {
		if quietMarked(in.Markers, r.Body) {
			out[r.DatabaseID] = true
		}
	}
	for _, c := range in.Comments {
		if quietMarked(in.Markers, c.Body) {
			out[c.ID] = true
		}
	}
	return out
}

// itemsInput is what openItemsOf makes the items of: the look's whole read
// of GitHub, magnum's last review that stands at the head (nil: none),
// whether a finished loop was handed that review (handed), the conflict of
// the head with its base (zero: none), the quiet rules, the threads of the
// operator's open optional calls (optional, magnumItemsOf) and whether the
// review's body has a `Description: ✗` line (described, reviewDescribes).
type itemsInput struct {
	read      externalRead
	sum       *store.ReviewSummary
	handed    bool
	conflict  fixConflict
	head      string
	quiet     quietRules
	optional  map[int64]bool
	described bool
}

// openItemsOf lists what is open on the PR (the keys of this file's
// comment), in a stable order: the other reviewers' threads, their review
// bodies and comments, magnum's threads that wait for an answer, the
// findings a loop skipped, magnum's review and the conflict.
func openItemsOf(in itemsInput) []openItem {
	return append(othersItemsOf(in.read, in.quiet), magnumItemsOf(in.read.mine, in.sum, in.handed, in.conflict, in.head, in.optional, in.described)...)
}

// othersItemsOf are the other reviewers' items of read: their unanswered
// threads, their unanswered review bodies and comments that are not quiet,
// and their threads a fixer login resolved. They do not depend on magnum's
// review, so the look keeps them with its read (openRead.Others).
func othersItemsOf(read externalRead, quiet quietRules) []openItem {
	var out []openItem
	classOf := func(author string) string {
		if github.IsBot("", author) {
			return classBot
		}
		return classPerson
	}
	for _, t := range read.items.Threads {
		if t.Answered || len(t.Comments) == 0 {
			continue
		}
		it := openItem{Key: threadKey(t.CommentID, t.Comments[len(t.Comments)-1].ID), Class: classOf(t.Comments[0].Author), Kind: itemThread, URL: t.URL,
			Decision: t.Decision, By: t.Comments[0].Author, Accepted: t.accepted}
		for _, c := range t.Comments {
			if c.Fix {
				it.FixerReplies++
			}
		}
		out = append(out, it)
	}
	for _, r := range read.items.Conversation {
		if r.Answered || quiet.login(r.Author) || read.quiet[r.ID] {
			continue
		}
		out = append(out, openItem{Key: remarkKey(r), Class: classOf(r.Author), Kind: r.Kind, URL: r.URL, By: r.Author, Changes: r.State == "CHANGES_REQUESTED",
			Accepted: r.accepted})
	}
	for _, rt := range read.resolved {
		class := classPerson
		if rt.Bot {
			class = classBot
		}
		out = append(out, openItem{Key: threadKey(rt.RootID, rt.LatestID), Class: class, Kind: itemThread, URL: rt.URL, FixerReplies: rt.FixerReplies,
			Resolved: true, By: rt.By, Accepted: rt.Accepted})
	}
	return out
}

// magnumItemsOf are magnum's items: its threads of mine whose rebuttal after
// sum (magnum's last review that stands at the head; nil: none) waits for
// an answer, the findings of sum the fixer skipped once a finished loop was
// handed it (handed), sum itself when it leaves the fixer work, and the
// conflict of head with its base (zero: none). optional are the threads,
// by their first comment's id, of the operator's open optional calls
// (fix_optional.go): no skipped finding, and no work in sum. described: sum's
// body has a `Description: ✗` line, which is work (reviewWork).
func magnumItemsOf(mine []magnumThread, sum *store.ReviewSummary, handed bool, conflict fixConflict, head string, optional map[int64]bool,
	described bool) []openItem {
	var out []openItem
	var since time.Time
	if sum != nil {
		since = sum.At
	}
	for _, mt := range mine {
		if mt.Rebuttal && mt.LatestAt.After(since) {
			out = append(out, openItem{Key: fmt.Sprintf("mthread:%d@%d", mt.RootID, mt.LatestID), Class: classMagnum, Kind: itemMagnum, URL: mt.URL})
		}
	}
	if handed && sum != nil {
		for _, mt := range mine {
			if mt.ReviewID != 0 && mt.ReviewID == sum.ReviewID && !mt.Answered && !optional[mt.RootID] {
				out = append(out, openItem{Key: fmt.Sprintf("rthread:%d", mt.RootID), Class: classMagnum, Kind: itemFinding, URL: mt.URL})
			}
		}
	}
	if sum != nil && reviewWork(*sum, optionalSplit(mine, sum, optional), described) {
		out = append(out, openItem{Key: reviewKey(sum.RunID), Class: classMagnum, Kind: itemFindings, URL: sum.URL})
	}
	if conflict.sha != "" && head != "" {
		out = append(out, openItem{Key: conflictKey(head), Class: classConflict, Kind: itemConflict})
	}
	return out
}

// threadKey is the key of another reviewer's thread: its first comment and
// its latest.
func threadKey(root, latest int64) string { return fmt.Sprintf("thread:%d@%d", root, latest) }

// decisionOf reports whether items hold your decision in the thread whose
// first comment is root (openItem.Decision).
func decisionOf(items []openItem, root int64) bool {
	prefix := fmt.Sprintf("thread:%d@", root)
	return slices.ContainsFunc(items, func(it openItem) bool { return it.Decision && strings.HasPrefix(it.Key, prefix) })
}

// reviewKey is the key of magnum's review of run.
func reviewKey(run string) string { return "review:" + run }

// conflictKey is the key of a conflict of head with its base.
func conflictKey(head string) string { return "conflict:" + head[:min(12, len(head))] }
