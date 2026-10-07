package reveal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

const wantCommand = testHerdr + " --session work"

func TestLaunchITerm(t *testing.T) {
	t.Run("opens a tab in the current window by default", func(t *testing.T) {
		r, f := newTest(terminalCfg("iTerm2", "work"), osaRule(func(string) (string, error) { return "", nil }))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		s := scriptOf(t, f, 0)
		mustContain(t, s,
			`tell application "iTerm"`, "activate",
			"if (count windows) > 0 then", "create tab with default profile", "create window with default profile",
			`tell targetSession to write text "`+wantCommand+`"`)
		if !f.Calls[0].Mutates {
			t.Error("launching must be marked Mutates")
		}
	})
	t.Run("opens a new window when asked", func(t *testing.T) {
		r, f := newTest(terminalCfg("iTerm2", "work"), osaRule(func(string) (string, error) { return "", nil }))
		if err := r.launch(ctx(), Options{NewWindow: true}); err != nil {
			t.Fatal(err)
		}
		s := scriptOf(t, f, 0)
		mustContain(t, s, "create window with default profile", `write text "`+wantCommand+`"`)
		if strings.Contains(s, "create tab") {
			t.Errorf("new window must not create a tab:\n%s", s)
		}
	})
	t.Run("a failing osascript is an error", func(t *testing.T) {
		boom := errors.New("not authorized")
		r, _ := newTest(terminalCfg("iTerm2", "work"), errRule(boom, osascript))
		if err := r.launch(ctx(), Options{}); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("escapes the command for AppleScript", func(t *testing.T) {
		f := &execx.Fake{Rules: []execx.Rule{osaRule(func(string) (string, error) { return "", nil })}}
		r := newRevealer(f, terminalCfg("iTerm2", `we"ird`), `/opt/my "tools"/herdr`)
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		s := scriptOf(t, f, 0)
		// The shell quotes the spaces, AppleScript then escapes the double quotes.
		mustContain(t, s, `write text "'/opt/my \"tools\"/herdr' --session 'we\"ird'"`)
	})
}

func TestLaunchTerminalApp(t *testing.T) {
	r, f := newTest(terminalCfg("Terminal", "work"), osaRule(func(string) (string, error) { return "", nil }))
	if err := r.launch(ctx(), Options{}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, scriptOf(t, f, 0), `tell application "Terminal"`, "activate", `do script "`+wantCommand+`"`)
}

func TestLaunchGhostty(t *testing.T) {
	t.Run("tab in the front window by default", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), osaRule(func(string) (string, error) { return "opened", nil }))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		mustContain(t, scriptOf(t, f, 0),
			`tell application "Ghostty"`, "set cfg to new surface configuration",
			`set command of cfg to "`+wantCommand+`"`,
			"new tab in front window with configuration cfg")
		if len(f.Calls) != 1 {
			t.Fatalf("no fallback expected: %v", names(f))
		}
	})
	t.Run("new window when asked", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), osaRule(func(string) (string, error) { return "opened", nil }))
		if err := r.launch(ctx(), Options{NewWindow: true}); err != nil {
			t.Fatal(err)
		}
		s := scriptOf(t, f, 0)
		mustContain(t, s, "new window with configuration cfg")
		if strings.Contains(s, "new tab") {
			t.Errorf("no tab expected:\n%s", s)
		}
	})
	t.Run("falls back to open -na when scripting does not answer opened", func(t *testing.T) {
		for name, osa := range map[string]execx.Rule{
			"error":  errRule(errors.New("denied"), osascript),
			"output": osaRule(func(string) (string, error) { return "weird", nil }),
		} {
			r, f := newTest(terminalCfg("Ghostty", "work"), osa, okRule(openBin))
			if err := r.launch(ctx(), Options{}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			c := f.CallsWithPrefix(openBin)
			if len(c) != 1 || !reflect.DeepEqual(c[0].Args, []string{"-na", "Ghostty", "--args", "-e", testHerdr, "--session", "work"}) {
				t.Fatalf("%s: %v", name, names(f))
			}
		}
	})
}

// A timed-out AppleScript may already have opened the tab: only start a second
// Ghostty when no herdr client for the session showed up.
func TestLaunchGhosttyTimeout(t *testing.T) {
	timedOut := errRule(fmt.Errorf("osascript: %w", context.DeadlineExceeded), osascript)
	openedFallback := func(f *execx.Fake) bool { return len(f.CallsWithPrefix(openBin)) > 0 }

	t.Run("no second instance when the session now has a client", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), timedOut, psRule(psWithWorkClient), okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if openedFallback(f) {
			t.Fatalf("started a second Ghostty although the tab opened: %v", names(f))
		}
	})
	t.Run("falls back when no client appeared", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), timedOut, psRule("101 ttys051  herdr --session other\n"), okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if !openedFallback(f) {
			t.Fatalf("want the open -na fallback: %v", names(f))
		}
	})
	t.Run("falls back when the process list fails", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), timedOut, errRule(errors.New("ps broke"), psBin), okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if !openedFallback(f) {
			t.Fatalf("want the open -na fallback: %v", names(f))
		}
	})
	t.Run("an ordinary scripting error never consults ps", func(t *testing.T) {
		r, f := newTest(terminalCfg("Ghostty", "work"), errRule(errors.New("denied"), osascript), okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if len(f.CallsWithPrefix(psBin)) != 0 || !openedFallback(f) {
			t.Fatalf("calls: %v", names(f))
		}
	})
}

func TestLaunchWezTerm(t *testing.T) {
	listPrefix := []string{testWezTerm, "cli", "list", "--format", "json"}
	listing := `[{"window_id":4,"pane_id":1,"tty_name":"/dev/ttys001"}]`

	t.Run("spawns into the first listed window and raises the app", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(listing)},
			execx.Rule{Prefix: []string{testWezTerm, "cli", "spawn"}, Result: stdout("9\n")},
			okRule(openBin, "-a", "WezTerm"))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		want := []string{
			testWezTerm + " cli list --format json",
			testWezTerm + " cli spawn --window-id 4 -- " + testHerdr + " --session work",
			openBin + " -a WezTerm",
		}
		if got := names(f); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("new window skips the listing", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: []string{testWezTerm, "cli", "spawn"}, Result: stdout("9")},
			okRule(openBin, "-a", "WezTerm"))
		if err := r.launch(ctx(), Options{NewWindow: true}); err != nil {
			t.Fatal(err)
		}
		if got := f.CallsWithPrefix(testWezTerm, "cli", "spawn"); len(got) != 1 ||
			!reflect.DeepEqual(got[0].Args, []string{"cli", "spawn", "--new-window", "--", testHerdr, "--session", "work"}) {
			t.Fatalf("got %v", names(f))
		}
		if len(f.CallsWithPrefix(testWezTerm, "cli", "list")) != 0 {
			t.Fatal("must not list panes for --new-window")
		}
	})
	t.Run("no listed window means a new window", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout("[]")},
			execx.Rule{Prefix: []string{testWezTerm, "cli", "spawn"}, Result: stdout("9")},
			okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if got := f.CallsWithPrefix(testWezTerm, "cli", "spawn"); len(got) != 1 || got[0].Args[2] != "--new-window" {
			t.Fatalf("got %v", names(f))
		}
	})
	t.Run("falls back to open -na when spawn does not print a pane id", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			execx.Rule{Prefix: listPrefix, Result: stdout(listing)},
			execx.Rule{Prefix: []string{testWezTerm, "cli", "spawn"}, Result: stdout("oops")},
			okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		c := f.CallsWithPrefix(openBin)
		if len(c) != 1 || !reflect.DeepEqual(c[0].Args, []string{"-na", "WezTerm", "--args", "start", "--", testHerdr, "--session", "work"}) {
			t.Fatalf("got %v", names(f))
		}
	})
	t.Run("falls back when the CLI is missing", func(t *testing.T) {
		r, f := newTest(terminalCfg("WezTerm", "work"),
			errRule(errors.New("not found"), testWezTerm), okRule(openBin))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if len(f.CallsWithPrefix(openBin)) != 1 {
			t.Fatalf("got %v", names(f))
		}
	})
}

func TestLaunchCustom(t *testing.T) {
	cfg := config.Terminal{App: "custom", Session: "work", Launcher: `"/Applications/My Term.app/launcher" -e {herdr} {args}`}

	t.Run("runs the expanded template detached through sh", func(t *testing.T) {
		r, f := newTest(cfg, okRule("/bin/sh"))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if len(f.Calls) != 1 {
			t.Fatalf("got %v", names(f))
		}
		c := f.Calls[0]
		if c.Name != "/bin/sh" || len(c.Args) < 4 || c.Args[0] != "-c" || !c.Mutates {
			t.Fatalf("unexpected: %s mutates=%v", c.String(), c.Mutates)
		}
		mustContain(t, c.Args[1], "nohup", `"$@"`, "&")
		want := []string{"/Applications/My Term.app/launcher", "-e", testHerdr, "--session", "work"}
		if !reflect.DeepEqual(c.Args[3:], want) {
			t.Fatalf("launcher argv: got %q want %q", c.Args[3:], want)
		}
	})
	t.Run("the launcher wins over a scriptable app when set", func(t *testing.T) {
		r, f := newTest(config.Terminal{App: "iTerm2", Session: "work", Launcher: "term -e {herdr} {args}"}, okRule("/bin/sh"))
		if err := r.launch(ctx(), Options{}); err != nil {
			t.Fatal(err)
		}
		if len(f.CallsWithPrefix(osascript)) != 0 || len(f.CallsWithPrefix("/bin/sh")) != 1 {
			t.Fatalf("got %v", names(f))
		}
	})
	t.Run("custom without a launcher is a configuration error", func(t *testing.T) {
		r, f := newTest(config.Terminal{App: "custom", Session: "work"})
		err := r.launch(ctx(), Options{})
		if err == nil || !strings.Contains(err.Error(), "launcher") {
			t.Fatalf("got %v", err)
		}
		if len(f.Calls) != 0 {
			t.Fatal("nothing must run")
		}
	})
	t.Run("an invalid template runs nothing", func(t *testing.T) {
		for _, tpl := range []string{"term -e", `sh -c "{herdr} {args}"`, `a "b`} {
			r, f := newTest(config.Terminal{App: "custom", Session: "work", Launcher: tpl})
			if err := r.launch(ctx(), Options{}); err == nil {
				t.Errorf("%q: want error", tpl)
			}
			if len(f.Calls) != 0 {
				t.Errorf("%q: ran %v", tpl, names(f))
			}
		}
	})
	t.Run("a launcher failure surfaces", func(t *testing.T) {
		boom := errors.New("launcher not found: term")
		r, _ := newTest(config.Terminal{App: "custom", Session: "work", Launcher: "term -e {herdr}"}, errRule(boom, "/bin/sh"))
		if err := r.launch(ctx(), Options{}); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLaunchGeneric(t *testing.T) {
	r, f := newTest(terminalCfg("Muxy Beta", "work"), okRule(openBin))
	if err := r.launch(ctx(), Options{}); err != nil {
		t.Fatal(err)
	}
	c := f.CallsWithPrefix(openBin)
	if len(c) != 1 || !reflect.DeepEqual(c[0].Args, []string{"-a", "Muxy Beta"}) || !c[0].Mutates {
		t.Fatalf("got %v", names(f))
	}
}

func TestLaunchEmptySessionNamesDefault(t *testing.T) {
	r, f := newTest(terminalCfg("Terminal", ""), osaRule(func(string) (string, error) { return "", nil }))
	if err := r.launch(ctx(), Options{}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, scriptOf(t, f, 0), `do script "`+testHerdr+` --session default"`)
}

func TestLaunchEmptyHerdrBinFallsBackToName(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{osaRule(func(string) (string, error) { return "", nil })}}
	r := newRevealer(f, terminalCfg("Terminal", "work"), "")
	if err := r.launch(ctx(), Options{}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, scriptOf(t, f, 0), `do script "herdr --session work"`)
}
