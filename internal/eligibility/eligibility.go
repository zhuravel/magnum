// Package eligibility is magnum's pure decision logic: which pull requests a
// watch picks up (Classify), when a picked-up PR may start its next review
// round (Throttle) and whether the daemon is inside its quiet hours
// (QuietHours).
//
// Nothing here does I/O, reads the clock or touches the store. Callers build a
// PRFacts from their rows, pass the current time explicitly, and persist or act
// on the returned decision.
package eligibility

import (
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// PRFacts is the snapshot of one pull request that the decisions need. Zero
// time values mean "unknown" or "never".
type PRFacts struct {
	Number int
	// Repo is the repository's name without owner ("" = unknown, never a
	// manual repository): manual_repos.
	Repo        string
	AuthorLogin string // GraphQL or REST form; a trailing "[bot]" is understood
	AuthorIsBot bool   // GraphQL __typename == "Bot"
	IsDraft     bool
	IsCrossRepo bool // opened from a fork
	// AuthorAssociation is GitHub's authorAssociation of the author with the
	// repository (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, …); "" = unknown.
	AuthorAssociation string
	Labels            []string
	SelfLogin         string // login of the user magnum polls as, for include_own

	HeadSHA     string // informational; no rule reads it
	ReviewedSHA string // "" until a round has been verified; selects first review vs re-review
	State       string // informational; no rule reads it

	HeadChangedAt time.Time // when the head last moved (the last push)
	// PushTimes are the PR's recent head changes (any order): Throttle
	// counts the ones within BurstWindow of the latest for the burst quiet
	// period.
	PushTimes          []time.Time
	PendingSince       time.Time // when the PR started waiting for a re-review
	LastRoundStartedAt time.Time
	RoundsToday        int // automatic rounds started on the current local day

	// RequestedAt is when a review request (or the PR's move from draft to
	// ready for review) arrived that no round has started for yet; zero =
	// none. Throttle then holds the PR only for the request debounce.
	RequestedAt time.Time
	// RepliedAt is the latest reply on magnum's review its judge has not
	// re-decided, on a head magnum reviewed (no push since); zero = none.
	// Throttle then holds the PR only for the reply debounce and the reply
	// interval since ReplyRoundAt, the start of the last round such replies
	// started on that head (zero = none).
	RepliedAt    time.Time
	ReplyRoundAt time.Time

	// The unreviewed delta (ReviewedSHA...HeadSHA) for the re-review
	// threshold. DeltaKnown is false when it was not measured, or a file of
	// it had no complete patch: the threshold then never holds the PR.
	DeltaKnown      bool
	DeltaLines      int       // changed lines the trivial-delta classifier counts as code
	DeltaAddedFiles int       // files added (or renamed, copied) since the review
	DeltaSince      time.Time // the first push the review does not cover
	// DeltaReadable: the delta was measured and every file of it was read
	// in full or is a modified binary file, counted as 0 lines (what a delta
	// check needs; DeltaKnown is false with such a file).
	DeltaReadable bool

	Forced bool // manual `magnum review`: Throttle lets it through
	Muted  bool // automation stopped for this PR: Classify rejects it
	Pinned bool // slot/session hold; informational, neither decision reads it
}

// Decision is Classify's verdict.
type Decision struct {
	Eligible bool
	Reason   string // why the PR is ineligible, in words that name the config key; empty when eligible
}

// Classify applies the watch's filters to a PR. It looks at Muted, the
// repository (manual_repos), the author (bots, skip_authors, own), Labels,
// IsDraft, IsCrossRepo and the author's association
// (skip_departed_authors), in that order, and reports the first rule that
// rejects the PR.
//
// Forced is deliberately not consulted: the filters describe what the daemon
// picks up on its own, and whether a manual request overrides them is the
// caller's decision.
func Classify(w config.Watch, f PRFacts) Decision {
	switch {
	case f.Muted:
		return reject("muted")
	case w.ManualRepo(f.Repo):
		return reject("manual repository (manual_repos)")
	case w.BotsSkipped() && isBot(f):
		return reject("bot author")
	case authorSkipped(w.SkipAuthors, f.AuthorLogin):
		return reject(fmt.Sprintf("author %q is in skip_authors", f.AuthorLogin))
	}
	for _, label := range f.Labels {
		if containsExact(w.SkipLabels, label) {
			return reject(fmt.Sprintf("label %q is in skip_labels", label))
		}
	}
	switch {
	case f.IsDraft && !w.DraftsIncluded():
		return reject("draft PR (include_drafts = false)")
	case !w.OwnIncluded() && isOwn(f):
		return reject("own PR (include_own = false)")
	case f.IsCrossRepo && w.CrossRepoSkipped():
		return reject("cross-repository PR (skip_cross_repository = true)")
	case !f.IsCrossRepo && w.DepartedAuthorsSkipped() && departed(f.AuthorAssociation):
		return reject(fmt.Sprintf("author %s left %s: no longer a member or collaborator (skip_departed_authors = true)",
			cmpOr(f.AuthorLogin, "ghost"), cmpOr(w.Owner, "the repository")))
	}
	return Decision{Eligible: true}
}

// departed reports an author association that has no access to the
// repository now. A branch of the repository needs write access, so its
// author had it once: CONTRIBUTOR and the like mean they lost it (left the
// organization, or were removed as a collaborator). "" (not fetched yet) is
// never departed.
func departed(assoc string) bool {
	switch strings.ToUpper(strings.TrimSpace(assoc)) {
	case "", "OWNER", "MEMBER", "COLLABORATOR":
		return false
	}
	return true
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func reject(reason string) Decision { return Decision{Reason: reason} }

const botSuffix = "[bot]"

func isBot(f PRFacts) bool {
	return f.AuthorIsBot || strings.HasSuffix(strings.ToLower(f.AuthorLogin), botSuffix)
}

// isOwn reports whether the PR was authored by SelfLogin. An unknown SelfLogin
// owns nothing, so two blank logins never match.
func isOwn(f PRFacts) bool {
	return f.SelfLogin != "" && strings.EqualFold(f.AuthorLogin, f.SelfLogin)
}

// authorSkipped matches login against skip_authors case-insensitively, ignoring
// a trailing "[bot]" on either side (GraphQL says "dependabot", REST says
// "dependabot[bot]"). Blank entries match nothing.
func authorSkipped(skip []string, login string) bool {
	login = normalizeLogin(login)
	if login == "" {
		return false
	}
	for _, s := range skip {
		if s = normalizeLogin(s); s != "" && s == login {
			return true
		}
	}
	return false
}

func normalizeLogin(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(s, botSuffix)
}

func containsExact(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
