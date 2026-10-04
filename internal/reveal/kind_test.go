package reveal

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectKind(t *testing.T) {
	cases := map[string]Kind{
		"iTerm2":                  KindITerm,
		"iTerm":                   KindITerm,
		"iTerm.app":               KindITerm,
		"com.googlecode.iterm2":   KindITerm,
		"Terminal":                KindTerminal,
		"Terminal.app":            KindTerminal,
		"com.apple.Terminal":      KindTerminal,
		"":                        KindTerminal,
		"Ghostty":                 KindGhostty,
		"com.mitchellh.ghostty":   KindGhostty,
		"WezTerm":                 KindWezTerm,
		"com.github.wez.wezterm":  KindWezTerm,
		"custom":                  KindCustom,
		"Custom":                  KindCustom,
		"generic":                 KindGeneric,
		"Muxy":                    KindGeneric,
		"Muxy Beta":               KindGeneric,
		"Alacritty":               KindGeneric,
		"Some Unknown Terminal 9": KindGeneric,
	}
	for app, want := range cases {
		if got := DetectKind(app); got != want {
			t.Errorf("DetectKind(%q) = %q, want %q", app, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                  "''",
		"herdr":             "herdr",
		"/opt/homebrew/bin": "/opt/homebrew/bin",
		"--session":         "--session",
		"a b":               "'a b'",
		"it's":              `'it'\''s'`,
		"$HOME":             "'$HOME'",
		"x;y":               "'x;y'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := shellJoin("/path with space/herdr", "agent", "attach", "reviewer"); got != `'/path with space/herdr' agent attach reviewer` {
		t.Errorf("shellJoin: %s", got)
	}
}

func TestParseShellWords(t *testing.T) {
	got, err := parseShellWords(`"/Applications/My Term.app/launcher" -e {herdr} 'a b' c\ d`)
	want := []string{"/Applications/My Term.app/launcher", "-e", "{herdr}", "a b", "c d"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := parseShellWords(`  `); err != nil || len(got) != 0 {
		t.Fatalf("blank: %q %v", got, err)
	}
	if got, err := parseShellWords(`a "" b`); err != nil || !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Fatalf("empty quoted word: %q %v", got, err)
	}
	if _, err := parseShellWords(`a "b`); err == nil {
		t.Fatal("unterminated quote must fail")
	}
	if _, err := parseShellWords(`a\`); err == nil {
		t.Fatal("trailing backslash must fail")
	}
}

func TestExpandCustomLauncher(t *testing.T) {
	t.Run("expands binary and args without a shell", func(t *testing.T) {
		exe, args, err := expandCustomLauncher(`"/Applications/My Term.app/launcher" -e {herdr} {args}`, "/opt/herdr", []string{"--session", "work"})
		if err != nil || exe != "/Applications/My Term.app/launcher" ||
			!reflect.DeepEqual(args, []string{"-e", "/opt/herdr", "--session", "work"}) {
			t.Fatalf("got %q %q %v", exe, args, err)
		}
	})
	t.Run("one command string", func(t *testing.T) {
		exe, args, err := expandCustomLauncher("launcher --command {command}", "/path with space/herdr", []string{"agent", "attach", "reviewer"})
		if err != nil || exe != "launcher" ||
			!reflect.DeepEqual(args, []string{"--command", `'/path with space/herdr' agent attach reviewer`}) {
			t.Fatalf("got %q %q %v", exe, args, err)
		}
	})
	t.Run("command embedded in a larger word", func(t *testing.T) {
		exe, args, err := expandCustomLauncher(`sh -c "cd /work && {command}"`, "/opt/herdr", []string{"attach"})
		if err != nil || exe != "sh" || !reflect.DeepEqual(args, []string{"-c", "cd /work && /opt/herdr attach"}) {
			t.Fatalf("got %q %q %v", exe, args, err)
		}
	})
	t.Run("args embedded in a larger word is rejected", func(t *testing.T) {
		_, _, err := expandCustomLauncher(`sh -c "{herdr} {args}"`, "/opt/herdr", []string{"attach"})
		if err == nil || !strings.Contains(err.Error(), "standalone word") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("requires a command placeholder", func(t *testing.T) {
		_, _, err := expandCustomLauncher("terminal -e", "/herdr", nil)
		if err == nil || !strings.Contains(err.Error(), "must contain") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("empty template", func(t *testing.T) {
		if _, _, err := expandCustomLauncher("   ", "/herdr", nil); err == nil {
			t.Fatal("empty template must fail")
		}
	})
	t.Run("only args placeholder is not enough", func(t *testing.T) {
		if _, _, err := expandCustomLauncher("terminal {args}", "/herdr", []string{"x"}); err == nil {
			t.Fatal("must contain {herdr} or {command}")
		}
	})
}
