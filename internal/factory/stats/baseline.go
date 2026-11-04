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
// never over the sessions. Accuracy is the share of labelled that Classify
// gets right. It refuses sessions without a key, a set with no message and an
// empty or mislabelled labelled set, because a baseline it cannot measure is
// worse than none. The result has no Frozen time; FreezeBaseline sets it.
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
			if c == nil {
				c = &count{}
				tickets[s.Key] = c
			}
			c.typed++
			if Classify(m) == registry.ClassStepStart {
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

// ReadSessions reads the operator's old transcripts under dir (every .jsonl
// file: a Claude Code transcript, or a Codex rollout when the file is named
// rollout-*) into sessions: the messages the operator typed, from the user
// turns agents.ClaudeUserTurns and agents.CodexUserTurns read, less turns a
// tool or a task notification began. A session's ticket is the key keyOf finds
// in the file's path below dir, else in its first message that holds one. A
// transcript with no message or no key is not guessed at: it is counted in
// skipped. Prompts queued while an agent worked are not read, so a count is
// never above what was typed. Symbolic links are not followed.
func ReadSessions(dir string, keyOf func(string) string) (sessions []Session, skipped int, err error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	if !st.IsDir() {
		return nil, 0, fmt.Errorf("%s is not a directory", dir)
	}
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
		messages, err := operatorMessages(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		key := keyOf(rel)
		for i := 0; key == "" && i < len(messages); i++ {
			key = keyOf(messages[i])
		}
		if len(messages) == 0 || key == "" {
			skipped++
			return nil
		}
		sessions = append(sessions, Session{Key: key, Messages: messages})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return sessions, skipped, nil
}

// operatorMessages are the texts the operator typed in the transcript or
// rollout at path.
func operatorMessages(path string) ([]string, error) {
	var turns []agents.UserTurn
	var err error
	if strings.HasPrefix(filepath.Base(path), "rollout-") {
		turns, err = agents.CodexUserTurns(path, time.Time{})
	} else {
		turns, err = agents.ClaudeUserTurns(path, time.Time{})
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range turns {
		if (t.Origin == "" || t.Origin == "human") && strings.TrimSpace(t.Text) != "" {
			out = append(out, t.Text)
		}
	}
	return out, nil
}
