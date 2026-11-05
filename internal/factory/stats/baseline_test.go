package stats_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

// magnumCommands are the slash commands magnum types with the built-in
// kinds, which the baseline drops as the factory does.
var magnumCommands = agents.MagnumCommands(config.Defaults())

// The tracked set is 20 invented messages. The operator's own 100 stay
// outside the repository and are read the same way.
func TestClassifierAccuracyOnTheTrackedSet(t *testing.T) {
	t.Parallel()
	set, err := stats.ReadLabelled(filepath.Join("testdata", "labelled.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 20 {
		t.Fatalf("the tracked set has %d messages, want 20", len(set))
	}
	classes := map[string]int{}
	right := 0
	for _, l := range set {
		classes[l.Class]++
		if got := stats.Classify(l.Text); got == l.Class {
			right++
		} else {
			t.Logf("Classify(%q) = %q, labelled %q", l.Text, got, l.Class)
		}
	}
	for _, class := range []string{registry.ClassStepStart, registry.ClassApproval, registry.ClassCorrection, registry.ClassAccess, registry.ClassInfo} {
		if classes[class] < 3 {
			t.Errorf("the tracked set has %d messages of class %s, want at least 3", classes[class], class)
		}
	}
	if acc := float64(right) / float64(len(set)); acc < 0.9 {
		t.Errorf("accuracy %.0f%% (%d of %d), want at least 90%%", acc*100, right, len(set))
	}
	b, err := stats.BuildBaseline([]stats.Session{{Key: "PR-12301", Messages: []string{"hello"}}}, set)
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(right) / float64(len(set)); b.Accuracy != want {
		t.Errorf("the baseline's accuracy = %v, want the classifier's %v on the same set", b.Accuracy, want)
	}
}

func TestReadLabelledNamesTheLineOfABadOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"class.jsonl": `{"text": "open PR", "class": "step_start"}` + "\n" + `{"text": "a secret sentence", "class": "chatter"}` + "\n",
		"json.jsonl":  `{"text": "open PR", "class": "step_start"}` + "\n" + `not json a secret sentence` + "\n",
		"text.jsonl":  `{"text": "  ", "class": "info"}` + "\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := stats.ReadLabelled(path)
		if err == nil || !strings.Contains(err.Error(), "line") {
			t.Errorf("%s: err = %v, want one that names the line", name, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error carries the operator's text: %v", name, err)
		}
	}
	if _, err := stats.ReadLabelled(filepath.Join(dir, "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file = %v, want fs.ErrNotExist", err)
	}
	blank := filepath.Join(dir, "blank.jsonl")
	if err := os.WriteFile(blank, []byte("\n"+`{"text": "open PR", "class": "step_start"}`+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := stats.ReadLabelled(blank); err != nil || len(got) != 1 {
		t.Errorf("blank lines are skipped: %v, %v", got, err)
	}
}

// Three sessions of one ticket are one ticket with the sum of their
// messages, never three tickets.
func TestBaselineCountsPerTicketNotPerSession(t *testing.T) {
	t.Parallel()
	sessions := []stats.Session{
		{Key: "PR-12301", Messages: []string{"look at the points sync", "open PR", "the refund path only matters for expired points", "yes"}},
		{Key: "PR-12301", Messages: []string{"merge master", "wrong table, use the ledger", "the spec output is in the log"}},
		{Key: "PR-12302", Messages: []string{"what does the retry do?", "push"}},
		{Key: "PR-12301", Messages: []string{"open PR", "ok", "lgtm", "  ", "thanks", "done"}}, // the blank is no message
		{Key: "PR-12303", Messages: []string{"hello", "the mailer is slow", "yes", "no, wrong", "ok", "push it"}},
	}
	labelled := []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}, {Text: "yes", Class: registry.ClassApproval},
		{Text: "the mailer is slow", Class: registry.ClassCorrection}, {Text: "hello", Class: registry.ClassInfo}}
	b, err := stats.BuildBaseline(sessions, labelled)
	if err != nil {
		t.Fatal(err)
	}
	if b.Tickets != 3 {
		t.Errorf("Tickets = %d, want 3 (five sessions of three keys)", b.Tickets)
	}
	// PR-12301: 4 + 3 + 5 messages = 12, and open PR, merge master, open PR = 3 step starts.
	// PR-12302: 2 messages, 1 step start (push). PR-12303: 6 messages, 1 step start (push it).
	wantTyped := stats.Dist{Median: 6, P25: 4, P75: 9, Min: 2, Max: 12}
	if b.TypedPerTicket != wantTyped {
		t.Errorf("TypedPerTicket = %+v, want %+v", b.TypedPerTicket, wantTyped)
	}
	wantSteps := stats.Dist{Median: 1, P25: 1, P75: 2, Min: 1, Max: 3}
	if b.StepStartsPerTicket != wantSteps {
		t.Errorf("StepStartsPerTicket = %+v, want %+v", b.StepStartsPerTicket, wantSteps)
	}
	// "the mailer is slow" is labelled a correction on purpose: 3 of 4 right.
	if b.Accuracy != 0.75 {
		t.Errorf("Accuracy = %v, want 0.75", b.Accuracy)
	}
	if !b.Frozen.IsZero() {
		t.Errorf("BuildBaseline stamps no freeze time; FreezeBaseline does: %s", b.Frozen)
	}
}

// The first message of a ticket (the first of its first session) is the one
// that starts the work: typed, and never a step start, as a task's text is
// on the factory's side. The first message of a later session keeps its
// class: a session opened only to start the next step is what the measure
// counts.
func TestBaselineFirstMessageOfATicketIsTypedAndNeverAStepStart(t *testing.T) {
	t.Parallel()
	sessions := []stats.Session{
		{Key: "PR-12301", Messages: []string{"open PR", "the refund path only matters for expired points", "push"}},
		{Key: "PR-12302", Messages: []string{"merge master"}},
		{Key: "PR-12301", Messages: []string{"open PR", "yes"}},
	}
	b, err := stats.BuildBaseline(sessions, []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	// PR-12301: 5 typed, step starts push and the later session's open PR = 2.
	// PR-12302: 1 typed, its only message is its first = 0.
	if want := (stats.Dist{Median: 3, P25: 2, P75: 4, Min: 1, Max: 5}); b.TypedPerTicket != want {
		t.Errorf("TypedPerTicket = %+v, want %+v", b.TypedPerTicket, want)
	}
	if want := (stats.Dist{Median: 1, P25: 0.5, P75: 1.5, Min: 0, Max: 2}); b.StepStartsPerTicket != want {
		t.Errorf("StepStartsPerTicket = %+v, want %+v (the first message of each ticket is no step start)", b.StepStartsPerTicket, want)
	}

	// A blank message opens nothing: the first message is the first that is one.
	blank := []stats.Session{{Key: "PR-12303", Messages: []string{" ", "open PR", "push"}}}
	b, err = stats.BuildBaseline(blank, []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	if b.TypedPerTicket.Max != 2 || b.StepStartsPerTicket.Max != 1 {
		t.Errorf("a ticket opened by a blank message: typed %v, step starts %v; want 2 and 1", b.TypedPerTicket.Max, b.StepStartsPerTicket.Max)
	}
}

func TestBuildBaselineRefusesWhatItCannotMeasure(t *testing.T) {
	t.Parallel()
	one := []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}}
	sessions := []stats.Session{{Key: "PR-12301", Messages: []string{"open PR"}}}
	for name, c := range map[string]struct {
		sessions []stats.Session
		labelled []stats.Labelled
	}{
		"no sessions":         {nil, one},
		"no message at all":   {[]stats.Session{{Key: "PR-12301", Messages: []string{" "}}}, one},
		"a session of no key": {[]stats.Session{{Messages: []string{"open PR"}}}, one},
		"no labelled set":     {sessions, nil},
		"an unknown class":    {sessions, []stats.Labelled{{Text: "open PR", Class: "chatter"}}},
	} {
		if _, err := stats.BuildBaseline(c.sessions, c.labelled); err == nil {
			t.Errorf("%s: a baseline was built", name)
		}
	}
}

func TestDistOfEvenAndOddCounts(t *testing.T) {
	t.Parallel()
	got := func(counts ...int) stats.Dist {
		var ss []stats.Session
		for i, n := range counts {
			ss = append(ss, stats.Session{Key: fmt.Sprintf("PR-%d", 12301+i), Messages: make([]string, n)})
			for j := range ss[i].Messages {
				ss[i].Messages[j] = "word"
			}
		}
		b, err := stats.BuildBaseline(ss, []stats.Labelled{{Text: "word", Class: registry.ClassInfo}})
		if err != nil {
			t.Fatal(err)
		}
		return b.TypedPerTicket
	}
	if d := got(7); d != (stats.Dist{Median: 7, P25: 7, P75: 7, Min: 7, Max: 7}) {
		t.Errorf("one ticket: %+v", d)
	}
	if d := got(10, 2); d != (stats.Dist{Median: 6, P25: 4, P75: 8, Min: 2, Max: 10}) {
		t.Errorf("two tickets: %+v", d)
	}
	if d := got(5, 1, 9, 3, 7); d != (stats.Dist{Median: 5, P25: 3, P75: 7, Min: 1, Max: 9}) {
		t.Errorf("five tickets: %+v", d)
	}
}

func sampleBaseline() stats.Baseline {
	return stats.Baseline{Frozen: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), Tickets: 12, Accuracy: 0.93,
		TypedPerTicket:      stats.Dist{Median: 41, P25: 30, P75: 77.5, Min: 12, Max: 120},
		StepStartsPerTicket: stats.Dist{Median: 9, P25: 5, P75: 14, Min: 1, Max: 30}}
}

// A baseline is frozen once: a second freeze, whatever it holds, leaves the
// first file as it is.
func TestFreezeBaselineNeverOverwrites(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "factory")
	path := filepath.Join(dir, "baseline.json")
	first := sampleBaseline()
	if err := stats.FreezeBaseline(path, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}

	second := first
	second.Tickets, second.Frozen = 99, first.Frozen.Add(time.Hour)
	err = stats.FreezeBaseline(path, second)
	if !errors.Is(err, stats.ErrBaselineFrozen) {
		t.Fatalf("a second freeze = %v, want ErrBaselineFrozen", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Errorf("the frozen file changed: %v\nbefore %s\nafter  %s", err, before, after)
	}

	loaded, err := stats.LoadBaseline(path)
	if err != nil || loaded == nil || *loaded != first {
		t.Errorf("LoadBaseline = %+v, %v; want what was frozen", loaded, err)
	}

	// A refused or a finished freeze leaves no temporary file behind.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v (%v), want only baseline.json", entries, err)
	}
}

// A path that is anything at all, a dangling link included, is not free.
func TestFreezeBaselineRefusesAnyExistingPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "baseline.json")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := stats.FreezeBaseline(link, sampleBaseline()); !errors.Is(err, stats.ErrBaselineFrozen) {
		t.Errorf("freeze over a dangling link = %v, want ErrBaselineFrozen", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the freeze wrote through the link: %v", err)
	}
}

// The moment of the freeze is the baseline's date; a caller that leaves it
// zero gets the clock's.
func TestFreezeBaselineStampsTheFreezeTime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "baseline.json")
	b := sampleBaseline()
	b.Frozen = time.Time{}
	before := time.Now()
	if err := stats.FreezeBaseline(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := stats.LoadBaseline(path)
	if err != nil || got == nil {
		t.Fatalf("LoadBaseline = %v, %v", got, err)
	}
	if got.Frozen.Before(before.Add(-time.Second)) || got.Frozen.After(time.Now().Add(time.Second)) {
		t.Errorf("Frozen = %s, want about now", got.Frozen)
	}
}

func TestLoadBaselineMissingAndBroken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, err := stats.LoadBaseline(filepath.Join(dir, "baseline.json")); got != nil || err != nil {
		t.Errorf("a missing baseline = %v, %v; want none and no error", got, err)
	}
	for name, body := range map[string]string{"junk": "not json", "empty": "{}", "no tickets": `{"frozen":"2026-11-02T09:30:00Z","tickets":0}`} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := stats.LoadBaseline(path); err == nil {
			t.Errorf("%s: loaded %+v", name, got)
		}
	}
}

// --- the old transcripts ---

func claudeLine(at time.Time, origin, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.UTC().Format(time.RFC3339), "turnOrigin": origin,
		"message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

func codexLine(at time.Time, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": at.UTC().Format(time.RFC3339),
		"payload": map[string]any{"type": "user_message", "message": text}})
	return string(b)
}

// claudeEntry is a typed prompt of a Claude Code transcript with the fields
// it records of its session (entrypoint, cwd, gitBranch).
func claudeEntry(at time.Time, text string, fields map[string]any) string {
	e := map[string]any{"type": "user", "timestamp": at.UTC().Format(time.RFC3339), "turnOrigin": "human",
		"message": map[string]any{"role": "user", "content": text}}
	maps.Copy(e, fields)
	b, _ := json.Marshal(e)
	return string(b)
}

// codexMeta is the session_meta line a Codex rollout begins with.
func codexMeta(at time.Time, originator string, source any, cwd, branch string) string {
	p := map[string]any{"id": "019e0000-0000-7000-8000-000000000001", "timestamp": at.UTC().Format(time.RFC3339),
		"originator": originator, "cwd": cwd}
	if source != nil {
		p["source"] = source
	}
	if branch != "" {
		p["git"] = map[string]any{"branch": branch}
	}
	b, _ := json.Marshal(map[string]any{"type": "session_meta", "timestamp": at.UTC().Format(time.RFC3339), "payload": p})
	return string(b)
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

var keyIn = regexp.MustCompile(`PR-[0-9]+`)

func firstKey(text string) string { return keyIn.FindString(text) }

// ReadSessions turns a directory of Claude Code transcripts and Codex
// rollouts into sessions of a ticket: the key comes from the file's path, else
// from its first messages, and a session with none is counted, not guessed.
func TestReadSessionsGroupsTranscriptsByTicketKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	writeTranscript(t, filepath.Join(dir, "proj-a", "one.jsonl"),
		claudeLine(at, "human", "work on PR-12301, the points sync loses archived rows"),
		claudeLine(at.Add(time.Minute), "human", "open PR"),
		claudeLine(at.Add(2*time.Minute), "task_notification", "<task-notification>done</task-notification>"),
		`{"type":"assistant","timestamp":"2026-10-01T10:03:00Z","message":{"role":"assistant","content":"done"}}`)
	writeTranscript(t, filepath.Join(dir, "proj-a", "two.jsonl"),
		claudeLine(at.Add(time.Hour), "human", "continue with PR-12301"),
		claudeLine(at.Add(2*time.Hour), "human", "merge master"))
	writeTranscript(t, filepath.Join(dir, "worktree-PR-12302", "three.jsonl"),
		claudeLine(at, "human", "look at the mailer"))
	writeTranscript(t, filepath.Join(dir, "codex", "rollout-2026-10-02T09-00-00-abc.jsonl"),
		codexLine(at, "fix PR-12303 please"), codexLine(at.Add(time.Minute), "push"))
	writeTranscript(t, filepath.Join(dir, "proj-b", "nokey.jsonl"), claudeLine(at, "human", "what is in this repo?"))
	writeTranscript(t, filepath.Join(dir, "proj-b", "empty.jsonl"))
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("PR-12399 is not a transcript"), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions, skipped, err := stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (stats.Skipped{NoKey: 1, NoMessage: 1}); skipped != want || skipped.Total() != 2 {
		t.Errorf("skipped = %+v, want %+v (the transcript with no key and the one with no message)", skipped, want)
	}
	byKey := map[string][]int{}
	for _, s := range sessions {
		byKey[s.Key] = append(byKey[s.Key], len(s.Messages))
	}
	want := map[string][]int{"PR-12301": {2, 2}, "PR-12302": {1}, "PR-12303": {2}}
	if len(byKey) != len(want) {
		t.Fatalf("sessions by key = %v, want %v", byKey, want)
	}
	for key, counts := range want {
		if !slices.Equal(byKey[key], counts) {
			t.Errorf("%s: sessions of %v messages, want %v (a task notification is no message)", key, byKey[key], counts)
		}
	}
	// Two sessions of one key are one ticket in the baseline.
	b, err := stats.BuildBaseline(sessions, []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Tickets != 3 || b.TypedPerTicket.Max != 4 {
		t.Errorf("baseline of the directory = %+v, want 3 tickets, the largest of 4 messages", b)
	}
}

// A session whose path and messages name no ticket is tied to the one its
// git branch names (a Claude entry's gitBranch, any of them; a rollout's
// session_meta git.branch), read with the same key function. A key in the
// path or the messages still wins over the branch.
func TestReadSessionsTiesASessionToTheTicketItsBranchNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	on := func(branch string) map[string]any {
		return map[string]any{"entrypoint": "cli", "cwd": "/work/shop", "gitBranch": branch}
	}
	writeTranscript(t, filepath.Join(dir, "proj", "a.jsonl"),
		claudeEntry(at, "look at the points sync", on("alice-PR-12370")), claudeEntry(at.Add(time.Minute), "open PR", on("alice-PR-12370")))
	writeTranscript(t, filepath.Join(dir, "proj", "b.jsonl"),
		claudeEntry(at.Add(time.Hour), "what is on master?", on("master")),
		claudeEntry(at.Add(2*time.Hour), "now the mailer", on("zhuravel-PR-12371-mailer")))
	writeTranscript(t, filepath.Join(dir, "codex", "rollout-2026-10-02T09-00-00-a.jsonl"),
		codexMeta(at, "codex-tui", "cli", "/work/shop", "zhuravel-PR-12372-refunds"), codexLine(at, "the refunds count twice"))
	writeTranscript(t, filepath.Join(dir, "worktree-PR-12373", "c.jsonl"),
		claudeEntry(at.Add(3*time.Hour), "the path names the ticket", on("alice-PR-12374")))
	writeTranscript(t, filepath.Join(dir, "proj", "d.jsonl"),
		claudeEntry(at.Add(4*time.Hour), "PR-12375: the first message names it", on("alice-PR-12376")))
	writeTranscript(t, filepath.Join(dir, "proj", "e.jsonl"), claudeEntry(at.Add(5*time.Hour), "what is in this repo?", on("master")))

	sessions, skipped, err := stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range sessions {
		got[s.Messages[0]] = s.Key
	}
	want := map[string]string{
		"look at the points sync":              "PR-12370",
		"what is on master?":                   "PR-12371", // a later entry's branch
		"the refunds count twice":              "PR-12372",
		"the path names the ticket":            "PR-12373",
		"PR-12375: the first message names it": "PR-12375",
	}
	if len(got) != len(want) {
		t.Errorf("sessions by first message = %v, want %v", got, want)
	}
	for first, key := range want {
		if got[first] != key {
			t.Errorf("the session of %q has the key %q, want %q", first, got[first], key)
		}
	}
	if want := (stats.Skipped{NoKey: 1}); skipped != want {
		t.Errorf("skipped = %+v, want %+v (the session on master alone)", skipped, want)
	}
}

// The baseline counts what the operator typed, so it reads only the
// operator's interactive sessions: a headless Claude Code run (claude -p, the
// SDK: an entrypoint other than cli), a subagent's transcript, a Codex
// session Claude Code started, `codex exec`, a Codex subagent and a session
// in a directory where magnum or the factory starts its own (the predicate
// the caller hands over) are left out, each counted by its reason, before
// their messages are read.
func TestReadSessionsSkipsTheSessionsTheOperatorNeverTypedIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	n := 0
	claude := func(fields map[string]any, path ...string) {
		t.Helper()
		n++
		writeTranscript(t, filepath.Join(append([]string{dir}, path...)...),
			claudeEntry(at.Add(time.Duration(n)*time.Minute), fmt.Sprintf("work on PR-%d", 12380+n), fields))
	}
	codex := func(originator string, source any, cwd string) {
		t.Helper()
		n++
		writeTranscript(t, filepath.Join(dir, "codex", fmt.Sprintf("rollout-2026-10-02T09-00-%02d-a.jsonl", n)),
			codexMeta(at, originator, source, cwd, ""), codexLine(at.Add(time.Duration(n)*time.Minute), fmt.Sprintf("work on PR-%d", 12380+n)))
	}
	interactive := map[string]any{"entrypoint": "cli", "cwd": "/work/shop"}
	claude(interactive, "proj", "kept.jsonl")
	claude(map[string]any{"cwd": "/work/shop"}, "proj", "older-claude-code.jsonl") // no entrypoint: kept
	codex("codex-tui", "cli", "/work/shop")                                        // kept
	codex("Codex Desktop", "vscode", "/work/shop")                                 // kept

	claude(map[string]any{"entrypoint": "sdk-cli", "cwd": "/work/shop"}, "proj", "claude-p.jsonl")
	claude(map[string]any{"entrypoint": "sdk-ts", "cwd": "/work/shop"}, "proj", "sdk.jsonl")
	writeTranscript(t, filepath.Join(dir, "proj", "headless-silent.jsonl"),
		`{"type":"system","entrypoint":"sdk-cli","cwd":"/work/shop"}`) // headless before it is silent
	codex("codex_exec", "exec", "/work/shop")
	codex("codex-tui", "exec", "/work/shop")

	claude(interactive, "proj", "0b8a", "subagents", "agent-a1.jsonl")
	codex("codex-tui", map[string]any{"subagent": "review"}, "/work/shop")
	codex("codex-tui", map[string]any{"subagent": map[string]any{"other": "guardian"}}, "/work/shop")
	codex("codex-tui", map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"depth": 1}}}, "/work/shop")

	codex("Claude Code", "exec", "/work/shop")

	claude(map[string]any{"entrypoint": "cli", "cwd": "/magnum/slots/shop.review3"}, "slot", "a.jsonl")
	codex("codex-tui", "cli", "/magnum/copies/factory2/app")

	writeTranscript(t, filepath.Join(dir, "proj", "silent.jsonl"), `{"type":"system","entrypoint":"cli","cwd":"/work/shop"}`)
	writeTranscript(t, filepath.Join(dir, "proj", "nokey.jsonl"), claudeEntry(at, "what is in this repo?", interactive))

	magnums := func(cwd string) bool { return strings.HasPrefix(cwd, "/magnum/") }
	sessions, skipped, err := stats.ReadSessions(dir, firstKey, magnumCommands, magnums)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range sessions {
		keys = append(keys, s.Key)
	}
	slices.Sort(keys)
	if want := []string{"PR-12381", "PR-12382", "PR-12383", "PR-12384"}; !slices.Equal(keys, want) {
		t.Errorf("sessions of %v, want %v (the interactive ones)", keys, want)
	}
	want := stats.Skipped{Headless: 5, Subagent: 4, ByAgent: 1, Magnum: 2, NoMessage: 1, NoKey: 1}
	if skipped != want || skipped.Total() != 14 {
		t.Errorf("skipped = %+v (total %d), want %+v (14)", skipped, skipped.Total(), want)
	}

	// Without a predicate no directory is magnum's.
	sessions, skipped, err = stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil || len(sessions) != 6 || skipped.Magnum != 0 {
		t.Errorf("without a predicate: %d sessions, skipped %+v, %v; want the two in magnum's directories kept", len(sessions), skipped, err)
	}
}

// The sessions come oldest first, whatever their files are called: the first
// of a ticket is the one its baseline counts as having started the work.
func TestReadSessionsComesOldestFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	writeTranscript(t, filepath.Join(dir, "a-later.jsonl"),
		claudeLine(at.Add(time.Hour), "human", "PR-12301: check the mailer"), claudeLine(at.Add(2*time.Hour), "human", "yes"))
	writeTranscript(t, filepath.Join(dir, "z-first.jsonl"),
		claudeLine(at, "human", "open PR for PR-12301"), claudeLine(at.Add(time.Minute), "human", "ok"))

	sessions, _, err := stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Messages[0] != "open PR for PR-12301" || sessions[1].Messages[0] != "PR-12301: check the mailer" {
		t.Fatalf("sessions %+v, want the one that began at 10:00 first", sessions)
	}
	b, err := stats.BuildBaseline(sessions, []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Tickets != 1 || b.TypedPerTicket.Max != 4 || b.StepStartsPerTicket.Max != 0 {
		t.Errorf("baseline %+v, want one ticket of 4 messages and no step start: the ticket's first message is the earlier session's open PR", b)
	}
}

// A transcript whose turns carry no timestamp cannot say when it began, so it
// comes after every session that can: it never becomes the first session of a
// ticket and never takes the exemption of the ticket's first message.
func TestReadSessionsPutsASessionWithoutTimestampsLast(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	untimed, err := json.Marshal(map[string]any{"type": "user", "turnOrigin": "human",
		"message": map[string]any{"role": "user", "content": "open PR for PR-12301"}})
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(dir, "a-untimed.jsonl"), string(untimed))
	writeTranscript(t, filepath.Join(dir, "b-timed.jsonl"), claudeLine(at, "human", "work on PR-12301"))

	sessions, skipped, err := stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil || skipped.Total() != 0 {
		t.Fatalf("ReadSessions = %v, skipped %+v, %v", sessions, skipped, err)
	}
	if len(sessions) != 2 || sessions[0].Messages[0] != "work on PR-12301" || sessions[1].Messages[0] != "open PR for PR-12301" {
		t.Fatalf("sessions %+v, want the timed one first and the untimed one last, though its name sorts first", sessions)
	}
	b, err := stats.BuildBaseline(sessions, []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	if b.TypedPerTicket.Max != 2 || b.StepStartsPerTicket.Max != 1 {
		t.Errorf("baseline %+v, want 2 messages and 1 step start: the untimed session's open PR is not the ticket's first message", b)
	}
}

// The record of a slash command magnum types (/model here) is no message of
// the baseline's, as it is none of what the factory records of its panes
// (runtime TestTheFactorysOwnPromptIsNoTypedTurn drops the same record), so
// both sides count the same; any other command record stays a message.
func TestReadSessionsDropsTheCommandsMagnumTypes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	writeTranscript(t, filepath.Join(dir, "a.jsonl"),
		claudeLine(at, "human", "fix PR-12316, the refund job counts twice"),
		claudeLine(at.Add(time.Minute), "", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"))
	writeTranscript(t, filepath.Join(dir, "b.jsonl"),
		claudeLine(at.Add(time.Hour), "human", "look at PR-12317"),
		claudeLine(at.Add(time.Hour+time.Minute), "", "<command-name>/cost</command-name>\n<command-message>cost</command-message>\n<command-args></command-args>"))

	sessions, _, err := stats.ReadSessions(dir, firstKey, magnumCommands, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || !slices.Equal(sessions[0].Messages, []string{"fix PR-12316, the refund job counts twice"}) ||
		len(sessions[1].Messages) != 2 {
		t.Fatalf("sessions %+v, want the /model record dropped and the /cost one kept", sessions)
	}
	b, err := stats.BuildBaseline(sessions[:1], []stats.Labelled{{Text: "open PR", Class: registry.ClassStepStart}})
	if err != nil {
		t.Fatal(err)
	}
	if b.TypedPerTicket.Max != 1 {
		t.Errorf("baseline %+v, want one typed message for the session with the /model record", b)
	}
}

func TestReadSessionsOfAMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, _, err := stats.ReadSessions(filepath.Join(t.TempDir(), "nope"), firstKey, magnumCommands, nil); err == nil {
		t.Error("a missing directory read as no sessions")
	}
}
