package tui

import "time"

// The fixtures are the data of the spec's home mockups (section 9.C). The
// names are placeholders: Jira keys PR-123xx, GitHub numbers #45xx, logins
// alice and rev-ann.

// factoryNow is the clock of "Home at 140 columns" (and of the 100 and 200
// column frames, which show the same data).
var factoryNow = time.Date(2026, 11, 3, 14, 9, 0, 0, time.Local)

// factoryNothingNow is the clock of "Home when nothing needs you".
var factoryNothingNow = time.Date(2026, 11, 3, 15, 20, 0, 0, time.Local)

func factoryAgo(now time.Time, d time.Duration) time.Time { return now.Add(-d) }

// fixtureHome is "Home at 140 columns": three tasks need the operator, three
// wait on others, one is done.
func fixtureHome() factoryFrame {
	now := factoryNow
	return factoryFrame{
		Repo: "talkable",
		Rows: []FactoryRow{
			{
				ID: 1, Key: "#4512", GitHub: "#4512", Jira: "PR-12310", Title: "Deploy list on campaigns",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{
					{Who: "you", What: "reply to alice", Short: "reply", OnYou: true},
					{Who: "agent", What: "fixing bart", Short: "bart", Long: "fixing 2 conflicts in bart"},
				},
				Move: "post the drafted reply", MoveShort: "post reply",
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, time.Hour)},
					{Name: "bart", State: "conflict", Seen: factoryAgo(now, 11*time.Minute)},
					{Name: "bastion", State: "qa", Count: "4/9", Seen: factoryAgo(now, 12*time.Minute)},
				},
				Since:     factoryAgo(now, 2*time.Hour),
				Help:      `alice on #4512: "Do we need the deploy list for archived campaigns too?" · reply drafted · also: agent fixing 2 conflicts in bart, 10m`,
				HelpShort: `alice: "Do we need the deploy list for archived campaigns too?" · also: agent fixing bart`,
				HelpLong:  `alice on #4512, 2h ago: "Do we need the deploy list for archived campaigns too, or only for live ones?" · reply drafted · also: agent fixing 2 conflicts in bart, try 1 of 2, 10m`,
			},
			{
				ID: 2, Key: "PR-12345", Title: "Credit expiry reminder email",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: "you", What: "3 questions before coding", Short: "3 questions",
					Long: "3 questions before coding · first: when is it sent?", OnYou: true}},
				Move: "answer 3 questions", MoveShort: "answer them",
				Since: factoryAgo(now, 41*time.Minute),
				Help:  `3 questions before coding · first: "When does the reminder go out?"`,
			},
			{
				ID: 3, Key: "PR-12360", Title: "Custom activities lose points on copy",
				Group: GroupNeedsYou, Mark: MarkNeedsYou,
				WaitsOn: []WaitsOn{{Who: "you", What: "ready to push · 3 files +48 −6", Short: "ready to push",
					Long: "ready to push · 3 files +48 −6, 1 not in the plan", OnYou: true}},
				Move: "read the diff, then push", MoveShort: "diff, then push",
				Since: factoryAgo(now, 12*time.Minute),
				Help:  "ready to push: 1 commit, not on GitHub yet · 1 file is not in the plan",
			},
			{
				ID: 4, Key: "PR-12371", Title: "Coupon CSV export times out",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "finding the cause, 2 of 3 done", Short: "finding the cause",
					Long: "finding the cause · reproduced, history read"}},
				Since: factoryAgo(now, 9*time.Minute),
				Help:  "finding the cause: 3 agents read the history and reproduce it",
			},
			{
				ID: 5, Key: "PR-12330", GitHub: "#4527", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "rev-ann", What: "review of #4527", Long: "review of #4527, requested 3h ago"}},
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, time.Hour)},
					{Name: "bart", State: "latest", Seen: factoryAgo(now, 2*time.Hour)},
					{Name: "bastion", State: "outdated", Seen: factoryAgo(now, 2*time.Minute)},
				},
				Since: factoryAgo(now, 3*time.Hour),
				Help:  "rev-ann was asked to review #4527 three hours ago",
			},
			{
				ID: 6, Key: "PR-12388", Title: "Weekly gem upgrades",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "memory", What: "full, starts below 80% swap", Short: "full",
					Long: "full (swap 91%), starts when swap is under 80%"}},
				Since: factoryAgo(now, 18*time.Minute),
				Help:  "memory is full: the task starts when swap is under 80%",
			},
			{
				ID: 7, Key: "PR-12302", GitHub: "#4519", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20 · not rated yet", Short: "merged 13:20"}},
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, 4*time.Hour)},
					{Name: "bart", State: "latest", Seen: factoryAgo(now, 4*time.Hour)},
					{Name: "bastion", State: "qa-pass", Seen: factoryAgo(now, 3*time.Hour)},
				},
				Help: "merged at 13:20 · not rated yet (+ or −)",
			},
		},
		Blocking:     []string{"memory full: new tasks wait"},
		BlockingLong: []string{"memory full: new tasks wait (swap 91%)"},
		RuleChip:     "1 rule to approve (L)",
		Flash:        "14:02 you said MFA done → bart deploy resumed",
		Hints: []hint{
			{"enter", "read and reply"}, {"a", "oldest ask"}, {"o", "pane fixing bart"}, {"n", "new task"}, {"?", "more"},
		},
		Decisions: 5,
	}
}

// fixtureNothing is "Home when nothing needs you": six tasks wait on others,
// the cursor is on the coupon export.
func fixtureNothing() factoryFrame {
	now := factoryNothingNow
	return factoryFrame{
		Repo: "talkable",
		Rows: []FactoryRow{
			{
				ID: 1, Key: "PR-12345", Title: "Credit expiry reminder email",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "writing the spec"}},
				Since:   factoryAgo(now, 12*time.Minute),
				Help:    "writing the spec: 2 agents read the ticket against the code",
			},
			{
				ID: 2, Key: "PR-12371", Title: "Coupon CSV export times out",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agent", What: "writing the plan"}},
				Since:   factoryAgo(now, 21*time.Minute),
				Help:    "writing the plan: 3 agents agree on the cause; a judge writes the plan, then coding starts",
			},
			{
				ID: 3, Key: "#4512", GitHub: "#4512", Title: "Deploy list on campaigns",
				Group: GroupWaits, Mark: MarkWorking,
				WaitsOn: []WaitsOn{{Who: "agents", What: "fixing bart, QA"}},
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, time.Hour)},
					{Name: "bart", State: "conflict", Seen: factoryAgo(now, 11*time.Minute)},
					{Name: "bastion", State: "qa", Count: "6/9", Seen: factoryAgo(now, 12*time.Minute)},
				},
				Since: factoryAgo(now, 14*time.Minute),
				Help:  "two agents fix the conflicts in bart and run QA",
			},
			{
				ID: 4, Key: "PR-12360", GitHub: "#4540", Title: "Custom activities lose points on copy",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "magnum", What: "review of #4540"}},
				Stagings: []StagingCell{
					{Name: "void", State: "none"}, {Name: "bart", State: "none"}, {Name: "bastion", State: "none"},
				},
				Since: factoryAgo(now, 40*time.Minute),
				Help:  "magnum is reviewing #4540",
			},
			{
				ID: 5, Key: "PR-12330", GitHub: "#4527", Title: "Advocate offer email locale",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "rev-ann", What: "review of #4527"}},
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, time.Hour)},
					{Name: "bart", State: "latest", Seen: factoryAgo(now, 2*time.Hour)},
					{Name: "bastion", State: "deploying", Seen: factoryAgo(now, 2*time.Minute)},
				},
				Since: factoryAgo(now, 4*time.Hour),
				Help:  "rev-ann was asked to review #4527",
			},
			{
				ID: 6, Key: "PR-12388", Title: "Weekly gem upgrades",
				Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "memory", What: "full"}},
				Since:   factoryAgo(now, time.Hour),
				Help:    "memory is full: the task starts when swap is under 80%",
			},
			{
				ID: 7, Key: "PR-12302", GitHub: "#4519", Title: "Remove old sync job",
				Group: GroupDone, Mark: MarkDone,
				WaitsOn: []WaitsOn{{Who: "nothing", What: "merged 13:20"}},
				Stagings: []StagingCell{
					{Name: "void", State: "qa-pass", Seen: factoryAgo(now, 4*time.Hour)},
					{Name: "bart", State: "latest", Seen: factoryAgo(now, 4*time.Hour)},
					{Name: "bastion", State: "qa-pass", Seen: factoryAgo(now, 3*time.Hour)},
				},
				Help: "merged at 13:20",
			},
		},
		Blocking: []string{"memory full: new tasks wait"},
		Flash:    "15:18 sent 3 answers to PR-12345 → coding started",
		Hints: []hint{
			{"enter", "task card"}, {"o", "pane writing the plan"}, {"tab", "agents"}, {"n", "new task"}, {"?", "more"},
		},
		Next:   "PR-12371's questions or its diff, once its plan is written.",
		Cursor: 1,
	}
}
