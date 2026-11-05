package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeLines writes each value as one JSON line of the file at path.
func writeLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		if s, ok := l.(string); ok {
			b.WriteString(s + "\n")
			continue
		}
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(j, '\n'))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A Claude Code transcript says how its session was started (entrypoint:
// cli for the terminal UI, sdk-cli for `claude -p`), where (the first
// entry's cwd) and on which git branches it ran, each once, in the order the
// entries first name them.
func TestClaudeSessionFactsSayHowWhereAndOnWhichBranches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	entry := func(typ, cwd, branch string) map[string]any {
		return map[string]any{"type": typ, "entrypoint": "sdk-cli", "cwd": cwd, "gitBranch": branch,
			"message": map[string]any{"role": typ, "content": "words"}}
	}
	writeLines(t, path,
		map[string]any{"type": "summary", "summary": "an older session"},
		entry("user", "/work/shop", "master"),
		entry("assistant", "/work/shop", "master"),
		`{"type":"user", half a line`,
		entry("user", "/work/shop/app", "zhuravel-PR-12350-points"),
		entry("user", "/work/shop", "master"),
		entry("user", "/work/shop", ""),
	)
	got, err := ClaudeSessionFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entrypoint != "sdk-cli" || got.Cwd != "/work/shop" || got.Originator != "" || got.Source != "" {
		t.Errorf("facts %+v, want entrypoint sdk-cli and the first cwd", got)
	}
	if want := []string{"master", "zhuravel-PR-12350-points"}; !slices.Equal(got.Branches, want) {
		t.Errorf("Branches = %q, want %q", got.Branches, want)
	}

	// An older Claude Code names no entrypoint: none is guessed.
	old := filepath.Join(dir, "old.jsonl")
	writeLines(t, old, map[string]any{"type": "user", "cwd": "/work/shop", "message": map[string]any{"role": "user", "content": "hi"}})
	if got, err := ClaudeSessionFacts(old); err != nil || got.Entrypoint != "" || got.Cwd != "/work/shop" || len(got.Branches) != 0 {
		t.Errorf("an older transcript: %+v, %v; want no entrypoint, its cwd and no branch", got, err)
	}
	if _, err := ClaudeSessionFacts(filepath.Join(dir, "gone.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing transcript = %v, want a not-exist error", err)
	}
}

// A Codex rollout says the same in its session_meta: the client that
// started it (originator), its source (a string, or an object for a
// subagent's), its cwd and its git branch.
func TestCodexSessionFactsReadTheSessionMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	meta := func(source any) map[string]any {
		return map[string]any{"type": "session_meta", "payload": map[string]any{"id": codexSID, "originator": "codex_exec",
			"cwd": "/work/shop", "source": source, "git": map[string]any{"branch": "alice-PR-12351", "commit_hash": "abc"}}}
	}
	for name, c := range map[string]struct {
		source any
		want   string
	}{
		"a string":            {"exec", "exec"},
		"a review subagent":   {map[string]any{"subagent": "review"}, CodexSourceSubagent},
		"another subagent":    {map[string]any{"subagent": map[string]any{"other": "guardian"}}, CodexSourceSubagent},
		"a spawned thread":    {map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": codexSID}}}, CodexSourceSubagent},
		"none":                {nil, ""},
		"an object of no use": {map[string]any{"elsewhere": true}, ""},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		writeLines(t, path, meta(c.source),
			map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "fix it"}})
		got, err := CodexSessionFacts(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Source != c.want || got.Originator != "codex_exec" || got.Cwd != "/work/shop" || !slices.Equal(got.Branches, []string{"alice-PR-12351"}) ||
			got.Entrypoint != "" {
			t.Errorf("%s: facts %+v, want source %q, originator codex_exec, its cwd and its branch", name, got, c.want)
		}
	}

	// A rollout of an older Codex has no session_meta: it says nothing.
	old := filepath.Join(dir, "old.jsonl")
	writeLines(t, old, map[string]any{"id": codexSID, "git": map[string]any{"branch": "alice-PR-12352"}},
		map[string]any{"type": "message", "role": "user"})
	if got, err := CodexSessionFacts(old); err != nil || got.Cwd != "" || got.Originator != "" || len(got.Branches) != 0 {
		t.Errorf("an older rollout: %+v, %v; want no facts", got, err)
	}
	if _, err := CodexSessionFacts(filepath.Join(dir, "gone.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing rollout = %v, want a not-exist error", err)
	}
}
