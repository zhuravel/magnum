package agenthooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Each pane's events pass a token bucket: rateBurst at once, then one per
// rateEvery. A Claude or Codex turn sends one or two; a pane past it is a
// flood (a forged one, a loop), and its events are dropped until its bucket
// refills.
const (
	rateBurst = 8
	rateEvery = 250 * time.Millisecond
	// maxBuckets bounds the panes the listener keeps a bucket for: past it
	// the full buckets (idle panes) are dropped, and an event of a new pane
	// is dropped while the rest are all at work.
	maxBuckets = 256
	// readBuffer is the socket's receive buffer: macOS's default
	// (net.local.dgram.recvspace, 4 KiB) holds two full datagrams.
	readBuffer = 64 << 10
	// readRetry is how long Serve waits after a read error that did not
	// close the socket.
	readRetry = 50 * time.Millisecond
)

// Send sends ev as one datagram (at most MaxDatagram bytes; a V of 0 goes as
// Version) to the socket at path, within SendTimeout. It never waits for a
// daemon that is not there: a missing socket (ENOENT) and one nobody reads
// (ECONNREFUSED) fail at once.
func Send(path string, ev Event) error {
	if len(path) >= SocketPathLimit {
		return fmt.Errorf("agenthooks: send: socket path is %d bytes, a Unix socket's must be under %d", len(path), SocketPathLimit)
	}
	if ev.V == 0 {
		ev.V = Version
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("agenthooks: send: %w", err)
	}
	if len(b) > MaxDatagram {
		return fmt.Errorf("agenthooks: send: the event is %d bytes, over %d", len(b), MaxDatagram)
	}
	d := net.Dialer{Timeout: SendTimeout}
	conn, err := d.Dial("unixgram", path)
	if err != nil {
		return fmt.Errorf("agenthooks: send: %w", err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(SendTimeout)); err != nil {
		return fmt.Errorf("agenthooks: send: %w", err)
	}
	if _, err := conn.Write(b); err != nil {
		return fmt.Errorf("agenthooks: send: %w", err)
	}
	return nil
}

// Listener reads the events sent to one socket (Listen, Serve).
type Listener struct {
	conn *net.UnixConn
	path string
	log  *slog.Logger
	once sync.Once
	// now is the rate limit's clock (time.Now).
	now     func() time.Time
	buckets map[string]*bucket // pane -> its token bucket; Serve's alone
}

// bucket is one pane's token bucket: tokens left, counted up to at.
type bucket struct {
	tokens int
	at     time.Time
	// cut is set while the pane's events are dropped, so a flood logs once.
	cut bool
}

// Listen opens the datagram socket at path for Serve. The caller holds the
// daemon lock, so no other daemon serves path: the socket's directory is
// made (or made again) the user's alone (0700), a socket a daemon left there
// (it stopped, or it was killed) is removed first, and the new one is 0600. A
// path of SocketPathLimit bytes or more, and a file at path that is no socket
// (which is never removed), are errors. log receives Serve's lines (nil =
// none).
func Listen(path string, log *slog.Logger) (*Listener, error) {
	if len(path) >= SocketPathLimit {
		return nil, fmt.Errorf("agenthooks: socket path %s is %d bytes, a Unix socket's must be under %d", path, len(path), SocketPathLimit)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agenthooks: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, which its owner must be able to enter
		return nil, fmt.Errorf("agenthooks: %w", err)
	}
	switch st, err := os.Lstat(path); {
	case err == nil && st.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("agenthooks: %s is no socket; it is left alone", path)
	case err == nil:
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("agenthooks: remove the stale socket: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("agenthooks: %w", err)
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("agenthooks: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		conn.Close()
		return nil, fmt.Errorf("agenthooks: %w", err)
	}
	_ = conn.SetReadBuffer(readBuffer) // the default still works, only smaller
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Listener{conn: conn, path: path, log: log, now: time.Now, buckets: map[string]*bucket{}}, nil
}

// Path is the socket's path.
func (l *Listener) Path() string { return l.path }

// Close closes the socket (Serve returns). Its file stays: the next Listen
// removes it, and a hook that sends to it meanwhile fails at once.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() { err = l.conn.Close() })
	return err
}

// Serve reads the socket until ctx ends, then closes it, and hands each
// event to handle, one at a time, on Serve's goroutine. It drops a datagram
// over MaxDatagram, one that is no JSON object, of another V than Version,
// or without a pane, an agent or an event this version knows, and a pane's
// events past its rate (rateBurst at once, then one per rateEvery). The ids
// of an event it hands on are ids (as Parse makes them) and its free text is
// cut, since anyone with the pane's env can send one. Each event it hands
// on logs one debug line with its name and pane, never another field. A
// handler that panics costs its event, not the listener.
func (l *Listener) Serve(ctx context.Context, handle func(Event)) {
	defer l.Close()
	defer context.AfterFunc(ctx, func() { _ = l.Close() })()
	buf := make([]byte, MaxDatagram+1) // a longer datagram fills it: too long
	for {
		n, _, err := l.conn.ReadFromUnix(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			l.log.Debug("hook socket read failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(readRetry):
			}
			continue
		}
		ev, why := decode(buf[:n])
		if why != "" {
			l.log.Debug("hook datagram dropped", "why", why)
			continue
		}
		if !l.allow(ev.Pane) {
			continue
		}
		l.log.Debug("hook event", "event", ev.Name, "pane", ev.Pane)
		l.dispatch(handle, ev)
	}
}

// decode reads one datagram into an Event, or says why it is none.
func decode(data []byte) (Event, string) {
	if len(data) > MaxDatagram {
		return Event{}, "over the datagram limit"
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, "no event JSON"
	}
	switch {
	case ev.V != Version:
		return Event{}, fmt.Sprintf("version %d", ev.V)
	case !knownAgent(ev.Agent) || !knownName(ev.Name) || hookID(ev.Pane) == "":
		return Event{}, "no pane, agent or event this version knows"
	}
	ev.SessionID, ev.TurnID, ev.AgentID, ev.CodexThread = hookID(ev.SessionID), hookID(ev.TurnID), hookID(ev.AgentID), hookID(ev.CodexThread)
	ev.TranscriptPath, ev.Error = freeText(ev.TranscriptPath, pathMax), freeText(ev.Error, errorMax)
	ev.Source, ev.ToolName = freeText(ev.Source, shortMax), freeText(ev.ToolName, shortMax)
	ev.Background = max(ev.Background, 0)
	return ev, ""
}

// allow reports whether pane's bucket has a token for one more event, and
// takes it.
func (l *Listener) allow(pane string) bool {
	now := l.now()
	b := l.buckets[pane]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		if len(l.buckets) >= maxBuckets {
			l.log.Debug("hook event dropped: too many panes at once", "pane", pane)
			return false
		}
		b = &bucket{tokens: rateBurst, at: now}
		l.buckets[pane] = b
	}
	b.refill(now)
	if b.tokens == 0 {
		if !b.cut {
			b.cut = true
			l.log.Debug("hook events dropped: the pane is over its rate", "pane", pane)
		}
		return false
	}
	b.tokens--
	b.cut = false
	return true
}

// refill adds the tokens that came since b.at, up to rateBurst.
func (b *bucket) refill(now time.Time) {
	if now.Before(b.at) { // the clock went back
		b.at = now
	}
	if n := int(now.Sub(b.at) / rateEvery); n > 0 {
		b.tokens = min(rateBurst, b.tokens+n)
		b.at = b.at.Add(time.Duration(n) * rateEvery)
	}
	if b.tokens == rateBurst {
		b.at = now
	}
}

// prune drops the buckets that are full again: their panes are idle.
func (l *Listener) prune(now time.Time) {
	for pane, b := range l.buckets {
		if b.refill(now); b.tokens == rateBurst {
			delete(l.buckets, pane)
		}
	}
}

// dispatch hands ev to handle; a panic is logged with the event's name and
// pane alone (its value may carry what the handler read).
func (l *Listener) dispatch(handle func(Event), ev Event) {
	defer func() {
		if p := recover(); p != nil {
			l.log.Error("hook event handler panicked", "event", ev.Name, "pane", ev.Pane)
		}
	}()
	handle(ev)
}
