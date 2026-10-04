package reveal

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

func TestAccessors(t *testing.T) {
	r := New(&execx.Fake{}, config.Terminal{App: "Ghostty"}, "")
	if r.Kind() != KindGhostty || r.Session() != "default" || r.herdrBin != "herdr" {
		t.Fatalf("got kind=%s session=%s bin=%s", r.Kind(), r.Session(), r.herdrBin)
	}
}

func TestRandomMarkerIsUniqueAndPrefixed(t *testing.T) {
	a, b := randomMarker(), randomMarker()
	if a == b || !strings.HasPrefix(a, "herdr-magnum-") || len(a) < len("herdr-magnum-")+8 {
		t.Fatalf("markers %q %q", a, b)
	}
}

func TestResolveWezTermBinFallsBackToPath(t *testing.T) {
	if got := resolveWezTermBin(); got == "" {
		t.Fatal("must never be empty")
	}
}

func TestGenericWithoutAppNameCannotActivate(t *testing.T) {
	for _, app := range []string{"generic", "Generic"} {
		r, _ := newTest(terminalCfg(app, "work"))
		if _, err := r.Reveal(ctx(), Options{}); err == nil || !strings.Contains(err.Error(), "does not name an application") {
			t.Errorf("%s: got %v", app, err)
		}
	}
}

func TestRevealEmptyLauncherCustomIsAnError(t *testing.T) {
	r, _ := newTest(config.Terminal{App: "custom", Session: "work"})
	if _, err := r.Reveal(ctx(), Options{}); err == nil {
		t.Fatal("want a configuration error")
	}
}

// Under DryRun the read-only ps still runs, the UI-touching commands are only
// recorded, and the result must not claim the client was focused.
func TestRevealUnderDryRun(t *testing.T) {
	t.Run("iterm", testRevealUnderDryRunITerm)
	t.Run("wezterm", testRevealUnderDryRunWezTerm)
}

func testRevealUnderDryRunITerm(t *testing.T) {
	inner := &execx.Fake{Rules: []execx.Rule{psRule(psWithWorkClient)}}
	dry := &execx.DryRun{Inner: inner}
	out, err := Reveal(ctx(), dry, terminalCfg("iTerm2", "work"), testHerdr, Options{})
	if err != nil || out.Action != ActionActivated {
		t.Fatalf("got %+v %v", out, err)
	}
	if len(inner.Calls) != 1 || inner.Calls[0].Name != psBin {
		t.Fatalf("only ps may really run: %v", names(inner))
	}
	var planned []string
	for _, c := range dry.Planned {
		planned = append(planned, c.Name)
	}
	if !reflect.DeepEqual(planned, []string{osascript, openBin}) {
		t.Fatalf("planned %v", planned)
	}
}

// WezTerm's activate-pane is skipped under DryRun like every UI command, so
// the focus must not be reported as done: Reveal falls back to activating the
// app (also only planned) with the reason in Detail.
func testRevealUnderDryRunWezTerm(t *testing.T) {
	listing := `[{"window_id":5,"pane_id":2,"tty_name":"/dev/ttys050"}]`
	inner := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{testWezTerm, "cli", "list", "--format", "json"}, Result: stdout(listing)},
		psRule(psWithWorkClient),
	}}
	dry := &execx.DryRun{Inner: inner}
	r := New(dry, terminalCfg("WezTerm", "work"), testHerdr)
	r.wezTermBin = testWezTerm

	out, err := r.Reveal(ctx(), Options{})
	if err != nil || out.Action != ActionActivated {
		t.Fatalf("got %+v %v; a dry run must not claim the pane was focused", out, err)
	}
	if !strings.Contains(out.Detail, "dry-run") {
		t.Errorf("detail %q should say why nothing was focused", out.Detail)
	}
	if got := names(inner); !reflect.DeepEqual(got, []string{
		testWezTerm + " cli list --format json", psBin + " -axo pid=,tty=,args=",
	}) {
		t.Fatalf("only the read-only lookups may really run: %v", got)
	}
	var planned []string
	for _, c := range dry.Planned {
		planned = append(planned, c.Name+" "+strings.Join(c.Args, " "))
	}
	if want := []string{testWezTerm + " cli activate-pane --pane-id 2", openBin + " -a WezTerm"}; !reflect.DeepEqual(planned, want) {
		t.Fatalf("planned %v, want %v", planned, want)
	}
	// FocusExisting itself reports Unavailable, with the reason.
	if res, err := r.FocusExisting(ctx()); res != Unavailable || err == nil {
		t.Fatalf("FocusExisting = %v, %v", res, err)
	}
}

func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"short unchanged", "héllo", 10, "héllo"},
		{"exact length unchanged", "héllo", 6, "héllo"},
		{"ascii cut", "abcdef", 3, "abc..."},
		{"cut falls inside a two-byte rune", strings.Repeat("a", 199) + "é" + "tail", 200, strings.Repeat("a", 199) + "..."},
		{"cut after a whole rune", strings.Repeat("a", 198) + "é" + "tail", 200, strings.Repeat("a", 198) + "é..."},
		{"cut falls inside a four-byte rune", "ab😀cd", 4, "ab..."},
		{"zero width", "héllo", 0, "..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result %q is not valid UTF-8", got)
			}
		})
	}
	// Invalid UTF-8 stays something we can cut without looping or panicking.
	if got := truncate("\x80\x80\x80\x80\x80", 3); got != "..." && got != "\x80\x80\x80..." {
		t.Errorf("invalid input: %q", got)
	}
}

// The custom launcher must return immediately even when the launcher process
// stays alive (a terminal that blocks until closed), and must not be killed
// with the runner's process group once Run returns.
//
// The launched command waits for a gate file that only this test creates, after
// Launch has returned, so "Launch did not wait for it" is checked without a
// wall-clock threshold: a Launch that waited would run into the runner's
// timeout (and fail) instead of racing a stopwatch on a loaded machine.
func TestCustomLauncherDetachesWithRealRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real background process")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	gate := filepath.Join(dir, "go")
	// Open the gate on the way out too, so a failed test leaves no waiting process.
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	launcher := `/bin/sh -c "while [ ! -e ` + gate + ` ]; do sleep 0.05; done; echo \"$0\" > ` + marker + `" {herdr}`
	r := New(&execx.Real{}, config.Terminal{App: "custom", Session: "work", Launcher: launcher}, "/opt/herdr-test")

	if err := r.Launch(ctx(), Options{}); err != nil {
		t.Fatalf("Launch must return while the launcher is still running: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the launcher finished before the gate opened")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && strings.HasSuffix(string(b), "\n") {
			if strings.TrimSpace(string(b)) != "/opt/herdr-test" {
				t.Fatalf("launcher got %q", b)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the detached launcher never ran")
}

func TestCustomLauncherMissingExecutableFailsWithRealRunner(t *testing.T) {
	r := New(&execx.Real{}, config.Terminal{App: "custom", Launcher: "/nonexistent/launcher {herdr}"}, "herdr")
	err := r.Launch(ctx(), Options{})
	var ee *execx.ExitError
	if !errors.As(err, &ee) || ee.Code != 127 || !strings.Contains(err.Error(), "launcher not found") {
		t.Fatalf("got %v", err)
	}
}

// Read-only check against the real process table: the ps invocation and parser
// work on this machine's actual output (skipped when ps is unavailable).
func TestClientTtysAgainstRealPs(t *testing.T) {
	if _, err := os.Stat(psBin); err != nil {
		t.Skip("no /bin/ps")
	}
	r := New(&execx.Real{}, config.Terminal{App: "iTerm2"}, "herdr")
	ttys, err := r.clientTtys(ctx())
	if err != nil {
		t.Fatal(err)
	}
	for _, tty := range ttys {
		if !strings.HasPrefix(tty, "/dev/") {
			t.Errorf("unexpected tty %q", tty)
		}
	}
}
