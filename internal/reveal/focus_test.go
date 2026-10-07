package reveal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

const psWithWorkClient = "100 ttys050  herdr --session work\n101 ttys051  herdr --session other\n"

func TestFocusITerm(t *testing.T) {
	t.Run("focused when a client tty is an iTerm session", func(t *testing.T) {
		r, f := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
			osaRule(func(string) (string, error) { return "/dev/ttys050", nil }))
		res, err := r.focusExisting(ctx())
		if res != Focused || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
		mustContain(t, scriptOf(t, f, 0), `tell application "iTerm"`, `whose tty is "/dev/ttys050"`)
		if c := f.CallsWithPrefix(osascript)[0]; !c.Mutates {
			t.Error("focusing a window acts on the UI and must be marked Mutates for dry-run")
		}
	})
	t.Run("missing when the tty is in another terminal", func(t *testing.T) {
		r, _ := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
			osaRule(func(string) (string, error) { return "miss", nil }))
		if res, err := r.focusExisting(ctx()); res != Missing || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("missing without running osascript when there is no client", func(t *testing.T) {
		r, f := newTest(terminalCfg("iTerm2", "work"), psRule("100 ttys050  herdr --session other\n"))
		if res, err := r.focusExisting(ctx()); res != Missing || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
		if len(f.CallsWithPrefix(osascript)) != 0 {
			t.Fatal("osascript must not run with no client tty")
		}
	})
	t.Run("unavailable when ps fails", func(t *testing.T) {
		boom := errors.New("ps failed")
		r, _ := newTest(terminalCfg("iTerm2", "work"), errRule(boom, psBin))
		if res, err := r.focusExisting(ctx()); res != Unavailable || !errors.Is(err, boom) {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("unavailable when osascript fails", func(t *testing.T) {
		boom := errors.New("not authorized")
		r, _ := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient), errRule(boom, osascript))
		if res, err := r.focusExisting(ctx()); res != Unavailable || !errors.Is(err, boom) {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("unavailable when osascript prints nothing", func(t *testing.T) {
		// Under DryRun a Mutates command returns an empty Result; that must not
		// read as "focused".
		r, _ := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient),
			osaRule(func(string) (string, error) { return "", nil }))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("got %v", res)
		}
	})
	t.Run("the spike fact: a Muxy client on ttys000 is not an iTerm tab", func(t *testing.T) {
		r, _ := newTest(terminalCfg("iTerm2", "default"), psRule(spikePS),
			osaRule(func(s string) (string, error) {
				mustContain(t, s, `whose tty is "/dev/ttys000"`)
				return "miss", nil
			}))
		if res, err := r.focusExisting(ctx()); res != Missing || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
	})
}

func TestFocusTerminalApp(t *testing.T) {
	r, f := newTest(terminalCfg("Terminal", "work"), psRule(psWithWorkClient),
		osaRule(func(string) (string, error) { return "/dev/ttys050", nil }))
	if res, err := r.focusExisting(ctx()); res != Focused || err != nil {
		t.Fatalf("got %v %v", res, err)
	}
	mustContain(t, scriptOf(t, f, 0), `tell application "Terminal"`, `set targetTtys to {"/dev/ttys050"}`)

	r, _ = newTest(terminalCfg("Terminal", "work"), psRule(psWithWorkClient),
		osaRule(func(string) (string, error) { return "miss", nil }))
	if res, _ := r.focusExisting(ctx()); res != Missing {
		t.Fatalf("got %v", res)
	}
}

func titleJSON(changed bool, reason string) string {
	if changed {
		return `{"result":{"changed":true,"reason":"` + reason + `"}}`
	}
	return `{"result":{"changed":false,"reason":"` + reason + `"}}`
}

func TestFocusGhostty(t *testing.T) {
	setPrefix := []string{testHerdr, "--session", "work", "terminal", "title", "set", testMarker}
	clearPrefix := []string{testHerdr, "--session", "work", "terminal", "title", "clear"}

	t.Run("marker flow focuses then clears", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(titleJSON(true, "set"))},
			osaRule(func(s string) (string, error) {
				mustContain(t, s, `is "`+testMarker+`"`)
				return "focused", nil
			}),
			okRule(clearPrefix...))
		res, err := r.focusExisting(ctx())
		if res != Focused || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
		want := []string{
			testHerdr + " --session work terminal title set " + testMarker,
			osascript + " -e tell application \"Ghostty\"...",
			testHerdr + " --session work terminal title clear",
		}
		if got := names(f); !reflect.DeepEqual(got, want) {
			t.Fatalf("call order:\n got %q\nwant %q", got, want)
		}
		if len(f.CallsWithPrefix("/bin/ps")) != 0 {
			t.Fatal("ghostty flow does not use ps")
		}
	})
	t.Run("accepts a bare (non-envelope) JSON reply", func(t *testing.T) {
		r, _ := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(`{"changed":true,"reason":"set"}`)},
			osaRule(func(string) (string, error) { return "focused", nil }),
			okRule(clearPrefix...))
		if res, err := r.focusExisting(ctx()); res != Focused || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("no foreground client is missing and nothing is cleared", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(titleJSON(false, "no_foreground_client"))})
		if res, err := r.focusExisting(ctx()); res != Missing || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
		if len(f.Calls) != 1 {
			t.Fatalf("only the set call expected: %v", names(f))
		}
	})
	t.Run("other unchanged reason is unavailable", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(titleJSON(false, "unsupported_terminal"))})
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("got %v", res)
		}
		if len(f.Calls) != 1 {
			t.Fatalf("no clear after an unchanged title: %v", names(f))
		}
	})
	t.Run("a failed set still clears, since a timeout may have changed the title", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"),
			errRule(errors.New("timeout"), setPrefix...),
			okRule(clearPrefix...))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("got %v", res)
		}
		if len(f.CallsWithPrefix(clearPrefix...)) != 1 {
			t.Fatalf("clear expected: %v", names(f))
		}
	})
	t.Run("invalid JSON is unavailable and clears", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout("not json")},
			okRule(clearPrefix...))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("got %v", res)
		}
		if len(f.CallsWithPrefix(clearPrefix...)) != 1 {
			t.Fatalf("clear expected: %v", names(f))
		}
	})
	t.Run("error envelope is unavailable", func(t *testing.T) {
		r, _ := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(`{"error":{"code":"server_not_running","message":"nope"}}`)},
			okRule(clearPrefix...))
		if res, err := r.focusExisting(ctx()); res != Unavailable || err == nil {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("osascript miss or failure is unavailable and clears", func(t *testing.T) {
		for name, osa := range map[string]execx.Rule{
			"miss":  osaRule(func(string) (string, error) { return "miss", nil }),
			"error": errRule(errors.New("denied"), osascript),
		} {
			r, f := newTest(terminalCfg("Ghostty", "work"),
				execx.Rule{Prefix: setPrefix, Result: stdout(titleJSON(true, "set"))}, osa, okRule(clearPrefix...))
			if res, _ := r.focusExisting(ctx()); res != Unavailable {
				t.Fatalf("%s: got %v", name, res)
			}
			if len(f.CallsWithPrefix(clearPrefix...)) != 1 {
				t.Fatalf("%s: clear expected: %v", name, names(f))
			}
		}
	})
	t.Run("clear is retried once", func(t *testing.T) {
		n := 0
		r, f := newTest(terminalCfg("Ghostty", "work"),
			execx.Rule{Prefix: setPrefix, Result: stdout(titleJSON(true, "set"))},
			osaRule(func(string) (string, error) { return "focused", nil }),
			execx.Rule{Prefix: clearPrefix, Fn: func(c execx.Cmd) (execx.Result, error) {
				n++
				if n == 1 {
					return execx.Result{}, errors.New("timeout")
				}
				return execx.Result{}, nil
			}})
		if res, _ := r.focusExisting(ctx()); res != Focused {
			t.Fatalf("got %v", res)
		}
		if len(f.CallsWithPrefix(clearPrefix...)) != 2 {
			t.Fatalf("clear should be retried: %v", names(f))
		}
	})
	t.Run("clear still runs with a live context when the caller's was cancelled mid-flow", func(t *testing.T) {
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		fake := &execx.Fake{Rules: []execx.Rule{
			{Prefix: setPrefix, Result: stdout(titleJSON(true, "set"))},
			osaRule(func(string) (string, error) { return "focused", nil }),
			okRule(clearPrefix...),
		}}
		probe := &probeRunner{inner: fake, cancelAfterSet: cancel}
		r := newRevealer(probe, terminalCfg("Ghostty", "work"), testHerdr)
		r.newMarker = func() string { return testMarker }
		_, _ = r.focusExisting(c)
		if !probe.cleared {
			t.Fatalf("clear must still run: %v", names(fake))
		}
		if probe.clearCtxErr != nil {
			t.Fatalf("clear must not inherit the cancelled context: %v", probe.clearCtxErr)
		}
	})
}

// probeRunner cancels the caller's context right after the title set and
// records the context state the clear call observed.
type probeRunner struct {
	inner          execx.Runner
	cancelAfterSet context.CancelFunc
	cleared        bool
	clearCtxErr    error
}

func (p *probeRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	isArg := func(s string) bool {
		for _, a := range c.Args {
			if a == s {
				return true
			}
		}
		return false
	}
	if isArg("clear") {
		p.cleared = true
		p.clearCtxErr = ctx.Err()
	}
	res, err := p.inner.Run(ctx, c)
	if isArg("set") {
		p.cancelAfterSet()
	}
	return res, err
}

func TestFocusWezTerm(t *testing.T) {
	listing := `[{"window_id":4,"pane_id":1,"tty_name":"/dev/ttys001"},{"window_id":5,"pane_id":2,"tty_name":"/dev/ttys050"}]`
	listPrefix := []string{testWezTerm, "cli", "list", "--format", "json"}

	t.Run("activates the pane then raises the app", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(listing)},
			psRule(psWithWorkClient),
			okRule(testWezTerm, "cli", "activate-pane", "--pane-id", "2"),
			okRule(openBin, "-a", "WezTerm"))
		if res, err := r.focusExisting(ctx()); res != Focused || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
		want := []string{
			testWezTerm + " cli list --format json",
			psBin + " -axo pid=,tty=,args=",
			testWezTerm + " cli activate-pane --pane-id 2",
			openBin + " -a WezTerm",
		}
		if got := names(f); !reflect.DeepEqual(got, want) {
			t.Fatalf("call order:\n got %q\nwant %q", got, want)
		}
	})
	t.Run("missing when no listed pane has a client tty", func(t *testing.T) {
		r, _ := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(`[{"window_id":4,"pane_id":1,"tty_name":"/dev/ttys001"}]`)},
			psRule(psWithWorkClient))
		if res, err := r.focusExisting(ctx()); res != Missing || err != nil {
			t.Fatalf("got %v %v", res, err)
		}
	})
	t.Run("unavailable when the listing is empty or the CLI fails", func(t *testing.T) {
		r, _ := newTest(terminalCfg("WezTerm", "work"), execx.Rule{Prefix: listPrefix, Result: stdout("")}, psRule(psWithWorkClient))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("empty listing: got %v", res)
		}
		r, _ = newTest(terminalCfg("WezTerm", "work"), errRule(errors.New("no such file"), testWezTerm), psRule(psWithWorkClient))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("cli failure: got %v", res)
		}
	})
	t.Run("unavailable when ps or activate-pane fails", func(t *testing.T) {
		r, _ := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(listing)}, errRule(errors.New("ps"), psBin))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("ps failure: got %v", res)
		}
		r, _ = newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(listing)}, psRule(psWithWorkClient),
			errRule(errors.New("boom"), testWezTerm, "cli", "activate-pane"))
		if res, _ := r.focusExisting(ctx()); res != Unavailable {
			t.Fatalf("activate failure: got %v", res)
		}
	})
}

func TestFocusUnscriptableTerminals(t *testing.T) {
	for _, app := range []string{"custom", "generic", "Muxy Beta", "Alacritty"} {
		r, f := newTest(terminalCfg(app, "work"))
		res, err := r.focusExisting(ctx())
		if res != Unavailable || err == nil {
			t.Errorf("%s: got %v %v", app, res, err)
		}
		if len(f.Calls) != 0 {
			t.Errorf("%s: must not run anything: %v", app, names(f))
		}
	}
}

func TestFocusCancelledContext(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	r, f := newTest(terminalCfg("iTerm2", "work"), psRule(psWithWorkClient))
	res, err := r.focusExisting(c)
	if res != Unavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v %v", res, err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("no calls expected: %v", names(f))
	}
}

func TestEmptySessionMeansDefault(t *testing.T) {
	r, _ := newTest(terminalCfg("iTerm2", ""), psRule("1 ttys001  herdr\n"),
		osaRule(func(s string) (string, error) {
			if !strings.Contains(s, `"/dev/ttys001"`) {
				t.Errorf("bare herdr should match the default session: %s", s)
			}
			return "/dev/ttys001", nil
		}))
	if res, _ := r.focusExisting(ctx()); res != Focused {
		t.Fatalf("got %v", res)
	}
}
