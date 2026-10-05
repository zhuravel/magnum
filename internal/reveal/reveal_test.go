package reveal

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

func TestRevealFocusesExistingClient(t *testing.T) {
	r, f := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
		osaRule(func(string) (string, error) { return "/dev/ttys050", nil }))
	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionFocused || out.Kind != KindITerm || out.Session != "work" {
		t.Fatalf("got %+v %v", out, err)
	}
	if len(f.CallsWithPrefix(osascript)) != 1 {
		t.Fatalf("only the focus script should run: %v", names(f))
	}
}

// Spike probe 9: the herdr client lives in Muxy, so iTerm2 reports a miss and
// magnum opens a NEW client tab in iTerm2.
func TestRevealLaunchesWhenClientIsInAnotherTerminal(t *testing.T) {
	r, f := newTest(terminalCfg("iTerm2", "default"), psRule(spikePS),
		osaRule(func(s string) (string, error) {
			if strings.Contains(s, "create tab") {
				return "", nil
			}
			return "miss", nil
		}))
	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionLaunched || out.Kind != KindITerm {
		t.Fatalf("got %+v %v", out, err)
	}
	if n := len(f.CallsWithPrefix(osascript)); n != 2 {
		t.Fatalf("focus + launch expected: %v", names(f))
	}
	mustContain(t, scriptOf(t, f, 1), "create tab with default profile", `write text "`+testHerdr+` --session default"`)
}

func TestRevealNewWindowOption(t *testing.T) {
	r, f := newTest(terminalCfg("iTerm2", "default"), psRule(spikePS),
		osaRule(func(s string) (string, error) {
			if strings.Contains(s, "create window") {
				return "", nil
			}
			return "miss", nil
		}))
	out, err := r.Reveal(ctx(), Options{NewWindow: true})
	if err != nil || out.Action != ActionLaunched {
		t.Fatalf("got %+v %v", out, err)
	}
	if s := scriptOf(t, f, 1); strings.Contains(s, "create tab") {
		t.Fatalf("NewWindow must not open a tab:\n%s", s)
	}
}

func TestRevealActivatesWhenFocusIsUnavailable(t *testing.T) {
	r, f := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
		errRule(errors.New("not authorized"), osascript), okRule(openBin))
	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionActivated || out.Detail == "" {
		t.Fatalf("got %+v %v", out, err)
	}
	c := f.CallsWithPrefix(openBin)
	if len(c) != 1 || !reflect.DeepEqual(c[0].Args, []string{"-a", "iTerm"}) {
		t.Fatalf("want open -a iTerm: %v", names(f))
	}
	if len(f.CallsWithPrefix(osascript)) != 1 {
		t.Fatalf("must not launch a second client when focus is merely unavailable: %v", names(f))
	}
}

func TestRevealGenericActivatesTheApp(t *testing.T) {
	r, f := newTest(terminalCfg("Muxy Beta", "default"), okRule(openBin))
	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionActivated || out.Kind != KindGeneric {
		t.Fatalf("got %+v %v", out, err)
	}
	if got := names(f); !reflect.DeepEqual(got, []string{openBin + " -a Muxy Beta"}) {
		t.Fatalf("got %q", got)
	}
}

func TestRevealCustomLaunchesEachTime(t *testing.T) {
	r, f := newTest(config.Terminal{App: "custom", Session: "work", Launcher: "term -e {herdr} {args}"}, okRule("/bin/sh"))
	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionLaunched || out.Kind != KindCustom {
		t.Fatalf("got %+v %v", out, err)
	}
	if len(f.CallsWithPrefix("/bin/sh")) != 1 {
		t.Fatalf("got %v", names(f))
	}
}

func TestRevealPropagatesLaunchErrors(t *testing.T) {
	boom := errors.New("osascript blew up")
	r, _ := newTest(terminalCfg("iTerm2", "default"), psRule(spikePS),
		execx.Rule{Prefix: []string{osascript}, Fn: func(c execx.Cmd) (execx.Result, error) {
			if strings.Contains(c.Args[1], "create tab") {
				return execx.Result{}, boom
			}
			return stdout("miss"), nil
		}})
	_, err := r.Reveal(ctx(), Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestRevealPackageFunction(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{okRule(openBin)}}
	out, err := Reveal(ctx(), f, terminalCfg("Muxy", ""), testHerdr, Options{})
	if err != nil || out.Action != ActionActivated || out.Session != "default" {
		t.Fatalf("got %+v %v", out, err)
	}
}

func TestOutcomeString(t *testing.T) {
	cases := []struct {
		o    Outcome
		want string
	}{
		{Outcome{Action: ActionFocused, Kind: KindITerm, Session: "default"}, "focused existing herdr client (iterm2, session default)"},
		{Outcome{Action: ActionLaunched, Kind: KindTerminal, Session: "work"}, "launched herdr client (terminal, session work)"},
		{Outcome{Action: ActionActivated, Kind: KindGeneric, Session: "default", Detail: "cannot focus"}, "activated terminal app (generic, session default): cannot focus"},
	}
	for _, c := range cases {
		if got := c.o.String(); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

// TestRevealNamesTheAutomationPermissionWhenMacOSRefuses: osascript's error
// -1743 ("Not authorized to send Apple events") means the app that runs magnum
// may not script the terminal. Reveal says so, with the fix, instead of
// bringing the terminal forward and calling that a success.
func TestRevealNamesTheAutomationPermissionWhenMacOSRefuses(t *testing.T) {
	denied := func(app string) execx.Rule {
		return execx.Rule{Prefix: []string{osascript}, Result: execx.Result{Code: 1,
			Stderr: []byte("execution error: Not authorized to send Apple events to " + app + ". (-1743)\n")}}
	}
	setPrefix := []string{testHerdr, "--session", "work", "terminal", "title", "set", testMarker}
	for _, tc := range []struct {
		app, name string
		rules     []execx.Rule
	}{
		{"iTerm2", "iTerm", []execx.Rule{psRule(psWithWorkClient), denied("iTerm")}},
		{"Terminal", "Terminal", []execx.Rule{psRule(psWithWorkClient), denied("Terminal")}},
		{"Ghostty", "Ghostty", []execx.Rule{{Prefix: setPrefix, Result: stdout(titleJSON(true, "set"))}, denied("Ghostty"),
			okRule(testHerdr, "--session", "work", "terminal", "title", "clear")}},
	} {
		t.Run(tc.app, func(t *testing.T) {
			r, f := newTest(terminalCfg(tc.app, "work"), append(tc.rules, okRule(openBin))...)
			out, err := r.Reveal(ctx(), Options{})
			var ae *AutomationError
			if !errors.As(err, &ae) || ae.App != tc.name {
				t.Fatalf("got %+v %v, want an AutomationError for %s", out, err, tc.name)
			}
			mustContain(t, err.Error(), "Automation", "-1743", "System Settings → Privacy & Security → Automation → allow "+tc.name)
			if out.Action == ActionActivated || len(f.CallsWithPrefix(openBin)) != 0 {
				t.Fatalf("must not bring %s forward and call it revealed: %+v %v", tc.app, out, names(f))
			}
		})
	}
	// Any other osascript failure still falls back to bringing the app forward.
	r, _ := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
		execx.Rule{Prefix: []string{osascript}, Result: execx.Result{Code: 1, Stderr: []byte("execution error: iTerm got an error (-1728)\n")}},
		okRule(openBin))
	if out, err := r.Reveal(ctx(), Options{}); err != nil || out.Action != ActionActivated {
		t.Fatalf("another error: %+v %v", out, err)
	}
}
