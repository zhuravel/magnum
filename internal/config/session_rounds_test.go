package config

import (
	"strings"
	"testing"
)

// The Codex judge of a PR was refused by Codex's cybersecurity classifier in
// round 15 of a session resumed across all fifteen rounds (29 million tokens
// of history), and such a refusal flags the PR for good. A role's
// max_session_rounds is how many review rounds one session of it may serve
// before the next round starts it fresh: 0 (no limit) or more, for session
// roles only. The built-in judge serves 6; the other built-in roles have no
// limit.
func TestMaxSessionRoundsIsValidated(t *testing.T) {
	def := Defaults()
	for _, r := range def.RolesFor(nil) {
		want := 0
		if r.Judge {
			want = DefaultJudgeSessionRounds
		}
		if r.MaxSessionRounds != want {
			t.Errorf("%s max_session_rounds = %d, want %d", r.Name, r.MaxSessionRounds, want)
		}
	}
	if DefaultJudgeSessionRounds != 6 {
		t.Errorf("DefaultJudgeSessionRounds = %d, want 6", DefaultJudgeSessionRounds)
	}

	for name, tc := range map[string]struct {
		role string
		want int
	}{
		"no limit":       {"name = \"codex-judge\"\nmax_session_rounds = 0\n", 0},
		"a user's limit": {"name = \"codex-judge\"\nmax_session_rounds = 3\n", 3},
		"inherited":      {"name = \"codex-judge\"\neffort = \"high\"\n", DefaultJudgeSessionRounds},
	} {
		cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "\n[[role]]\n" + tc.role})
		if j := cfg.JudgeFor(nil); j.MaxSessionRounds != tc.want {
			t.Errorf("%s: judge max_session_rounds = %d, want %d", name, j.MaxSessionRounds, tc.want)
		}
	}
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "\n[[role]]\nname = \"codex-judge\"\n\n[[role]]\nname = \"claude-review\"\nmax_session_rounds = 4\n"})
	if r, ok := cfg.RoleByNameOrAlias(nil, "claude-review"); !ok || r.MaxSessionRounds != 4 {
		t.Errorf("claude-review max_session_rounds = %d (%v), want 4", r.MaxSessionRounds, ok)
	}

	for name, body := range map[string]string{
		"negative":     "\n[[role]]\nname = \"codex-judge\"\nmax_session_rounds = -1\n",
		"a shell role": "\n[[role]]\nname = \"codex-judge\"\n\n[[role]]\nname = \"codex-review\"\nmax_session_rounds = 6\n",
	} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + body})
		if err == nil || !strings.Contains(err.Error(), "max_session_rounds") {
			t.Errorf("%s: err = %v, want one naming max_session_rounds", name, err)
		}
	}
}
