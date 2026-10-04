package reveal

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// spikePS is the real `ps -axo pid=,tty=,args=` shape recorded in docs/spikes.md
// (probe 8): the server has no tty and a full path, the TUI client is a bare
// `herdr` on ttys000, and unrelated processes mention herdr in their argv.
const spikePS = `
17058 ??       /opt/homebrew/bin/herdr server
17055 ttys000  herdr
17552 ??       /Users/bohdan/.local/share/mise/installs/node/24.21.0/bin/node /Users/bohdan/.config/herdr/plugins/github/hhdebb.herdr-radar-afabda667cbe/bin/agent-state.js --animate
47610 ??       /Users/bohdan/.config/herdr/plugins/github/herdr.collie-1edf0e1e987e/bin/collie _exec-bridge
98378 ttys012  ugrep -G --ignore-files --hidden -I -i herdr
`

func TestParseClientTtysSpikeFacts(t *testing.T) {
	bin := "/opt/homebrew/bin/herdr"
	if got := parseClientTtys(spikePS, bin, "default"); !reflect.DeepEqual(got, []string{"/dev/ttys000"}) {
		t.Fatalf("default session: got %v", got)
	}
	if got := parseClientTtys(spikePS, bin, "work"); len(got) != 0 {
		t.Fatalf("a bare herdr must not read as session work: got %v", got)
	}
	// The server (no tty, `server` subcommand) is never a client.
	for _, p := range parseHerdrProcesses(spikePS, bin) {
		if p.pid == "17058" {
			t.Fatalf("the tty-less server must be skipped: %+v", p)
		}
	}
}

func TestParseClientTtys(t *testing.T) {
	const processes = `
31029 ??       /opt/herdr server
30001 ttys001  herdr
30002 ttys002  herdr --session work
30003 ttys003  herdr session attach review
30004 ttys004  zsh
30005 ttys005  herdr --session default
`
	cases := []struct {
		session string
		want    []string
	}{
		{"default", []string{"/dev/ttys001", "/dev/ttys005"}},
		{"", []string{"/dev/ttys001", "/dev/ttys005"}},
		{"  default ", []string{"/dev/ttys001", "/dev/ttys005"}},
		{"work", []string{"/dev/ttys002"}},
		{"review", []string{"/dev/ttys003"}},
		{"nope", nil},
	}
	for _, c := range cases {
		got := parseClientTtys(processes, "/opt/herdr", c.session)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("session %q: got %v want %v", c.session, got, c.want)
		}
	}
}

// Regression ported from the extension: the remote bridge is a bare
// `herdr client` and a --remote attach drives another host's server.
func TestParseClientTtysRejectsRemote(t *testing.T) {
	const remote = `
48369 ttys041  herdr --remote clouddesk --session work
48678 ttys041  /opt/herdr client
`
	for _, s := range []string{"work", "default"} {
		if got := parseClientTtys(remote, "/opt/herdr", s); len(got) != 0 {
			t.Errorf("session %s: remote must not be a local client, got %v", s, got)
		}
	}
}

// Regression ported from the extension: a CLI call carries the same --session
// prefix as a client but is not one.
func TestParseClientTtysRejectsCLICalls(t *testing.T) {
	const calls = `
70001 ttys010  herdr --session work pane read w1:p1 --lines 200
70002 ttys011  herdr --session work api snapshot
70003 ttys012  herdr session list --json
70004 ttys013  herdr --session work agent prompt claude "go"
70005 ttys014  herdr status --json
70006 ttys015  herdr --machine box agent list
`
	if got := parseClientTtys(calls, "/opt/herdr", "work"); len(got) != 0 {
		t.Fatalf("CLI calls must not be clients: %v", got)
	}
	if got := parseClientTtys(calls, "/opt/herdr", "default"); len(got) != 0 {
		t.Fatalf("CLI calls must not be default clients: %v", got)
	}
}

func TestParseClientTtysOtherGlobalFlags(t *testing.T) {
	const clients = `
70010 ttys015  herdr --handoff --session work
70011 ttys016  herdr --session=work
70012 ttys017  herdr session attach work
`
	want := []string{"/dev/ttys015", "/dev/ttys016", "/dev/ttys017"}
	if got := parseClientTtys(clients, "/opt/herdr", "work"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseClientTtysBinaryPathWithSpace(t *testing.T) {
	const spaced = `60001 ttys007  /tmp/my tools/herdr --session work`
	got := parseClientTtys(spaced, "/tmp/my tools/herdr", "work")
	if !reflect.DeepEqual(got, []string{"/dev/ttys007"}) {
		t.Fatalf("got %v", got)
	}
}

func TestParseClientTtysFallbacks(t *testing.T) {
	// A differently located `herdr` is matched by basename; a pty already
	// carrying /dev/ is not double-prefixed; duplicate ttys collapse; quoted
	// whole-argv strings are unwrapped; other binaries are ignored.
	const out = `
1 ttys001  /usr/local/bin/herdr --session work
2 /dev/ttys002  herdr --session work
3 ttys001  herdr --session work
4 ttys003  "herdr --session work"
5 ttys004  herdrctl --session work
6 ttys005  /usr/bin/ssh herdr
garbage line
`
	got := parseClientTtys(out, "/opt/homebrew/bin/herdr", "work")
	want := []string{"/dev/ttys001", "/dev/ttys002", "/dev/ttys003"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestArgvSession(t *testing.T) {
	cases := []struct {
		argv string
		kind sessionKind
		name string
	}{
		{"", sessionBare, ""},
		{"--session work", sessionNamed, "work"},
		{"--session=work", sessionNamed, "work"},
		{"--session", sessionBare, ""},
		{"--handoff --session work", sessionNamed, "work"},
		{"--no-session", sessionBare, ""},
		{"session attach review", sessionNamed, "review"},
		{"session attach review extra", sessionOther, ""},
		{"session list", sessionOther, ""},
		{"server", sessionOther, ""},
		{"client", sessionOther, ""},
		{"--remote host --session work", sessionOther, ""},
		{"--remote=host", sessionOther, ""},
		{"--session work pane read x", sessionOther, ""},
		{"--default-config", sessionBare, ""},
		{"-V", sessionBare, ""},
		{"--remote-keybindings foo", sessionBare, ""},
	}
	for _, c := range cases {
		got := argvSession(c.argv)
		if got.kind != c.kind || got.name != c.name {
			t.Errorf("argvSession(%q) = %+v, want kind=%v name=%q", c.argv, got, c.kind, c.name)
		}
	}
}

func TestClientTtysUsesPsNotPgrep(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"/bin/ps"},
		Result: execx.Result{Stdout: []byte(spikePS)},
	}}}
	r := New(f, terminalCfg("iTerm2", "default"), "/opt/homebrew/bin/herdr")
	ttys, err := r.clientTtys(context.Background())
	if err != nil || !reflect.DeepEqual(ttys, []string{"/dev/ttys000"}) {
		t.Fatalf("got %v %v", ttys, err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want exactly one ps call, got %d", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Name != "/bin/ps" || !reflect.DeepEqual(c.Args, []string{"-axo", "pid=,tty=,args="}) {
		t.Fatalf("unexpected ps invocation: %s", c.String())
	}
	if c.Mutates {
		t.Fatal("ps is read-only")
	}
	if len(f.CallsWithPrefix("pgrep")) != 0 || len(f.CallsWithPrefix("/usr/bin/pgrep")) != 0 {
		t.Fatal("pgrep must not be used")
	}
}

func TestClientTtysReportsPsFailure(t *testing.T) {
	boom := errors.New("ps timed out")
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/ps"}, Err: boom}}}
	r := New(f, terminalCfg("iTerm2", "default"), "/opt/homebrew/bin/herdr")
	ttys, err := r.clientTtys(context.Background())
	if ttys != nil || !errors.Is(err, boom) {
		t.Fatalf("got %v %v", ttys, err)
	}
}

func TestClientTtysNoProcessesIsConfirmedEmpty(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/ps"}, Result: execx.Result{Stdout: []byte("1 ttys001 zsh\n")}}}}
	r := New(f, terminalCfg("iTerm2", "default"), "/opt/homebrew/bin/herdr")
	ttys, err := r.clientTtys(context.Background())
	if err != nil || len(ttys) != 0 {
		t.Fatalf("got %v %v", ttys, err)
	}
}
