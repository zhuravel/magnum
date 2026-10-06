package agents

import (
	"cmp"
	"strconv"
	"strings"
)

// PostReviewFile is the file the judge writes its review to for `magnum
// post-review`, in the report directory next to its result file
// (JudgeData.ReviewFile).
const PostReviewFile = "review.json"

// PostRepliesFile is the file the judge of a reply round writes its thread
// replies to for `magnum post-review --replies` (JudgeData.RepliesFile).
const PostRepliesFile = "replies.json"

// PostReviewLine is the shell line the judge runs to post its review
// (JudgeData.PostReviewCommand, `post_review` in the <magnum> block):
// `magnum post-review` with the run's facts as flags, each value
// shell-quoted. --gh-config-dir only for an identity with its own gh
// config; --dry-run in a dry run; --local-base (the base the blind replay's
// diff starts at) in a blind one, so the tool reads nothing from GitHub.
func PostReviewLine(d JudgeData) string {
	args := postArgs(d)
	if d.DryRun && d.Blind && d.BaseSHA != "" {
		args = append(args, "--local-base", d.BaseSHA)
	}
	return shellLine(append(args, "--review", d.ReviewFile))
}

// PostRepliesLine is the shell line the judge of a reply round runs to
// answer in its threads instead of posting a review
// (JudgeData.PostRepliesCommand, `post_replies`): the post_review line with
// --replies and the replies file in place of --review.
func PostRepliesLine(d JudgeData) string {
	return shellLine(append(postArgs(d), "--replies", d.RepliesFile))
}

// postArgs are the flags both lines share.
func postArgs(d JudgeData) []string {
	args := []string{cmp.Or(d.Magnum, "magnum"), "post-review",
		"--repo", d.Owner + "/" + d.Repo, "--pr", strconv.Itoa(d.Number), "--head", d.HeadSHA,
		"--run-id", d.RunID, "--login", d.ReviewerLogin}
	for _, f := range d.FormerLogins {
		args = append(args, "--former-login", f)
	}
	if d.GhConfigDir != "" {
		args = append(args, "--gh-config-dir", d.GhConfigDir)
	}
	if d.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// shellLine joins args, each shell-quoted.
func shellLine(args []string) string {
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	return strings.Join(args, " ")
}
