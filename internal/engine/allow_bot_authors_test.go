package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// helperBot is a PR by the App helper as the poll reads it: GraphQL's login
// "helper" with __typename Bot, REST's helper[bot].
func helperBot(n int, head string) prSpec {
	return prSpec{n: n, head: head, author: "helper", authorType: "Bot"}
}

// wantBotAuthor checks that PR #n is skipped as a bot's.
func wantBotAuthor(t *testing.T, h *harness, n int) {
	t.Helper()
	if pr := h.wantState(n, store.PRIneligible); deref(pr.SkipReason) != "bot author" {
		t.Fatalf("PR #%d skip_reason = %q, want bot author", n, deref(pr.SkipReason))
	}
}

// With skip_bot_authors = true, a PR by a bot allow_bot_authors lists
// ("Helper", which matches helper[bot] in any case and either form) is
// reviewed as a person's PR would be, while another bot's PR is skipped as
// before; a [[repo]] block of the repository that adds nothing to the list
// keeps the watch's.
func TestAListedBotsPRIsReviewedAlthoughBotsAreSkipped(t *testing.T) {
	h := newHarness(t)
	w := &h.cfg.Watches[0]
	w.SkipBotAuthors, w.AllowBotAuthors = new(true), []string{"Helper"}
	h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", Stakes: "Internal tooling."})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, helperBot(2, "b1"), prSpec{n: 3, head: "c1", author: "other", authorType: "Bot"})
	h.tick()
	h.wantState(2, store.PRQueued)
	wantBotAuthor(t, h, 3)

	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
	wantBotAuthor(t, h, 3)
	if rounds := h.rd.all(); len(rounds) != 1 || rounds[0].PR.Number != 2 {
		t.Fatalf("rounds = %d, want the one of #2", len(rounds))
	}
}

// A bot's PR the poll skipped ("bot author") is picked up after the
// operator lists the bot and restarts the daemon, with no head change and
// nothing else new on GitHub: the start classifies the ineligible PRs again
// under the new list, so a PR first seen ineligible is queued and the first
// poll's dispatch reviews it, and a PR open since the first sync goes back
// to baseline, as a PR the first sync records is. A [[repo]] block's list
// names the bot here.
func TestAnIneligibleBotPRBecomesEligibleAtTheNextPoll(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"}, helperBot(3, "c1"))
	h.startup()
	h.tick() // first sync: #1 and #3 baseline
	h.open(prSpec{n: 1, head: "base1"}, helperBot(2, "b1"), helperBot(3, "c1"))
	h.tick()
	wantBotAuthor(t, h, 2)
	h.advance(time.Hour)
	h.e = New(h.d) // a restart reads the skipped baseline PR as skipped
	h.startup()
	wantBotAuthor(t, h, 2)
	wantBotAuthor(t, h, 3)
	h.tick()
	wantBotAuthor(t, h, 2)
	reqWantRounds(t, h, 0)

	h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", AllowBotAuthors: []string{"helper[bot]"}})
	h.advance(time.Hour)
	h.e = New(h.d) // the daemon restarts on the new list
	h.startup()
	h.wantState(2, store.PRQueued)
	h.tick() // the first poll: nothing changed on GitHub
	h.wantState(2, store.PRReviewed)
	h.wantState(3, store.PRBaseline)
	if rounds := h.rd.all(); len(rounds) != 1 || rounds[0].PR.Number != 2 || rounds[0].TargetSHA != "b1" {
		t.Fatalf("rounds = %d, want the one of #2 at b1", len(rounds))
	}
	evs := subjectEvents(t, h, "pr:talkable/talkable#2")
	if heads := kindOf(evs, "pr.head_changed"); len(heads) != 0 {
		t.Fatalf("head changes: %+v", heads)
	}
	queued := kindOf(evs, "pr.queued")
	if len(queued) != 1 || !strings.HasPrefix(queued[0].Message, "ineligible → queued (now eligible)") {
		t.Fatalf("pr.queued events: %+v, want the one of the restart", queued)
	}
	if got := kindOf(subjectEvents(t, h, "pr:talkable/talkable#3"), "pr.baseline"); len(got) != 1 {
		t.Fatalf("#3's pr.baseline events: %+v", got)
	}
}
