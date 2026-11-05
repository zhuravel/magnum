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
// words alone, in this order: a message that hands over a credential or
// grants access is access whatever else it says, and one that refuses a step
// ("don't push yet") is a correction, not a step start. Only the acts count:
// "log in to the sandbox" is access, "fix the login page" is information,
// "push it" starts a step and "the push failed" does not. Stagings live in the
// operator's config, not here: any staging-<name> branch counts. The classes
// are the registry's; the factory's engine stores the class of your words
// with the input when it records them.
func Classify(text string) string {
	t := normalize(text)
	switch {
	case t == "":
		return registry.ClassInfo
	case accessRE.MatchString(t):
		return registry.ClassAccess
	case correctionRE.MatchString(t):
		return registry.ClassCorrection
	case startsAStep(t):
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

// startsAStep reports whether the normalized message asks for a step. A push
// notification is a subject, not a push, so it is taken out first.
func startsAStep(t string) bool {
	t = strings.ReplaceAll(t, "push notification", "")
	return stepStartRE.MatchString(t) || pushRE.MatchString(t) || goOnRE.MatchString(t)
}

// phrases compiles alternatives, each a whole phrase, into one regular
// expression matched on word edges.
func phrases(alternatives ...string) *regexp.Regexp {
	return regexp.MustCompile(`\b(?:` + strings.Join(alternatives, "|") + `)\b`)
}

// secrets are the nouns of what you hand an agent to let it in.
const secrets = `(?:password|passcode|credentials?|api[ -]?keys?|(?:access|auth|bearer|secret|api) tokens?|secret keys?|ssh keys?|` +
	`(?:2fa|mfa|otp|one[- ]time|verification|sms|login|auth) (?:codes?|passwords?)|otp)`

var (
	// stepStartRE: the phrases that start the next step of the work.
	stepStartRE = phrases(
		`(?:open|create|make|raise|submit|file)(?: me)? (?:a |the |new )*(?:draft )?(?:pr|pull request)`,
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

	// pushRE: push as an act asked for, the message's own verb or the one after
	// a comma, a conjunction, "please" or "can you". "the push failed" and
	// "the pushed commit" are about a push, and start nothing.
	pushRE = regexp.MustCompile(`(?:^|[,;:.!?] ?|\b(?:and|then|ok|okay|now|please|just|also|go) |\b(?:can|could|would|will) you (?:please )?)` +
		`(?:git )?push(?: |$|[,;:.!?])`)

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

	// accessRE: handing over a credential, logging in, granting access, the acts
	// of a VPN or a second factor. A word like "login", "sso" or "password" in
	// a ticket's subject is none of these, so a noun alone is not meant to
	// match: it takes a verb that hands the credential over, a possessive, or a
	// login to do. Some sentences about a secret still match ("api keys are
	// rotated every 30 days"); the labelled set measures how often.
	accessRE = regexp.MustCompile(`\b(?:` + strings.Join([]string{
		// handing a credential over, or saying what is wrong with it
		`(?:here(?:'s| is| are)|this is|it(?:'s| is)) (?:my |the |your |our |a |an |the new |a new )?` + secrets,
		secrets + ` (?:is|are|was)`,
		secrets + `[:=] ?\w+`,
		`(?:my|your|our) ` + secrets,
		secrets + ` (?:expired|has expired|changed|was rotated|is wrong|is invalid|stopped working)`,
		// logging in is something to do
		`log in (?:to|into|with|again|first|as)`, `log into`, `sign in (?:to|into|with|again|as)`, `sign into`,
		`(?:you(?:'re| are)|i(?:'m| am)) (?:now )?logged in`, `re-?authenticate`,
		// granting access
		`(?:gave|give|given|granted|grant|giving) (?:you|it|the agent|the factory)(?: \w+)? access`,
		`you (?:now )?have(?: \w+)? access`, `access (?:granted|for you)`,
		// the acts of a VPN or a second factor
		`(?:connect(?:ed)?|turn(?:ed)? on|join(?:ed)?|start(?:ed)?) (?:to )?(?:the |my |a )?vpn`,
		`(?:approve[ds]?|accept(?:ed)?|confirm(?:ed)?) (?:the |a |your |my )?(?:2fa|mfa|two[- ]factor|sso|sign[- ]in|login)(?: prompt| request| push)?`,
	}, `|`) + `)\b`)

	// approvalRE: a short yes, alone or with a few words.
	approvalRE = regexp.MustCompile(`^(?:yes|yep|yeah|yup|y|ok|okay|sure|fine|good|great|perfect|nice|agreed|approved?|lgtm|looks good|` +
		`sounds good|go ahead|go for it|do it|that works|correct|right|exactly|thanks|thank you)\b(?:[ ,.!;:-]+[a-z']+){0,5}[ .!]*$`)
)
