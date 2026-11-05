package tui

import "time"

// The card fixtures are the data of the spec's card mockups (section 9.C).
// The names are placeholders: Jira keys PR-123xx, GitHub numbers #45xx,
// logins alice, rev-ann and zhuravel, copies talkable.factory2 and 3.

// cardQuestionNow is the clock of the question card: asked 41m ago.
var cardQuestionNow = time.Date(2026, 11, 3, 14, 12, 0, 0, time.Local)

// cardTaskNow is the clock of the task card, "Task card at 140".
var cardTaskNow = factoryNow

func cardAt(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.Local) }

func askRevAnn() CardOption {
	return CardOption{Label: "Ask rev-ann in Jira (the task waits)", Means: "drafts a comment for you to check", Send: "ask:rev-ann"}
}

// fixtureQuestionCard is "Question card, question 2 of 3": the first question
// is answered, the second is on the card with a recommendation.
func fixtureQuestionCard() QuestionCard {
	return QuestionCard{
		TaskKey: "PR-12345", Title: "Credit expiry reminder email", Kind: "feature",
		Asked: cardQuestionNow.Add(-41 * time.Minute), Current: 1, AsOf: cardAt(14, 12),
		Cursor: 0, Needless: true,
		Questions: []CardQuestion{
			{
				Text: "Who gets the reminder?", Recommended: 0,
				Options: []CardOption{{Label: "Advocates only", Means: "friends get nothing"}, {Label: "Advocates and their friends", Means: "both get the email"}, {Label: "Type your own answer", Own: true}},
				Why:     "the ticket names advocates", Answer: "advocates only", AnsweredAt: cardAt(13, 31),
			},
			{
				Text:    "When does the reminder go out?",
				Context: `The ticket says "before the credit expires" and names no day.`,
				Example: "a credit that expires on Fri 14 Nov.",
				Options: []CardOption{
					{Label: "Seven days before it expires", Means: "the email goes out Fri 7 Nov"},
					{Label: "One day before it expires", Means: "Thu 13 Nov"},
					{Label: "On the day it expires", Means: "Fri 14 Nov, 09:00 merchant time"},
					askRevAnn(),
					{Label: "Type your own answer", Own: true},
				},
				Recommended: 0, Why: "the coupon reminder already uses 7 days, so the two emails match",
			},
			fixtureNoRecommendation(),
		},
	}
}

// fixtureNoRecommendation is "When the evidence gives no clear answer".
func fixtureNoRecommendation() CardQuestion {
	return CardQuestion{
		Text:    "Can a merchant turn it off?",
		Context: "The ticket does not say, and no other campaign email has an off switch.",
		Example: "a merchant opens Settings, then Emails, and looks for the reminder.",
		Options: []CardOption{
			{Label: "Yes: a switch in the email settings, on by default", Means: "the merchant sees a switch"},
			{Label: "No: always on", Means: "no switch anywhere"},
			askRevAnn(),
			{Label: "Type your own answer", Own: true},
		},
		Recommended: -1, Why: "the ticket and the code give no hint",
	}
}

// fixtureNoRecommendationCard has the third question on the card.
func fixtureNoRecommendationCard() QuestionCard {
	c := fixtureQuestionCard()
	c.Questions[1].Answer, c.Questions[1].AnsweredAt, c.Questions[1].TookRecommendation =
		"seven days before it expires", cardAt(13, 40), true
	c.Current, c.Cursor = 2, -1
	return c
}

// needlessReasons are the reasons a needless flag takes, by digit.
var needlessReasons = []string{"the ticket answers it", "the code answers it", "any answer works", "you asked it before"}

// fixtureNeedlessCard is the question card after −: it asks why the
// question is needless.
func fixtureNeedlessCard() QuestionCard {
	c := fixtureQuestionCard()
	c.NeedlessReasons = needlessReasons
	return c
}

// fixtureRecap is "Recap, one y for the batch": all three questions answered,
// the last one by asking rev-ann in Jira.
func fixtureRecap() QuestionCard {
	c := fixtureQuestionCard()
	c.Questions[0].Answer = "advocates only"
	c.Questions[1].Answer = "seven days before it expires (expires Fri 14 Nov → email Fri 7 Nov)"
	c.Questions[1].TookRecommendation = true
	c.Questions[2].Answer = "ask rev-ann in Jira"
	c.Questions[2].Asks = "rev-ann"
	c.Questions[2].Comment = "rev-ann, should merchants be able to turn off the credit expiry reminder,\nor is it always on? The ticket does not say."
	return c
}

// fixtureReadyCard is "Ready card, before the diff was opened", with the
// refinements of revision 1: the FLAGS line, the local review's leftovers
// (one of each kind of the PR body's Known list), the flagged files first
// and every file unread.
func fixtureReadyCard() ReadyCard {
	return ReadyCard{
		TaskKey: "PR-12360", Title: "Custom activities lose points on copy", Kind: "bug",
		Branch: "zhuravel-PR-12360-activity-points-on-copy", Commits: 1, Waited: 12 * time.Minute,
		Cause: "copying a campaign copies its activities before their reward rules",
		Fix:   "copy the reward rules first, then point each activity at its copy",
		Proof: `spec "copies reward rules with the activities": red before (0 points, want 25), green after · 214 specs near the change green · rubocop clean`,
		Flags: []string{
			"not in the plan: spec/factories/reward_rules.rb",
			"an old guard at app/services/campaign_copier.rb:22 (2 years): skips archived campaigns, added when a copy of one lost its points",
			"CI jobs not run here: ci.yml: jest, ci.yml: cypress",
		},
		Files: []CardFile{
			{Path: "app/services/campaign_copier.rb", Added: 31, Removed: 6, InPlan: true, Flagged: true},
			{Path: "spec/services/campaign_copier_spec.rb", Added: 15, InPlan: true},
			{Path: "spec/factories/reward_rules.rb", Added: 2},
		},
		Doubts: []string{"the writer was unsure if archived campaigns copy too: not tested"},
		Leftovers: []string{
			"P3 app/services/campaign_copier.rb:18: the name copy_rules hides that it also points the activities at the copies",
			"declined: P2 spec/services/campaign_copier_spec.rb:40: one spec per reward rule. The risk of the suggested change: 3 slower specs that prove the same thing",
			"a doubt: P2 app/services/campaign_copier.rb:31: archived rules may be skipped (it came back twice after its fix)",
			"a round of the review went without codex-review's report",
		},
		Unread:      []string{"app/services/campaign_copier.rb", "spec/factories/reward_rules.rb", "spec/services/campaign_copier_spec.rb"},
		Screenshots: 2, ScreenshotsNote: "from the copy's own server: the copied campaign shows 25 points",
		Recommended: 0, Why: "the spec proves the fix; the file outside the plan is test data for it",
		Cursor: -1,
	}
}

// fixtureReadyCardAfter is "Ready card, after the diff was opened", with one
// file left unread and the question of the push asked, which names what the
// push frees.
func fixtureReadyCardAfter() ReadyCard {
	c := fixtureReadyCard()
	c.DiffOpened, c.Cursor = cardAt(14, 21), 0
	c.Unread = []string{"spec/services/campaign_copier_spec.rb"}
	c.Ask = "Push zhuravel-PR-12360-activity-points-on-copy and open a PR?"
	c.Frees = "talkable.factory3; PR-12395 starts in it"
	return c
}

// sendBackReasons are the reason keys of a send back, by digit.
var sendBackReasons = []string{"wrong cause or fix", "changed too much", "missed what I said", "tests too thin"}

// fixtureReadyCardSendBack is the ready card after "send back": it asks for
// the reason's key before the note.
func fixtureReadyCardSendBack() ReadyCard {
	c := fixtureReadyCardAfter()
	c.Ask, c.Cursor, c.Reasons = "", 1, sendBackReasons
	return c
}

// fixtureTaskCard is "Task card at 140", cut to the sections of milestone 1a.
func fixtureTaskCard() TaskCard {
	st := func(name, state, proof string, took time.Duration, h, m int) CardStage {
		s := CardStage{Name: name, State: state, Proof: proof, Took: took}
		if h > 0 {
			s.Finished = cardAt(h, m)
		}
		return s
	}
	return TaskCard{
		Key: "PR-12330", Kind: "bug", Title: "Advocate offer email locale", PR: "#4527",
		Branch: "zhuravel-PR-12330-offer-email-locale", Copy: "copy 2 of 3, server off",
		Started: cardAt(8, 40), YourTime: 6 * time.Minute,
		WaitingFor: "rev-ann: review of #4527, requested 3h ago · meanwhile the factory moves bastion to your latest head (no agent)",
		Stages: []CardStage{
			st("start", "done", "Jira PR-12330 read · bug, from the Jira type", 0, 8, 40),
			st("investigate", "done", "reproduced with a failing spec · 2 agents found the same cause", 24*time.Minute, 9, 4),
			st("questions", "skipped", "skipped: nothing was open", 0, 0, 0),
			st("coding", "done", "spec red before, green after · 3 files +22 −4", 31*time.Minute, 9, 35),
			st("tests", "done", "216 specs green · rubocop clean · local review: 2 rounds, nothing blocks", 9*time.Minute, 9, 44),
			st("ready to push", "done", "you read the diff 09:52 and pushed", 14*time.Minute, 9, 58),
			st("pushed", "done", "draft #4527 · magnum approved round 1 · you marked it ready for review", 71*time.Minute, 11, 9),
			st("review", "waiting", "rev-ann asked 11:09, nothing yet · 0 comments open", 3*time.Hour, 0, 0),
			st("CI", "done", "41 of 41 green at 9c4e2d1 · 3 tries, see CI", 86*time.Minute, 11, 31),
			st("merge master", "done", "not needed: 0 commits behind master", 0, 11, 32),
			st("void", "done", "your latest head deployed · QA ✓ 9 of 9", 52*time.Minute, 12, 24),
			st("bart", "done", "your latest head deployed · no QA on bart", 23*time.Minute, 11, 55),
			st("bastion", "working", "merging your latest head (bastion has an older one)", 2*time.Minute, 0, 0),
			st("merge", "todo", "yours: the factory never merges to master", 0, 0, 0),
			st("cleanup", "todo", "after your merge: free the copy", 0, 0, 0),
		},
		Cursor:     12,
		AgentsNote: "the factory merges bastion itself",
		Agents: []CardAgent{
			{Name: "QA on void", Why: "t:qa on void: 9 of 9 checks, 4 screenshots", State: "done", Output: cardAt(12, 24)},
			{Name: "fixing bart", Why: "kept both changes in the offer mailer", State: "done", Output: cardAt(11, 48)},
			{Name: "fixing CI", Why: "t:fix-ci: the locale fallback in the mailer", State: "done", Output: cardAt(10, 40)},
			{Name: "writer", Why: "wrote the failing spec, then the fix", State: "done", Output: cardAt(9, 44)},
			{Name: "3 investigators", Why: "reproduced it, found the cause, read the history", State: "done", Output: cardAt(9, 4)},
		},
		Report: []string{
			"cause the offer email takes the site's locale, not the advocate's",
			`proof spec "sends the offer email in the advocate's locale": red before, green after`,
			"plan 3 steps, all done · no migration · visible: the email text, 2 screenshots",
		},
		Learned: []string{`magnum's P2 "falls back to the site's locale" → a test case in the replay set`},
	}
}

// fixtureRetryCard is a task that needs the operator because its local
// review did not finish (its tests stage: specs, lint, then the local
// review): a retry goes on at that stage, so the card offers r.
func fixtureRetryCard() TaskCard {
	st := func(name, state, proof string, took time.Duration, h, m int) CardStage {
		s := CardStage{Name: name, State: state, Proof: proof, Took: took}
		if h > 0 {
			s.Finished = cardAt(h, m)
		}
		return s
	}
	return TaskCard{
		Key: "PR-12393", Kind: "bug", Title: "Referral export misses friends' emails",
		Branch: "zhuravel-PR-12393-export-friend-emails", Copy: "copy 1 of 3, server off",
		Started: cardAt(10, 2), YourTime: 2 * time.Minute,
		WaitingFor: WaitsOnYou + ": review unavailable (timeout) · round 2 of the local review ran out of time; r runs it again",
		Stages: []CardStage{
			st("start", "done", "Jira PR-12393 read · bug, from the Jira type", 0, 10, 2),
			st("investigate", "done", "reproduced with a failing spec · 2 agents found the same cause", 19*time.Minute, 10, 21),
			st("questions", "skipped", "skipped: nothing was open", 0, 0, 0),
			st("coding", "done", "spec red before, green after · 2 files +18 −3", 27*time.Minute, 10, 48),
			st("tests", "failed", "88 specs green · rubocop clean · local review: round 1 fixed a P2, round 2 timed out after 60m",
				104*time.Minute, 12, 32),
			st("ready to push", "todo", "after a clean local review", 0, 0, 0),
			st("pushed", "todo", "the factory pushes after your y", 0, 0, 0),
		},
		Cursor: 4, Retry: true,
		Agents: []CardAgent{
			{Name: "writer", Why: "fixed the P2 of round 1", State: "done", Output: cardAt(11, 30)},
			{Name: "3 investigators", Why: "reproduced it, found the cause, read the history", State: "done", Output: cardAt(10, 21)},
		},
		Report: []string{
			"cause the export joins the friends' emails after it pages, so later pages lose them",
			`proof spec "exports the friends' emails of every page": red before, green after`,
		},
	}
}

// fixtureNewTaskJira is "New task: from a Jira key".
func fixtureNewTaskJira(now time.Time) NewTaskForm {
	return NewTaskForm{
		Text:     "PR-12345 the reminder is for advocates only, not their friends",
		Guidance: "the reminder is for advocates only, not their friends",
		Jira: &JiraPreview{
			Key: "PR-12345", Type: "Story", Status: "To Do", Priority: "High", Reporter: "rev-ann",
			Summary: "Credit expiry reminder email",
			Excerpt: "Send advocates a reminder before their referral credit expires.",
			Read:    now.Add(-2 * time.Second), More: true,
		},
		Kind: "feature", KindWhy: "from the Jira type Story", Branch: "zhuravel-PR-12345-credit-expiry-reminder",
		Starts: "now, in talkable.factory2 (free)", Repo: "talkable",
		Agents: []string{"test each point of the ticket against the code", "collect the open questions"},
		Rules: []string{
			"ask before changing an email's words", "seed data into the copy's own databases",
			"screenshots come from the copy's own server",
		},
		Overlap: "1 commit names PR-12345 (a71f0c3 by alice) · #4533 by rev-ann changes app/mailers/credit_mailer.rb",
		Ask:     "Start feature PR-12345 with 2 agents in talkable.factory2?", ShowRules: true,
	}
}

// fixtureNewTaskLinks is a text that starts with a Jira link and holds a
// GitHub PR link and an Actions run link: the key task of the Jira link,
// with the other two offered for later milestones.
func fixtureNewTaskLinks(now time.Time) NewTaskForm {
	f := fixtureNewTaskJira(now)
	f.Text = "https://example.atlassian.net/browse/PR-12345 the same fix as https://github.com/example/talkable/pull/4512; " +
		"CI is red since https://github.com/example/talkable/actions/runs/1234567"
	f.Guidance = "the same fix as https://github.com/example/talkable/pull/4512; " +
		"CI is red since https://github.com/example/talkable/actions/runs/1234567"
	f.Links = []FormLink{
		{Kind: "jira", URL: "https://example.atlassian.net/browse/PR-12345", Offer: "its key, PR-12345"},
		{Kind: "pr", URL: "https://github.com/example/talkable/pull/4512", Offer: "adoption comes in milestone 2"},
		{Kind: "run", URL: "https://github.com/example/talkable/actions/runs/1234567", Offer: "a CI task comes in milestone 3"},
	}
	f.Overlap, f.ShowRules, f.Ask = "", false, ""
	return f
}

// fixtureNewTaskFound is a key found inside the text: the key task of
// PR-12345, with the whole text as the guidance.
func fixtureNewTaskFound(now time.Time) NewTaskForm {
	f := fixtureNewTaskJira(now)
	f.Text = "fix the expiry reminder of PR-12345: advocates only, not their friends"
	f.Guidance = f.Text
	f.Jira.Found = "found in your text"
	f.Overlap, f.ShowRules = "", false
	return f
}

// fixtureNewTaskRefused is a found key Jira refused: the task starts from
// the operator's words, as a prompt task.
func fixtureNewTaskRefused() NewTaskForm {
	return NewTaskForm{
		Text: "the refund of PR-12345 is wrong for advocates who changed their plan",
		Err:  "Jira refused PR-12345: the Jira token file is missing; starts from your words",
		Kind: "bug", KindWhy: `guessed from "is wrong"`,
		Title:  "Refund of PR-12345 is wrong for advocates who changed their plan",
		Branch: "zhuravel-refund-wrong-after-plan-change", Repo: "talkable",
		Starts: "now, in talkable.factory2 (free)",
		Agents: []string{"reproduce it", "find the cause on its own", "read the history of the code"},
		Ask:    `Start bug "Refund of PR-12345 is wrong for advocates…" with 3 agents in talkable.factory2?`,
	}
}

// fixtureNewTaskSeveral names two keys Jira knows: the prompt task they are,
// whose questions ask which issue it is. Its y/N names the task by its
// title, never by a key.
func fixtureNewTaskSeveral() NewTaskForm {
	return NewTaskForm{
		Text: "the copier change of PR-12345 also breaks PR-12360: fix both in one place",
		Keys: []string{"PR-12345", "PR-12360"},
		Kind: "bug", KindWhy: `guessed from "breaks"`,
		Title:  "Copier change of PR-12345 also breaks PR-12360",
		Branch: "zhuravel-copier-change-breaks-copy", Repo: "talkable",
		Starts: "now, in talkable.factory2 (free)",
		Agents: []string{"reproduce it", "find the cause on its own", "read the history of the code"},
		Ask:    `Start bug "Copier change of PR-12345 also breaks PR-12360" with 3 agents in talkable.factory2?`,
	}
}

// fixtureNewTaskReading is the form while the daemon reads the key's issue.
func fixtureNewTaskReading() NewTaskForm {
	return NewTaskForm{Text: "PR-12345 the reminder is for advocates only, not their friends", Reading: true, Repo: "talkable"}
}

// fixtureNewTaskPrompt is "New task: from a free prompt", queued by a full memory.
func fixtureNewTaskPrompt() NewTaskForm {
	return NewTaskForm{
		Text: "The weekly digest email shows last week's numbers for merchants east of UTC+10.\n" +
			"Use the merchant's time zone for the week window. Check a merchant in UTC-11 too.",
		Kind: "bug", KindWhy: `guessed from "shows last week's numbers"`,
		Title:  "Weekly digest uses the wrong week east of UTC+10",
		Branch: "zhuravel-weekly-digest-time-zone", Repo: "talkable",
		Starts: "queued: memory full (swap 91%) · starts when swap is under 80%",
		Agents: []string{"reproduce it", "find the cause on its own", "read the history of the code"},
		Rules: []string{
			"ask before changing an email's words", "seed data into the copy's own databases",
			"screenshots come from the copy's own server",
		},
		Ask: `Queue bug "Weekly digest uses the wrong week…"? It starts when memory allows.`,
	}
}
