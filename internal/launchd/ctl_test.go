package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	testLabel = "zhuravel.magnum"
	eioStderr = "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors."
)

func testPlist() []byte {
	return Plist(Options{Label: testLabel, ProgramArguments: []string{"/bin/true"}})
}

// argvs renders each recorded call as a single space-joined string.
func argvs(f *execx.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		out = append(out, strings.Join(append([]string{c.Name}, c.Args...), " "))
	}
	return out
}

// eioThenOK scripts bootstrap to fail with EIO n times and then succeed.
func eioThenOK(n int, calls *int) execx.Rule {
	return execx.Rule{
		Prefix: []string{"launchctl", "bootstrap"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			*calls++
			if *calls <= n {
				res := execx.Result{Code: 5, Stderr: []byte(eioStderr)}
				return res, &execx.ExitError{Cmd: c, Code: 5, Stderr: eioStderr}
			}
			return execx.Result{}, nil
		},
	}
}

// notFound is launchd's answer to `launchctl print` for an unknown service.
var notFound = execx.Result{Code: 113, Stderr: []byte(`Bad request.
Could not find service "zhuravel.magnum" in domain for user gui: 501`)}

// okRules scripts a clean install: the job is gone after bootout and the
// domain exists. extra rules come first and win.
func okRules(extra ...execx.Rule) []execx.Rule {
	rules := append([]execx.Rule{}, extra...)
	return append(rules,
		execx.Rule{Prefix: []string{"launchctl", "bootout"}},
		execx.Rule{Prefix: []string{"launchctl", "print", "gui/501"}, Result: execx.Result{Stdout: []byte("system domain listing")}},
		execx.Rule{Prefix: []string{"launchctl", "print"}, Result: notFound},
		execx.Rule{Prefix: []string{"launchctl", "enable"}},
		execx.Rule{Prefix: []string{"launchctl", "bootstrap"}},
	)
}

// fakeClock is install's injectable clock: sleeping advances it instantly and
// is recorded, so no test waits.
type fakeClock struct {
	t      time.Time
	slept  []time.Duration
	onWait func() // called on every sleep, if set
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) timing() timing {
	return timing{now: func() time.Time { return c.t }, sleep: c.sleep}
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	if c.onWait != nil {
		c.onWait()
	}
	return ctx.Err()
}

func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.slept {
		sum += d
	}
	return sum
}

// loadedFor scripts `launchctl print gui/501/<label>` to report the job as
// loaded for the first n calls and as unknown afterwards.
func loadedFor(n int, calls *int) execx.Rule {
	return execx.Rule{
		Prefix: []string{"launchctl", "print", "gui/501/" + testLabel},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			*calls++
			if *calls <= n {
				return execx.Result{Stdout: []byte(statusRunning)}, nil
			}
			return notFound, &execx.ExitError{Cmd: c, Code: notFound.Code, Stderr: string(notFound.Stderr)}
		},
	}
}

func TestInstallWritesFileAndRunsLaunchctlInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LaunchAgents", testLabel+".plist")
	f := &execx.Fake{Rules: okRules()}
	plist := testPlist()
	if err := Install(context.Background(), f, 501, path, plist); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(plist) {
		t.Fatalf("file content: %v %q", err, got)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", st.Mode().Perm())
	}
	want := []string{
		"launchctl bootout gui/501/zhuravel.magnum",
		"launchctl print gui/501/zhuravel.magnum", // not loaded: nothing to wait for
		"launchctl enable gui/501/zhuravel.magnum",
		"launchctl bootstrap gui/501 " + path,
	}
	if !reflect.DeepEqual(argvs(f), want) {
		t.Fatalf("calls = %q\nwant   %q", argvs(f), want)
	}
	for _, c := range f.Calls {
		if want := c.Args[0] != "print"; c.Mutates != want {
			t.Errorf("%s: Mutates = %v, want %v (print is read-only)", c.String(), c.Mutates, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestInstallFixesPermissionsOfExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{Rules: okRules()}
	if err := Install(context.Background(), f, 501, path, testPlist()); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", st.Mode().Perm())
	}
}

// The collie lesson: bootout returns before teardown finishes, so bootstrap
// can fail with EIO. Two EIO failures followed by success must succeed.
func TestInstallRetriesBootstrapOnEIO(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var n int
	f := &execx.Fake{Rules: okRules(eioThenOK(2, &n))}
	clk := newClock()
	if err := install(context.Background(), f, 501, path, testPlist(), clk.timing()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := len(f.CallsWithPrefix("launchctl", "bootstrap")); got != 3 {
		t.Fatalf("bootstrap attempts = %d, want 3", got)
	}
	if want := []time.Duration{time.Second, time.Second}; !reflect.DeepEqual(clk.slept, want) {
		t.Fatalf("sleeps = %v, want %v", clk.slept, want)
	}
	// bootout and enable run once, not per attempt.
	if len(f.CallsWithPrefix("launchctl", "bootout")) != 1 || len(f.CallsWithPrefix("launchctl", "enable")) != 1 {
		t.Fatalf("calls = %q", argvs(f))
	}
}

// The old job outlives bootout for a while: bootstrap waits for Status to say
// "not loaded" instead of burning its EIO retries on the teardown window.
func TestInstallWaitsForTheOldJobToUnload(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var prints int
	f := &execx.Fake{Rules: okRules(loadedFor(20, &prints))}
	clk := newClock()
	if err := install(context.Background(), f, 501, path, testPlist(), clk.timing()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if prints != 21 {
		t.Fatalf("print calls = %d, want 21 (20 loaded, 1 gone)", prints)
	}
	if want := 20 * bootoutPoll; clk.total() != want || len(clk.slept) != 20 {
		t.Fatalf("slept %v in %d sleeps, want %v in 20", clk.total(), len(clk.slept), want)
	}
	calls := argvs(f)
	lastPrint, bootstrap := -1, -1
	for i, c := range calls {
		switch {
		case strings.HasPrefix(c, "launchctl print "):
			lastPrint = i
		case strings.HasPrefix(c, "launchctl bootstrap "):
			bootstrap = i
		}
	}
	if bootstrap < 0 || bootstrap < lastPrint {
		t.Fatalf("bootstrap ran before the job unloaded: %q", calls)
	}
	if got := len(f.CallsWithPrefix("launchctl", "bootstrap")); got != 1 {
		t.Fatalf("bootstrap attempts = %d, want 1 (the wait made retries unnecessary)", got)
	}
}

// A job that never leaves is waited for 30 s (no longer), then bootstrap is
// still tried and the error says the old job was still loaded, not that the
// Mac has no console session.
func TestInstallWaitIsBoundedAndReportsTheStuckJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var prints, boots int
	f := &execx.Fake{Rules: okRules(
		loadedFor(1_000_000, &prints),
		execx.Rule{Prefix: []string{"launchctl", "bootstrap"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			boots++
			return execx.Result{Code: 5, Stderr: []byte(eioStderr)}, &execx.ExitError{Cmd: c, Code: 5, Stderr: eioStderr}
		}},
	)}
	clk := newClock()
	err := install(context.Background(), f, 501, path, testPlist(), clk.timing())
	if err == nil {
		t.Fatal("want error")
	}
	// 30 one-second sleeps for the wait, 2 for the EIO fallback.
	if clk.total() != bootoutWait+2*bootstrapDelay {
		t.Fatalf("slept %v, want %v", clk.total(), bootoutWait+2*bootstrapDelay)
	}
	if prints != 31 || boots != bootstrapAttempts {
		t.Fatalf("prints=%d bootstraps=%d, want 31 and %d", prints, boots, bootstrapAttempts)
	}
	if !strings.Contains(err.Error(), "still loaded") || strings.Contains(err.Error(), "console") {
		t.Fatalf("error should name the stuck job and not the console session: %v", err)
	}
	if len(f.CallsWithPrefix("launchctl", "print", "gui/501")) != 0 {
		t.Fatalf("domain probe is pointless when the job itself is loaded: %q", argvs(f))
	}
}

// A Status failure that is not "no such service" ends the wait immediately:
// the state is unknown, and bootstrap decides.
func TestInstallStopsWaitingWhenStatusFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	f := &execx.Fake{Rules: okRules(execx.Rule{
		Prefix: []string{"launchctl", "print", "gui/501/" + testLabel},
		Result: execx.Result{Code: 1, Stderr: []byte("Operation not permitted")},
	})}
	clk := newClock()
	if err := install(context.Background(), f, 501, path, testPlist(), clk.timing()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(clk.slept) != 0 || len(f.CallsWithPrefix("launchctl", "bootstrap")) != 1 {
		t.Fatalf("sleeps=%v calls=%q", clk.slept, argvs(f))
	}
}

func TestInstallStopsWhenContextCancelledDuringTheWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var prints int
	f := &execx.Fake{Rules: okRules(loadedFor(1_000_000, &prints))}
	ctx, cancel := context.WithCancel(context.Background())
	clk := newClock()
	clk.onWait = cancel
	err := install(ctx, f, 501, path, testPlist(), clk.timing())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.CallsWithPrefix("launchctl", "bootstrap")) != 0 {
		t.Fatalf("bootstrap ran after cancellation: %q", argvs(f))
	}
}

func TestInstallGivesUpAfterThreeEIOAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var n int
	// The gui domain is gone: `launchctl print gui/501` fails too.
	f := &execx.Fake{Rules: okRules(
		eioThenOK(100, &n),
		execx.Rule{Prefix: []string{"launchctl", "print", "gui/501"}, Result: execx.Result{Code: 113, Stderr: []byte("Could not find domain for user gui: 501")}},
	)}
	clk := newClock()
	err := install(context.Background(), f, 501, path, testPlist(), clk.timing())
	if err == nil {
		t.Fatal("want error")
	}
	if got := len(f.CallsWithPrefix("launchctl", "bootstrap")); got != 3 {
		t.Fatalf("bootstrap attempts = %d, want 3", got)
	}
	if len(clk.slept) != 2 {
		t.Fatalf("sleeps = %v, want 2 (none after the last attempt)", clk.slept)
	}
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("error should wrap the ExitError: %v", err)
	}
	// The domain probe failed, which is the evidence for the console hint.
	if !strings.Contains(err.Error(), "gui/501") || !strings.Contains(err.Error(), "console login session") {
		t.Fatalf("error lacks the gui-domain hint: %v", err)
	}
	// The plist stays on disk so a later login + `install` can pick it up.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("plist should remain written: %v", statErr)
	}
}

// With the domain alive, EIO has some other cause: report the real bootstrap
// error and do not send the user to the console.
func TestInstallDoesNotBlameTheConsoleWithoutEvidence(t *testing.T) {
	for name, probe := range map[string]execx.Rule{
		"domain exists":         {Prefix: []string{"launchctl", "print", "gui/501"}, Result: execx.Result{Stdout: []byte("ok")}},
		"probe cannot be run":   {Prefix: []string{"launchctl", "print", "gui/501"}, Err: errors.New("exec: launchctl: not found")},
		"probe times out (ctx)": {Prefix: []string{"launchctl", "print", "gui/501"}, Err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), testLabel+".plist")
			var n int
			f := &execx.Fake{Rules: okRules(eioThenOK(100, &n), probe)}
			err := install(context.Background(), f, 501, path, testPlist(), newClock().timing())
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), "console") || !strings.Contains(err.Error(), "Input/output error") {
				t.Fatalf("error = %v; want the real bootstrap error and no console hint", err)
			}
		})
	}
}

func TestInstallDoesNotRetryOtherBootstrapFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	f := &execx.Fake{Rules: okRules(execx.Rule{Prefix: []string{"launchctl", "bootstrap"}, Result: execx.Result{Code: 78, Stderr: []byte("Bootstrap failed: 78: Invalid property list")}})}
	clk := newClock()
	err := install(context.Background(), f, 501, path, testPlist(), clk.timing())
	if err == nil || !strings.Contains(err.Error(), "Invalid property list") {
		t.Fatalf("err = %v", err)
	}
	if got := len(f.CallsWithPrefix("launchctl", "bootstrap")); got != 1 || len(clk.slept) != 0 {
		t.Fatalf("attempts=%d sleeps=%v, want 1 and none", got, clk.slept)
	}
}

// bootout fails when the job is not loaded (first install); enable failures
// are best-effort too because bootstrap is the arbiter.
func TestInstallIgnoresBootoutAndEnableErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	f := &execx.Fake{Rules: okRules(
		execx.Rule{Prefix: []string{"launchctl", "bootout"}, Result: execx.Result{Code: 3, Stderr: []byte("Boot-out failed: 3: No such process")}},
		execx.Rule{Prefix: []string{"launchctl", "enable"}, Err: errors.New("boom")},
	)}
	if err := Install(context.Background(), f, 501, path, testPlist()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(f.CallsWithPrefix("launchctl", "bootstrap")) != 1 {
		t.Fatalf("calls = %q", argvs(f))
	}
}

func TestInstallStopsWhenContextCancelledDuringBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	var n int
	f := &execx.Fake{Rules: okRules(eioThenOK(100, &n))}
	ctx, cancel := context.WithCancel(context.Background())
	clk := newClock()
	clk.onWait = cancel
	err := install(ctx, f, 501, path, testPlist(), clk.timing())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := len(f.CallsWithPrefix("launchctl", "bootstrap")); got != 1 {
		t.Fatalf("bootstrap attempts = %d, want 1", got)
	}
}

func TestInstallRejectsPlistWithoutLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.plist")
	f := &execx.Fake{}
	for _, bad := range [][]byte{nil, []byte("not xml"), []byte("<plist><dict><key>RunAtLoad</key><true/></dict></plist>")} {
		if err := Install(context.Background(), f, 501, path, bad); err == nil {
			t.Fatalf("Install(%q) should fail", bad)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("no launchctl call expected, got %q", argvs(f))
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("no file should be written for an invalid plist")
	}
}

func TestLabelFromPlist(t *testing.T) {
	got, err := labelFromPlist(Plist(Options{Label: "a&b.c", ProgramArguments: []string{"/bin/true"}}))
	if err != nil || got != "a&b.c" {
		t.Fatalf("label = %q, %v", got, err)
	}
	// A "Label" string nested in EnvironmentVariables must not win.
	got, err = labelFromPlist(Plist(Options{Label: "real", ProgramArguments: []string{"x"}, Env: map[string]string{"Label": "fake"}}))
	if err != nil || got != "real" {
		t.Fatalf("label = %q, %v", got, err)
	}
}

func TestUninstallBootsOutAndRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	if err := os.WriteFile(path, testPlist(), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{Rules: okRules()}
	if err := Uninstall(context.Background(), f, 501, testLabel, path); err != nil {
		t.Fatal(err)
	}
	if want := []string{"launchctl bootout gui/501/zhuravel.magnum"}; !reflect.DeepEqual(argvs(f), want) {
		t.Fatalf("calls = %q", argvs(f))
	}
	if !f.Calls[0].Mutates {
		t.Error("bootout must be marked Mutates")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist should be removed: %v", err)
	}
}

// Uninstalling something that was never loaded (and has no file) is not an error.
func TestUninstallIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"launchctl", "bootout"}, Result: execx.Result{Code: 3, Stderr: []byte("Boot-out failed: 3: No such process")}},
		{Prefix: []string{"launchctl", "print"}, Result: execx.Result{Code: 113, Stderr: []byte(`Could not find service "zhuravel.magnum" in domain for user gui: 501`)}},
	}}
	if err := Uninstall(context.Background(), f, 501, testLabel, path); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

// If bootout fails and the job is still loaded, deleting the plist would leave
// an orphaned daemon behind; surface the error and keep the file.
func TestUninstallKeepsFileWhenJobStillLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), testLabel+".plist")
	if err := os.WriteFile(path, testPlist(), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"launchctl", "bootout"}, Result: execx.Result{Code: 1, Stderr: []byte("Boot-out failed: 1: Operation not permitted")}},
		{Prefix: []string{"launchctl", "print"}, Result: execx.Result{Stdout: []byte(statusRunning)}},
	}}
	err := Uninstall(context.Background(), f, 501, testLabel, path)
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("plist must stay: %v", statErr)
	}
}

// bootout fails and `print` fails for some reason other than "no such
// service": the daemon may still be running, so the error comes back and the
// plist stays.
func TestUninstallKeepsFileWhenStatusFails(t *testing.T) {
	for name, printRule := range map[string]execx.Rule{
		"print exits with a permission error": {Prefix: []string{"launchctl", "print"}, Result: execx.Result{Code: 1, Stderr: []byte("Operation not permitted")}},
		"print cannot run":                    {Prefix: []string{"launchctl", "print"}, Err: errors.New("exec: launchctl: not found")},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), testLabel+".plist")
			if err := os.WriteFile(path, testPlist(), 0o644); err != nil {
				t.Fatal(err)
			}
			f := &execx.Fake{Rules: []execx.Rule{
				{Prefix: []string{"launchctl", "bootout"}, Result: execx.Result{Code: 1, Stderr: []byte("Boot-out failed: 1: Operation not permitted")}},
				printRule,
			}}
			err := Uninstall(context.Background(), f, 501, testLabel, path)
			if err == nil || !strings.Contains(err.Error(), "launchctl bootout") || !strings.Contains(err.Error(), "state is unknown") {
				t.Fatalf("err = %v", err)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("plist must stay when the job's state is unknown: %v", statErr)
			}
		})
	}
}

// Bootout alone treats a bootout error as success only when print says 113.
func TestBootoutSwallowsErrorOnlyForANotLoadedJob(t *testing.T) {
	failing := execx.Rule{Prefix: []string{"launchctl", "bootout"}, Result: execx.Result{Code: 3, Stderr: []byte("Boot-out failed: 3: No such process")}}
	f := &execx.Fake{Rules: []execx.Rule{failing, {Prefix: []string{"launchctl", "print"}, Result: notFound}}}
	if err := Bootout(context.Background(), f, 501, testLabel); err != nil {
		t.Fatalf("not loaded: %v", err)
	}
	f = &execx.Fake{Rules: []execx.Rule{failing, {Prefix: []string{"launchctl", "print"}, Result: execx.Result{Code: 1, Stderr: []byte("denied")}}}}
	if err := Bootout(context.Background(), f, 501, testLabel); err == nil {
		t.Fatal("a print failure must not make a failed bootout look successful")
	}
}

func TestKickstart(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"launchctl", "kickstart"}}}}
	if err := Kickstart(context.Background(), f, 501, testLabel); err != nil {
		t.Fatal(err)
	}
	if want := []string{"launchctl kickstart -k gui/501/zhuravel.magnum"}; !reflect.DeepEqual(argvs(f), want) {
		t.Fatalf("calls = %q", argvs(f))
	}
	if !f.Calls[0].Mutates {
		t.Error("kickstart must be marked Mutates")
	}

	f = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"launchctl", "kickstart"}, Result: execx.Result{Code: 113, Stderr: []byte("Could not find service")}}}}
	err := Kickstart(context.Background(), f, 501, testLabel)
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want wrapped ExitError", err)
	}
}
