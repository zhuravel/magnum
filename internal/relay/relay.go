// Package relay is magnum-relay, the small service that receives a GitHub App's webhook deliveries and keeps
// them for magnum's daemon, which sleeps behind NAT and cannot receive them itself.
//
// POST /github/webhook refuses, before it reads the body or takes a turn, a request whose
// X-Hub-Signature-256, X-GitHub-Event or X-GitHub-Delivery header is missing or malformed (401, 400), and
// closes its connection. Then it waits for one of a few turns and reads the body whole (at most 26 MiB, 413
// above), both within DefaultBodyTimeout of the headers (408 for a body that stalls, which frees its turn; 503
// when no turn came), checks the signature as the HMAC-SHA256 of those exact bytes with the current or the
// previous secret (401 when neither matches, and nothing is stored or parsed), then reads only the routing
// fields of the payload (Envelope) and inserts them under the delivery's GUID: 202 when stored, 200 for a
// delivery already stored, 503 when the journal refuses. A delivery is answered within WebhookDeadline of its
// headers, under GitHub's 10 s. The body itself is never stored or logged.
//
// GET /v1/events lists the journal after a cursor ("<epoch>:<seq>") in journal order, for a bearer token
// (CheckToken's bounds) whose SHA-256 is among the accepted hashes, and waits up to `wait` for a first event
// when there is none (a long poll woken by the next insert; at most MaxWaiting wait at once, and one more
// answers 429). A cursor of another journal (epoch), or one older than the journal's oldest cursor (its
// events were pruned) or ahead of its high water, answers 410 "gap", and the daemon then resyncs by polling
// GitHub. GET /healthz answers without auth and without event data, from counters the journal keeps and a
// snapshot read again at most every 2 s.
//
// Retention deletes the events older than Options.Retention (30 days by default), as a prefix of the
// journal, so the oldest cursor only moves forward. Logs carry delivery ids, event names, sequence numbers
// and error classes, never a body, a secret, a token or an error's text.
package relay

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxBody is the largest delivery read (GitHub caps payloads at 25 MB).
	DefaultMaxBody = 26 << 20
	// DefaultRetention is how long the journal keeps an event.
	DefaultRetention = 30 * 24 * time.Hour
	// MinRetention is the shortest retention Open accepts.
	MinRetention = time.Hour
	// MaxWait is the longest long poll a listing may ask for.
	MaxWait = 30 * time.Second
	// DefaultLimit is the events of a listing that asks for no limit.
	DefaultLimit = 200
	// MaxLimit is the most events one listing returns.
	MaxLimit = 500
	// DefaultMaxWaiting is how many long polls may wait at once; one more answers 429.
	DefaultMaxWaiting = 16

	// DefaultBodyTimeout is how long a delivery has, from its headers, to wait for its turn and send its
	// whole body (BodyTimeout may only shorten it). A body still arriving then is cut (408), which frees
	// its turn.
	DefaultBodyTimeout = 7 * time.Second
	// WebhookDeadline is the longest a delivery takes from its headers to its answer: the body timeout
	// and the journal's write. It stays under GitHub's 10 s.
	WebhookDeadline = DefaultBodyTimeout + insertTimeout

	// MinTokenLength is the shortest bearer token, in `magnum-relay hash-token` and in the listing alike
	// (CheckToken).
	MinTokenLength = 32
	// MaxTokenLength is the longest bearer token, in `magnum-relay hash-token` and in the listing alike.
	MaxTokenLength = 512

	// concurrentBodies bounds the deliveries read at once (each holds up to MaxBody in memory), and
	// bodyQueueWait how long a delivery waits for its turn before it answers 503.
	concurrentBodies = 4
	bodyQueueWait    = 2 * time.Second
	// insertTimeout bounds the journal's write.
	insertTimeout = 2 * time.Second
	// listTimeout bounds one read of the journal.
	listTimeout = 5 * time.Second
	// healthEvery is how old the health check's snapshot of the journal may be before a check reads it
	// again; healthTimeout bounds that read.
	healthEvery   = 2 * time.Second
	healthTimeout = 2 * time.Second
)

// Options configures a Relay.
type Options struct {
	// DB is the journal's SQLite file; its directory is created 0700 and the file 0600.
	DB string
	// Secrets are the webhook secrets a signature may match: the current one, then the previous one
	// during a rotation.
	Secrets [][]byte
	// TokenHashes are the SHA-256 digests of the bearer tokens the listing accepts.
	TokenHashes [][sha256.Size]byte
	// Retention is how long an event is kept (DefaultRetention when zero, at least MinRetention).
	Retention time.Duration
	// MaxBody is the largest delivery read (DefaultMaxBody when zero).
	MaxBody int64
	// BodyTimeout is how long a delivery has to wait for its turn and send its body (DefaultBodyTimeout
	// when zero, at most DefaultBodyTimeout).
	BodyTimeout time.Duration
	// MaxWaiting is how many long polls may wait at once (DefaultMaxWaiting when zero).
	MaxWaiting int
	// Logger receives the relay's logs (discarded when nil).
	Logger *slog.Logger
	// Now is the relay's clock (time.Now when nil).
	Now func() time.Time
}

// Relay is the receiver, the journal and the listing.
type Relay struct {
	j           *journal
	secrets     [][]byte
	tokens      [][sha256.Size]byte
	retention   time.Duration
	maxBody     int64
	bodyTimeout time.Duration
	log         *slog.Logger
	now         func() time.Time

	bodies   chan struct{} // a turn to read a delivery's body
	waiters  chan struct{} // a place for a waiting long poll
	notify   notifier
	stop     chan struct{}
	stopOnce sync.Once

	// The health check answers from a snapshot of the journal's counters, read again by one check at a
	// time once it is older than healthEvery; healthReads counts those reads.
	healthMu    sync.Mutex
	healthSnap  atomic.Pointer[healthSnapshot]
	healthReads atomic.Int64
}

// Open checks the options and opens (or creates) the journal.
func Open(ctx context.Context, opts Options) (*Relay, error) {
	if len(opts.Secrets) == 0 {
		return nil, errors.New("relay: a webhook secret is required")
	}
	for _, s := range opts.Secrets {
		if len(s) == 0 {
			return nil, errors.New("relay: a webhook secret is empty")
		}
	}
	if len(opts.TokenHashes) == 0 {
		return nil, errors.New("relay: at least one token hash is required")
	}
	maxWaiting := cmp.Or(opts.MaxWaiting, DefaultMaxWaiting)
	if maxWaiting < 0 {
		return nil, fmt.Errorf("relay: MaxWaiting %d is negative", maxWaiting)
	}
	r := &Relay{
		secrets:     opts.Secrets,
		tokens:      opts.TokenHashes,
		retention:   cmp.Or(opts.Retention, DefaultRetention),
		maxBody:     opts.MaxBody,
		bodyTimeout: cmp.Or(opts.BodyTimeout, DefaultBodyTimeout),
		log:         opts.Logger,
		now:         opts.Now,
		bodies:      make(chan struct{}, concurrentBodies),
		waiters:     make(chan struct{}, maxWaiting),
		stop:        make(chan struct{}),
	}
	if r.retention < MinRetention {
		return nil, fmt.Errorf("relay: retention %v is shorter than %v", r.retention, MinRetention)
	}
	if r.bodyTimeout <= 0 || r.bodyTimeout > DefaultBodyTimeout {
		return nil, fmt.Errorf("relay: BodyTimeout %v is not within (0, %v]", r.bodyTimeout, DefaultBodyTimeout)
	}
	if r.maxBody <= 0 {
		r.maxBody = DefaultMaxBody
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	j, err := openJournal(ctx, opts.DB)
	if err != nil {
		return nil, err
	}
	r.j = j
	r.notify.ch = make(chan struct{})
	r.refreshHealth(ctx)
	return r, nil
}

// Epoch is the journal's random id, the first part of every cursor.
func (r *Relay) Epoch() string { return r.j.epoch }

// Handler serves the webhook, the listing and the health check.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /github/webhook", r.receive)
	mux.HandleFunc("GET /v1/events", r.list)
	mux.HandleFunc("GET /healthz", r.health)
	return mux
}

// StopWaiting answers every long poll at once and keeps later ones from waiting, before a shutdown.
func (r *Relay) StopWaiting() { r.stopOnce.Do(func() { close(r.stop) }) }

// Close closes the journal.
func (r *Relay) Close() error { return r.j.close() }

// Prune deletes the events older than the retention and returns how many it deleted.
func (r *Relay) Prune(ctx context.Context) (int64, error) {
	return r.j.prune(ctx, r.now().Add(-r.retention))
}

// RunRetention prunes at once and then every interval, until ctx ends.
func (r *Relay) RunRetention(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := r.Prune(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Error("prune failed", "class", errorClass(err))
		case n > 0:
			r.log.Info("pruned", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// notifier wakes the long polls: wait returns a channel the next wake closes.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) wake() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}
