package agents

import (
	"strings"
	"testing"
)

// The judge's post_review line carries every fact of the run as a flag, the
// review file next to the result file, each value shell-quoted.
func TestPostReviewLineCarriesTheRunsFacts(t *testing.T) {
	d := judgeFixture()
	d.Magnum = "/Users/bohdan/Projects/magnum/bin/magnum"
	got := d.completed().PostReviewCommand
	want := "/Users/bohdan/Projects/magnum/bin/magnum post-review --repo talkable/talkable --pr 11920 " +
		"--head d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3 --run-id r-20261003T120000-7 --login 'talkable[bot]' " +
		"--gh-config-dir /Users/bohdan/Projects/magnum/state/gh/talkable-app " +
		"--review /Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/review.json"
	if got != want {
		t.Errorf("line =\n%s\nwant\n%s", got, want)
	}

	// The gh identity has no config dir; former logins repeat the flag; a
	// dry run says so; a path with a space is quoted.
	d.GhConfigDir, d.FormerLogins, d.DryRun = "", []string{"zhuravel", "talkable-old[bot]"}, true
	d.ResultFile = "/tmp/with space/codex-judge.json"
	got = d.completed().PostReviewCommand
	for _, part := range []string{" --former-login zhuravel --former-login 'talkable-old[bot]' ", " --dry-run ", " --review '/tmp/with space/review.json'"} {
		if !strings.Contains(got, part) {
			t.Errorf("line lacks %q:\n%s", part, got)
		}
	}
	if strings.Contains(got, "--gh-config-dir") || strings.Contains(got, "--local-base") {
		t.Errorf("line = %s", got)
	}

	// A blind replay checks its anchors against the local diff from base_sha.
	d.Blind = true
	if got = d.completed().PostReviewCommand; !strings.Contains(got, " --dry-run --local-base 0123456789abcdef0123456789abcdef01234567 --review ") {
		t.Errorf("blind line = %s", got)
	}
	// Without a binary path the line runs magnum from PATH.
	d.Magnum = ""
	if got = d.completed().PostReviewCommand; !strings.HasPrefix(got, "magnum post-review ") {
		t.Errorf("line = %s", got)
	}
}

// The post_replies line is the post_review line with the replies file (next
// to the result file) instead of the review file. Every thread reply goes
// through it, so every round after an earlier review has one (a reply round
// too, whatever else it knows); a first review has none.
func TestPostRepliesLineFollowsAnEarlierReview(t *testing.T) {
	want := "/Users/bohdan/Projects/magnum/bin/magnum post-review --repo talkable/talkable --pr 11920 " +
		"--head d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3 --run-id r-20261003T120000-7 --login 'talkable[bot]' " +
		"--gh-config-dir /Users/bohdan/Projects/magnum/state/gh/talkable-app " +
		"--replies /Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/replies.json"
	first := judgeFixture()
	first.Magnum = "/Users/bohdan/Projects/magnum/bin/magnum"
	first.PreviousReviewID, first.PreviousHeadSHA, first.PreviousReviews = 0, "", nil
	if got := first.completed(); got.PostRepliesCommand != "" || got.RepliesFile != "" {
		t.Fatalf("a first review: post_replies %q, file %q", got.PostRepliesCommand, got.RepliesFile)
	}
	reviewed, headOnly, replies := first, first, first
	reviewed.PreviousReviewID = 3012345678
	headOnly.PreviousHeadSHA = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0" // the latest review was another login's
	replies.Replies = 2
	for name, d := range map[string]JudgeData{"earlier review": reviewed, "earlier head": headOnly, "reply round": replies} {
		got := d.completed()
		if got.PostRepliesCommand != want || got.RepliesFile != "/Users/bohdan/Projects/magnum/state/reviews/talkable/talkable/11920/d4e5f6a/replies.json" {
			t.Errorf("%s: line =\n%s\nwant\n%s (file %s)", name, got.PostRepliesCommand, want, got.RepliesFile)
		}
	}
	reviewed.DryRun = true
	if got := reviewed.completed().PostRepliesCommand; !strings.Contains(got, " --dry-run --replies ") {
		t.Errorf("dry-run line = %s", got)
	}
}
