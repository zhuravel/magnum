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

func TestProbe(t *testing.T) {
	t.Run("iTerm", func(t *testing.T) {
		r, f := newTest(terminalCfg("iTerm2", "work"),
			osaRule(func(string) (string, error) { return "/dev/ttys050, /dev/ttys051", nil }))
		ttys, err := r.Probe(ctx())
		if err != nil || !reflect.DeepEqual(ttys, []string{"/dev/ttys050", "/dev/ttys051"}) {
			t.Fatalf("got %v %v", ttys, err)
		}
		mustContain(t, scriptOf(t, f, 0), "tty of every session of every tab of every window")
		if f.Calls[0].Mutates {
			t.Error("listing ttys is read-only")
		}
	})
	t.Run("Terminal", func(t *testing.T) {
		r, f := newTest(terminalCfg("Terminal", "work"),
			osaRule(func(string) (string, error) { return "/dev/ttys001", nil }))
		if ttys, err := r.Probe(ctx()); err != nil || len(ttys) != 1 {
			t.Fatalf("got %v %v", ttys, err)
		}
		mustContain(t, scriptOf(t, f, 0), "tty of every tab of every window")
	})
	t.Run("WezTerm", func(t *testing.T) {
		r, _ := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: []string{testWezTerm, "cli", "list"}, Result: stdout(`[{"window_id":1,"pane_id":3,"tty_name":"/dev/ttys009"}]`)})
		if ttys, err := r.Probe(ctx()); err != nil || !reflect.DeepEqual(ttys, []string{"/dev/ttys009"}) {
			t.Fatalf("got %v %v", ttys, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		boom := errors.New("denied")
		r, _ := newTest(terminalCfg("iTerm2", "work"), errRule(boom, osascript))
		if _, err := r.Probe(ctx()); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
		for _, app := range []string{"Ghostty", "custom", "Muxy"} {
			r, _ := newTest(terminalCfg(app, "work"))
			if _, err := r.Probe(ctx()); err == nil {
				t.Errorf("%s: want unsupported error", app)
			}
		}
	})
}
