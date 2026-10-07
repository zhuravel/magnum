// Package herdr is magnum's client for the herdr terminal multiplexer's
// Unix-socket API (verified against herdr 0.9.x, protocol 22).
//
// Protocol: newline-delimited JSON. Each request {"id","method","params"}
// goes out on a fresh connection; the server writes exactly one response,
// {"id","result":{"type":…}} or {"id","error":{"code","message"}}, and closes
// the connection.
//
// Server errors surface as *Error carrying herdr's code (agent_blocked,
// agent_prompt_stalled, timeout, ui_busy, invalid_key, …). An unreachable
// socket wraps ErrUnavailable; a client-side deadline wraps ErrTimeout.
// Every call logs the equivalent `herdr …` CLI command, redacted, through
// the optional Logger so a human can replay what magnum did.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// Protocol is the herdr socket protocol version this client was verified
// against (ServerInfo.Protocol); `magnum doctor` warns on any other.
const Protocol = 22

// requiredMethods are the socket methods magnum sends: this package's typed
// calls, plugin.list (sent through Call by doctor) and, through the herdr
// CLI reveal runs (`herdr terminal title set|clear`), client.window_title.*.
var requiredMethods = []string{
	"ping", "session.snapshot", "workspace.create", "workspace.close", "workspace.report_metadata",
	"pane.split", "pane.send_input", "pane.send_keys", "pane.read", "pane.wait_for_output", "pane.rename",
	"pane.process_info", "pane.get", "agent.start", "agent.prompt", "agent.read",
	"agent.focus", "agent.rename", "agent.send_keys", "notification.show", "plugin.pane.open", "plugin.list",
	"client.window_title.set", "client.window_title.clear", "worktree.list",
}

// notTyped are the required methods magnum sends other than through this
// package's typed calls: plugin.list through Call, client.window_title.*
// through the herdr CLI (reveal's Ghostty focus marker).
var notTyped = []string{"plugin.list", "client.window_title.set", "client.window_title.clear"}

// RequiredMethods returns the socket methods magnum needs the herdr server
// to offer (`magnum doctor` checks them against `herdr api schema`).
func RequiredMethods() []string { return slices.Clone(requiredMethods) }

// DefaultTimeout bounds a call that has no server-side wait when
// Client.Timeout is zero.
const DefaultTimeout = 15 * time.Second

// maxAgentStartTimeout is herdr's upper bound for agent.start timeout_ms; it
// budgets an agent.start sent without an explicit timeout.
const maxAgentStartTimeout = 300 * time.Second

// Herdr error codes magnum reacts to.
const (
	CodeAgentBlocked       = "agent_blocked"
	CodeAgentPromptStalled = "agent_prompt_stalled"
	CodeTimeout            = "timeout"
	CodeUIBusy             = "ui_busy"
	CodeInvalidKey         = "invalid_key"
	CodeInvalidRequest     = "invalid_request"
	CodeAgentNotFound      = "agent_not_found"
	CodePaneNotFound       = "pane_not_found"
	CodeTabNotFound        = "tab_not_found"
	CodeWorkspaceNotFound  = "workspace_not_found"
)

var (
	// ErrUnavailable wraps failures to reach the socket (no server running,
	// stale socket file, permission denied).
	ErrUnavailable = errors.New("herdr unavailable")
	// ErrTimeout wraps client-side deadlines (Client.Timeout, ctx deadline,
	// WaitIdleShell). IsTimeout also matches herdr's own "timeout" code.
	ErrTimeout = errors.New("herdr request timed out")
)

// Error is an error response from the herdr server.
type Error struct {
	Method  string `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.Method, e.Code, e.Message)
}

// IsCode reports whether err is (or wraps) a herdr *Error with the given code.
func IsCode(err error, code string) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == code
}

// IsTimeout reports whether err is a client-side deadline or herdr's own
// "timeout" error.
func IsTimeout(err error) bool {
	return errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || IsCode(err, CodeTimeout)
}

// DefaultSocket returns $HERDR_SOCKET_PATH (set for herdr plugins and panes)
// or ~/.config/herdr/herdr.sock.
func DefaultSocket() string {
	if s := os.Getenv("HERDR_SOCKET_PATH"); s != "" {
		return s
	}
	return paths.Expand("~/.config/herdr/herdr.sock")
}

// Client talks to one herdr server. The zero value uses DefaultSocket and
// DefaultTimeout. A Client is safe for concurrent use: it holds no
// connection between calls.
type Client struct {
	// Socket is the herdr.sock path; "" means DefaultSocket().
	Socket string
	// Timeout bounds calls without a server-side wait; zero means
	// DefaultTimeout. Calls that wait server-side get their wait on top.
	Timeout time.Duration
	// Logger, when set, receives one debug line per call with the equivalent
	// redacted `herdr …` CLI command, its duration and error.
	Logger execx.Logger
}

func (c *Client) socket() string {
	if c.Socket != "" {
		return c.Socket
	}
	return DefaultSocket()
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// callSpec describes one request/response round trip.
type callSpec struct {
	method string
	params any
	out    any
	cli    string        // equivalent CLI line for the log
	wait   time.Duration // server-side wait budget added to Timeout
	// unbounded marks a server-side wait without a timeout: only ctx bounds it.
	unbounded bool
}

// Call sends one request and decodes the response's result object (the
// value of "result", including its "type" field) into out when out is
// non-nil. params nil sends {}. The call is bounded by Client.Timeout and
// ctx; use the typed methods for requests that wait server-side.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	return c.do(ctx, callSpec{method: method, params: params, out: out, cli: socketLine(method, params)})
}

func (c *Client) do(ctx context.Context, sp callSpec) error {
	start := time.Now()
	err := c.roundTrip(ctx, sp)
	c.logf(sp.cli, time.Since(start), err)
	return err
}

var reqSeq atomic.Uint64

func nextID() string { return fmt.Sprintf("magnum-%d-%d", os.Getpid(), reqSeq.Add(1)) }

// aLongTimeAgo is a deadline in the past, used to unblock pending I/O.
var aLongTimeAgo = time.Unix(1, 0)

func (c *Client) roundTrip(ctx context.Context, sp callSpec) error {
	if !sp.unbounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout()+sp.wait)
		defer cancel()
	}
	conn, err := c.dial(ctx, sp.method)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := watch(ctx, conn)
	defer stop()

	id := nextID()
	if err := writeRequest(conn, id, sp.method, sp.params); err != nil {
		return ioErr(ctx, sp.method, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return ioErr(ctx, sp.method, err)
	}
	return decodeResponse(sp.method, id, line, sp.out)
}

// dial connects to the socket; failures other than ctx wrap ErrUnavailable.
func (c *Client) dial(ctx context.Context, method string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ioErr(ctx, method, err)
		}
		return nil, fmt.Errorf("herdr %s: %w: %w", method, ErrUnavailable, err)
	}
	return conn, nil
}

// watch applies ctx's deadline to conn (no deadline clears any earlier one)
// and unblocks pending I/O when ctx is done. The returned func stops the
// watch. The deadline is set before the cancellation hook is registered, so a
// cancellation that fires in between is never overwritten by the deadline.
func watch(ctx context.Context, conn net.Conn) (stop func() bool) {
	dl, _ := ctx.Deadline()
	conn.SetDeadline(dl)
	return context.AfterFunc(ctx, func() { conn.SetDeadline(aLongTimeAgo) })
}

func writeRequest(w io.Writer, id, method string, params any) error {
	if params == nil {
		params = struct{}{}
	}
	b, err := json.Marshal(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// decodeResponse parses one response line into out (the result object).
func decodeResponse(method, id string, line []byte, out any) error {
	var env struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("herdr %s: decode response: %w", method, err)
	}
	if env.Error != nil {
		env.Error.Method = method
		return env.Error
	}
	if env.ID != id {
		return fmt.Errorf("herdr %s: response id %q does not match request %q", method, env.ID, id)
	}
	if len(env.Result) == 0 {
		return fmt.Errorf("herdr %s: response has neither result nor error", method)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("herdr %s: decode result: %w", method, err)
		}
	}
	return nil
}

// ioErr classifies a transport error: ctx cancellation, deadline (ErrTimeout),
// a connection closed before the response, or anything else.
func ioErr(ctx context.Context, method string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		if errors.Is(cerr, context.DeadlineExceeded) {
			return fmt.Errorf("herdr %s: %w: %w", method, ErrTimeout, cerr)
		}
		return fmt.Errorf("herdr %s: %w", method, cerr)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("herdr %s: %w: %w", method, ErrTimeout, err)
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("herdr %s: connection closed without a response: %w", method, err)
	}
	return fmt.Errorf("herdr %s: %w", method, err)
}

func (c *Client) logf(cli string, d time.Duration, err error) {
	if c.Logger == nil {
		return
	}
	if err != nil {
		c.Logger.Printf("debug herdr: %s (%s) err=%s", cli, d.Round(time.Millisecond), execx.Redact(err.Error()))
		return
	}
	c.Logger.Printf("debug herdr: %s (%s)", cli, d.Round(time.Millisecond))
}

// maxLogArg caps one logged argument (prompts can be pages long).
const maxLogArg = 160

// cliLine renders the equivalent herdr CLI command for the log: arguments
// are redacted, flattened to one line, truncated and shell-quoted.
func cliLine(args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "herdr")
	for _, a := range args {
		parts = append(parts, execx.ShellQuote(logArg(a)))
	}
	return strings.Join(parts, " ")
}

// socketLine renders a raw socket call (no CLI equivalent) for the log.
func socketLine(method string, params any) string {
	b, err := json.Marshal(params)
	if err != nil || params == nil {
		b = []byte("{}")
	}
	return "herdr-socket " + method + " " + logArg(string(b))
}

func logArg(s string) string {
	s = execx.Redact(s)
	s = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
	return textx.Clip(s, maxLogArg)
}
