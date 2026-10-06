package eligibility

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// replyDaemon is schedDaemon with the shipped reply keys: a 3m debounce and
// one reply round per PR and head every 2h.
func replyDaemon() config.Daemon {
	d := schedDaemon()
	d.ReplyDebounce = dur(3 * time.Minute)
	d.ReplyMinInterval = dur(2 * time.Hour)
	return d
}

// A reply on the reviewed head waits only the reply debounce after the last
// reply: the quiet period, the re-review interval and the daily cap do not
// hold it (the round re-decides threads; no code changed).
func TestThrottleRepliesWaitOnlyTheDebounceAfterTheLastReply(t *testing.T) {
	d := replyDaemon()
	d.MaxRoundsPerPRPerDay = 2
	f := PRFacts{ReviewedSHA: reviewed, HeadChangedAt: ago(time.Minute), PendingSince: ago(time.Minute),
		LastRoundStartedAt: ago(5 * time.Minute), RoundsToday: 2}

	f.RepliedAt = ago(time.Minute)
	got := Throttle(d, f, now)
	wantDecision(t, got, false, now.Add(2*time.Minute), ReasonReplies)
	if got.Rule != RuleReplies {
		t.Fatalf("rule = %q, want %q", got.Rule, RuleReplies)
	}

	f.RepliedAt = ago(3 * time.Minute)
	wantDecision(t, Throttle(d, f, now), true, now, "")
}

// At most one reply round per PR and head every reply_min_interval, counted
// from the start of the last one on that head.
func TestThrottleRepliesComeAtMostOncePerHeadEveryInterval(t *testing.T) {
	d := replyDaemon()
	f := reviewedFacts()
	f.RepliedAt = ago(10 * time.Minute)
	f.ReplyRoundAt = ago(30 * time.Minute)
	got := Throttle(d, f, now)
	wantDecision(t, got, false, now.Add(90*time.Minute), ReasonReplies)
	if got.Rule != RuleReplyInterval {
		t.Fatalf("rule = %q, want %q", got.Rule, RuleReplyInterval)
	}
	f.ReplyRoundAt = ago(2 * time.Hour)
	wantDecision(t, Throttle(d, f, now), true, now, "")

	d.ReplyMinInterval = dur(0)
	f.ReplyRoundAt = ago(time.Minute)
	wantDecision(t, Throttle(d, f, now), true, now, "")
}

// A review request wins over replies: it skips the reply interval too, and
// waits its own debounce.
func TestThrottleARequestWinsOverReplies(t *testing.T) {
	d := replyDaemon()
	f := reviewedFacts()
	f.RepliedAt = ago(10 * time.Second)
	f.ReplyRoundAt = ago(time.Minute)
	f.RequestedAt = ago(2 * time.Minute)
	wantDecision(t, Throttle(d, f, now), true, now, "")
}
