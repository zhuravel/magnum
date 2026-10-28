package launchd

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// Fixtures follow `launchctl print gui/<uid>/<label>` on macOS (trimmed; the
// nested blocks matter because they contain deeper "pid"/"state" lines).
const statusRunning = `gui/501/zhuravel.magnum = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/zhuravel.magnum.plist
	type = LaunchAgent
	state = running

	program = /opt/homebrew/bin/mise
	arguments = {
		/opt/homebrew/bin/mise
		-C
		/Users/me/Projects/magnum
		exec
		--
		/Users/me/Projects/magnum/bin/magnum
		daemon
	}

	stdout path = /Users/me/Projects/magnum/state/logs/launchd.log
	environment = {
		HOME => /Users/me
		pid = 1
		state = bogus
	}

	LWCR = {
		"reqs" => {
			"cdhash" => {
				"$in" => [
					0 = 				]
			}
		}
		"vers" => 1
	}

	domain = gui/501 [100002]
	minimum runtime = 5
	exit timeout = 5
	runs = 3
	pid = 6289
	immediate reason = speculative
	forks = 0
	last exit code = (never exited)
}
`

const statusNotRunning = `gui/501/zhuravel.magnum = {
	active count = 0
	path = /Users/me/Library/LaunchAgents/zhuravel.magnum.plist
	type = LaunchAgent
	state = not running

	program = /opt/homebrew/bin/mise
	domain = gui/501 [100002]
	runs = 750
	last exit code = 78
}
`

const statusJetsam = `gui/501/zhuravel.magnum = {
	active count = 0
	state = not running
	domain = gui/501 [100002]
	runs = 3
	last exit reason = JETSAM_REASON_MEMORY_IDLE_EXIT
}
`

const statusSpawnScheduled = `gui/501/zhuravel.magnum = {
	state = spawn scheduled
	runs = 12
	last exit code = 1
}
`

// statusSpawnFailed is the job on 2026-10-28 after a rebuild: launchd failed
// every spawn of the new binary with EX_CONFIG and tried again each throttle.
const statusSpawnFailed = `gui/501/zhuravel.magnum = {
	active count = 0
	path = /Users/me/Library/LaunchAgents/zhuravel.magnum.plist
	type = LaunchAgent
	state = spawn scheduled

	domain = gui/501 [100002]
	runs = 9
	job state = spawn failed
	properties = needs LWCR update | keepalive | runatload
	last exit code = 78: EX_CONFIG
}
`

func printFake(res execx.Result) *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"launchctl", "print"}, Result: res}}}
}

func TestStatusParsing(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Info
	}{
		{"running", statusRunning, Info{State: Running, PID: 6289, Runs: 3}},
		{"not running with exit code", statusNotRunning, Info{State: "not running", Runs: 750, LastExitCode: 78, Exited: true}},
		{"exit reason instead of code", statusJetsam, Info{State: "not running", Runs: 3}},
		{"spawn scheduled", statusSpawnScheduled, Info{State: "spawn scheduled", Runs: 12, LastExitCode: 1, Exited: true}},
		{"spawn failed", statusSpawnFailed, Info{State: "spawn scheduled", Runs: 9, LastExitCode: 78, Exited: true, SpawnFailed: true}},
		{"job state of a live job", "gui/501/x = {\n\tstate = running\n\tpid = 7\n\tjob state = running\n}\n", Info{State: Running, PID: 7}},
		{"exit code zero", "gui/501/x = {\n\tstate = not running\n\truns = 1\n\tlast exit code = 0\n}\n", Info{State: "not running", Runs: 1, Exited: true}},
		{"crlf line endings", "gui/501/x = {\r\n\tstate = running\r\n\tpid = 7\r\n}\r\n", Info{State: Running, PID: 7}},
		{"no state line", "gui/501/x = {\n\tpath = /p\n}\n", Info{State: "unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := printFake(execx.Result{Stdout: []byte(tc.out)})
			got, err := Status(context.Background(), f, 501, testLabel)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if !got.Loaded() {
				t.Fatal("Loaded() = false")
			}
		})
	}
}

// launchd cannot start the job when it reports no process and either its
// own spawn failure (job state) or EX_CONFIG (78), the exit code it gives a
// job whose program it could not run. A program that ran and failed, a clean
// exit and a running job are not that.
func TestCannotSpawnIsLaunchdsOwnFailure(t *testing.T) {
	cases := []struct {
		name string
		st   Info
		want bool
	}{
		{"job state spawn failed", parsePrint(statusSpawnFailed), true},
		{"spawn scheduled after EX_CONFIG", Info{State: "spawn scheduled", Runs: 3, LastExitCode: 78, Exited: true}, true},
		{"job state alone", Info{State: "spawn scheduled", SpawnFailed: true}, true},
		{"not running after EX_CONFIG", parsePrint(statusNotRunning), true},
		{"the program exited 1", parsePrint(statusSpawnScheduled), false},
		{"a clean exit", Info{State: "spawn scheduled", Runs: 2, Exited: true}, false},
		{"never exited", Info{State: "spawn scheduled", Runs: 1}, false},
		{"running", parsePrint(statusRunning), false},
		{"running after an old EX_CONFIG", Info{State: Running, PID: 7, LastExitCode: 78, Exited: true}, false},
		{"not loaded", Info{State: NotLoaded}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.st.CannotSpawn(); got != tc.want {
				t.Fatalf("CannotSpawn(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}

func TestStatusInvokesPrintReadOnly(t *testing.T) {
	f := printFake(execx.Result{Stdout: []byte(statusRunning)})
	if _, err := Status(context.Background(), f, 501, testLabel); err != nil {
		t.Fatal(err)
	}
	if want := []string{"launchctl print gui/501/zhuravel.magnum"}; !reflect.DeepEqual(argvs(f), want) {
		t.Fatalf("calls = %q", argvs(f))
	}
	if f.Calls[0].Mutates {
		t.Error("print is read-only and must not be marked Mutates (dry-run must still run it)")
	}
}

func TestStatusNotLoaded(t *testing.T) {
	f := printFake(execx.Result{Code: 113, Stderr: []byte("Bad request.\nCould not find service \"zhuravel.magnum\" in domain for user gui: 501")})
	got, err := Status(context.Background(), f, 501, testLabel)
	if err != nil {
		t.Fatalf("a non-zero launchctl exit means not loaded, not an error: %v", err)
	}
	if got.State != NotLoaded || got.Loaded() || got.PID != 0 {
		t.Fatalf("got %+v", got)
	}
}

// Only launchd's "no such service" answer means not loaded: exit 113, or the
// message on stderr whatever the code.
func TestStatusNotLoadedRecognizesNoSuchService(t *testing.T) {
	for name, res := range map[string]execx.Result{
		"exit 113, no message":    {Code: 113},
		"message with other code": {Code: 1, Stderr: []byte(`Could not find service "zhuravel.magnum" in domain for user gui: 501`)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Status(context.Background(), printFake(res), 501, testLabel)
			if err != nil || got.State != NotLoaded {
				t.Fatalf("got %+v, %v; want NotLoaded, nil", got, err)
			}
		})
	}
}

// Any other launchctl failure says nothing about the job: it may be loaded.
// Reporting it as not loaded would let Bootout swallow its own failure and
// Uninstall delete the plist of a running daemon.
func TestStatusReportsOtherLaunchctlFailures(t *testing.T) {
	for name, res := range map[string]execx.Result{
		"permission":            {Code: 1, Stderr: []byte("Operation not permitted")},
		"domain lookup":         {Code: 5, Stderr: []byte("Could not find domain for user gui: 501")},
		"unexpected exit 112":   {Code: 112},
		"killed by launchctl":   {Code: 137, Stderr: []byte("Killed: 9")},
		"bad request, no match": {Code: 64, Stderr: []byte("Bad request.")},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Status(context.Background(), printFake(res), 501, testLabel)
			var ee *execx.ExitError
			if !errors.As(err, &ee) || ee.Code != res.Code {
				t.Fatalf("err = %v, want the wrapped ExitError %d", err, res.Code)
			}
			if got.State != NotLoaded || got.PID != 0 {
				t.Fatalf("got %+v; the zero Info is returned next to the error", got)
			}
		})
	}
}

// Failing to run launchctl at all (missing binary, timeout) is not "not loaded".
func TestStatusReportsRunnerFailures(t *testing.T) {
	boom := errors.New("exec: launchctl: not found")
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"launchctl", "print"}, Err: boom}}}
	got, err := Status(context.Background(), f, 501, testLabel)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	if got.State != NotLoaded {
		t.Fatalf("got %+v", got)
	}
}

// Status must never expose the raw `print` output: it echoes the job's full
// argument list and environment, which can carry secrets.
func TestStatusDoesNotLeakRawOutput(t *testing.T) {
	typ := reflect.TypeFor[Info]()
	for field := range typ.Fields() {
		if field.Type.Kind() == reflect.String && field.Name != "State" {
			t.Fatalf("unexpected string field %s on Info", field.Name)
		}
	}
}
