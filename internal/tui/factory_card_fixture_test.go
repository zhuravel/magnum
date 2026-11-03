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
	return CardOption{"Ask rev-ann in Jira (the task waits)", "drafts a comment for you to check"}
}

// fixtureQuestionCard is "Question card, question 2 of 3": the first question
// is answered, the second is on the card with a recommendation.
func fixtureQuestionCard() QuestionCard {
	return QuestionCard{
		TaskKey: "PR-12345", Title: "Credit expiry reminder email", Kind: "feature",
		Asked: cardQuestionNow.Add(-41 * time.Minute), Current: 1, AsOf: cardAt(14, 12),
		Questions: []CardQuestion{
			{
				Text: "Who gets the reminder?", Recommended: 0,
				Options: []CardOption{{"Advocates only", "friends get nothing"}, {"Advocates and their friends", "both get the email"}, {"Type your own answer", ""}},
				Why:     "the ticket names advocates", Answer: "advocates only", AnsweredAt: cardAt(13, 31),
			},
			{
				Text:    "When does the reminder go out?",
				Context: `The ticket says "before the credit expires" and names no day.`,
				Example: "a credit that expires on Fri 14 Nov.",
				Options: []CardOption{
					{"Seven days before it expires", "the email goes out Fri 7 Nov"},
					{"One day before it expires", "Thu 13 Nov"},
					{"On the day it expires", "Fri 14 Nov, 09:00 merchant time"},
					askRevAnn(),
					{"Type your own answer", ""},
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
			{"Yes: a switch in the email settings, on by default", "the merchant sees a switch"},
			{"No: always on", "no switch anywhere"},
			askRevAnn(),
			{"Type your own answer", ""},
		},
		Recommended: -1, Why: "the ticket and the code give no hint",
	}
}

// fixtureNoRecommendationCard has the third question on the card.
func fixtureNoRecommendationCard() QuestionCard {
	c := fixtureQuestionCard()
	c.Questions[1].Answer, c.Questions[1].AnsweredAt, c.Questions[1].TookRecommendation =
		"seven days before it expires", cardAt(13, 40), true
	c.Current = 2
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

// fixtureReadyCard is "Ready card, before the diff was opened".
func fixtureReadyCard() ReadyCard {
	return ReadyCard{
		TaskKey: "PR-12360", Title: "Custom activities lose points on copy", Kind: "bug",
		Branch: "zhuravel-PR-12360-activity-points-on-copy", Commits: 1, Waited: 12 * time.Minute,
		Cause: "copying a campaign copies its activities before their reward rules",
		Fix:   "copy the reward rules first, then point each activity at its copy",
		Proof: `spec "copies reward rules with the activities": red before (0 points, want 25), green after · 214 specs near the change green · rubocop clean`,
		Files: []CardFile{
			{"app/services/campaign_copier.rb", 31, 6, true},
			{"spec/services/campaign_copier_spec.rb", 15, 0, true},
			{"spec/factories/reward_rules.rb", 2, 0, false},
		},
		Doubts: []string{
			"the writer was unsure if archived campaigns copy too: not tested",
			"Codex noted one query per rule; fixed in the same commit",
		},
		Screenshots: 2, ScreenshotsNote: "from the copy's own server: the copied campaign shows 25 points",
		Recommended: 0, Why: "the spec proves the fix; the file outside the plan is test data for it",
	}
}

// fixtureReadyCardAfter is "Ready card, after the diff was opened", with the
// question of the push asked.
func fixtureReadyCardAfter() ReadyCard {
	c := fixtureReadyCard()
	c.DiffOpened = cardAt(14, 21)
	c.Ask = "Push zhuravel-PR-12360-activity-points-on-copy and open a draft PR?"
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
			st("tests", "done", "216 specs green · rubocop clean · Codex review: 1 note, fixed", 9*time.Minute, 9, 44),
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

// fixtureNewTaskJira is "New task: from a Jira key".
func fixtureNewTaskJira(now time.Time) NewTaskForm {
	return NewTaskForm{
		Text: "PR-12345 the reminder is for advocates only, not their friends",
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
		Ask: "Start feature PR-12345 with 2 agents in talkable.factory2?", ShowRules: true,
	}
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
