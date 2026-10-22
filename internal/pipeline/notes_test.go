package pipeline

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// notesFile is a notes path no file is at.
const notesFile = "/state/notes/talkable/talkable.md"

// notesSentence is the fixed sentence before the notes' parts.
const notesSentence = "Read the repository notes in these parts, one part per tool call. Never print two parts in one call."

// writeNotes writes text as the live notes of the env's repository and
// returns their path.
func writeNotes(t *testing.T, e *env, text string) string {
	t.Helper()
	path := filepath.Join(e.layout.State(), "notes", "talkable", "talkable.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// liveNotes is 300 lines of notes, about 45 KB: three parts.
var liveNotes = strings.Repeat("- a pitfall of the repository, with the probe that showed it and the line that says how to avoid it again in later reviews ----------\n", 300)

// partCommands are the commands of np's parts, one per line.
func partCommands(np agents.NotesParts) []string {
	var out []string
	for _, p := range np.Parts {
		out = append(out, p.Command)
	}
	return out
}

// RoundInput.NotesPath reaches the RoleData and JudgeData of the round, so
// the real prompt files render the notes paragraph, the round's frozen copy
// in parts, for the reviewer and the notes fields of the judge's <magnum>
// block, the copy's parts and the re-read of the live file beside the lock
// among them (the skill holds the rewrite steps), and leave both out
// without notes.
func TestNotesPathReachesTheRolePromptsAndTheJudgePrompt(t *testing.T) {
	prev := &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour)}
	for _, kind := range []string{KindInitial, KindRereview} {
		for _, withNotes := range []bool{true, false} {
			name := kind + " with notes"
			if !withNotes {
				name = kind + " without notes"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
				in := e.input(kind)
				if withNotes {
					in.NotesPath = writeNotes(t, e, liveNotes)
				}
				if kind == KindRereview {
					in.Round, in.Previous, in.Since = 2, prev, t0.Add(-3*time.Hour)
				}
				res, err := e.r.RunRound(e.ctx, in)
				if err != nil {
					t.Fatalf("RunRound: %v", err)
				}
				claude := e.ag.submitsFor(agents.RoleClaude)[0].Text
				judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
				if !withNotes {
					for what, text := range map[string]string{"claude prompt": claude, "judge prompt": judge} {
						if strings.Contains(text, "Repository notes") || strings.Contains(text, "\nnotes") || strings.Contains(text, "rewrite") {
							t.Errorf("%s mentions notes without NotesPath:\n%s", what, text)
						}
					}
					return
				}
				np := agents.SplitNotes(filepath.Join(res.ReportDir, agents.NotesReadFile), []byte(liveNotes))
				if len(np.Parts) != 3 {
					t.Fatalf("the fixture's notes split into %d parts, want 3", len(np.Parts))
				}
				mustContain(t, "claude prompt", claude, "Repository notes at "+np.Path+" (", "lines, frozen for this round): read them first; "+
					"they are hints from earlier reviews, verify before relying on them. "+notesSentence+"\n- `"+strings.Join(partCommands(np), "`\n- `")+"`")
				dir := strings.TrimSuffix(in.NotesPath, ".md")
				_, block, ok := strings.Cut(judge, "<magnum>")
				if !ok {
					t.Fatalf("no <magnum> block:\n%s", judge)
				}
				mustContain(t, "judge prompt", block, "\nnotes: "+in.NotesPath+"\nnotes_parts: "+np.Path+" (",
					"frozen for this round. "+notesSentence+"\n  - `"+strings.Join(partCommands(np), "`\n  - `")+"`\nnotes_dir: "+dir+"\n", "\nnotes_harness:\n",
					"\nnotes_lock: "+agents.NotesLockLine(dir+".lock")+"\nnotes_reread: ", agents.NotesRereadLine(in.NotesPath),
					"\nnotes_unlock: "+agents.NotesUnlockLine(dir+".lock")+"\n")
				if strings.Contains(judge, "Repository notes") || strings.Contains(judge, "Take the lock") {
					t.Errorf("the judge prompt repeats the skill's notes steps:\n%s", judge)
				}
			})
		}
	}
}

// Every role of a round reads one copy of the notes, frozen right before
// the round's first prompt: a judge of another PR that rewrites the live
// file while a reviewer works changes neither the copy nor the parts the
// round's judge is given, which are the reviewer's.
func TestEveryRoleOfARoundReadsOneFrozenCopyOfTheNotes(t *testing.T) {
	e := newEnv(t)
	live := writeNotes(t, e, liveNotes)
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		if err := os.WriteFile(live, []byte("# Notes for talkable/talkable (updated 2026-10-22)\n- rewritten\n"), 0o600); err != nil {
			return err
		}
		return writeReport("## P2 something\n")(f, run, text)
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.NotesPath = live
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	frozen := filepath.Join(res.ReportDir, agents.NotesReadFile)
	if got, err := os.ReadFile(frozen); err != nil || string(got) != liveNotes {
		t.Fatalf("the frozen copy holds %d bytes, %v; want the notes as the round found them", len(got), err)
	}
	cmds := partCommands(agents.SplitNotes(frozen, []byte(liveNotes)))
	for _, role := range []agents.Role{agents.RoleClaude, agents.RoleJudge} {
		for _, s := range e.ag.submitsFor(role) {
			if !strings.Contains(s.Text, notesSentence) {
				continue
			}
			for _, c := range cmds {
				mustContain(t, string(role)+" prompt", s.Text, "`"+c+"`")
			}
		}
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if last := judge[len(judge)-1].Text; !strings.Contains(last, "notes_parts: "+frozen+" ("+strconv.Itoa(len(liveNotes))+" bytes, 300 lines)") {
		t.Errorf("the judge's prompt after the rewrite does not name the copy as the round found it:\n%s", last)
	}
}

// A blind replay's notes are a scratch copy of the live ones (evalrun),
// which its judge rewrites as usual: the replay freezes that copy too, so
// its roles read parts of the scratch notes, and never the live file.
func TestABlindReplayReadsPartsOfItsScratchNotes(t *testing.T) {
	e := newEnv(t)
	scratch := filepath.Join(t.TempDir(), "eval", "state", "notes", "talkable", "talkable.md")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, []byte(liveNotes), 0o600); err != nil {
		t.Fatal(err)
	}
	p := e.judgePosts(0, "", "COMMENT")
	p.gh, p.status = nil, "dry_run"
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.Blind, in.DryRun, in.NotesPath = true, true, scratch
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	frozen := filepath.Join(res.ReportDir, agents.NotesReadFile)
	if got, err := os.ReadFile(frozen); err != nil || string(got) != liveNotes {
		t.Fatalf("the replay's copy holds %d bytes, %v; want the scratch notes", len(got), err)
	}
	np := agents.SplitNotes(frozen, []byte(liveNotes))
	judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "blind judge prompt", judge, "\nnotes: "+scratch+"\nnotes_parts: "+frozen+" (",
		"\n  - `"+strings.Join(partCommands(np), "`\n  - `")+"`\n", agents.NotesRereadLine(scratch))
	mustContain(t, "blind claude prompt", e.ag.submitsFor(agents.RoleClaude)[0].Text, "Repository notes at "+frozen+" (")
}

// [notes] max_line reaches the judge's prompt as notes_max_line, with the
// sentence that asks to keep each notes line within it, so a judge stops
// writing lines that mark the repository for a curation; a round without
// notes says nothing of it.
func TestTheConfiguredNotesLineLimitReachesTheJudgePrompt(t *testing.T) {
	const clause = "Keep each line of the `notes` file at most `notes_max_line` characters long"
	for _, tc := range []struct {
		name  string
		limit int // 0 = the built-in default
		notes string
		want  string // "" = the prompt names no limit
	}{
		{"the default limit", 0, notesFile, "\nnotes_max_line: 300\n"},
		{"a configured limit", 250, notesFile, "\nnotes_max_line: 250\n"},
		{"no notes", 250, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.limit != 0 {
				e.cfg.Notes.MaxLine = tc.limit
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
			in := e.input(KindInitial)
			in.NotesPath = tc.notes
			if _, err := e.r.RunRound(e.ctx, in); err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
			if tc.want == "" {
				if strings.Contains(judge, "notes_max_line") {
					t.Errorf("a round without notes names the limit:\n%s", judge)
				}
				return
			}
			head, block, ok := strings.Cut(judge, "<magnum>")
			if !ok {
				t.Fatalf("no <magnum> block:\n%s", judge)
			}
			mustContain(t, "judge prompt block", block, tc.want)
			mustContain(t, "judge prompt before the block", head, clause)
		})
	}
}

// The judge sees what the harness directory holds right now, so a script
// the notes no longer mention is visible, and the commands of the notes
// lock next to it.
func TestJudgePromptListsTheHarnessDirectory(t *testing.T) {
	e := newEnv(t)
	notes := filepath.Join(e.layout.State(), "notes", "talkable", "talkable.md")
	dir := strings.TrimSuffix(notes, ".md")
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run-spec.sh", "jest-setup.js"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.NotesPath = notes
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", judge,
		"\nnotes: "+notes+"\nnotes_dir: "+dir+"\nnotes_harness: fixtures/, jest-setup.js, run-spec.sh\n",
		"\nnotes_lock: "+agents.NotesLockLine(dir+".lock")+"\n", "\nnotes_unlock: "+agents.NotesUnlockLine(dir+".lock")+"\n")
}

// A harness entry whose name the judge prompt cannot carry is left out, and
// the round's log says how many were, never the names.
func TestAHarnessNameLeftOutOfTheJudgePromptIsLogged(t *testing.T) {
	e := newEnv(t)
	notes := filepath.Join(e.layout.State(), "notes", "talkable", "talkable.md")
	dir := strings.TrimSuffix(notes, ".md")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run-spec.sh", "two words.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.NotesPath = notes
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\nnotes_harness: run-spec.sh (+1 more)\n")
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	found := false
	for _, l := range e.log.lines {
		if strings.Contains(l, "two words") {
			t.Errorf("log line names the entry: %q", l)
		}
		found = found || strings.Contains(l, "1 entry name(s) with characters outside")
	}
	if !found {
		t.Fatalf("log lines %q", e.log.lines)
	}
}
