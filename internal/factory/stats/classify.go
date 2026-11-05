package stats

// The message classifier: what an operator's message was for. The factory's
// stats count the messages that only start a step (the target is zero), and
// the baseline runs the same patterns over the operator's old transcripts, so
// both sides of the comparison are counted by one rule.

import (
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// Classify returns the class of a message: step_start (it only starts a step:
// open the PR, push, merge the parent in, merge or release into a staging
// branch, take the recommendation, run or test things, resolve the
// conflicts or the review, submit the review, run a skill of such a step
// (/resolve-review, /create-pr), or let the agent go on: "go ahead",
// "implement it", "apply", "next step", "fix it", "do it", a bare "go",
// after any yes or none), approval (a yes: "yes", "ok", "sounds good",
// "confirm" alone or with "both", or a choice among the options the agent
// offered: "go with B", "option 2"), correction (the agent was wrong or must
// do it otherwise: "make sure ...", "you do that", "no need to ...", "don't
// push", "the summary was not posted", "I don't see you ...", "instead", "I
// said", "still broken"), access (credentials, a login, permission, a server
// to connect to) or info (anything else, which is also the class of an empty
// message, of a slash command of no step and of pasted output). It reads the
// words alone, the operator's own (ownWords: not the wrapper a tool puts
// around them, not the output they paste), in this order: a message that
// hands over a credential or grants access is access whatever else it says,
// and one that refuses a step ("don't push yet") is a correction, not a step
// start; a guard ("... and make sure they pass") or a report of what is not
// done ("the branch isn't pushed, push it") yields to the step the message
// asks for. Only the acts count: "log in to the sandbox" is access, "fix the
// login page" is information, "push it" starts a step and "the push failed"
// does not, what a thing should (not) be ("the export should be
// configurable") is no correction, and neither is a wish or a fact ("I don't
// want to expose it", "don't need this alert"). Stagings live in the
// operator's config, not here: any staging-<name> branch counts. The classes
// are the registry's; the factory's engine stores the class of your words
// with the input when it records them.
func Classify(text string) string {
	if name, ok := agents.SlashCommand(text); ok {
		if stepCommandRE.MatchString(name) {
			return registry.ClassStepStart
		}
		return registry.ClassInfo
	}
	t := normalize(keyVarRE.ReplaceAllString(ownWords(text), " "+secretVar+" "))
	switch {
	case t == "":
		return registry.ClassInfo
	case accessRE.MatchString(t):
		return registry.ClassAccess
	case refuses(t), correctionRE.MatchString(t), youDoThe(t), stillHave(t):
		return registry.ClassCorrection
	case startsAStep(t):
		return registry.ClassStepStart
	case guardRE.MatchString(t), notDone(t):
		return registry.ClassCorrection
	case approvalRE.MatchString(t):
		return registry.ClassApproval
	}
	return registry.ClassInfo
}

// ownWords are the operator's own words in text: after the request header
// a tool puts before them ("# In app browser: ... ## My request for Codex:"),
// without the blocks a tool adds (<bash-input>, <system-reminder>, an
// <ide-context>), the placeholders of an attachment ("[Image #2]") and the
// lines of pasted output (a stack frame, a failing spec, a warning, an
// error's heading).
func ownWords(text string) string {
	if locs := requestHeaderRE.FindAllStringIndex(text, -1); len(locs) > 0 {
		text = text[locs[len(locs)-1][1]:]
	}
	text = toolBlockRE.ReplaceAllString(text, " ")
	text = placeholderRE.ReplaceAllString(text, " ")
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !outputLineRE.MatchString(l) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
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
	return stepStartRE.MatchString(t) || pushRE.MatchString(t) || ownStepRE.MatchString(t) || goOnRE.MatchString(t) ||
		doItRE.MatchString(t) || testRE.MatchString(t)
}

// refuses reports whether the normalized message opens with a refusal: a
// "don't" or "do not" that orders the agent ("don't push", "do not touch"),
// not one that says what the operator wants, needs or knows ("don't need
// this alert").
func refuses(t string) bool {
	m := refusalRE.FindStringSubmatch(t)
	return m != nil && !stateVerbs[m[1]]
}

// stateVerbs say what the operator wants, needs or knows after a "don't".
var stateVerbs = map[string]bool{"need": true, "needs": true, "want": true, "know": true, "have": true, "think": true, "care": true,
	"mind": true, "see": true, "like": true, "remember": true, "understand": true, "get": true, "believe": true, "expect": true}

// youDoThe reports whether the normalized message tells the agent to do a
// part itself ("you do the testing"): "you do the rest" lets it go on.
func youDoThe(t string) bool {
	m := youDoTheRE.FindStringSubmatch(t)
	return m != nil && m[1] != "rest"
}

// stillHave reports whether the normalized message says the operator still
// has what the agent should have taken away ("I still have the old ruby"),
// not a question they still have.
func stillHave(t string) bool {
	m := stillHaveRE.FindStringSubmatch(t)
	return m != nil && !stillQuestions[m[1]]
}

// stillQuestions are what the operator may still have that is no fault.
var stillQuestions = map[string]bool{"question": true, "questions": true, "doubt": true, "doubts": true, "concern": true, "concerns": true}

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

// yesWords are the words of a yes: alone they approve (approvalRE), before a
// go-ahead they only open it (lead).
const yesWords = `yes|yep|yeah|yup|y|ok|okay|sure|fine|good|great|perfect|nice|cool|alright|agreed|approved?|lgtm|looks good|` +
	`sounds good|that works|correct|right|exactly|thanks|thank you`

// lead is what may open a message before its own words: a yes of any kind, a
// then ("sure, go ahead", "now apply").
const lead = `^(?:(?:` + yesWords + `|so|then|now|please|and)(?: to me)?[ ,.!;:-]+)*`

// shortTail is what may follow a go-on verb that would otherwise lead a new
// request ("implement it", "apply the suggestions", "implement, also ..."),
// unlike "implement a retry for the webhook".
const shortTail = `(?:[ .!]*$|[,;:.!?]|\s+(?:it|them|this|that|these|those|option [a-z0-9]+|all(?: of (?:it|them))?|both|everything|` +
	`now|please|and|then|too|first|as (?:planned|proposed|suggested|recommended|discussed)|` +
	`(?:(?:the|your|my|these|those|all|all the) )?(?:plan|fix|fixes|changes?|suggestions?|recommendations?|edits?|patch|diff|rest))\b)`

// bareVerb is a verb of going on that ends the message ("go", "..., retry").
const bareVerb = `(?:go|do|add|apply|implement|proceed|continue|fix|build|commit|retry)(?: (?:it|now|please|again))*[ .!]*$`

// secrets are the nouns of what you hand an agent to let it in.
const secrets = `(?:password|passcode|credentials?|api[ -]?keys?|(?:access|auth|bearer|secret|api) tokens?|secret keys?|ssh keys?|` +
	`(?:2fa|mfa|otp|one[- ]time|verification|sms|login|auth) (?:codes?|passwords?)|otp)`

// secretVar stands, in the normalized text, for a secret named by its
// environment variable (keyVarRE).
const secretVar = "secret_var"

// envVars are the secrets and environment variables you set up for an agent.
const envVars = `(?:secrets|env(?:ironment)? ?var(?:iable)?s?)`

// vaults are where a secret is kept.
const vaults = `(?:vault|1password|keychain|bitwarden|vaultwarden|lastpass|password manager|secrets? manager|env file|\.env[a-z.]*|` +
	`clipboard|mise(?:\.local)?\.toml)`

var (
	// stepStartRE: the phrases that start the next step of the work.
	stepStartRE = phrases(
		`(?:open|create|make|raise|submit|file)(?: me)?(?: (?:a|an|the|new|draft|separate|companion|another|second|follow-?up|small|`+
			`single|one|[a-z0-9]+-[a-z0-9-]+))* (?:prs?|pull requests?)`,
		`(?:merge|update from|rebase (?:on|onto)) (?:the )?(?:latest )?(?:master|main|parent|base|develop)(?: branch)?`,
		`(?:merge|deploy|ship|put)\b[^.!?]{0,40}\bstaging-[a-z0-9][a-z0-9_-]*`,
		`(?:merge|deploy|ship|put)(?: (?:it|this|that|them|everything))? (?:to|on|into) (?:all )?(?:the )?stagings`,
		`merge\b[^.!?]{0,60}\band (?:release|deploy|ship)`,
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

	// ownStepRE: the steps of a PR's life as the message's own verb
	// (ownVerb): resolving the conflicts or the review, a review again,
	// submitting or posting the review, a release to the stagings. "will
	// that post the review?" and "the release notes" are about them.
	ownStepRE = regexp.MustCompile(ownVerb + `(?:resolve (?:the |all )?(?:merge )?conflicts?|` +
		`resolve (?:the |all )?(?:reviews?|review (?:comments|threads|feedback))|re-?review|` +
		`(?:submit|post|publish) (?:the |your |my |a |all )?(?:pending )?(?:review|inline comments|comments)|` +
		`release(?: (?:it|this|that|them|everything))? (?:to|into|on) (?:all )?(?:the )?(?:stagings?|staging-[a-z0-9][a-z0-9_-]*|next staging))\b`)

	// pushRE: push as an act asked for, the message's own verb or the one after
	// a comma, a conjunction, "please" or "can you". "the push failed" and
	// "the pushed commit" are about a push, and start nothing.
	pushRE = regexp.MustCompile(ownVerb + `(?:git )?push(?: |$|[,;:.!?])`)

	// goOnRE: "continue", "go ahead" and their kin start a step only as the
	// message's own words, not inside a sentence about a job that should
	// continue ("how do we proceed?"); "implement", "apply" and "next step"
	// only with a short tail (shortTail), so they never lead a new request.
	goOnRE = regexp.MustCompile(lead + `(?:(?:you|u|just) )?(?:(?:continue|proceed|carry on|go on|move on|go ahead|go for it)\b|` +
		`(?:go implement|implement|next step|apply|build (?:it )?now|build it)` + shortTail + `)`)

	// doItRE: a verb of doing whose object is what the agent proposed ("fix
	// it", "do all of them"), or a bare verb that ends the message ("go",
	// "add", "..., retry"): both let the agent take the step it offered, the
	// first at the message's start, the bare verb also after a comma. A
	// demonstrative is that object only at the end or before a conjunction
	// ("do this first"): "fix this bug in the parser" names a new one.
	doItRE = regexp.MustCompile(lead + `(?:(?:you|u|just) )?(?:` +
		`(?:do|fix|apply|implement|build|add) (?:it|them|both|everything|the rest|all(?: of (?:it|them))?)\b|` +
		`(?:do|fix|apply|implement|build|add) (?:all (?:of )?)?(?:this|that|these|those)(?:[ ,]+(?:and|then|too|now|please|first)\b|[ .!]*$)|` +
		bareVerb + `)|[,;.!] (?:(?:and|then|so|now|please|just) )*` + bareVerb)

	// testRE: "test" as the message's own verb with what to test ("test
	// mailer, jobs and the api"), never the noun ("the test fails", "test
	// fails").
	testRE = regexp.MustCompile(lead + `(?:re-?test|test) (?:it|this|that|them|everything|all|again|both|locally|manually|the|in|on|with|` +
		`backend|frontend|api|ui|[a-z]+, [a-z]+)\b`)

	// stepCommandRE: the slash commands of a skill that runs a step of the
	// work (resolve-review in every variant, review, create-pr, deploy,
	// merge-parent, fix-ci), with or without a plugin's prefix (/t:deploy).
	stepCommandRE = regexp.MustCompile(`(?i)^/(?:[a-z0-9_-]+:)?(?:resolve-reviews?(?:-[a-z0-9]+)*|(?:code-)?review(?:-?[0-9]+)?|` +
		`create-pr|deploy|merge-parent|fix-ci)$`)

	// requestHeaderRE: the header a tool puts before the operator's words
	// ("## My request for Codex:").
	requestHeaderRE = regexp.MustCompile(`(?im)^#{1,6}[ \t]*my request for [^\n:]*:[ \t]*\n?`)

	// toolBlockRE: the blocks a tool adds around or beside the words.
	toolBlockRE = regexp.MustCompile(`(?s)<(?:bash-input|system-reminder|[a-z][a-z-]*-context)\b[^>]*>.*?</(?:bash-input|system-reminder|[a-z][a-z-]*-context)>`)

	// placeholderRE: what stands in for an attachment ("[Image #2]").
	placeholderRE = regexp.MustCompile(`\[(?:Image|Pasted text|File)[^\]]*#\d+[^\]]*\]`)

	// outputLineRE: a line of pasted output: a stack frame or a failing spec
	// (a file and its line), the heading of a failure, an error or a warning.
	outputLineRE = regexp.MustCompile(`(?i)^\s*(?:` + strings.Join([]string{
		`#?\s*\.{0,2}/?[\w./-]+\.(?:rb|js|jsx|ts|tsx|py|go|php|java|kt|rs|ex|exs|cs|erb|vue)(?::\d+)+`,
		`from \S+:\d+`, `at \S+ \(\S+:\d+:\d+\)`, `\(\w+\):\d+:in `, `rspec \./\S+:\d+`, `traceback \(most recent call last\)`,
		`\d+ examples?, \d+ failures?`,
		`(?:failures?|errors?|failed examples?|failure/error|deprecation warning|deprecationwarning|warning|warn|error|fatal|panic|exception)\s*:`,
	}, "|") + `)`)

	// keyVarRE: a secret named by its environment variable, upper case
	// (POINTS_API_KEY, SHOP_TOKEN), not a code word (cache_key, csrf_token).
	keyVarRE = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*_(?:API_KEY|API_TOKEN|ACCESS_TOKEN|AUTH_TOKEN|SECRET_KEY|TOKEN|KEY|SECRET|PASSWORD)\b`)

	// refusalRE: a "don't" or "do not" that opens the message, and its verb.
	refusalRE = regexp.MustCompile(`^(?:don't|do not|dont)\b ?([a-z]*)`)

	// stillHaveRE: "I still have ...", and what.
	stillHaveRE = regexp.MustCompile(`\bi still have (?:an? |one |a few |some )?([a-z]+)`)

	// youDoTheRE: "you do the <part>" opening the message.
	youDoTheRE = regexp.MustCompile(`^(?:you|u) do the ([a-z]+)`)

	// correctionRE: the agent was wrong, or must do it otherwise. A refusal
	// at the start of the message counts (refuses); a negation in the middle
	// does not ("push it, don't wait" starts a step). What a thing should or
	// should not be ("the export should be configurable") is a request, not
	// a correction; that something still fails or looks wrong, that the
	// agent should do it itself ("you do that") or otherwise ("instead", "I
	// said", "no need to", a "make sure" that opens the message) is one.
	correctionRE = regexp.MustCompile(`^(?:no|nope|wrong|not that|actually|never)\b|` +
		`^(?:you|u) do (?:that|it|this|them)\b|` +
		`^(?:(?:make it|keep it|be|much|even|way|a bit) )?(?:shorter|simpler|clearer|briefer|more concise|less verbose)(?:[ ,]+[a-z']+){0,3}[ .!]*$|` +
		`^make sure\b|` +
		`\b(?:that's not|that is not|this is not|is wrong|are wrong|isn't right|not what i (?:asked|meant|wanted)|` +
		`you (?:forgot|missed|broke|didn't|did not|should have|shouldn't have|should not have)|` +
		`instead|rather than|revert|undo|roll ?back|i said|no need (?:to|for)|` +
		`i(?:'m| am) not (?:forcing|asking|telling) you|i (?:don't|do not|didn't|did not|can't|cannot) see you|` +
		`still (?:fail|fails|failing|failed|broken|wrong|not|doesn't|does not|don't|isn't|can't|cannot|shows?|showing|happens|happening)|` +
		`i still (?:see|get)|` +
		`your (?:fix|fixes|change|changes|code|commit|edits?|patch)(?: [a-z']+){0,2} (?:breaks?|broke|caused?|causes)|` +
		`(?:might|may|could) be (?:because of |caused by |from )?your (?:fix|fixes|change|changes|code|commit|edits?|patch)|` +
		`looks? (?:wrong|off|broken|bad|weird|odd|ugly|oversized|undersized|too (?:big|small|large|wide|narrow|long))|` +
		`wrong (?:file|table|branch|place|one|approach|way))\b`)

	// guardRE: a "make sure" after the message's own words ("run the specs
	// and make sure they pass"): a correction only when the message asks for
	// no step.
	guardRE = regexp.MustCompile(`(?:[,;:.!?] ?|\b(?:and|but|also|please|just|pls) )make sure\b`)

	// notDoneRE: something is not so (notDone).
	notDoneRE = regexp.MustCompile(`\b(?:was|were|is|are)(?:n't| not)(?: yet)? (?:[a-z]+ )?` +
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
		`(?:here(?:'s| is| are)|this is|it(?:'s| is)) (?:my |the |your |our |a |an |the new |a new )?` + secrets,
		`there(?:'s| is| are) (?:my |the |your |our |a |an |the new |a new )?` + secrets + `(?:$| (?:in|on|at|for)\b|[:=] ?\w)`,
		secrets + ` (?:is|are|was)`,
		secrets + `[:=] ?\w+`,
		`(?:my|your|our) ` + secrets,
		secrets + ` (?:expired|has expired|changed|was rotated|is wrong|is invalid|stopped working)`,
		`you (?:now |already )?have (?:[a-z0-9]+ ){0,3}` + secrets,
		`(?:the |my |your |our |an? )?(?:tokens?|keys?|passwords?|credentials?|` + secrets + `) (?:is|are|was) (?:now )?(?:in|on) ` +
			`(?:the |my |your |our )?` + vaults,
		`(?:^|(?:the|my|your|api|access|auth) )(?:keys?|tokens?|passwords?|codes?)(?: is| was| are| were)? (?:copied|pasted|in (?:the |your |my )?clipboard)`,
		`copied (?:the |my |a |an |your )?(?:api |access |auth )?(?:keys?|tokens?|passwords?|codes?)`,
		// a secret by its variable's name, or the secrets set up for the agent
		`(?:here(?:'s| is)|there(?:'s| is| are)|use|i(?:'ve| have)? (?:set|added|exported|defined|put)) (?:the |an? |my |your |our )?` + secretVar,
		secretVar + ` (?:is |are )?(?:now )?(?:defined|set|exported|added|available)`,
		`(?:added|exported|stored|saved|i(?:'ve| have)? (?:set|put|added|exported)) (?:the |my |new |all |a |an |your |our )?` + envVars,
		`^(?:the )?` + envVars + ` (?:are |is )?(?:now )?(?:set|added|exported|in place)`,
		// logging in is something to do
		`log in (?:to|into|with|again|first|as)`, `log into`, `sign in (?:to|into|with|again|as)`, `sign into`,
		`(?:you(?:'re| are)|i(?:'m| am)) (?:now )?logged in`, `re-?authenticate`,
		`(?:^|(?:so|then|and|now|ok|okay) )i(?:'ll| will|'m going to| am going to)? (?:log|sign) ?in`,
		// granting access or permissions
		`(?:gave|give|given|granted|grant|giving) (?:you|it|the agent|the factory)(?: \w+)? access`,
		`you (?:now )?have(?: \w+)? access`, `access (?:granted|for you)`,
		`(?:added|granted|gave|given) (?:you |it |the (?:agent|app|bot) )?(?:the |new |more |full |admin |write |read )*permissions? (?:to|for)`,
		// connecting to a server is something to do
		`(?:^|you |please |, ?)ssh (?:to|into)`,
		`(?:^|you |please |, ?)connect (?:to|into) (?:the |a |my |our )?(?:[a-z0-9_.-]+ ){0,2}` +
			`(?:server|box|host|instance|vpn|db|database|console|machine|cluster|ssh|prod|production|staging|sandbox)`,
		// the acts of a VPN or a second factor
		`(?:connect(?:ed)?|turn(?:ed)? on|join(?:ed)?|start(?:ed)?) (?:to )?(?:the |my |a )?vpn`,
		`(?:approve[ds]?|accept(?:ed)?|confirm(?:ed)?) (?:the |a |your |my )?(?:2fa|mfa|two[- ]factor|sso|sign[- ]in|login)(?: prompt| request| push)?`,
	}, `|`) + `)\b`)

	// approvalRE: a short yes, alone or with a few words; "confirm" alone or
	// with what it confirms ("confirm both"); a choice among the options the
	// agent offered ("go with B", "take option 2", "option 2"), which
	// startsAStep reads first when it names the recommendation. A go-ahead
	// ("go ahead", "do it") is a step start, which is read first.
	approvalRE = regexp.MustCompile(`^(?:` + yesWords + `)\b(?:[ ,.!;:-]+[a-z']+){0,5}[ .!]*$|` +
		`^confirm(?:ed)?(?: (?:both|all|it|them|that|this|all of them))?[ .!]*$|` +
		lead + `(?:let's |i'd |i'll |we'll )?(?:go with|pick|choose|take) (?:option |variant |approach |plan |choice )?` +
		`(?:[a-e]|[0-9]+|#[0-9]+|both|first|second|third|last|the (?:first|second|third|last|former|latter|simpler|simplest|safer|smaller)` +
		`(?: one| option| approach| variant)?)[ .!]*$|` +
		lead + `(?:let's |i'd |we'll )?go with (?:[a-z0-9'-]+ ?){1,4}[ .!]*$|` +
		`^(?:option|variant|approach|choice) (?:[a-e]|[0-9]+)[ .!]*$`)
)
