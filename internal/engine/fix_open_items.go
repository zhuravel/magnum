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
// edit, so an edit in place does too (remarkKey). Another reviewer's item
// also names its reviewer, says when a review body that requests changes
// got its latest word, and whether its reviewer approved the PR after its
// latest word, which accepts it (acceptedBy): the ledger's sort reads them
// (sortOpen). Items carry kinds, links, counts, logins and times only,
// never PR text.

import (
	"cmp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

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
		out = append(out, openItem{Key: remarkKey(r), Class: classOf(r.Author), Kind: r.Kind, URL: r.URL, By: r.Author, ChangesAt: r.changesAt(),
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
