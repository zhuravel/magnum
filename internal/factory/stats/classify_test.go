package stats_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

// The messages that only start a step are the ones the factory exists to
// make unnecessary: opening the PR, pushing, merging the parent in, merging
// into a staging branch, taking the recommendation.
func TestClassifyKnowsTheStepStartingMessages(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"open PR",
		"Open a PR",
		"please open the pull request now",
		"create a pull request",
		"push",
		"push it",
		"git push origin HEAD",
		"ok, push the branch",
		"merge master",
		"Merge the parent branch and run the specs",
		"merge into staging-void",
		"merge it into staging-void please",
		// stagings live in config, so any staging-<name> branch counts
		"merge into staging-zeta",
		"deploy this to staging-bastion",
		"do as recommended",
		"Do it as recommended",
		"go with your recommendation",
		// a word of the ticket's subject beside the act does not hide it
		"open PR for the login fix",
		"merge master and push",
		"fix the password reset, then push it",
		"can you push it?",
		"git push",
		// ruling: a go-ahead starts the step it lets go
		"go ahead",
	} {
		if got := stats.Classify(text); got != registry.ClassStepStart {
			t.Errorf("Classify(%q) = %q, want %q", text, got, registry.ClassStepStart)
		}
	}
}

// Every class is told from the words that carry it, and a message that
// names a step but refuses it is a correction, not a step start.
func TestClassifyTellsTheOtherClasses(t *testing.T) {
	t.Parallel()
	for want, texts := range map[string][]string{
		registry.ClassApproval: {"yes", "Yes, that works", "ok", "lgtm", "looks good", "sounds good to me"},
		registry.ClassCorrection: {
			"no, that is the wrong table",
			"wrong file",
			"don't push yet",
			"no, do not open the PR",
			"that's not what I asked for",
			"you forgot the migration",
			"use the ledger table instead",
			"the spec still fails",
		},
		registry.ClassAccess: {
			"I gave you access to the sandbox",
			"the password is in the vault",
			"log in with my account",
			"here is the 2FA code: 000000",
			"your api key expired, use the new one",
			"log in to the sandbox first",
			"I granted you access to the staging console",
			"you have access now",
			"you are logged in as me",
			"connect to the vpn and try again",
			"I approved the 2fa prompt",
			"use my credentials",
		},
		registry.ClassInfo: {
			"the refund path only matters for expired points",
			"what does the sync job do?",
			"the push notification text lives in the mailer",
			"this table has 3 million rows",
			"",
			"   ",
			// the words of a ticket's subject are not acts
			"fix the login page",
			"the signin button is broken",
			"the user has no access to the report",
			"the sso redirect drops the cart",
			"the password reset email is late",
			"the vpn docs need a new section",
			"the credentials form shows twice",
			"the push failed on the worker",
			"push notifications arrive late",
			"the pushed commit is on the branch",
		},
	} {
		for _, text := range texts {
			if got := stats.Classify(text); got != want {
				t.Errorf("Classify(%q) = %q, want %q", text, got, want)
			}
		}
	}
}

// The everyday words of the operator's old sessions, each beside a message
// that shares its words and must not take its class. Ruling (2026-11-05):
// "go ahead" and the bare verbs of going on start a step; "yes", "ok",
// "sounds good" and "confirm" alone stay approvals. A correction says the
// agent was wrong or must do it otherwise. The phrases are general, never a
// match of one message.
func TestClassifyKnowsTheEverydayStepStartsAndCorrections(t *testing.T) {
	t.Parallel()
	step, approval, correction, access, info := registry.ClassStepStart, registry.ClassApproval, registry.ClassCorrection,
		registry.ClassAccess, registry.ClassInfo
	for _, c := range []struct{ text, want string }{
		// the go-aheads and the bare verbs of going on
		{"go ahead", step},
		{"did the customer give the go ahead?", info},
		{"go implement", step},
		{"where did the implementation go?", info},
		{"implement", step},
		{"implement, also add the seed data", step},
		{"the implementation is in lib/points", info},
		{"proceed", step},
		{"how do we proceed with the refund?", info},
		{"next step", step},
		{"what is the next step?", info},
		{"apply", step},
		{"apply the changes, keep the old ones", step},
		{"you apply and tell me what changed", step},
		{"the discount does not apply to archived points", info},
		{"fix it", step},
		{"the fix is in the branch", info},
		{"do it", step},
		{"ok, do it now", step},
		{"do all of them", step},
		{"fix both", step},
		{"do they all fail?", info},
		{"do this first", step},
		{"fix this bug in the parser first", info},
		{"build it", step},
		{"build now", step},
		{"the build failed on the worker", info},
		{"go", step},
		{"do", step},
		{"add", step},
		{"go figure", info},
		// running and testing things
		{"test mailer, jobs and the api", step},
		{"test it in the browser", step},
		{"the test fails on the worker", info},
		{"tests are slow on the worker", info},
		{"rerun the backend tests", step},
		{"the specs run every night", info},
		{"does the cron run its review step?", info},
		{"migrate the test database", step},
		{"the migration of the test database is slow", info},
		{"resolve conflicts", step},
		{"merge master and resolve the conflicts", step},
		{"the conflicts are in the schema file", info},
		{"the bot could not resolve conflicts", info},
		{"post your comments and submit the review", step},
		{"the review is in the PR", info},
		{"will the bot post the review later?", info},
		// approvals stay approvals
		{"yes", approval},
		{"ok", approval},
		{"sounds good", approval},
		{"confirm", approval},
		{"ok, confirmed", approval},
		{"confirm the totals match on the sandbox", info},
		// corrections: the agent was wrong, or must do it otherwise
		{"make sure the emails use the shop's name", correction},
		{"use the ledger, and make sure the totals match", correction},
		{"I want to make sure the totals add up", info},
		{"you do that", correction},
		{"u do the migration", correction},
		{"do you do that in the job?", info},
		{"no need to ping the team, they know", correction},
		{"keep the old name, no need for a migration", correction},
		{"do we need a migration?", info},
		{"don't drop the old coupons", correction},
		{"the summary was not posted as a comment", correction},
		{"the badge is not visible on mobile", correction},
		{"the webhook isn't working", correction},
		{"was the summary posted?", info},
		{"change the router so that the old menu is not visible", info},
		{"ok, so the badge isn't visible", correction},
		{"check the page first, I don't see you opening it", correction},
		{"I don't see the point of a second table", info},
		{"I'm not forcing you to keep the cache, compare the options", correction},
		{"I'm not an admin of the workspace", info},
		{"instead, write the totals to the ledger", correction},
		{"the git insteadOf rule rewrites the url", info},
		{"I said the ledger table", correction},
		{"the customer said it fails on Tuesdays", info},
		{"the page still shows the old total", correction},
		{"I still see the old badge", correction},
		{"still waiting for the import?", info},
		{"is the import still running?", info},
		{"your fix probably breaks the export", correction},
		{"the old banner came back, it might be your changes", correction},
		{"your fix is in the release notes", info},
		{"the logo looks too big on mobile", correction},
		{"looks like the job runs every night", info},
		{"shorter", correction},
		{"shorter, with fewer bullets", correction},
		{"shorter names live in the glossary file", info},
		// a request of what a thing should be is no correction
		{"the limit should be configurable per shop", info},
		{"owners should be able to turn it off", info},
		// access
		{"you have the sandbox api key, try it", access},
		{"you have two tasks left", info},
		{"there's POINTS_API_KEY in the env file", access},
		{"rename the POINTS_API_KEY variable in the docs", info},
		{"I added secrets to the sandbox", access},
		{"the env vars are set now", access},
		{"set the env var DEBUG to see more logs", info},
		{"I granted new permissions to the app", access},
		{"we added permission checks to the api", info},
		{"ssh into the shop box and read the logs", access},
		{"you connect to the sandbox db and count the rows", access},
		{"the worker fails to connect to the database", info},
	} {
		if got := stats.Classify(c.text); got != c.want {
			t.Errorf("Classify(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// The review's and the held-out check's gaps, each rule beside a near miss
// that keeps its old class: a go-ahead after any approval word, a guard
// ("make sure ...") or a report of what is not done that yields to the step
// the message asks for, a go-on verb that leads a new request, the words of
// a wrapped message, the slash commands of a step, the steps of a PR's
// life, choosing an option, handing over a login, a wish or a fact that is
// no refusal, and pasted output.
func TestClassifyKnowsTheReviewsAndTheHeldOutChecksGaps(t *testing.T) {
	t.Parallel()
	step, approval, correction, access, info := registry.ClassStepStart, registry.ClassApproval, registry.ClassCorrection,
		registry.ClassAccess, registry.ClassInfo
	for _, c := range []struct{ text, want string }{
		// a go-ahead after any approval word starts the step; the word alone approves
		{"sure, go ahead", step},
		{"sounds good, implement it", step},
		{"looks good, go ahead", step},
		{"lgtm, proceed", step},
		{"thanks, continue", step},
		{"yeah, fix it", step},
		{"sure", approval},
		{"sounds good", approval},
		{"lgtm, thanks", approval},
		// a guard yields to the step the message asks for
		{"run the specs and make sure they pass", step},
		{"push it and make sure the build is green", step},
		{"open PR, make sure the description lists the steps", step},
		{"use the ledger, and make sure the totals match", correction},
		// what is not done yields to the step asked for, and a remark is no correction
		{"the branch isn't pushed yet, push it", step},
		{"the email wasn't sent, retry", step},
		{"1. not now 2. fix both in this PR", info},
		{"the summary was not posted as a comment", correction},
		{"it has not been deployed yet, so we can't check it", info},
		// a go-on verb leads a new request only with a short tail
		{"implement a retry for the webhook with exponential backoff", info},
		{"apply the same rounding to the mailer totals", info},
		{"next step is to find why the job hangs", info},
		{"implement it", step},
		{"implement the plan", step},
		{"apply the suggestions", step},
		{"apply changes", step},
		// you do the rest is a go-on, a question is no correction
		{"you do the rest", step},
		{"u do the migration", correction},
		{"I still have a question about the refund path", info},
		{"I still have the old badge", correction},
		{"why do I still have the old banner?", correction},
		// code words are no secrets; an upper-case variable is
		{"use the cache_key of the record", info},
		{"the csrf_token is set by the layout", info},
		{"use the foreign_key option and run the migrations", step},
		{"use SHOP_TOKEN from the vault", access},
		{"there's POINTS_API_KEY in the env file", access},
		// "there's" hands over a secret only when it ends there or gives its value or place
		{"there's a password reset bug on the shop page", info},
		{"there's an api key in the vault", access},
		{"there's the password: abc123", access},
		// connecting is access only to a server
		{"connect to the queue and drain it", info},
		{"you connect to the sandbox db and count the rows", access},
		// wrapped messages: only the words after the request header
		{"# In app browser:\n- Current page: the open PR list\n\n## My request for Codex:\nwhy is this list slow?", info},
		{"# Files mentioned by the user:\n\n## shot.png: /tmp/shot.png\n\n## My request for Codex:\nopen PR", step},
		{"<ide-context>open PR page</ide-context>what does this file do?", info},
		{"[Image #2] the logo looks too big on mobile", correction},
		{"# Notes\nopen PR", step},
		// a slash command of a step of the work starts it; others are info
		{"<command-name>/resolve-review</command-name>\n<command-message>resolve-review</command-message>\n<command-args>1234</command-args>", step},
		{"<command-message>t:resolve-review-2</command-message>\n<command-name>/t:resolve-review-2</command-name>\n<command-args></command-args>", step},
		{"<command-name>/review</command-name>\n<command-message>review</command-message>\n<command-args></command-args>", step},
		{"<command-name>/create-pr</command-name>\n<command-message>create-pr</command-message>\n<command-args></command-args>", step},
		{"<command-name>/t:deploy</command-name>\n<command-message>t:deploy</command-message>\n<command-args>zeta</command-args>", step},
		{"<command-name>/merge-parent</command-name>\n<command-message>merge-parent</command-message>\n<command-args></command-args>", step},
		{"<command-name>/fix-ci</command-name>\n<command-message>fix-ci</command-message>\n<command-args></command-args>", step},
		{"<command-name>/reload-skills</command-name>\n<command-message>reload-skills</command-message>\n<command-args></command-args>", info},
		{"<command-name>/review-notes</command-name>\n<command-message>review-notes</command-message>\n<command-args></command-args>", info},
		{"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>push it</command-args>", info},
		{"<bash-input>git push</bash-input>", info},
		// the steps of a PR's life
		{"resolve review", step},
		{"resolve the review on 1234", step},
		{"resolve conflict", step},
		{"rereview", step},
		{"re-review 1234", step},
		{"the rereview found nothing new", info},
		{"release to staging-zeta", step},
		{"release it to the stagings", step},
		{"deploy to stagings", step},
		{"the release notes mention the stagings", info},
		{"merge 1234 and release", step},
		{"create PR in the docs repo", step},
		{"open a separate PR for the docs", step},
		{"the PR description is long", info},
		{"open first if the PR title has two keys", info},
		// choosing an option approves; choosing the recommendation starts the step
		{"confirm both", approval},
		{"yes all", approval},
		{"yes, it", approval},
		{"go with B", approval},
		{"take option 2", approval},
		{"option 2", approval},
		{"go with the second one", approval},
		{"go with your recommendation", step},
		{"take a look at the logs", info},
		// handing over a login
		{"I'll log in", access},
		{"so I log in first", access},
		{"key copied", access},
		{"the token is in 1password", access},
		{"how do I log in?", info},
		{"the cache key copied from the old record is stale", info},
		{"the key is in the wrong column", info},
		// a wish or a fact is no refusal; an order is
		{"I don't want to expose the admin api", info},
		{"don't need this alert", info},
		{"archived points should not count", info},
		{"don't push yet", correction},
		{"do not touch the schema", correction},
		{"you shouldn't have pushed", correction},
		// pasted output is information; your words beside it keep their class
		{"Failures:\n\n  1) Points sync drops archived rows\n     Failure/Error: expect(rows).to eq(3)\n     # ./spec/points_spec.rb:12:in `block'", info},
		{"DEPRECATION WARNING: the points helper is deprecated and will be removed", info},
		{"(irb):27:in 'block in <main>': undefined method 'slug' for an instance of Shop (NoMethodError)", info},
		{"the specs still fail, see:\n# ./spec/points_spec.rb:12:in `block'", correction},
	} {
		if got := stats.Classify(c.text); got != c.want {
			t.Errorf("Classify(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// Ruling: Classify names no environment. The stagings are the operator's
// config, and the defaults list none.
func TestClassifyHardcodesNoEnvironmentName(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("classify.go")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`\b(void|bart|bastion)\b`).FindString(string(b)); m != "" {
		t.Errorf("classify.go names the environment %q", m)
	}
}

// The classes Classify returns are the ones a registry input can carry, so
// the engine can store them with ClassifyInput.
func TestClassifyReturnsOnlyClassesTheRegistryStores(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	task := createTask(t, r, "PR-12301")
	id, err := r.RecordInput(t.Context(), registry.Input{TaskID: task.ID, Kind: registry.InputPaneTyped, Text: "open PR"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"open PR", "yes", "wrong", "api key", "hello"} {
		if err := r.ClassifyInput(t.Context(), id, stats.Classify(text)); err != nil {
			t.Errorf("ClassifyInput(Classify(%q)): %v", text, err)
		}
	}
}
