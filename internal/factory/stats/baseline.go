package stats

// The baseline: what the operator's messages looked like before the factory,
// counted per ticket (a ticket takes 5 to 20 sessions, so a count per session
// would flatter the factory) with the classifier that counts the factory's
// own. It is frozen once, before the first task, to baseline.json.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// Dist is how a count spreads over the tickets: the median, the quartiles
// (linear interpolation between the two nearest tickets) and the extremes.
type Dist struct {
	Median float64 `json:"median"`
	P25    float64 `json:"p25"`
	P75    float64 `json:"p75"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// Baseline is the operator's way of working before the factory: over Tickets
// tickets, the typed messages and the step-starting messages per ticket, and
// Accuracy, the share of the operator's labelled messages that Classify got
// right (so how far the step-start counts can be trusted).
type Baseline struct {
	Frozen              time.Time `json:"frozen"`
	Tickets             int       `json:"tickets"`
	TypedPerTicket      Dist      `json:"typed_per_ticket"`
	StepStartsPerTicket Dist      `json:"step_starts_per_ticket"`
	Accuracy            float64   `json:"accuracy"`
}

// Session is one agent session of the operator's: the ticket it worked on
// and the messages the operator typed to it.
type Session struct {
	Key      string
	Messages []string
}

// Labelled is a message with the class the operator gave it by hand.
type Labelled struct {
	Text  string `json:"text"`
	Class string `json:"class"`
}

// classes are the classes a message can have: the registry's.
var classes = []string{registry.ClassStepStart, registry.ClassApproval, registry.ClassCorrection, registry.ClassAccess, registry.ClassInfo}

// BuildBaseline counts the operator's messages per ticket: the sessions of
// one key are one ticket, with the sum of their messages (a blank message is
// none), and the median, quartiles and extremes are taken over the tickets,
// never over the sessions. The sessions come oldest first (ReadSessions
// sorts them): the first message of a ticket's first session is the one that
// starts the work, so it counts as typed and never as a step start, as the
// message that starts a task does on the factory's side; the first message of
// a later session keeps its class, because a session opened only to start
// the next step is what the measure counts. Accuracy is the share of
// labelled that Classify gets right. It refuses sessions without a key,
// a set with no message and an empty or mislabelled labelled set, because
// a baseline it cannot measure is worse than none. The result has no Frozen
// time; FreezeBaseline sets it.
func BuildBaseline(sessions []Session, labelled []Labelled) (Baseline, error) {
	type count struct{ typed, steps int }
	tickets := map[string]*count{}
	for _, s := range sessions {
		if strings.TrimSpace(s.Key) == "" {
			return Baseline{}, errors.New("a session names no ticket key: a baseline counts per ticket")
		}
		for _, m := range s.Messages {
			if strings.TrimSpace(m) == "" {
				continue
			}
			c := tickets[s.Key]
			first := c == nil
			if first {
				c = &count{}
				tickets[s.Key] = c
			}
			c.typed++
			if !first && Classify(m) == registry.ClassStepStart {
				c.steps++
			}
		}
	}
	if len(tickets) == 0 {
		return Baseline{}, errors.New("no ticket has a message of the operator's")
	}
	if len(labelled) == 0 {
		return Baseline{}, errors.New("no labelled message: the classifier's accuracy cannot be measured")
	}
	right := 0
	for i, l := range labelled {
		if !slices.Contains(classes, l.Class) {
			return Baseline{}, fmt.Errorf("labelled message %d has the unknown class %q (want %s)", i+1, l.Class, strings.Join(classes, ", "))
		}
		if Classify(l.Text) == l.Class {
			right++
		}
	}
	typed, steps := make([]float64, 0, len(tickets)), make([]float64, 0, len(tickets))
	for _, c := range tickets {
		typed, steps = append(typed, float64(c.typed)), append(steps, float64(c.steps))
	}
	return Baseline{Tickets: len(tickets), TypedPerTicket: distOf(typed), StepStartsPerTicket: distOf(steps),
		Accuracy: float64(right) / float64(len(labelled))}, nil
}

// distOf is the distribution of vals, which must not be empty.
func distOf(vals []float64) Dist {
	v := slices.Sorted(slices.Values(vals))
	return Dist{Median: quantile(v, 0.5), P25: quantile(v, 0.25), P75: quantile(v, 0.75), Min: v[0], Max: v[len(v)-1]}
}

// quantile is the q-quantile of the sorted values, interpolating linearly
// between the two nearest ranks.
func quantile(sorted []float64, q float64) float64 {
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(sorted)-1)
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// String spells the distribution: its median, its middle half and its
// extremes, each to one decimal at most.
func (d Dist) String() string {
	return fmt.Sprintf("median %s, middle half %s to %s, all %s to %s", num(d.Median), num(d.P25), num(d.P75), num(d.Min), num(d.Max))
}

// num spells a count with at most one decimal.
func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

// Where says where v lies against the middle half of the tickets: "below"
// the first quartile, "above" the third, else "within".
func (d Dist) Where(v float64) string {
	switch {
	case v < d.P25:
		return "below"
	case v > d.P75:
		return "above"
	}
	return "within"
}

// ErrBaselineFrozen is FreezeBaseline's refusal: the file exists.
var ErrBaselineFrozen = errors.New("the baseline is frozen once and never rewritten")

// FreezeBaseline writes b to path, once. A path that exists, whatever it is
// (a file, a link, a directory), is refused with ErrBaselineFrozen and left
// as it is; the file appears whole or not at all (it is written beside path
// and linked into place, which cannot replace a file). A zero Frozen becomes
// the clock's now. The file is private (0600) and holds counts, no text.
func FreezeBaseline(path string, b Baseline) error {
	if b.Tickets < 1 {
		return errors.New("freeze baseline: a baseline of no tickets")
	}
	if b.Frozen.IsZero() {
		b.Frozen = time.Now()
	}
	b.Frozen = b.Frozen.UTC()
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("freeze baseline: %w", err)
	}
	data = append(data, '\n')
	frozen := fmt.Errorf("%w: %s exists", ErrBaselineFrozen, path)
	if _, err := os.Lstat(path); err == nil {
		return frozen
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("freeze baseline: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("freeze baseline: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".baseline-*.tmp")
	if err != nil {
		return fmt.Errorf("freeze baseline: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("freeze baseline: %w", werr)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return frozen
		}
		return fmt.Errorf("freeze baseline: %w", err)
	}
	return nil
}

// LoadBaseline reads the frozen baseline at path: nil and no error when
// there is none yet, an error for a file that is not a baseline.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%s is not a baseline: %w", path, err)
	}
	if b.Tickets < 1 || b.Frozen.IsZero() {
		return nil, fmt.Errorf("%s is not a baseline: it names no tickets or no freeze time", path)
	}
	return &b, nil
}

// ReadLabelled reads the operator's labelled messages: one JSON object per
// line, {"text": "...", "class": "step_start"}, blank lines skipped. An error
// names the line, never its text.
func ReadLabelled(path string) ([]Labelled, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Labelled
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var l Labelled
		if err := json.Unmarshal(line, &l); err != nil {
			return nil, fmt.Errorf("%s: line %d is not a JSON object with text and class", path, n)
		}
		if strings.TrimSpace(l.Text) == "" {
			return nil, fmt.Errorf("%s: line %d has no text", path, n)
		}
		if !slices.Contains(classes, l.Class) {
			return nil, fmt.Errorf("%s: line %d has the class %q (want %s)", path, n, l.Class, strings.Join(classes, ", "))
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// Skipped counts the transcripts ReadSessions leaves out, by why: each is
// counted once, under the first reason that holds, in this order.
type Skipped struct {
	// Headless: nobody typed in it: Claude Code run without its terminal UI
	// (claude -p, the SDK: an entrypoint other than cli) or `codex exec`
	// (originator codex_exec, or source exec).
	Headless int
	// Subagent: another agent's thread: a Claude Code subagent's transcript
	// (a file under a subagents directory) or a Codex subagent's rollout (a
	// source that names a subagent).
	Subagent int
	// ByAgent: a Codex session Claude Code started (originator "Claude
	// Code").
	ByAgent int
	// Magnum: it started in a directory where magnum or the factory starts
	// sessions of its own, which hold magnum's prompts.
	Magnum int
	// NoMessage: no message of the operator's.
	NoMessage int
	// NoKey: no ticket key in its path, its messages or its git branches.
	NoKey int
}

// Total is how many transcripts were left out.
func (s Skipped) Total() int {
	return s.Headless + s.Subagent + s.ByAgent + s.Magnum + s.NoMessage + s.NoKey
}

// ReadSessions reads the operator's old transcripts under dir (every .jsonl
// file: a Claude Code transcript, or a Codex rollout when the file is named
// rollout-*) into sessions: the messages the operator typed, from the user
// turns agents.ClaudeUserTurns and agents.CodexUserTurns read, less turns a
// tool or a task notification began and the records of the slash commands
// magnum types (magnum: agents.MagnumCommands, the list the factory drops
// from what it records of its panes, so both sides count the same; nil drops
// none). Only the operator's interactive sessions are read: a transcript of
// a headless run, a subagent's, a Codex session Claude Code started, or one
// whose session started in a directory magnumDir reports as one where magnum
// or the factory starts its own (nil: none) is left out before its messages
// are read (agents.ClaudeSessionFacts, agents.CodexSessionFacts; Skipped
// says which is which). A session's ticket is the key keyOf finds in the
// file's path below dir, else in its first message that holds one, else in
// the first of its git branches that holds one. A transcript with no message
// or no key is not guessed at either. Every transcript left out is counted
// in skipped, by its reason. The sessions come oldest first, by the time of
// their first message, so that the first session of a ticket is the first
// one (BuildBaseline); a transcript whose turns carry no timestamp comes
// after the rest, and is never a ticket's first session. Prompts queued
// while an agent worked are not read, so a count is never above what was
// typed. Symbolic links are not followed.
func ReadSessions(dir string, keyOf func(string) string, magnum map[string]bool, magnumDir func(string) bool) (sessions []Session, skipped Skipped, err error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, Skipped{}, err
	}
	if !st.IsDir() {
		return nil, Skipped{}, fmt.Errorf("%s is not a directory", dir)
	}
	type dated struct {
		session Session
		began   time.Time
	}
	var found []dated
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if slices.Contains(strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/"), "subagents") {
			skipped.Subagent++
			return nil
		}
		codex := strings.HasPrefix(filepath.Base(path), "rollout-")
		facts, err := sessionFacts(path, codex)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		switch {
		case facts.Originator == "Claude Code":
			skipped.ByAgent++
			return nil
		case facts.Entrypoint != "" && facts.Entrypoint != "cli", facts.Originator == "codex_exec", facts.Source == "exec":
			skipped.Headless++
			return nil
		case facts.Source == agents.CodexSourceSubagent:
			skipped.Subagent++
			return nil
		case magnumDir != nil && facts.Cwd != "" && magnumDir(facts.Cwd):
			skipped.Magnum++
			return nil
		}
		turns, err := operatorTurns(path, codex, magnum)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if len(turns) == 0 {
			skipped.NoMessage++
			return nil
		}
		messages := make([]string, len(turns))
		for i, t := range turns {
			messages[i] = t.Text
		}
		key := keyOf(rel)
		for i := 0; key == "" && i < len(messages); i++ {
			key = keyOf(messages[i])
		}
		for i := 0; key == "" && i < len(facts.Branches); i++ {
			key = keyOf(facts.Branches[i])
		}
		if key == "" {
			skipped.NoKey++
			return nil
		}
		found = append(found, dated{Session{Key: key, Messages: messages}, turns[0].At})
		return nil
	})
	if err != nil {
		return nil, Skipped{}, err
	}
	slices.SortStableFunc(found, func(a, b dated) int {
		switch {
		case a.began.IsZero() && b.began.IsZero():
			return 0
		case a.began.IsZero(): // no timestamp to say when it began: after the rest
			return 1
		case b.began.IsZero():
			return -1
		}
		return a.began.Compare(b.began)
	})
	for _, f := range found {
		sessions = append(sessions, f.session)
	}
	return sessions, skipped, nil
}

// sessionFacts reads what the transcript (a Codex rollout when codex) at
// path says of its session.
func sessionFacts(path string, codex bool) (agents.SessionFacts, error) {
	if codex {
		return agents.CodexSessionFacts(path)
	}
	return agents.ClaudeSessionFacts(path)
}

// operatorTurns are the turns the operator typed in the transcript (a Codex
// rollout when codex) at path: none a tool or a notification began, and no
// record of a slash command of magnum's (agents.MagnumCommand).
func operatorTurns(path string, codex bool, magnum map[string]bool) ([]agents.UserTurn, error) {
	var turns []agents.UserTurn
	var err error
	if codex {
		turns, err = agents.CodexUserTurns(path, time.Time{})
	} else {
		turns, err = agents.ClaudeUserTurns(path, time.Time{})
	}
	if err != nil {
		return nil, err
	}
	var out []agents.UserTurn
	for _, t := range turns {
		if (t.Origin == "" || t.Origin == "human") && strings.TrimSpace(t.Text) != "" && !agents.MagnumCommand(t.Text, magnum) {
			out = append(out, t)
		}
	}
	return out, nil
}
