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

// helperApp is helper's PR with the authorAssociation GitHub reports for
// an App's: NONE, since an App is never an owner, a member or a
// collaborator of the organization, whatever its installation may write.
func helperApp(n int, head string) prSpec {
	p := helperBot(n, head)
	p.assoc = "NONE"
	return p
}

// skippedApp records app, helper's App PR #2, as the poll skips it ("bot
// author"): open before the first sync and skipped as a baseline by a
// restart (baseline), or first seen ineligible after it. It returns the
// row.
func skippedApp(t *testing.T, h *harness, baseline bool, app prSpec) store.PR {
	t.Helper()
	if baseline {
		h.open(prSpec{n: 1, head: "base1"}, app)
	} else {
		h.open(prSpec{n: 1, head: "base1"})
	}
	h.startup()
	h.tick() // first sync
	if baseline {
		h.e = New(h.d) // a restart reads the baseline PR as skipped
		h.startup()
	} else {
		h.open(prSpec{n: 1, head: "base1"}, app)
		h.tick()
	}
	wantBotAuthor(t, h, 2)
	pr := h.pr(2)
	if pr.DetailsAt == nil || deref(pr.AuthorType) != "Bot" || deref(pr.AuthorAssociation) != "NONE" || deref(pr.ReviewedSHA) != "" {
		t.Fatalf("PR #2 = %+v, want an App's PR with details and no review", pr)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, KVPRSkippedBaseline(pr.ID)); ok != baseline {
		t.Fatalf("skipped baseline key present = %v, want %v", ok, baseline)
	}
	return pr
}

// The live case of spec 54: an App's PR sat ineligible for "bot author",
// then the operator added allow_bot_authors to the repository's [[repo]]
// block (which held only its stakes) under a watch that skips bots by
// default, includes the repository, lists another one in manual_repos and
// has no auto_approve, and restarted the daemon a day after the PR's last
// push. The start lets it through although the App's association is NONE
// (skip_departed_authors reads only people's): a PR skipped as a baseline
// goes back to baseline, one first seen ineligible is queued and reviewed
// at the first poll, each with its event.
func TestAListedAppsPRLeavesIneligibleAtTheRestart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseline bool
	}{{"a skipped baseline PR", true}, {"a PR first seen ineligible", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Watches[0].ManualRepos = []string{"wordpress"}
			h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", Stakes: "Internal tooling."})
			pr := skippedApp(t, h, tc.baseline, helperApp(2, "b1"))

			h.advance(24 * time.Hour)
			h.cfg.Repos[len(h.cfg.Repos)-1].AllowBotAuthors = []string{"helper[bot]"}
			h.e = New(h.d) // the daemon restarts on the new list
			h.startup()
			evs := subjectEvents(t, h, "pr:talkable/talkable#2")
			if tc.baseline {
				h.wantState(2, store.PRBaseline)
				if got := kindOf(evs, "pr.baseline"); len(got) != 1 || got[0].Message != "ineligible → baseline: the configuration no longer skips it" {
					t.Fatalf("pr.baseline events: %+v", got)
				}
				if _, ok, _ := h.st.GetKV(h.ctx, KVPRSkippedBaseline(pr.ID)); ok {
					t.Fatal("the skipped baseline key outlived the restore")
				}
				h.tick()
				h.wantState(2, store.PRBaseline)
				reqWantRounds(t, h, 0)
				return
			}
			h.wantState(2, store.PRQueued)
			if got := kindOf(evs, "pr.queued"); len(got) != 1 || !strings.HasPrefix(got[0].Message, "ineligible → queued (now eligible)") {
				t.Fatalf("pr.queued events: %+v", got)
			}
			h.tick() // the first poll: nothing changed on GitHub
			h.wantState(2, store.PRReviewed)
			if rounds := h.rd.all(); len(rounds) != 1 || rounds[0].PR.Number != 2 || rounds[0].TargetSHA != "b1" {
				t.Fatalf("rounds = %d, want the one of #2 at b1", len(rounds))
			}
		})
	}
}

// A PR the start classifies again and still skips, by another rule than
// the one it was skipped for, says so: its skip reason becomes the rule
// that rejects it now, with a pr.ineligible event, whether it is a skipped
// baseline PR or one first seen ineligible; a start that finds the same
// rule writes nothing. Here the list lets helper's App PR past the bot
// rule and skip_labels rejects it.
func TestAReclassifiedPRNamesTheRuleThatSkipsItNow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseline bool
	}{{"a skipped baseline PR", true}, {"a PR first seen ineligible", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := &h.cfg.Watches[0]
			w.SkipLabels = []string{"wip"}
			app := helperApp(2, "b1")
			app.labels = []string{"wip"}
			pr := skippedApp(t, h, tc.baseline, app) // the bot rule goes before skip_labels

			w.AllowBotAuthors = []string{"helper"}
			h.e = New(h.d) // the daemon restarts on the new list
			h.startup()
			const reason = `label "wip" is in skip_labels`
			if got := h.wantState(2, store.PRIneligible); deref(got.SkipReason) != reason {
				t.Fatalf("PR #2 skip_reason = %q, want %q", deref(got.SkipReason), reason)
			}
			want := "ineligible → ineligible: " + reason
			if got := kindOf(subjectEvents(t, h, "pr:talkable/talkable#2"), "pr.ineligible"); len(got) == 0 || got[len(got)-1].Message != want {
				t.Fatalf("pr.ineligible events: %+v, want the last %q", got, want)
			}
			if _, ok, _ := h.st.GetKV(h.ctx, KVPRSkippedBaseline(pr.ID)); ok != tc.baseline {
				t.Fatalf("skipped baseline key present = %v, want %v", ok, tc.baseline)
			}

			before := len(subjectEvents(t, h, "pr:talkable/talkable#2"))
			h.e = New(h.d)
			h.startup()
			if got := subjectEvents(t, h, "pr:talkable/talkable#2"); len(got) != before {
				t.Fatalf("a start on the same rule wrote %+v", got[before:])
			}
		})
	}
}
