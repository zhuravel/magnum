package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"

	"github.com/charmbracelet/x/term"
	"github.com/zhuravel/magnum/internal/execx"
)

// Log rotation defaults for the daemon log.
const (
	LogMaxBytes = 10 << 20 // rotate state/logs/daemon.log at 10 MB
	LogKeep     = 3        // keep daemon.log.1 … daemon.log.3
)

// RotatingFile is an io.Writer that appends to a file and rotates it by size:
// when a write would push the file past Max bytes, path.N-1 becomes path.N
// (the oldest beyond Keep is dropped), path becomes path.1 and a new file is
// started. A rotation that fails keeps writing to path (reopened), and the
// next write past Max tries again; a file that could not be reopened is
// opened again by the next write. Only Close stops it. Safe for concurrent
// use.
type RotatingFile struct {
	Path string
	Max  int64
	Keep int

	mu     sync.Mutex
	f      *os.File
	size   int64
	closed bool
}

// OpenRotating opens (or creates, 0600) path for appending.
func OpenRotating(path string, max int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{Path: path, Max: max, Keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return fmt.Errorf("log dir: %w", err)
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log: %w", err)
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.f == nil {
		// A rotation could not reopen the file: try again.
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.Max > 0 && r.size > 0 && r.size+int64(len(p)) > r.Max {
		// A failed rotation leaves path open for appending (or for the
		// next write to open): the line is kept, never the writer closed.
		_ = r.rotate()
		if r.f == nil {
			if err := r.open(); err != nil {
				return 0, err
			}
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts the rotated files, moves path to path.1 and opens a new
// path. Whatever fails, path is opened again (appending to the old file
// when it could not be moved); r.f is nil only when that open failed too.
func (r *RotatingFile) rotate() error {
	cerr := r.f.Close()
	r.f = nil
	keep := max(r.Keep, 1)
	_ = os.Remove(r.Path + "." + strconv.Itoa(keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(r.Path+"."+strconv.Itoa(i), r.Path+"."+strconv.Itoa(i+1))
	}
	var rerr error
	if err := os.Rename(r.Path, r.Path+".1"); err != nil && !os.IsNotExist(err) {
		rerr = fmt.Errorf("rotate log: %w", err)
	}
	if cerr != nil {
		rerr = errors.Join(fmt.Errorf("close log: %w", cerr), rerr)
	}
	return errors.Join(rerr, r.open())
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// openDaemonLog opens the daemon log for appending. Only the daemon (daemon
// true) rotates it by size. Every other process just appends: a short-lived
// CLI command must not rename the file the running daemon holds open.
func openDaemonLog(path string, daemon bool) (io.WriteCloser, error) {
	if daemon {
		rf, err := OpenRotating(path, LogMaxBytes, LogKeep)
		if err != nil {
			return nil, err
		}
		return rf, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	return f, nil
}

// RedactHandler wraps a slog.Handler and passes the message and every string
// attribute (errors and Stringers included) through execx.Redact, so a token
// that slips into a log call never reaches a sink.
type RedactHandler struct{ Inner slog.Handler }

// Enabled implements slog.Handler.
func (h RedactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, execx.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.Inner.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return RedactHandler{Inner: h.Inner.WithAttrs(red)}
}

// WithGroup implements slog.Handler.
func (h RedactHandler) WithGroup(name string) slog.Handler {
	return RedactHandler{Inner: h.Inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, execx.Redact(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, x := range g {
			red[i] = redactAttr(x)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, execx.Redact(safeText(x, func() string { return x.Error() })))
		case fmt.Stringer:
			return slog.String(a.Key, execx.Redact(safeText(x, func() string { return x.String() })))
		case []byte:
			return slog.String(a.Key, execx.Redact(string(x)))
		default:
			// Slices, maps and structs: redact their JSON text. Only a value
			// that actually changed is replaced (by the redacted text), so
			// clean values keep their structure in the JSON log.
			text := anyText(x)
			if red := execx.Redact(text); red != text {
				return slog.String(a.Key, red)
			}
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// safeText renders v, an error or a Stringer, through text (a closure that
// calls the method, so a method value bound to a nil pointer cannot panic
// before the recover) the way slog's own handlers do: "<nil>" when it panics
// on a nil pointer (a typed-nil error), "!PANIC: …" for another panic, so a
// bad value never takes the logging goroutine down.
func safeText(v any, text func() string) (s string) {
	defer func() {
		if r := recover(); r != nil {
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
				s = "<nil>"
				return
			}
			s = fmt.Sprintf("!PANIC: %v", r)
		}
	}()
	return text()
}

// anyText renders v the way the log sink would show it: its JSON encoding,
// or fmt.Sprint when v cannot be encoded.
func anyText(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}

// multiHandler fans a record out to several handlers.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range m {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// NewLogger builds magnum's logger: JSON lines to file (when non-nil) plus
// human-readable text to stderr (when non-nil), both behind RedactHandler.
func NewLogger(file io.Writer, stderr io.Writer, level slog.Leveler) *slog.Logger {
	if level == nil {
		level = slog.LevelInfo
	}
	var hs multiHandler
	if file != nil {
		hs = append(hs, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: level}))
	}
	if stderr != nil {
		hs = append(hs, slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	}
	if len(hs) == 0 {
		return slog.New(slog.DiscardHandler)
	}
	var h slog.Handler = hs
	if len(hs) == 1 {
		h = hs[0]
	}
	return slog.New(RedactHandler{Inner: h})
}

// Interactive reports whether f is a terminal (so logs are mirrored to it
// and prompts may be shown). It asks the terminal driver (the termios ioctl
// behind isatty), so /dev/null, which launchd and background jobs hand out
// as stdin/stderr and which is also a character device, is not a terminal.
func Interactive(f *os.File) bool { return f != nil && term.IsTerminal(f.Fd()) }

// Printf adapts a slog.Logger to execx.Logger (and herdr/slots/notify
// loggers): each line becomes one record at level with the given message
// prefix as the "src" attribute.
type Printf struct {
	Logger *slog.Logger
	Level  slog.Level
	Src    string
}

// Printf implements execx.Logger.
func (p Printf) Printf(format string, args ...any) {
	if p.Logger == nil {
		return
	}
	p.Logger.Log(context.Background(), p.Level, fmt.Sprintf(format, args...), "src", p.Src)
}

// Logf implements execx.LevelLogger: the runner picks the level (successful
// commands at Debug, failures at Warn).
func (p Printf) Logf(level slog.Level, format string, args ...any) {
	if p.Logger == nil {
		return
	}
	p.Logger.Log(context.Background(), level, fmt.Sprintf(format, args...), "src", p.Src)
}

// LogAttrs implements execx.AttrLogger: an audit event's mirror keeps its
// subject and kind as attributes (execx.LogEvent), after "src".
func (p Printf) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	if p.Logger == nil {
		return
	}
	p.Logger.LogAttrs(context.Background(), level, msg, append([]slog.Attr{slog.String("src", p.Src)}, attrs...)...)
}

var (
	_ execx.Logger      = Printf{}
	_ execx.LevelLogger = Printf{}
	_ execx.AttrLogger  = Printf{}
)
