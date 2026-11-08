package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The data of the factory's home screen. The painter (factory_view.go) draws
// it and nothing else: the factory package turns its registry into these rows.

// FactoryGroup is the band of the home list a task sits in, by who it waits on.
type FactoryGroup int

const (
	// GroupNeedsYou holds the tasks that are stopped until the operator decides or acts.
	GroupNeedsYou FactoryGroup = iota
	// GroupWaits holds the tasks that wait on someone or something else.
	GroupWaits
	// GroupYours holds the open PRs from magnum's registry that magnum's
	// look leaves alone, or that magnum does not review (LEFT ALONE: a
	// baseline, a mute, a draft), your own and the published tasks' alike:
	// they ask nothing. The other PRs sit in GroupNeedsYou or GroupWaits by
	// what they wait on, fix off included (magnum still reviews them).
	GroupYours
	// GroupDone holds the tasks that finished today.
	GroupDone
)

// WaitsOnYou is the Who of a blocker that is the operator: the one word the
// painter draws in yellow ("you:"), and the word the data writes for it
// (the factory's own wait on you is this constant).
const WaitsOnYou = "you"

// AgentWaitsOnYou is the state of an agent that waits on the operator, the
// only agent state drawn in yellow, as the painter and the factory's
// screen data both spell it.
const AgentWaitsOnYou = "waits on " + WaitsOnYou

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
	// Repo is the repository the row is of ("owner/name"): the task's, or
	// your PR's. The painter draws it in a column of its own, unless the
	// title names one repository and every row is of it.
	Repo     string
	Group    FactoryGroup
	Mark     FactoryMark
	WaitsOn  []WaitsOn
	Move     string // the step the factory recommends; NeedsYou rows only
	Stagings []StagingCell
	Since    time.Time // when the task began to wait on this; zero = not shown
	Help     string    // the line under the rule while the row is selected

	// Jira is the Jira key of a task keyed by its GitHub number, for the wide
	// screen's JIRA cell. Empty: the Key, unless the Key is the GitHub number.
	Jira string
	// MoveShort is Move on a narrow screen.
	MoveShort string
	// HelpShort and HelpLong are Help on a narrow and on a wide screen.
	HelpShort, HelpLong string
	// Frees is what frees a queued task ("frees when PR-12360 is pushed"):
	// the text of its last blocker on a 140-column screen ("copy: frees when
	// PR-12360 is pushed"), after the blocker's own text on a wide one; a
	// narrow one keeps the blocker's Short, or Frees when it has none.
	Frees string

	// CI, Findings and Approvals are the row's PR as magnum's registry tells
	// it, for the CI, FINDINGS and APPR columns: the head's CI and magnum's
	// last posted review as the board's CI and FINDINGS columns say them,
	// and the people's reviews GitHub counts. nil on a row without a PR, or
	// before magnum read the PR.
	CI        *CIInfo
	Findings  *FindingsInfo
	Approvals *FactoryApprovals

	// PR is the row's open PR in magnum's registry, which f, F, K, r, R, z,
	// M and U act on: on a row of your own PR, not a task (in any group but DONE
	// TODAY), whose ID is the negative of magnum's id of the PR, which no
	// task's has (yourPR); and on a task's row whose PR magnum read. nil on
	// a task's row without one.
	PR *FactoryPR
}

// yourPR reports whether r is a row of your own PR, not a task's: it has a
// PR and the negative of magnum's id of it.
func (r FactoryRow) yourPR() bool { return r.PR != nil && r.ID < 0 }

// FactoryPR is a row's open PR on the factory screen (your own PR's, or a
// task's), as magnum's registry tells it: what the row's keys go by.
type FactoryPR struct {
	// Ref names it as magnum's commands do ("owner/name#4512").
	Ref string
	// Fixing says magnum's fix loop runs on it.
	Fixing bool
	// FixOff says you switched it out of autofix (`magnum fix --off`).
	FixOff bool
	// Off is why magnum's look leaves it alone (its verdict is off: a
	// baseline, fix off, a watch that leaves your PRs out), in its words;
	// "" when the look covers it. The row is drawn dim: it asks nothing.
	Off string
	// Restores says magnum's open of the PR (o) restores it in a slot,
	// which claims the slot and holds the PR for Hold ([daemon] open_hold):
	// o asks y/N first, as the board's does; false when it focuses a live
	// pane, which asks nothing.
	Restores bool
	Hold     time.Duration
	// Facts are what r, R, z, M and U go by, as magnum's registry tells
	// them; nil when the screen read none: the keys then refuse only what
	// the rest of the row tells, and magnum's daemon decides the rest.
	Facts *FactoryPRFacts
}

// FactoryPRFacts are what the board's row actions (review, fresh review,
// snooze, mute, unmute) know of a PR on the factory screen, read from
// magnum's registry, read only, and from its watch: the facts the board's
// refusals and y/N questions go by, so a key refuses where the board's does.
type FactoryPRFacts struct {
	// State is magnum's state of the PR ("reviewed", "reviewing"), GHState
	// GitHub's (OPEN, CLOSED, MERGED).
	State, GHState string
	// Head is the PR's head commit, Reviewed the head magnum reviewed last
	// ("" when never).
	Head, Reviewed string
	// Muted says the PR is muted (`magnum mute`, M); SnoozedUntil is when its
	// snooze ends (zero: not snoozed; `magnum snooze`, z).
	Muted        bool
	SnoozedUntil time.Time
	// CodexFlag is the PR's Codex flag in one cell ("" when not flagged): the
	// review keys refuse it, as `magnum review` does.
	CodexFlag string
	// MergedUnreviewed says GitHub merged it before magnum reviewed its last
	// push; FlagDismissed that a mute dismissed that flag (M and U act on it).
	MergedUnreviewed, FlagDismissed bool
	// Replies counts the replies on magnum's review its judge has not
	// re-decided yet (store.PR.PendingReplies): r then has the judge alone
	// re-decide them, as the board's r does.
	Replies int
	// Autofix says the PR's watch starts fix loops by itself (autofix "own"
	// or "open"): the questions say what a key does to them.
	Autofix bool
}

// FactoryApprovals is what the people's reviews of a PR say, as GitHub's
// merge gate counts them: Approved and Changes count the reviewers whose
// latest opinion is an approval or a changes request, Stale the approvals
// of an older commit than the head; Decision is GitHub's review decision
// (APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED; "" when the base branch
// requires no review).
type FactoryApprovals struct {
	Approved, Changes, Stale int
	Decision                 string
}

// Digest is what happened while the operator was away, shown until the next
// key. Since is when the operator was last seen. Done are the tasks that
// finished since then, one line each ("PR-12302 Remove old sync job · merged
// 21:15"), drawn under "DONE SINCE <day time>" in place of the DONE TODAY
// group; Chip is the chip line's segment ("away 14h: 2 done, 2 new for you"),
// in place of the done count.
type Digest struct {
	Since time.Time
	Done  []string
	Chip  string
}

// WaitsOn is one thing a task waits on. Who is "you" (WaitsOnYou), "agent", "CI", a
// login, "magnum", "memory", "usage", "copy" or "nothing"; What says why.
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

	// Cursor is the option of the current question under the mark, which j k
	// move; out of range (-1) = none. The screen starts it on the
	// recommended option, and on none when there is no recommendation.
	Cursor int
	// Needless offers the − key: flag the current question as needless.
	Needless bool
	// NeedlessReasons are the reasons a needless flag takes, picked by their
	// digit: while they are set (after −) the footer asks for one.
	NeedlessReasons []string
	// Hints are the footer's key hints; none: the card's own (questionHints).
	Hints []hint `json:"-"`

	// Revision is the revision of the questions on the card: the batch of
	// answers binds to it, and the factory refuses a batch for another
	// (ErrQuestionsChanged).
	Revision int
	// Ask is the y/N question being asked; empty: the key hints (or the
	// line you type).
	Ask string

	// The screen's own state, which the factory never fills: the note on the
	// rule (styled), the line typed in the footer, and the recap's "the
	// questions changed" variant.
	rule    string
	input   *cardInput
	changed *recapChange
	sending bool // the batch is on its way: the recap draws its keys, not its y/N
}

// cardInput is a line typed in a card's footer (your own answer, the note
// of a send back): what it is for, and the text so far.
type cardInput struct{ label, text string }

// recapChange is the recap after the factory refused the batch because the
// questions changed: when, why (may be empty) and the question to see again
// (from 1; 0: the questions).
type recapChange struct {
	at    time.Time
	why   string
	again int
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

	// sent marks an answer the factory already has: the recap shows it, and
	// the batch of the screen neither sends nor counts it.
	sent bool
}

// AskComment is the Jira comment the factory drafts when you ask login a
// question of task key instead of answering it: the question, why it is
// asked, its example and the options (the ask and your-own-words options
// of a card left out). The factory writes it for you to post, and the
// recap shows the same text before you send.
func AskComment(login, key, question, context, example string, options []CardOption) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, a question on %s: %s\n", login, key, question)
	if context != "" {
		fmt.Fprintf(&b, "\n%s\n", context)
	}
	if example != "" {
		fmt.Fprintf(&b, "\nFor example: %s\n", example)
	}
	options = slices.DeleteFunc(slices.Clone(options), func(o CardOption) bool {
		return o.Own || strings.HasPrefix(strings.TrimSpace(o.Send), "ask:")
	})
	if len(options) > 0 {
		b.WriteString("\nThe options as we see them:\n")
		for _, o := range options {
			if o.Means != "" {
				fmt.Fprintf(&b, "- %s: %s\n", o.Label, o.Means)
			} else {
				fmt.Fprintf(&b, "- %s\n", o.Label)
			}
		}
	}
	return b.String()
}

// CardOption is one choice and what it means for the question's example.
type CardOption struct {
	Label, Means string
	// Send is what the option answers: "" for its Label; "ask:<login>" asks
	// that person instead (the factory drafts the comment).
	Send string
	// Own marks the option that takes your own words ("Type your own
	// answer"): picking it asks for them.
	Own bool
}

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
	// Recommended is the option the factory picks: 0 push + PR, 1 send back,
	// 2 drop; -1 for none.
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

	// Flags are the FLAGS line: what needs a second look before the push,
	// one each ("not in the plan: spec/factories/reward_rules.rb").
	Flags []string
	// Leftovers are the KNOWN row, what the local review left open that does
	// not block (the PR body's "Known" list), one each: its leftovers, the
	// findings the writer declined with their risks, the doubts, and the
	// reviewers a round went without.
	Leftovers []string
	// Unread are the changed files the operator has not opened in the diff,
	// drawn as "unread: <file>".
	Unread []string
	// Frees is what the push frees ("talkable.factory3; PR-12395 starts in
	// it"): the push's y/N names it ("It frees ..."). A drop's y/N is the
	// spec's sentence, which names the copy itself.
	Frees string
	// Reasons are the reason keys of a send back, picked by their digit:
	// while they are set the footer asks for one, before the note.
	Reasons []string
	// Cursor is the option under the mark, which j k and 1-3 move; -1 (any
	// value out of range) = no mark. It is -1 until the operator moves it or
	// opens the diff: before the diff nothing is marked (spec 9.C). Its zero
	// value marks push + PR.
	Cursor int
	// Hints are the footer's key hints; none: the card's own (readyHints).
	Hints []hint `json:"-"`

	// Copy is the copy the task holds ("talkable.factory3"), which a drop
	// frees: its y/N names it.
	Copy string
	// NoPush says why push is not offered on this card ("the base conflicts:
	// the writer merges it"); empty: it is. The screen refuses a push then,
	// and the card says so on the option.
	NoPush string
	// Failed are the checks that failed ("rubocop: 2 offenses"), drawn under
	// PROOF behind a red ✗; the recommendation is then send back.
	Failed []string
	// QA is the QA row, after PROOF: what the change's visual QA found
	// ("clean: 3 pages walked at abc1234, 1 note", "not needed: no
	// user-facing file changed"); empty: no row. QAReport is the QA run's
	// report (summary.md, absolute), which e opens in the pager: "" for
	// none, and no e then.
	QA, QAReport string

	// The screen's own state: the note on the rule (styled) and the line
	// typed in the footer.
	rule  string
	input *cardInput
}

// CardFile is one changed file of a ready card.
type CardFile struct {
	Path           string
	Added, Removed int
	// InPlan is false for a file the plan did not name: checking it is on the
	// operator.
	InPlan bool
	// Flagged marks a file a FLAGS line names (a migration, a sensitive
	// file, an old guard): it is drawn first, with the files not in the plan.
	Flagged bool
}

// TaskCard is a task's card, cut to what milestone 1a draws: the header, what
// the task waits for, its stages, agents, report and lessons.
type TaskCard struct {
	Key, Kind, Title, Branch string
	// Repo is the task's repository ("owner/name"), the first fact under
	// the header with its PR's number.
	Repo string
	// Copy says which copy the task holds ("copy 2 of 3, server off").
	Copy       string
	Started    time.Time
	WaitingFor string // starts with who it waits on ("rev-ann: review of #4527")
	// Links are what the task waits on you for, as links (the threads and
	// findings its PR's verdict says need you), drawn under WAITING FOR.
	Links []string
	// FixNeeds are what magnum's fixer left you on the PR, one item each in
	// its own words (its report's Needs you, the comment id left out), drawn
	// under WAITING FOR before the links; FixReport is that report (absolute),
	// which e opens in the pager: set for any fix attention whose report
	// magnum keeps, "" for none.
	FixNeeds  []string
	FixReport string
	// PRInfo is the PR's own lines (CardPR), drawn first in the card's
	// body: on your PR's card and on a published task's once magnum read
	// its PR; nil before.
	PRInfo *CardPR
	Stages []CardStage
	Agents []CardAgent
	// Report lines each start with a label word and a space ("cause the offer
	// email takes ...").
	Report []string
	// Learned is what the task taught the factory, one lesson per entry.
	Learned []string

	// PR is the task's GitHub number once pushed ("#4527"); PRURL its page,
	// which b opens: "" until the factory published the PR or adopted one.
	PR, PRURL string
	// Issue is the task's Jira key ("PR-12330"), "" for a task of your
	// words; IssueURL its page, which t opens: the key's link by the [board]
	// trackers, as the board links a PR's issue, "" when no template names
	// the key.
	Issue, IssueURL string
	// YourTime is how long the operator spent on the task.
	YourTime time.Duration
	// Cursor is the line the mark is on: the stages, then the agents, then
	// the REPORT line, counted in that order; out of range = none.
	Cursor int
	// AgentsNote is the AGENTS line's remark ("the factory merges bastion itself").
	AgentsNote string
	// Spinner is the frame of the working marks, the card's only motion.
	Spinner int
	// Retry is true when a retry can go on: the task needs the operator at
	// a stage that checks again what stopped it (magnum factory retry), not
	// for its questions. The card's hints then offer r.
	Retry bool
	// AcceptQA is what A accepts: the QA defects the writer declined at the
	// task's head, which its QA fix loop stopped on ("1 QA defect the writer
	// declined at abc1234 (flow on /settings/security)", magnum factory
	// retry --accept-qa); "" for none, and no A then.
	AcceptQA string
	// Hints are the footer's key hints; none: the card's own (taskHints).
	Hints []hint `json:"-"`
	// Ask is the y/N question being asked (a retry, a drop); empty: the key
	// hints.
	Ask string

	rule string // the screen's note on the rule (styled)
}

// CardPR is what a PR is, on its card (your PR's, or a published task's),
// as magnum's registry tells it: each line is drawn only when it has its
// data, under its label in WAITING FOR's column. State is the PR line (its
// state, head, branches, last activity); CI is drawn as the board's CI
// cell; Review is magnum's last posted review in words, after its
// findings' cell (Findings); Reviewers are the people's reviews and whom
// the PR asks, after the APPR cell (Approvals); Threads are the replies
// magnum's judge has not read; FixLoop is magnum's fix loop on it.
type CardPR struct {
	State     string
	CI        *CIInfo
	Review    string
	Findings  *FindingsInfo
	Approvals *FactoryApprovals
	Reviewers string
	Threads   string
	FixLoop   string
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
	// AgentsByKind are the agents each kind's start launches, by the kind's
	// name: the AGENTS row and the y/N of a kind picked with tab draw that
	// kind's (Agents are the guessed kind's). None for a kind: Agents stay.
	AgentsByKind map[string][]string
	Err          string // a problem with the text, drawn in the JIRA row
	Repo         string // the repository, named in "3 talkable rules go into its prompts"
	Title        string // the title of a task started from words
	Ask          string // the y/N question being asked; empty: the key hints
	// ShowRules lists the rules under their count; false: the count alone.
	ShowRules bool

	// Links are the links the text holds, in its order.
	Links []FormLink
	// Overlap is the advisory line of what else touches the work: commits
	// and PRs that name the key, open PRs that change the same files.
	Overlap string
	// Reading is true while the daemon reads Jira: the JIRA row says so,
	// behind the spinner frame Spinner.
	Reading bool
	Spinner int
	// Guidance is what goes to every agent with the ticket, as the daemon
	// parsed the text: the words after a key or a Jira link at its start, or
	// the whole text for a key found in it. The form draws it as it is and
	// never parses the text; empty: no GUIDANCE row.
	Guidance string
	// Keys are the keys Jira knows that the text names, when it names
	// several: the task is the prompt task it is, whose questions ask which
	// issue it is, and no ticket is drawn as its own.
	Keys []string
	// WordsOnly is true when the key at the start of the text is one Jira
	// does not know (Err says so): enter then asks to start from your words
	// only, the key kept as words and never read as the task's ticket.
	WordsOnly bool
	// RepoWhy says how the factory chose Repo: "from the EX tracker" (a key
	// of the tracker of that repository alone), "from the link" (a GitHub
	// link of it) or "your pick".
	RepoWhy string
	// Repos are the factory's repositories, in the config's order: with two
	// or more the form draws a REPO row, whose key cycles through them, and
	// with none chosen (Repo empty) it draws that row only, and enter starts
	// nothing.
	Repos []string
	// NotJira is the key the text starts with when its tracker is not Jira:
	// the task's key, never read from Jira; the task starts from your words.
	NotJira string
	// Hints are the footer's key hints; none: the form's own (newTaskHints).
	Hints []hint `json:"-"`

	rule string // the screen's note on the rule (styled)
}

// FormLink is a link in the new task text. Kind is "jira" (a Jira link is
// its key), "pr" (a GitHub PR, which offers adoption) or "run" (a GitHub
// Actions run, which offers a CI task); Offer says what the factory makes of
// it ("its key PR-12345", "adoption comes in milestone 2").
type FormLink struct{ Kind, URL, Offer string }

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
