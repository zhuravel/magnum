package reveal

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	psColumns = "pid=,tty=,args="
	// psTimeout is generous next to the extension's 250 ms: `ps -ax` on a busy
	// machine takes a moment and magnum is not on a UI hot path.
	psTimeout = 3 * time.Second
)

// herdrProcess is one process whose argv starts with the herdr binary.
type herdrProcess struct {
	pid, tty string
	// args is the argv after the executable, so a binary path containing
	// spaces cannot shift the arguments.
	args string
}

var (
	psLine     = regexp.MustCompile(`^(\d+)\s+(\S+)\s+(.+)$`)
	edgeQuotes = regexp.MustCompile(`^['"]|['"]$`)
	attachArgv = regexp.MustCompile(`^session\s+attach\s+(\S+)$`)
)

// parseHerdrProcesses parses `ps -axo pid=,tty=,args=` into the herdr
// processes that own a tty. `comm` is deliberately not requested because macOS
// truncates it to 16 characters. The tty-less server (`herdr server`, probe 8
// in docs/spikes.md) never qualifies.
func parseHerdrProcesses(output, binary string) []herdrProcess {
	binaryName := filepath.Base(binary)
	var procs []herdrProcess
	for _, line := range strings.Split(output, "\n") {
		m := psLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		pid, tty, args := m[1], m[2], m[3]
		if tty == "??" || tty == "?" {
			continue
		}
		argv := edgeQuotes.ReplaceAllString(strings.TrimSpace(args), "")
		fields := strings.Fields(argv)
		if len(fields) == 0 {
			continue
		}
		first := fields[0]
		var rest string
		switch {
		case argv == binary || strings.HasPrefix(argv, binary+" "):
			// The resolved binary path is matched whole so a path with spaces
			// still yields the right argument list.
			rest = argv[len(binary):]
		case filepath.Base(first) == binaryName || filepath.Base(first) == "herdr":
			rest = argv[len(first):]
		default:
			continue
		}
		if !strings.HasPrefix(tty, "/dev/") {
			tty = "/dev/" + tty
		}
		procs = append(procs, herdrProcess{pid: pid, tty: tty, args: strings.TrimSpace(rest)})
	}
	return procs
}

type sessionKind int

const (
	// sessionNamed is `herdr --session x` or `herdr session attach x`.
	sessionNamed sessionKind = iota
	// sessionBare is a plain `herdr`, which joins the default session.
	sessionBare
	// sessionOther is not a local client: a server, a CLI call, the remote
	// bridge, or a remote attach.
	sessionOther
)

type argvInfo struct {
	kind sessionKind
	name string // set for sessionNamed
}

// Global options a client may carry before any subcommand.
var (
	flagsWithValue = []string{"--session", "--remote", "--remote-keybindings"}
	globalFlags    = []string{"--no-session", "--handoff", "--default-config", "--version", "-V", "--help", "-h"}
)

// argvSession classifies the arguments of a herdr process. A client is herdr
// with global options only, or `herdr session attach <name>`; anything else is
// a subcommand, so a CLI call such as `herdr --session work pane read` is not
// a client even though it names a session. The remote bridge (`herdr client`)
// and a `--remote` attach drive another host's server, so neither is a client
// of a local session. (Copied from the Raycast extension's argvSession.)
func argvSession(argv string) argvInfo {
	words := strings.Fields(argv)
	session, set := "", false
	for i := 0; i < len(words); {
		word := words[i]
		flag, inline, hasInline := word, "", false
		if strings.HasPrefix(word, "--") {
			if f, v, ok := strings.Cut(word, "="); ok {
				flag, inline, hasInline = f, v, true
			}
		}
		if flag == "--remote" {
			return argvInfo{kind: sessionOther}
		}
		if slices.Contains(flagsWithValue, flag) {
			if flag == "--session" {
				switch {
				case hasInline:
					session, set = inline, true
				case i+1 < len(words):
					session, set = words[i+1], true
				default:
					session, set = "", false
				}
			}
			if hasInline {
				i++
			} else {
				i += 2
			}
			continue
		}
		if slices.Contains(globalFlags, flag) {
			i++
			continue
		}
		// The only subcommand a client runs.
		if m := attachArgv.FindStringSubmatch(strings.Join(words[i:], " ")); m != nil {
			return argvInfo{kind: sessionNamed, name: m[1]}
		}
		return argvInfo{kind: sessionOther}
	}
	if set {
		return argvInfo{kind: sessionNamed, name: session}
	}
	return argvInfo{kind: sessionBare}
}

// parseClientTtys returns the ttys of herdr clients that may be revealed for
// session. A plain `herdr` reads as the default session because argv cannot
// show a client that joined a named session through an inherited HERDR_SESSION.
func parseClientTtys(output, binary, session string) []string {
	wanted := strings.TrimSpace(session)
	if wanted == "" {
		wanted = "default"
	}
	var ttys []string
	for _, p := range parseHerdrProcesses(output, binary) {
		a := argvSession(p.args)
		ok := (a.kind == sessionNamed && a.name == wanted) || (a.kind == sessionBare && wanted == "default")
		if ok && !slices.Contains(ttys, p.tty) {
			ttys = append(ttys, p.tty)
		}
	}
	return ttys
}

// clientTtys lists the ttys of the session's herdr clients with one `ps` call
// (not pgrep: `pgrep -x herdr` finds nothing on this machine while ps shows the
// client, probe 8). A ps failure is an error; no clients is an empty result.
func (r *Revealer) clientTtys(ctx context.Context) ([]string, error) {
	res, err := r.run.Run(ctx, execx.Cmd{
		Name:    cmdPS,
		Args:    []string{"-axo", psColumns},
		Timeout: psTimeout,
		Label:   "list herdr clients",
	})
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return parseClientTtys(string(res.Stdout), r.herdrBin, r.session), nil
}
