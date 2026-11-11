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
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// Each pane's events pass a token bucket: rateBurst at once, then one per
// rateEvery. A Claude or Codex turn sends one or two; a pane past it is a
// flood (a forged one, a loop), and its events are dropped until its bucket
// refills. The pane is the datagram's claim, so all panes also share one
// bucket (globalBurst, then one per globalEvery): an agent that invents a
// pane for each event cannot flood the daemon either.
const (
	rateBurst   = 8
	rateEvery   = 250 * time.Millisecond
	globalBurst = 32
	globalEvery = 25 * time.Millisecond
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
	global  *bucket            // all panes' bucket; Serve's alone
	// read counts the datagrams Serve is done with, dropped ones too.
	read atomic.Int64
}

// bucket is a token bucket: tokens left of burst, one more each every,
// counted up to at.
type bucket struct {
	burst  int
	every  time.Duration
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
	return &Listener{conn: conn, path: path, log: log, now: time.Now, buckets: map[string]*bucket{},
		global: &bucket{burst: globalBurst, every: globalEvery, tokens: globalBurst}}, nil
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
// or without an agent or an event this version knows, one that names
// neither a pane nor a pane key, and a pane's events past its rate
// (rateBurst at once, then one per rateEvery, and all panes' past
// globalBurst, then one per globalEvery; an event without a pane counts
// against its pane key's). The ids of an event it hands on are ids (as
// Parse makes them; a pane or key that is none is left out) and its free
// text is cut, since anyone with the pane's env can send one. Each event it
// hands on logs one debug line with its name, its pane (paneForLog) and its
// key (keyForLog), never another field. A handler that panics costs its
// event, not the listener.
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
		l.take(buf[:n], handle)
		l.read.Add(1)
	}
}

// take hands the event datagram data holds to handle, or drops it (see
// Serve).
func (l *Listener) take(data []byte, handle func(Event)) {
	ev, why := decode(data)
	if why != "" {
		l.log.Debug("hook datagram dropped", "why", why)
		return
	}
	if !l.allow(ev.bucket()) {
		return
	}
	l.log.Debug("hook event", "event", ev.Name, "pane", paneForLog(ev.Pane), "key", keyForLog(ev.PaneKey))
	l.dispatch(handle, ev)
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
	case !knownAgent(ev.Agent) || !knownName(ev.Name):
		return Event{}, "no agent or event this version knows"
	}
	ev.Pane, ev.PaneKey = hookID(ev.Pane), hookID(ev.PaneKey)
	if ev.Pane == "" && ev.PaneKey == "" {
		return Event{}, "no pane and no pane key"
	}
	ev.SessionID, ev.TurnID, ev.AgentID, ev.CodexThread = hookID(ev.SessionID), hookID(ev.TurnID), hookID(ev.AgentID), hookID(ev.CodexThread)
	ev.TranscriptPath, ev.Error = freeText(ev.TranscriptPath, pathMax), freeText(ev.Error, errorMax)
	ev.Source, ev.ToolName = freeText(ev.Source, shortMax), freeText(ev.ToolName, shortMax)
	ev.Background = max(ev.Background, 0)
	return ev, ""
}

// bucket is the rate bucket ev counts against: its herdr pane, else its
// pane key (the two never collide: a key's bucket is prefixed).
func (ev Event) bucket() string {
	if ev.Pane != "" {
		return ev.Pane
	}
	return "key:" + ev.PaneKey
}

// allow reports whether pane's bucket and the bucket of all panes both
// have a token for one more event, and takes one from each.
func (l *Listener) allow(pane string) bool {
	now := l.now()
	b := l.buckets[pane]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		if len(l.buckets) >= maxBuckets {
			l.log.Debug("hook event dropped: too many panes at once", "pane", paneForLog(pane))
			return false
		}
		b = &bucket{burst: rateBurst, every: rateEvery, tokens: rateBurst, at: now}
		l.buckets[pane] = b
	}
	b.refill(now)
	l.global.refill(now)
	switch {
	case b.tokens == 0:
		if !b.cut {
			b.cut = true
			l.log.Debug("hook events dropped: the pane is over its rate", "pane", paneForLog(pane))
		}
		return false
	case l.global.tokens == 0:
		if !l.global.cut {
			l.global.cut = true
			l.log.Debug("hook events dropped: the panes together are over their rate")
		}
		return false
	}
	b.tokens--
	l.global.tokens--
	b.cut, l.global.cut = false, false
	return true
}

// refill adds the tokens that came since b.at, up to b.burst.
func (b *bucket) refill(now time.Time) {
	if now.Before(b.at) || b.at.IsZero() { // the clock went back, or the first event
		b.at = now
	}
	if n := int(now.Sub(b.at) / b.every); n > 0 {
		b.tokens = min(b.burst, b.tokens+n)
		b.at = b.at.Add(time.Duration(n) * b.every)
	}
	if b.tokens == b.burst {
		b.at = now
	}
}

// prune drops the buckets that are full again: their panes are idle.
func (l *Listener) prune(now time.Time) {
	for pane, b := range l.buckets {
		if b.refill(now); b.tokens == b.burst {
			delete(l.buckets, pane)
		}
	}
}

// herdrPane is the shape of herdr's pane ids (w1:p2, w2J:p1).
var herdrPane = regexp.MustCompile(`^w[0-9A-Za-z]{1,16}:p[0-9]{1,6}$`)

// paneForLog is pane as a log line names it: a pane in herdr's shape as it
// is, any other as "other" (an agent picks the pane, and could make it look
// like a token).
func paneForLog(pane string) string {
	if herdrPane.MatchString(pane) {
		return pane
	}
	return "other"
}

// agentKey is the shape of magnum's agent names (agents.AgentName: herdr's
// ^[a-z][a-z0-9_-]{0,31}$), which a pane key has.
var agentKey = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// keyForLog is a pane key as a log line names it: one in the shape of
// magnum's agent names as it is, "" for none, any other as "other".
func keyForLog(key string) string {
	if key == "" || agentKey.MatchString(key) {
		return key
	}
	return "other"
}

// dispatch hands ev to handle; a panic is logged with the event's name and
// pane alone (its value may carry what the handler read).
func (l *Listener) dispatch(handle func(Event), ev Event) {
	defer func() {
		if p := recover(); p != nil {
			l.log.Error("hook event handler panicked", "event", ev.Name, "pane", paneForLog(ev.Pane))
		}
	}()
	handle(ev)
}
