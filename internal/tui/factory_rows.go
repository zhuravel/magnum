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
