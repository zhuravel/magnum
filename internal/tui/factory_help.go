package tui

// The factory screen's help (?: its keys and the words of spec 9.E that the
// screen draws, each with its gloss; a word of a feature still to come, such
// as staging, QA, snooze, the learning view or the rating, comes with it)
// and its action log (!: the outcome of every action, then every alert the
// factory raised, with its time; spec 9.F: nothing vanishes).

import "github.com/charmbracelet/x/ansi"

// factoryWords are the screen's words and their glosses (spec 9.E), only
// those the screen draws: the stages are the built-in kinds'.
var factoryWords = [][2]string{
	{"needs you", "stopped until you decide or act"},
	{"Nothing waits on you", "no task is blocked on you"},
	{"waits on others", "blocked by someone or something, not you"},
	{"LEFT ALONE", "your PRs magnum's look leaves alone (a baseline, fix off): they ask nothing; the others sit with the tasks"},
	{"3 wait on others, 2 at work", "2 of them have an agent or magnum's fixer at work now (the moving mark)"},
	{"talkable, widget", "the factory's repositories on the title line (\"3 repos\" when they do not fit); with more than one, each row names its own first"},
	{"#4530", "the row's PR on GitHub, beside its Jira key from 120 columns"},
	{"done today", "its PR merged today"},
	{"WAITS ON", "who the task waits on, and why"},
	{"CI", "the PR's checks at its head, as the board says them: ✓ 41/41, ✗ the worst required check, – one that did not run"},
	{"FINDINGS", "what magnum's last review of the PR found, by priority (✗ P1 P2×3), or ✔ clean"},
	{"APPR", "the people's reviews GitHub counts: ✔ 2 approvals (⟳ one of an older commit), ✗ 1 changes request"},
	{"YOUR MOVE", "the step the factory recommends to you; on a narrow screen the help line says it"},
	{"WAITED", "how long it has waited on this"},
	{"decisions", "single answers you owe, across all tasks"},
	{"you:", "you are the blocker"},
	{"agent:", "an AI agent is working on it"},
	{"CI:", "GitHub's checks are running or failing"},
	{"alice:, rev-ann:", "that person must act, for example review"},
	{"magnum:", "magnum reviews or reads the PR, or waits for its daemon or a reopen"},
	{"claude-fix:", "what magnum's fixer left you on the PR, from its report (e opens it)"},
	{"reviewers:", "the PR's reviewers must review, approve or settle"},
	{"memory:", "the Mac is short on memory"},
	{"usage:", "an agent plan's limit is reached"},
	{"nothing:", "finished; nobody needs to act"},
	{"memory full: new tasks wait", "new tasks start when memory frees"},
	{"investigate", "agents reproduce, find the cause, read history"},
	{"judge", "an agent weighs the findings and writes the report and its questions"},
	{"critic", "an agent reads the plan before coding (on a wide plan)"},
	{"questions", "agents stopped for your answers"},
	{"reproduction", "the bug must reproduce before the fix"},
	{"coding", "a failing spec first, then the change"},
	{"local review", "magnum's review on this Mac, before the PR"},
	{"pr body", "an agent drafts the PR's description"},
	{"ready", "the ready card waits for your push, send back or drop"},
	{"publish", "the factory pushes the branch and opens the PR"},
	{"FLAGS", "what needs a second look before the push"},
	{"Known", "the local review's notes that do not block, listed in the PR body"},
	{"review unavailable", "the local review did not finish; never read as clean"},
	{"needless", "you flagged a question that the factory did not need to ask"},
	{"ready to push", "committed on this Mac, not on GitHub"},
	{"push + PR", "push the branch, open a PR ready for review"},
	{"you pushed 11:02", "you said y; the factory pushed"},
	{"send back with a note", "the writer tries again with your note"},
	{"drop", "stop, free the copy, keep the branch"},
	{"proof", "the spec failed before the fix, passes after"},
	{"doubts", "what the agents were unsure of or skipped"},
	{"not in the plan", "a changed file the plan did not name"},
	{"recommended", "the factory's pick; its reason is underneath"},
	{"no recommendation", "the evidence favours no answer; you choose"},
	{"ask someone", "post a Jira question; the task waits"},
	{"question 2 of 3", "the second of three questions here"},
	{"head", "the newest commit of your branch"},
	{"queued", "waits for a free copy or memory"},
	{"copy", "the task's own checkout, databases and port"},
	{"step in", "open the real agent pane in herdr"},
}

// factoryKeyHelp are the screen's keys, view by view.
var factoryKeyHelp = []struct {
	title string
	keys  []hint
}{
	{"Home", []hint{{"j k g G", "move"}, {"enter", "the row's card: its decision when it needs you, else its task card; your PR's card on its row"},
		{"a", "the oldest decision card, then the next"},
		{"o", "step into the pane the row waits on (asks nothing); magnum's pane on your PR's row and while magnum fixes the PR (y/N to restore a parked one)"},
		{"b t", "the row's PR, its Jira issue in the browser (asks nothing)"},
		{"f F K", "on a row with a PR magnum read, yours or a task's: magnum fixes what is open, fix off (or on again), stop its fix loop (y/N)"},
		{"x", "on your PR's row: stop magnum's fix loop, as K (y/N)"},
		{reportKey, "the report of magnum's fixer (claude-fix.md) in the pager, on a row whose fix loop left you items (asks nothing)"},
		{"n", "new task"},
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
		{"b t", "its PR, its Jira issue in the browser (asks nothing)"},
		{reportKey, "the report of magnum's fixer, when its fix loop left you items (asks nothing)"},
		{"f F K", "on a card with a PR: magnum fixes what is open, fix off (or on again), stop its fix loop (y/N)"},
		{"", "your PR's card has the PR's keys alone: f F K, o magnum's pane, b t e; no task to retry, diff or drop"},
		{"esc", "back"}}},
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
