package tui

import "time"

// The data of the factory's home screen. The painter (factory_view.go) draws
// it and nothing else: the factory package turns its registry into these rows.

// FactoryGroup is the band of the home list a task sits in, by who it waits on.
type FactoryGroup int

const (
	// GroupNeedsYou holds the tasks that are stopped until the operator decides or acts.
	GroupNeedsYou FactoryGroup = iota
	// GroupWaits holds the tasks that wait on someone or something else.
	GroupWaits
	// GroupDone holds the tasks that finished today.
	GroupDone
)

// FactoryMark is the one-cell mark before a row: what the task is doing now.
type FactoryMark int

const (
	// MarkNeedsYou is drawn in yellow: the task is on the operator.
	MarkNeedsYou FactoryMark = iota
	// MarkWorking is the only moving mark: an agent works on the task.
	MarkWorking
	// MarkWaits marks a task that waits on someone or something else.
	MarkWaits
	// MarkDone marks a task that finished.
	MarkDone
)

// FactoryRow is one task on the home list, one line.
//
// The fields after Help are optional extra forms of the text before them:
// the painter draws the form that fits the screen and falls back to the
// plain one when a form is empty.
type FactoryRow struct {
	ID int64
	// Key is what the task is called in the list: its Jira key, or its GitHub
	// number ("#4512") for a PR the operator wrote by hand. GitHub is the
	// PR's number, shown beside the Jira key on a wide screen.
	Key, GitHub, Title string
	Group              FactoryGroup
	Mark               FactoryMark
	WaitsOn            []WaitsOn
	Move               string // the step the factory recommends; NeedsYou rows only
	Stagings           []StagingCell
	Since              time.Time // when the task began to wait on this; zero = not shown
	Help               string    // the line under the rule while the row is selected

	// Jira is the Jira key of a task keyed by its GitHub number, for the wide
	// screen's JIRA cell. Empty: the Key, unless the Key is the GitHub number.
	Jira string
	// MoveShort is Move on a narrow screen.
	MoveShort string
	// HelpShort and HelpLong are Help on a narrow and on a wide screen.
	HelpShort, HelpLong string
}

// WaitsOn is one thing a task waits on. Who is "you", "agent", "CI", a
// login, "magnum", "memory", "usage" or "nothing"; What says why.
type WaitsOn struct {
	Who, What string
	OnYou     bool // the blocker is the operator: drawn in yellow, whatever Who says

	// Short and Long are What on a narrow and on a wide screen.
	Short, Long string
}

// StagingCell is one staging server's column on a row. State is one of
// "none", "outdated", "merging", "conflict", "ci", "deploying", "qa",
// "qa-pass", "qa-fail", "latest" or "unknown". Count is the progress of a
// "ci" or "qa" cell ("12/41", "4/9"). Seen is when the factory read it: a
// wide screen shows its age.
type StagingCell struct {
	Name, State, Count string
	Seen               time.Time

	// OnYou marks a conflict that is out of fix tries, which is on the
	// operator: the cell turns yellow.
	OnYou bool
}

// The data of the factory's cards (spec 9.C): the question card and its recap,
// the ready card, the task card and the new task form. The painters
// (factory_card.go) draw them and nothing else: the factory package builds
// them from its registry.

// QuestionCard is the agents' questions before coding, one at a time, and
// their recap. Questions are drawn in the order given; Current is the one on
// the card.
type QuestionCard struct {
	// TaskKey is the task's Jira key ("PR-12345") or its GitHub number.
	TaskKey, Title, Kind string
	Asked                time.Time // when the batch was asked
	Questions            []CardQuestion
	Current              int // the index of the question on the card

	// AsOf is the revision of the questions the answers are for ("as of
	// 14:12"), drawn on the recap. Zero: the sentence is left out.
	AsOf time.Time
}

// CardQuestion is one question with its options and, once answered, its answer.
type CardQuestion struct {
	Text    string
	Context string // one line of why it is asked
	Example string // one example, drawn behind "Example:"
	// Options are drawn in the order given, the recommended one first. The
	// last one is usually "Type your own answer", which has no Means.
	Options []CardOption
	// Recommended is the index of the option the factory picks; -1 means no
	// recommendation, and nothing is marked.
	Recommended int
	// Why is the reason of the recommendation. With no recommendation it is
	// why there is none ("the ticket and the code give no hint").
	Why        string
	Answer     string // the answer as the recap says it; empty: not answered
	AnsweredAt time.Time

	// TookRecommendation is true when the answer is the recommended option.
	TookRecommendation bool
	// Comment is the drafted Jira comment of an answer that asks someone: the
	// recap draws it, and counts the question as an ask, not an answer.
	Comment string
	// Asks is who the Comment asks ("rev-ann").
	Asks string
}

// CardOption is one choice and what it means for the question's example.
type CardOption struct{ Label, Means string }

// ReadyCard is a finished task waiting for the operator's decision: read the
// diff, then push, send back or drop. The recommendation is hidden until the
// operator opened the diff.
type ReadyCard struct {
	// TaskKey is the task's Jira key or its GitHub number.
	TaskKey, Title, Branch string
	Waited                 time.Duration // how long the card has waited
	Cause, Fix, Proof      string
	Files                  []CardFile
	Doubts                 []string // what the agents were unsure of or skipped
	Screenshots            int
	// DiffOpened is when the operator opened the diff; zero: not yet, and
	// neither Recommended nor Why is drawn.
	DiffOpened time.Time
	// Recommended is the option the factory picks: 0 push + draft PR, 1 send
	// back with a note, 2 drop; -1 for none.
	Recommended int
	Why         string

	// Kind is the task's kind ("bug"), Commits the commits on the branch.
	Kind    string
	Commits int
	// ScreenshotsNote says what the screenshots show ("from the copy's own
	// server: the copied campaign shows 25 points").
	ScreenshotsNote string
	// Ask is the y/N question being asked; empty: the key hints.
	Ask string
}

// CardFile is one changed file of a ready card.
type CardFile struct {
	Path           string
	Added, Removed int
	// InPlan is false for a file the plan did not name: checking it is on the
	// operator.
	InPlan bool
}

// TaskCard is a task's card, cut to what milestone 1a draws: the header, what
// the task waits for, its stages, agents, report and lessons.
type TaskCard struct {
	Key, Kind, Title, Branch string
	// Copy says which copy the task holds ("copy 2 of 3, server off").
	Copy       string
	Started    time.Time
	WaitingFor string // starts with who it waits on ("rev-ann: review of #4527")
	Stages     []CardStage
	Agents     []CardAgent
	// Report lines each start with a label word and a space ("cause the offer
	// email takes ...").
	Report []string
	// Learned is what the task taught the factory, one lesson per entry.
	Learned []string

	// PR is the task's GitHub number once pushed ("#4527").
	PR string
	// YourTime is how long the operator spent on the task.
	YourTime time.Duration
	// Cursor is the line the mark is on: the stages, then the agents, then
	// the REPORT line, counted in that order; out of range = none.
	Cursor int
	// AgentsNote is the AGENTS line's remark ("the factory merges bastion itself").
	AgentsNote string
}

// CardStage is one stage of a task. State is one of "done", "skipped",
// "waiting", "working", "failed" or "todo". Proof says what makes the stage
// done, in words.
type CardStage struct {
	Name, State, Proof string
	Took               time.Duration
	Finished           time.Time
}

// CardAgent is one agent of a task. State is "working", "done" or any other
// state the agents view names ("waits on you", "stopped"). Why says what it is
// for, Now what it does at the moment (a working agent), For how long it has
// worked and Output when its pane last printed.
type CardAgent struct {
	Name, Why, Now, State string
	For                   time.Duration
	Output                time.Time
}

// NewTaskForm is the new task screen: the operator's text and what the
// factory understood of it. A row is drawn only when its field is filled.
type NewTaskForm struct {
	Text string
	// Jira is the issue the text starts with, read by the daemon; nil: the
	// text is a prompt.
	Jira                  *JiraPreview
	Kind, KindWhy, Branch string
	Starts                string // "now, in talkable.factory2 (free)" or why it is queued
	Agents, Rules         []string
	Err                   string // a problem with the text, drawn in the JIRA row
	Repo                  string // the repository, named in "3 talkable rules go into its prompts"
	Title                 string // the title of a task started from words
	Ask                   string // the y/N question being asked; empty: the key hints
	// ShowRules lists the rules under their count; false: the count alone.
	ShowRules bool
}

// JiraPreview is a Jira issue as the daemon read it.
type JiraPreview struct {
	Key, Type, Status, Priority, Reporter, Summary, Excerpt string
	Read                                                    time.Time
	// More is true when the description has more than the Excerpt.
	More bool
	// Found is the note drawn after the key on its row when the text names
	// it elsewhere than at its start ("found in your text"); empty for a key
	// at the start.
	Found string
}
