package stats

// The message classifier: what an operator's message was for. The factory's
// stats count the messages that only start a step (the target is zero), and
// the baseline runs the same patterns over the operator's old transcripts, so
// both sides of the comparison are counted by one rule.

import (
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// Classify returns the class of a message: step_start (it only starts a step:
// open the PR, push, merge the parent in, merge into a staging branch, take
// the recommendation), approval (a yes), correction (the agent was wrong or
// must do it otherwise), access (credentials, a login, permission) or info
// (anything else, which is also the class of an empty message). It reads the
// words alone, in this order: a message that hands over a credential is
// access whatever else it says, and one that refuses a step ("don't push
// yet") is a correction, not a step start. Stagings live in the operator's
// config, not here: any staging-<name> branch counts. The engine stores the
// class with registry.ClassifyInput; the classes are the registry's.
func Classify(text string) string {
	t := normalize(text)
	switch {
	case t == "":
		return registry.ClassInfo
	case accessRE.MatchString(t):
		return registry.ClassAccess
	case correctionRE.MatchString(t):
		return registry.ClassCorrection
	case stepStartRE.MatchString(strings.ReplaceAll(t, "push notification", "")), goOnRE.MatchString(t):
		return registry.ClassStepStart
	case approvalRE.MatchString(t):
		return registry.ClassApproval
	}
	return registry.ClassInfo
}

// normalize lower-cases text, folds typographic quotes and collapses white
// space, so the patterns read one spelling.
func normalize(text string) string {
	text = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`).Replace(strings.ToLower(text))
	return strings.Join(strings.Fields(text), " ")
}

// phrases compiles alternatives, each a whole phrase, into one regular
// expression matched on word edges.
func phrases(alternatives ...string) *regexp.Regexp {
	return regexp.MustCompile(`\b(?:` + strings.Join(alternatives, "|") + `)\b`)
}

var (
	// stepStartRE: the phrases that start the next step of the work. Push is
	// the word alone (a push notification is a subject, removed beforehand).
	stepStartRE = phrases(
		`(?:open|create|make|raise|submit|file)(?: me)? (?:a |the |new )*(?:draft )?(?:pr|pull request)`,
		`push`,
		`(?:merge|update from|rebase (?:on|onto)) (?:the )?(?:latest )?(?:master|main|parent|base|develop)(?: branch)?`,
		`(?:merge|deploy|ship|put)\b[^.!?]{0,40}\bstaging-[a-z0-9][a-z0-9_-]*`,
		`(?:do|go) (?:it )?as (?:you )?(?:recommend|recommended|suggest|suggested)`,
		`as recommended`,
		`do what you recommend`,
		`(?:go with|follow|take) (?:your|the) (?:recommend\w*|suggest\w*)`,
		`(?:run|start|launch) (?:the )?(?:tests?|specs?|lint|linter|ci|qa|review|code review|checks?)`,
		`start (?:the )?(?:implementation|coding|work)`,
		`commit (?:it|this|that|everything|the changes)`,
	)

	// goOnRE: "continue" and its kin start a step only as the message's own
	// words, not inside a sentence about a job that should continue.
	goOnRE = regexp.MustCompile(`^(?:(?:ok|okay|yes|so|then|now|please),? )*(?:continue|proceed|carry on|go on|move on)\b`)

	// correctionRE: the agent was wrong, or must do it otherwise. A refusal
	// at the start of the message counts; a negation in the middle does not
	// ("push it, don't wait" starts a step).
	correctionRE = regexp.MustCompile(`^(?:no|nope|wrong|not that|actually|never|don't|do not|dont)\b|` +
		`\b(?:that's not|that is not|this is not|is wrong|are wrong|isn't right|not what i (?:asked|meant|wanted)|` +
		`you (?:forgot|missed|broke|didn't|did not|should have|shouldn't have|should not have)|` +
		`instead of|rather than|should be|shouldn't|should not|revert|undo|roll ?back|` +
		`still (?:fails|failing|broken|wrong|not working)|wrong (?:file|table|branch|place|one|approach|way))\b|` +
		`\binstead\b[.!]?$`)

	// accessRE: credentials, a login, a permission. A word like "login"
	// alone is a ticket's subject, so only the acts count.
	accessRE = phrases(
		`passwords?`, `passcode`, `credentials?`, `api[ -]?keys?`, `access[ -]?tokens?`, `auth(?:entication)? tokens?`, `secret keys?`,
		`ssh keys?`, `2fa`, `two[- ]factor`, `one[- ]time (?:code|password)`, `otp`, `sso`, `vpn`,
		`log ?(?:in|into)(?: to| with| again| first)?`, `sign ?(?:in|into)(?: to| with| again)?`, `logged in`, `re-?authenticate`,
		`(?:gave|give|given|have|has|grant(?:ed)?) (?:you |me )?access`, `access to`, `(?:can't|cannot|can not|no) access`,
		`permission denied`, `unauthori[sz]ed`,
	)

	// approvalRE: a short yes, alone or with a few words.
	approvalRE = regexp.MustCompile(`^(?:yes|yep|yeah|yup|y|ok|okay|sure|fine|good|great|perfect|nice|agreed|approved?|lgtm|looks good|` +
		`sounds good|go ahead|go for it|do it|that works|correct|right|exactly|thanks|thank you)\b(?:[ ,.!;:-]+[a-z']+){0,5}[ .!]*$`)
)
