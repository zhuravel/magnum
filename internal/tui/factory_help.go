package tui

// The factory screen's help (?: its keys and the words of spec 9.E, each
// with its gloss) and its action log (!: the outcome of every action, then
// every alert the factory raised, with its time; spec 9.F: nothing
// vanishes).

import "github.com/charmbracelet/x/ansi"

// factoryWords are the screen's words and their glosses (spec 9.E).
var factoryWords = [][2]string{
	{"needs you", "stopped until you decide or act"},
	{"Nothing waits on you", "no task is blocked on you"},
	{"waits on others", "blocked by someone or something, not you"},
	{"YOUR PRS", "your open PRs that no task has, from magnum's registry; one that needs you is in NEEDS YOU"},
	{"done today", "its PR merged today"},
	{"WAITS ON", "who the task waits on, and why"},
	{"YOUR MOVE", "the step the factory recommends to you"},
	{"WAITED", "how long it has waited on this"},
	{"decisions", "single answers you owe, across all tasks"},
	{"you:", "you are the blocker"},
	{"agent:", "an AI agent is working on it"},
	{"CI:", "GitHub's checks are running or failing"},
	{"alice:, rev-ann:", "that person must act, for example review"},
	{"magnum:", "magnum reviews or reads the PR, or waits for its daemon or a reopen"},
	{"reviewers:", "the PR's reviewers must review, approve or settle"},
	{"memory:", "the Mac is short on memory"},
	{"usage:", "an agent plan's limit is reached"},
	{"nothing:", "finished; nobody needs to act"},
	{"memory full: new tasks wait", "new tasks start when memory frees"},
	{"codex limit: waits till 16:00", "Codex stages pause until the limit resets"},
	{"start", "read the Jira issue or your prompt"},
	{"investigate", "agents reproduce, find the cause, read history"},
	{"questions", "agents stopped for your answers"},
	{"coding", "a failing spec first, then the change"},
	{"tests", "the factory's proof and the precheck: specs, lint, migrations"},
	{"local review", "magnum's review on this Mac, before the PR"},
	{"FLAGS", "what needs a second look before the push"},
	{"Known", "the local review's notes that do not block, listed in the PR body"},
	{"review unavailable", "the local review did not finish; never read as clean"},
	{"published", "the PR is open; work on it goes on"},
	{"runbook", "the steps of a manual migration, for you to run"},
	{"needless", "you flagged a question that the factory did not need to ask"},
	{"ready to push", "committed on this Mac, not on GitHub"},
	{"push + PR", "push the branch, open a PR ready for review"},
	{"you pushed 11:02", "you said y; the factory pushed"},
	{"pushed", "the branch is on GitHub, the PR is open"},
	{"review", "magnum's rounds and people's comments"},
	{"merge master", "master merged into the branch, conflicts fixed"},
	{"merge", "your final merge into master"},
	{"cleanup", "free the copy after your merge"},
	{"send back with a note", "the writer tries again with your note"},
	{"drop", "stop, free the copy, keep the branch"},
	{"proof", "the spec failed before the fix, passes after"},
	{"doubts", "what the agents were unsure of or skipped"},
	{"not in the plan", "a changed file the plan did not name"},
	{"recommended", "the factory's pick; its reason is underneath"},
	{"no recommendation", "the evidence favours no answer; you choose"},
	{"ask someone", "post a Jira question; the task waits"},
	{"question 2 of 3", "the second of three questions here"},
	{"void, bart, bastion", "the three staging servers"},
	{"–", "nothing of this task is there"},
	{"outdated", "an older head is there; updated automatically"},
	{"latest", "your latest head is deployed there"},
	{"merging", "merging your latest head into that branch"},
	{"conflict", "the merge clashes; an agent fixes it"},
	{"CI 12/41", "12 of 41 checks done on that branch"},
	{"deploying", "merged; the server is not on it yet"},
	{"QA 4/9", "t:qa has run 4 of 9 checks"},
	{"QA ✓ / QA ✗", "QA passed or failed on your latest head"},
	{"?", "unknown: the factory could not read it"},
	{"bart: no QA", "bart gets merges and deploys, never QA"},
	{"bastion waits for void QA", "bastion QA starts after void passes"},
	{"head", "the newest commit of your branch"},
	{"code failed", "a check failed because of the code"},
	{"runner died", "CI's machine died; rerun, no code change"},
	{"try 1 of 2", "first try; a second failure asks you"},
	{"out of fix tries", "the agent gave up; you decide now"},
	{"ready to merge", "QA passed on void and bastion, latest head"},
	{"access needed", "a login, MFA or VPN step for you"},
	{"snooze", "hide it 2h, or until it changes"},
	{"queued", "waits for a free copy or memory"},
	{"copy", "the task's own checkout, databases and port"},
	{"your PR, written by hand", "you wrote it; the factory ships it"},
	{"no output 9m", "the pane printed nothing for 9 minutes"},
	{"step in", "open the real agent pane in herdr"},
	{"say one line", "send one message to that agent"},
	{"you're in it", "you're in that pane; the factory sends nothing"},
	{"lesson", "one thing a task taught the factory"},
	{"rule", "a lesson added to one repo's prompts"},
	{"skill edit", "a change to a factory skill or prompt"},
	{"test case", "an earlier task kept for replays"},
	{"replay", "rerun an earlier task to test a change"},
	{"used in", "tasks whose prompts carried the rule"},
	{"repeats seen after the rule", "the same mistake, seen in later tasks"},
	{"rule to approve", "a proposed rule waits for your y"},
	{"rate + / −", "say whether a finished task was right"},
}

// factoryKeyHelp are the screen's keys, view by view.
var factoryKeyHelp = []struct {
	title string
	keys  []hint
}{
	{"Home", []hint{{"j k g G", "move"}, {"enter", "the row's card: its decision when it needs you, else its task card"},
		{"a", "the oldest decision card, then the next"}, {"o", "step into the pane the row waits on (asks nothing)"},
		{"b t", "the row's PR, its Jira issue in the browser (asks nothing)"},
		{"f F x", "on your PR's row: magnum fixes what is open, fix off (or on again), stop its fix loop (y/N)"},
		{"enter", "on your PR's row: its keys (it has no card)"}, {"n", "new task"},
		{logKey, "the action log and the alerts"}, {"?", "this help"}, {settingsKey, "settings"},
		{"m", "mouse on / off"}, {"ctrl+r", "refresh"}, {"q", "quit"}}},
	{"Question card", []hint{{"j k", "move the mark"}, {"1-9", "answer and move on (nothing is sent yet)"},
		{"enter", "take the marked one"}, {"−", "then a digit: the question was needless, and why"},
		{"backspace", "the previous question"}, {"esc", "back, the answers kept"}}},
	{"Recap", []hint{{"y", "send the batch (the one y)"}, {"1-9", "change that answer"}, {"esc", "back to the questions"}}},
	{"Ready card", []hint{{"d", "the diff in the pager"}, {"j k", "move the mark"}, {"1", "push + PR (y/N)"},
		{"2", "send back: a reason's digit, then a note (y/N)"}, {"3", "drop: free the copy, keep the branch (y/N)"},
		{"enter", "the marked one"}}},
	{"Task card", []hint{{"j k", "move"}, {"enter", "the decision card its task waits on: its questions, its ready card"},
		{"r", "retry: it goes on at its stage (y/N, when it can)"}, {"d", "the diff"},
		{"x", "drop: stop its agents, free its copy, keep the branch (y/N)"}, {"o", "step into its pane"},
		{"b t", "its PR, its Jira issue in the browser (asks nothing)"}, {"esc", "back"}}},
	{"New task", []hint{{"enter", "start (y/N)"}, {"ctrl+j", "a new line"}, {"ctrl+g", "your editor"},
		{"tab", "the kind: bug, feature, chore"}, {repoKey, "the repository, when the factory has several"},
		{"esc", "back, the draft kept"}}},
}

// helpContent is the help's lines at screen width w: the keys, the mouse,
// then the words.
func (m factoryModel) helpContent(w int) []string {
	width := max(w-6, 20)
	lines := []string{m.st.Title.Render("Keys"), m.st.Dim.Render("y only confirms: any other key cancels a question.")}
	for _, sec := range factoryKeyHelp {
		lines = append(lines, "", m.st.Section.Render(sec.title))
		for _, h := range sec.keys {
			lines = append(lines, truncate(m.st.hints(0, h), width))
		}
	}
	lines = append(lines, "", m.st.Section.Render("Mouse"))
	for _, h := range mouseHelp(false, "opens its card") {
		lines = append(lines, truncate(m.st.hints(0, h), width))
	}
	lines = append(lines, "", m.st.Title.Render("Words"))
	for _, wd := range factoryWords {
		lines = append(lines, truncate(m.st.Label.Render(wd[0])+m.st.Dim.Render("  "+wd[1]), width))
	}
	return lines
}

// logContent is the action log's lines at screen width w, then the alerts,
// newest first, each with its time.
func (m factoryModel) logContent(w int) []string {
	width := max(w-6, 20)
	entries, _ := m.bar.log.snapshot()
	lines := logContent(m.st, m.g, entries, width)
	lines = append(lines, "", m.st.Title.Render("Alerts (newest first)"))
	if len(m.data.Alerts) == 0 {
		return append(lines, m.st.Dim.Render("no alerts yet"))
	}
	for _, a := range m.data.Alerts {
		line := m.st.Dim.Render(a.At.Local().Format("15:04:05")) + " " + cleanText(a.Text)
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return lines
}
