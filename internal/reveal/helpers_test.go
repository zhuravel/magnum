package reveal

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

const (
	testHerdr   = "/opt/homebrew/bin/herdr"
	testWezTerm = "/Applications/WezTerm.app/Contents/MacOS/wezterm"
	testMarker  = "herdr-magnum-test"
	osascript   = "/usr/bin/osascript"
	openBin     = "/usr/bin/open"
	psBin       = "/bin/ps"
)

func terminalCfg(app, session string) config.Terminal {
	return config.Terminal{App: app, Session: session}
}

// newTest builds a revealer over a scripted Fake with a fixed Ghostty marker
// and WezTerm binary so tests never touch the filesystem or randomness.
func newTest(cfg config.Terminal, rules ...execx.Rule) (*revealer, *execx.Fake) {
	f := &execx.Fake{Rules: rules}
	r := newRevealer(f, cfg, testHerdr)
	r.newMarker = func() string { return testMarker }
	r.wezTermBin = testWezTerm
	return r, f
}

func stdout(s string) execx.Result { return execx.Result{Stdout: []byte(s)} }

func psRule(out string) execx.Rule {
	return execx.Rule{Prefix: []string{psBin}, Result: stdout(out)}
}

// osaRule answers every osascript call; fn gets the script text.
func osaRule(fn func(script string) (string, error)) execx.Rule {
	return execx.Rule{Prefix: []string{osascript}, Fn: func(c execx.Cmd) (execx.Result, error) {
		out, err := fn(c.Args[len(c.Args)-1])
		return stdout(out), err
	}}
}

func okRule(prefix ...string) execx.Rule {
	return execx.Rule{Prefix: prefix, Result: stdout("")}
}

func errRule(err error, prefix ...string) execx.Rule {
	return execx.Rule{Prefix: prefix, Err: err}
}

// names renders the recorded calls as "name arg0 arg1 ..." for order assertions
// (osascript scripts are shortened to their first line).
func names(f *execx.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		line := c.Name
		for _, a := range c.Args {
			if i := strings.IndexByte(a, '\n'); i >= 0 {
				a = a[:i] + "..."
			}
			line += " " + a
		}
		out = append(out, line)
	}
	return out
}

func ctx() context.Context { return context.Background() }

func mustContain(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("missing %q in:\n%s", n, haystack)
		}
	}
}

// scriptOf returns the AppleScript of the i-th osascript call.
func scriptOf(t *testing.T, f *execx.Fake, i int) string {
	t.Helper()
	calls := f.CallsWithPrefix(osascript)
	if i >= len(calls) {
		t.Fatalf("want osascript call #%d, have %d (%v)", i, len(calls), names(f))
	}
	args := calls[i].Args
	if len(args) != 2 || args[0] != "-e" {
		t.Fatalf("osascript args should be -e <script>: %q", args)
	}
	return args[1]
}
