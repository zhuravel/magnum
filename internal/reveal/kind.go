package reveal

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Kind is the family of terminal application magnum reveals herdr in.
type Kind string

// The supported terminal kinds. Anything DetectKind does not recognise
// (including Muxy) is KindGeneric: the app can be activated but not scripted.
const (
	KindTerminal Kind = "terminal" // macOS Terminal.app
	KindITerm    Kind = "iterm2"
	KindGhostty  Kind = "ghostty"
	KindWezTerm  Kind = "wezterm"
	KindCustom   Kind = "custom"  // user-supplied launcher template
	KindGeneric  Kind = "generic" // `open -a <App>` only
)

// DetectKind maps config.Terminal.App to a Kind. Matching is case-insensitive
// and tolerant of a ".app" suffix or a bundle id. An empty name means the
// built-in Terminal.app, as in the Raycast extension.
func DetectKind(app string) Kind {
	id := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(app)), ".app")
	switch {
	case id == "":
		return KindTerminal
	case id == "custom":
		return KindCustom
	case id == "generic", strings.Contains(id, "muxy"):
		return KindGeneric
	case id == "terminal", strings.Contains(id, "com.apple.terminal"):
		return KindTerminal
	case strings.Contains(id, "iterm"):
		return KindITerm
	case strings.Contains(id, "ghostty"):
		return KindGhostty
	case strings.Contains(id, "wezterm"):
		return KindWezTerm
	default:
		return KindGeneric
	}
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes one word for a POSIX shell, leaving plain words readable
// (the command is typed into a visible terminal tab).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin renders argv as one shell command line.
func shellJoin(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// parseShellWords splits a launcher template into words, honouring single and
// double quotes and backslash escapes, without involving a shell.
func parseShellWords(input string) ([]string, error) {
	var (
		words   []string
		current strings.Builder
		quote   rune // 0, '\'' or '"'
		escaped bool
		started bool
	)
	for _, ch := range strings.TrimSpace(input) {
		switch {
		case escaped:
			current.WriteRune(ch)
			escaped, started = false, true
		case ch == '\\' && quote != '\'':
			escaped, started = true, true
		case ch == '\'' && quote != '"':
			if quote == '\'' {
				quote = 0
			} else {
				quote = '\''
			}
			started = true
		case ch == '"' && quote != '\'':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			started = true
		case unicode.IsSpace(ch) && quote == 0:
			if started {
				words = append(words, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(ch)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("launcher cannot end with an unescaped backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("launcher has an unterminated %c quote", quote)
	}
	if started {
		words = append(words, current.String())
	}
	return words, nil
}

// expandCustomLauncher turns a launcher template into an executable and its
// argv. {args} must be a standalone word and expands to the herdr arguments;
// {herdr} is the binary and {command} the shell-quoted binary plus arguments
// (for launchers that take one command string). Nothing goes through a shell.
func expandCustomLauncher(template, binary string, args []string) (string, []string, error) {
	words, err := parseShellWords(template)
	if err != nil {
		return "", nil, err
	}
	if len(words) == 0 {
		return "", nil, errors.New("launcher template is empty")
	}
	command := shellJoin(append([]string{binary}, args...)...)
	var expanded []string
	for _, w := range words {
		switch {
		case w == "{args}":
			expanded = append(expanded, args...)
		case strings.Contains(w, "{args}"):
			return "", nil, errors.New("{args} must be a standalone word in the launcher")
		default:
			w = strings.ReplaceAll(w, "{herdr}", binary)
			w = strings.ReplaceAll(w, "{command}", command)
			expanded = append(expanded, w)
		}
	}
	if !strings.Contains(template, "{herdr}") && !strings.Contains(template, "{command}") {
		return "", nil, errors.New("launcher must contain {herdr} or {command}")
	}
	if len(expanded) == 0 {
		return "", nil, errors.New("launcher expands to nothing")
	}
	return expanded[0], expanded[1:], nil
}
