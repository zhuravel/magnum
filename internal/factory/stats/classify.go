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
// the recommendation, run or test things, resolve the conflicts, submit the
// review, or let the agent go on: "go ahead", "implement", "apply", "next
// step", "fix it", "do it", a bare "go"), approval (a yes: "yes", "ok",
// "sounds good", "confirm" alone), correction (the agent was wrong or must do
// it otherwise: "make sure ...", "you do that", "no need to ...", "the summary
// was not posted", "I don't see you ...", "instead", "I said", "still
// broken"), access (credentials, a login, permission, a server to connect to)
// or info (anything else, which is also the class of an empty message). It
// reads the words alone, in this order: a message that hands over a
// credential or grants access is access whatever else it says, and one that
// refuses a step ("don't push yet") is a correction, not a step start. Only
// the acts count: "log in to the sandbox" is access, "fix the login page" is
// information, "push it" starts a step and "the push failed" does not, and
// what a thing should be ("the export should be configurable") is no
// correction. Stagings live in the operator's config, not here: any
// staging-<name> branch counts. The classes are the registry's; the
// factory's engine stores the class of your words with the input when it
// records them.
func Classify(text string) string {
	t := normalize(text)
	switch {
	case t == "":
		return registry.ClassInfo
	case accessRE.MatchString(t):
		return registry.ClassAccess
	case correctionRE.MatchString(t), notDone(t):
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
	return stepStartRE.MatchString(t) || pushRE.MatchString(t) || submitRE.MatchString(t) || goOnRE.MatchString(t) || doItRE.MatchString(t) ||
		testRE.MatchString(t)
}

// notDone reports whether the normalized message says that something the
// agent did is not so ("the summary was not posted", "the badge isn't
// visible"), outside a clause of purpose ("so that the menu is not
// visible", which asks for it).
func notDone(t string) bool {
	loc := notDoneRE.FindStringIndex(t)
	if loc == nil {
		return false
	}
	sentence := t[:loc[0]]
	if ends := sentenceEndRE.FindAllStringIndex(sentence, -1); len(ends) > 0 {
		sentence = sentence[ends[len(ends)-1][1]:]
	}
	return !purposeRE.MatchString(sentence)
}

// phrases compiles alternatives, each a whole phrase, into one regular
// expression matched on word edges.
func phrases(alternatives ...string) *regexp.Regexp {
	return regexp.MustCompile(`\b(?:` + strings.Join(alternatives, "|") + `)\b`)
}

// ownVerb is where a verb is the message's own: at its start, after a
// comma, a conjunction, "please" or "can you".
const ownVerb = `(?:^|[,;:.!?] ?|\b(?:and|then|ok|okay|now|please|just|also|go) |\b(?:can|could|would|will) you (?:please )?)`

// lead is what may open a message before its own words: an ok, a yes, a
// then ("ok, do it", "now apply").
const lead = `^(?:(?:ok|okay|yes|yep|so|then|now|please|alright|great|good|perfect|cool),? )*`

// secrets are the nouns of what you hand an agent to let it in.
const secrets = `(?:password|passcode|credentials?|api[ -]?keys?|(?:access|auth|bearer|secret|api) tokens?|secret keys?|ssh keys?|` +
	`(?:2fa|mfa|otp|one[- ]time|verification|sms|login|auth) (?:codes?|passwords?)|otp)`

// keyVarName is a secret named by its environment variable (POINTS_API_KEY,
// normalized to points_api_key).
const keyVarName = `[a-z0-9]+(?:_[a-z0-9]+)*_(?:api_key|api_token|access_token|auth_token|token|secret|secret_key|password|key)`

// envVars are the secrets and environment variables you set up for an agent.
const envVars = `(?:secrets|env(?:ironment)? ?var(?:iable)?s?)`

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
		`(?:re-?run|run|start|launch) (?:(?:the|all|full|backend|frontend|unit|integration|system|feature|e2e|browser|failing|new) )*`+
			`(?:tests?|specs?|suites?|lint|linter|ci|qa|review|code review|checks?|migrations?)`,
		`migrate (?:the )?(?:dev |development |test |local )?(?:db|database)s?`,
		`start (?:the )?(?:implementation|coding|work)`,
		`commit (?:it|this|that|everything|the changes)`,
	)

	// submitRE: resolving the conflicts, submitting or posting the review, as
	// the message's own verb (ownVerb): "will that post the review?" is about
	// a post.
	submitRE = regexp.MustCompile(ownVerb + `(?:resolve (?:the )?(?:merge )?conflicts?|` +
		`(?:submit|post|publish) (?:the |your |my |a |all )?(?:pending )?(?:review|inline comments|comments))\b`)

	// pushRE: push as an act asked for, the message's own verb or the one after
	// a comma, a conjunction, "please" or "can you". "the push failed" and
	// "the pushed commit" are about a push, and start nothing.
	pushRE = regexp.MustCompile(ownVerb + `(?:git )?push(?: |$|[,;:.!?])`)

	// goOnRE: "continue", "go ahead", "implement", "apply", "next step" and
	// their kin start a step only as the message's own words, not inside a
	// sentence about a job that should continue ("how do we proceed?").
	goOnRE = regexp.MustCompile(lead + `(?:(?:you|u|just) )?(?:continue|proceed|carry on|go on|move on|go ahead|go for it|go implement|` +
		`implement|next step|apply|build (?:it )?now|build it)\b`)

	// doItRE: a verb of doing whose object is what the agent proposed ("fix
	// it", "do all of them"), or a bare verb that is the whole message ("go",
	// "add"): both let the agent take the step it offered. A demonstrative is
	// that object only at the end or before a conjunction ("do this first"):
	// "fix this bug in the parser" names a new one.
	doItRE = regexp.MustCompile(lead + `(?:(?:you|u|just) )?(?:` +
		`(?:do|fix|apply|implement|build|add) (?:it|them|both|everything|the rest|all(?: of (?:it|them))?)\b|` +
		`(?:do|fix|apply|implement|build|add) (?:all (?:of )?)?(?:this|that|these|those)(?:[ ,]+(?:and|then|too|now|please|first)\b|[ .!]*$)|` +
		`(?:go|do|add|apply|implement|proceed|continue|fix|build|commit|retry)(?: (?:it|now|please|again))*[ .!]*$)`)

	// testRE: "test" as the message's own verb with what to test ("test
	// mailer, jobs and the api"), never the noun ("the test fails", "test
	// fails").
	testRE = regexp.MustCompile(lead + `(?:re-?test|test) (?:it|this|that|them|everything|all|again|both|locally|manually|the|in|on|with|` +
		`backend|frontend|api|ui|[a-z]+, [a-z]+)\b`)

	// correctionRE: the agent was wrong, or must do it otherwise. A refusal
	// at the start of the message counts; a negation in the middle does not
	// ("push it, don't wait" starts a step). What a thing should be ("the
	// export should be configurable") is a request, not a correction; that
	// something was not done ("the summary was not posted"), still fails or
	// looks wrong, that the agent should do it itself ("you do that") or
	// otherwise ("instead", "I said", "no need to", "make sure") is one.
	correctionRE = regexp.MustCompile(`^(?:no|nope|wrong|not that|actually|never|don't|do not|dont)\b|` +
		`^(?:you|u) do (?:that|it|this|the|them)\b|` +
		`^(?:(?:make it|keep it|be|much|even|way|a bit) )?(?:shorter|simpler|clearer|briefer|more concise|less verbose)(?:[ ,]+[a-z']+){0,3}[ .!]*$|` +
		`(?:^|[,;:.!?] ?|\b(?:and|but|also|please|just|pls) )make sure\b|` +
		`\b(?:that's not|that is not|this is not|is wrong|are wrong|isn't right|not what i (?:asked|meant|wanted)|` +
		`you (?:forgot|missed|broke|didn't|did not|should have|shouldn't have|should not have)|` +
		`instead|rather than|shouldn't|should not|revert|undo|roll ?back|i said|no need (?:to|for)|` +
		`i(?:'m| am) not (?:forcing|asking|telling) you|i (?:don't|do not|didn't|did not|can't|cannot) see you|` +
		`still (?:fails|failing|failed|broken|wrong|not|doesn't|does not|don't|isn't|can't|cannot|shows?|showing|happens|happening)|i still (?:have|see|get)|` +
		`your (?:fix|fixes|change|changes|code|commit|edits?|patch)(?: [a-z']+){0,2} (?:breaks?|broke|caused?|causes)|` +
		`(?:might|may|could) be (?:because of |caused by |from )?your (?:fix|fixes|change|changes|code|commit|edits?|patch)|` +
		`looks? (?:wrong|off|broken|bad|weird|odd|ugly|oversized|undersized|too (?:big|small|large|wide|narrow|long))|` +
		`wrong (?:file|table|branch|place|one|approach|way))\b`)

	// notDoneRE: something is not so, or was not done (notDone).
	notDoneRE = regexp.MustCompile(`\b(?:(?:was|were|is|are)(?:n't| not)|(?:has|have)(?:n't| not) been)(?: yet)? (?:[a-z]+ )?` +
		`(?:posted|working|visible|shown|showing|displayed|rendered|applied|saved|updated|fixed|deployed|pushed|created|added|removed|` +
		`changed|loaded|sent|triggered)\b`)

	// sentenceEndRE: the end of a sentence (the dot of ".html" ends none).
	sentenceEndRE = regexp.MustCompile(`[.!?;:](?: |$)`)

	// purposeRE: the words that open a clause of purpose ("so that it is not
	// shown" asks for it).
	purposeRE = regexp.MustCompile(`\b(?:so that|such that|in order that|ensure|ensures|make it so)\b`)

	// accessRE: handing over a credential, logging in, granting access, the acts
	// of a VPN or a second factor. A word like "login", "sso" or "password" in
	// a ticket's subject is none of these, so a noun alone is not meant to
	// match: it takes a verb that hands the credential over, a possessive, or a
	// login to do. Some sentences about a secret still match ("api keys are
	// rotated every 30 days"); the labelled set measures how often.
	accessRE = regexp.MustCompile(`\b(?:` + strings.Join([]string{
		// handing a credential over, or saying what is wrong with it
		`(?:here(?:'s| is| are)|there(?:'s| is| are)|this is|it(?:'s| is)) (?:my |the |your |our |a |an |the new |a new )?` + secrets,
		secrets + ` (?:is|are|was)`,
		secrets + `[:=] ?\w+`,
		`(?:my|your|our) ` + secrets,
		secrets + ` (?:expired|has expired|changed|was rotated|is wrong|is invalid|stopped working)`,
		`you (?:now |already )?have (?:[a-z0-9]+ ){0,3}` + secrets,
		// a secret by its variable's name, or the secrets set up for the agent
		`(?:here(?:'s| is)|there(?:'s| is| are)|use|i(?:'ve| have)? (?:set|added|exported|defined|put)) (?:the |an? |my |your |our )?` + keyVarName,
		keyVarName + ` (?:is |are )?(?:now )?(?:defined|set|exported|added|available)`,
		`(?:added|exported|stored|saved|i(?:'ve| have)? (?:set|put|added|exported)) (?:the |my |new |all |a |an |your |our )?` + envVars,
		`^(?:the )?` + envVars + ` (?:are |is )?(?:now )?(?:set|added|exported|in place)`,
		// logging in is something to do
		`log in (?:to|into|with|again|first|as)`, `log into`, `sign in (?:to|into|with|again|as)`, `sign into`,
		`(?:you(?:'re| are)|i(?:'m| am)) (?:now )?logged in`, `re-?authenticate`,
		// granting access or permissions
		`(?:gave|give|given|granted|grant|giving) (?:you|it|the agent|the factory)(?: \w+)? access`,
		`you (?:now )?have(?: \w+)? access`, `access (?:granted|for you)`,
		`(?:added|granted|gave|given) (?:you |it |the (?:agent|app|bot) )?(?:the |new |more |full |admin |write |read )*permissions? (?:to|for)`,
		// connecting to a server is something to do
		`(?:^|you |please |, ?)(?:connect|ssh) (?:to|into)`,
		// the acts of a VPN or a second factor
		`(?:connect(?:ed)?|turn(?:ed)? on|join(?:ed)?|start(?:ed)?) (?:to )?(?:the |my |a )?vpn`,
		`(?:approve[ds]?|accept(?:ed)?|confirm(?:ed)?) (?:the |a |your |my )?(?:2fa|mfa|two[- ]factor|sso|sign[- ]in|login)(?: prompt| request| push)?`,
	}, `|`) + `)\b`)

	// approvalRE: a short yes, alone or with a few words, or "confirm" alone.
	// A go-ahead ("go ahead", "do it") is a step start, which is read first.
	approvalRE = regexp.MustCompile(`^(?:yes|yep|yeah|yup|y|ok|okay|sure|fine|good|great|perfect|nice|agreed|approved?|lgtm|looks good|` +
		`sounds good|that works|correct|right|exactly|thanks|thank you)\b(?:[ ,.!;:-]+[a-z']+){0,5}[ .!]*$|^confirm(?:ed)?[ .!]*$`)
)
