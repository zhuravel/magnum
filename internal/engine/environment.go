package engine

// Failures of the review machine: the judge reports the commands that could
// not run on the machine magnum reviews on (a test database missing for the
// worktree, a seed that fails), and the round records them in one
// round.environment event (pipeline/posted.go); the author never sees them.
// The operator fixes the machine, so a command that fails in
// MachineToastRounds rounds of one repository within MachineWindow toasts
// once, and `magnum status` lists every group of the window.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// MachineEventKind is the event a round records the review machine's
	// failures in: subject pr:<owner>/<name>#<N>, data {"failures": [{"cmd",
	// "error"} or a bare error string, ...]}.
	MachineEventKind = "round.environment"
	// MachineWindow is how far back the failures are grouped, and how long a
	// group's toast stays deduped.
	MachineWindow = 24 * time.Hour
	// MachineToastRounds is the number of rounds of one repository a command
	// fails in within MachineWindow before the operator gets a toast.
	MachineToastRounds = 3
)

// kindMachine counts the review machine's failures in a batch summary.
var kindMachine = notify.Kind{One: "review machine failure", Many: "review machine failures"}

// MachineGroup is one command failing on the review machine in the rounds
// of one repository.
type MachineGroup struct {
	Repo string // owner/name
	// Cmd is the command, its test-file arguments left out (machineCommand);
	// "" when the judge named none.
	Cmd string
	// Error is what the newest round said of it (untrusted agent text).
	Error string
	// Rounds counts the events (one per round) that list the command.
	Rounds int
	// First and Last are the oldest and the newest of those events.
	First, Last time.Time
}

// MachineGroups groups the round.environment events of evs (other kinds
// and events without a PR subject are skipped) by repository and command.
// An event counts once per group however often it lists the command; an
// event whose failures cannot be read counts as one failure without a
// command, its message the error. Most rounds come first, then the newest.
func MachineGroups(evs []store.Event) []MachineGroup {
	type key struct{ repo, cmd string }
	idx := map[key]int{}
	var out []MachineGroup
	for _, ev := range evs {
		repo := machineRepo(ev)
		if repo == "" {
			continue
		}
		fs := machineFailures(ev.Data)
		if len(fs) == 0 {
			fs = []machineFailure{{Error: ev.Message}}
		}
		var keys []key // this event's groups, in order
		errs := map[key]string{}
		for _, f := range fs {
			k := key{repo, machineCommand(f.Cmd)}
			if _, ok := errs[k]; !ok {
				keys = append(keys, k)
			}
			if errs[k] == "" {
				errs[k] = strings.TrimSpace(f.Error)
			}
		}
		for _, k := range keys {
			i, ok := idx[k]
			if !ok {
				i = len(out)
				idx[k] = i
				out = append(out, MachineGroup{Repo: k.repo, Cmd: k.cmd, First: ev.At})
			}
			g := &out[i]
			g.Rounds++
			if ev.At.Before(g.First) {
				g.First = ev.At
			}
			if !ev.At.Before(g.Last) {
				g.Last = ev.At
				if errs[k] != "" || g.Error == "" {
					g.Error = errs[k]
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b MachineGroup) int {
		return cmp.Or(cmp.Compare(b.Rounds, a.Rounds), b.Last.Compare(a.Last))
	})
	return out
}

// machineFailure is one entry of a round.environment event's failures.
type machineFailure struct {
	Cmd   string `json:"cmd"`
	Error string `json:"error"`
}

// machineFailures reads an event's failures leniently: an entry that is a
// JSON string is an error without a command, a malformed one is skipped.
func machineFailures(data json.RawMessage) []machineFailure {
	var d struct {
		Failures []json.RawMessage `json:"failures"`
	}
	if len(data) == 0 || json.Unmarshal(data, &d) != nil {
		return nil
	}
	var out []machineFailure
	for _, raw := range d.Failures {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, machineFailure{Error: s})
			}
			continue
		}
		var f machineFailure
		if json.Unmarshal(raw, &f) != nil || strings.TrimSpace(f.Cmd+f.Error) == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// machineRepo is the repository (owner/name) of an event about a PR
// (subject pr:<owner>/<name>#<N>); "" for any other event.
func machineRepo(ev store.Event) string {
	if ev.Kind != MachineEventKind || ev.Subject == nil {
		return ""
	}
	s, ok := strings.CutPrefix(*ev.Subject, "pr:")
	i := strings.LastIndexByte(s, '#')
	if !ok || i < 0 || !strings.Contains(s[:i], "/") {
		return ""
	}
	return s[:i]
}

var (
	// envAssignRe matches a leading VAR=value word of a command.
	envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// fileArgRe matches a file argument without a directory: x_spec.rb,
	// x_spec.rb:12.
	fileArgRe = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]*(:\d+)*$`)
)

// machineCommand is the command a failure is grouped by: its words on one
// line, up to the first file argument after the program (a word with a
// slash or a file extension), so `bundle exec rspec spec/a_spec.rb` and
// `bundle exec rspec spec/b_spec.rb:12` are both `bundle exec rspec`, the
// command that fails for every PR. Leading VAR=value words stay.
func machineCommand(cmd string) string {
	words := strings.Fields(cmd)
	program := false
	for i, w := range words {
		if !program {
			program = !envAssignRe.MatchString(w)
			continue
		}
		if strings.Contains(w, "/") || fileArgRe.MatchString(w) {
			return strings.Join(words[:i], " ")
		}
	}
	return strings.Join(words, " ")
}

// machineText is agent text for a toast: secrets redacted, control
// characters and runs of whitespace one space, cut to n runes.
func machineText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, execx.Redact(s))
	return textx.Clip(strings.Join(strings.Fields(s), " "), n)
}

// noteMachine toasts each command that failed on the review machine in
// MachineToastRounds rounds of one repository within MachineWindow, once
// per window: e.machineToasted keeps the groups offered in this run, the
// batcher's dedupe in the registry outlives a restart. It groups the
// window's events only when a new one was written since its last look.
func (e *Engine) noteMachine(ctx context.Context) {
	now := e.now()
	evs, err := e.st.EventsOfKindsSince(ctx, now.Add(-MachineWindow), MachineEventKind)
	if err != nil {
		if e.logOnce("machine", err.Error(), now) {
			e.log.Warn("review machine failures", "err", err)
		}
		return
	}
	if len(evs) == 0 || evs[len(evs)-1].ID == e.machineLast {
		return
	}
	e.machineLast = evs[len(evs)-1].ID
	if e.machineToasted == nil {
		e.machineToasted = map[string]time.Time{}
	}
	for k, at := range e.machineToasted {
		if now.Sub(at) >= MachineWindow {
			delete(e.machineToasted, k)
		}
	}
	for _, g := range MachineGroups(evs) {
		if g.Rounds < MachineToastRounds {
			continue
		}
		key := "machine:" + g.Repo + ":" + g.Cmd
		if _, ok := e.machineToasted[key]; ok {
			continue
		}
		e.machineToasted[key] = now
		e.info(machineToast(key, g, now))
	}
}

// machineToast is the toast of a group that reached MachineToastRounds.
func machineToast(key string, g MachineGroup, now time.Time) notify.Item {
	_, name, _ := strings.Cut(g.Repo, "/")
	what := "the review machine fails"
	if cmd := machineText(g.Cmd, 40); cmd != "" {
		what = "`" + cmd + "` fails on the review machine"
	}
	first, today := g.First.Local(), now.Local()
	since := first.Format("15:04")
	if first.YearDay() != today.YearDay() || first.Year() != today.Year() {
		since = first.Format("Mon 15:04")
	}
	rounds := textx.Count(g.Rounds, "round", "rounds")
	body := fmt.Sprintf("%s of %s since %s", rounds, g.Repo, since)
	if msg := machineText(g.Error, 200); msg != "" {
		body += ": " + msg
	}
	body += "; `magnum status` lists the machine's failures."
	return notify.Item{Key: key, Title: "magnum: " + what + " (" + name + ")", Body: body,
		Line: name + ": " + what + " (" + rounds + ")", Kind: kindMachine, Window: MachineWindow}
}
