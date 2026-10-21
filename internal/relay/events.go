package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Page is the answer of GET /v1/events.
type Page struct {
	// Epoch is the journal's random id; a journal created again has another, and every cursor of the old
	// one answers 410.
	Epoch string `json:"epoch"`
	// Events are the events after the cursor, in journal order (never null).
	Events []Event `json:"events"`
	// Next is the cursor to pass as `after` next: the last event's, or the request's when none came.
	Next string `json:"next"`
	// HighWater is the cursor of the newest event the journal ever stored.
	HighWater string `json:"high_water"`
	// Oldest is the cursor just before the oldest kept event: `after=<oldest>` lists every kept event, as
	// `after=` does, and a cursor below it answers 410 (its next events were pruned).
	Oldest string `json:"oldest"`
	// ServerTime is the relay's clock (UTC).
	ServerTime time.Time `json:"server_time"`
}

const (
	maxCursor = 100
)

// list is GET /v1/events?after=<cursor>&limit=<1..500>&wait=<0..30s>, for an accepted bearer token.
func (r *Relay) list(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req.Header.Values("Authorization")) {
		r.refusals.add(refusedListingToken)
		w.Header().Set("WWW-Authenticate", `Bearer realm="magnum-relay"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	q := req.URL.Query()
	limit, wait, ok := listParams(q.Get("limit"), q.Get("wait"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	after, ok := r.parseCursor(q.Get("after"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	if after == foreignCursor {
		r.answerGap(req.Context(), w)
		return
	}
	// A long poll answers after its wait, which may outlast the server's write timeout: it sets its own.
	deadline := time.Now().Add(wait)
	_ = http.NewResponseController(w).SetWriteDeadline(deadline.Add(listTimeout + 5*time.Second))
	waiting := false
	defer func() {
		if waiting {
			<-r.waiters
		}
	}()
	for {
		woken := r.notify.wait()
		ctx, cancel := context.WithTimeout(req.Context(), listTimeout)
		events, b, err := r.j.list(ctx, after, limit)
		cancel()
		switch {
		case errors.Is(err, errGap):
			r.log.Info("listing gap", "class", "cursor")
			writeJSON(w, http.StatusGone, r.gap(b))
			return
		case err != nil:
			if req.Context().Err() == nil {
				r.log.Error("listing failed", "class", errorClass(err))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "journal"})
			}
			return
		}
		left := time.Until(deadline)
		if len(events) > 0 || left <= 0 || r.stopped() {
			writeJSON(w, http.StatusOK, r.page(events, after, b))
			return
		}
		if !waiting {
			select {
			case r.waiters <- struct{}{}:
				waiting = true
			default:
				r.log.Warn("listing refused", "class", "too_many_waiting")
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too_many_waiting"})
				return
			}
		}
		t := time.NewTimer(left)
		select {
		case <-woken:
		case <-t.C:
		case <-r.stop:
		case <-req.Context().Done():
			t.Stop()
			return
		}
		// Read again: woken by an insert it lists it; at the deadline or a shutdown it answers what is there.
		t.Stop()
	}
}

// foreignCursor is parseCursor's answer for a cursor of another journal.
const foreignCursor = -2

// parseCursor reads "<epoch>:<seq>": -1 for "" (from the oldest kept event), foreignCursor for another
// journal's epoch, and !ok for a malformed cursor.
func (r *Relay) parseCursor(s string) (int64, bool) {
	if s == "" {
		return -1, true
	}
	if len(s) > maxCursor {
		return 0, false
	}
	epoch, seq, found := strings.Cut(s, ":")
	if !found || epoch == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 0 || strings.HasPrefix(seq, "+") {
		return 0, false
	}
	if epoch != r.j.epoch {
		return foreignCursor, true
	}
	return n, true
}

// listParams reads limit (1..MaxLimit, DefaultLimit when "") and wait (0..MaxWait: a Go duration, or whole
// seconds without a unit).
func listParams(limitS, waitS string) (limit int, wait time.Duration, ok bool) {
	limit = DefaultLimit
	if limitS != "" {
		n, err := strconv.Atoi(limitS)
		if err != nil || n < 1 || n > MaxLimit {
			return 0, 0, false
		}
		limit = n
	}
	if waitS != "" {
		// Whole seconds are bounded before they become a Duration, which a large count would overflow.
		if n, err := strconv.Atoi(waitS); err == nil {
			if n < 0 || n > int(MaxWait/time.Second) {
				return 0, 0, false
			}
			wait = time.Duration(n) * time.Second
		} else if d, err := time.ParseDuration(waitS); err == nil && d >= 0 && d <= MaxWait {
			wait = d
		} else {
			return 0, 0, false
		}
	}
	return limit, wait, true
}

// CheckToken says whether token can be one of the relay's bearer tokens: one word of MinTokenLength to
// MaxTokenLength printable ASCII characters. `magnum-relay hash-token` and the listing apply it alike, so a
// token setup accepts is one the listing accepts. Its errors never quote the token.
func CheckToken(token string) error {
	if len(token) < MinTokenLength || len(token) > MaxTokenLength {
		return fmt.Errorf("a token has %d to %d characters", MinTokenLength, MaxTokenLength)
	}
	for i := range len(token) {
		if c := token[i]; c < '!' || c > '~' {
			return errors.New("a token is one word of printable ASCII characters")
		}
	}
	return nil
}

// authorized reports whether the one Authorization header is "Bearer <token>" for an accepted token; it
// compares the token's SHA-256 with every accepted hash, in constant time.
func (r *Relay) authorized(headers []string) bool {
	if len(headers) != 1 {
		return false
	}
	scheme, token, found := strings.Cut(headers[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || CheckToken(token) != nil {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	ok := 0
	for _, h := range r.tokens {
		ok |= subtle.ConstantTimeCompare(sum[:], h[:])
	}
	return ok == 1
}

func (r *Relay) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *Relay) cursor(seq int64) string { return r.j.epoch + ":" + strconv.FormatInt(seq, 10) }

func (r *Relay) page(events []Event, after int64, b bounds) Page {
	next := after
	if next < 0 {
		next = b.pruned
	}
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	return Page{
		Epoch:      r.j.epoch,
		Events:     events,
		Next:       r.cursor(next),
		HighWater:  r.cursor(b.high),
		Oldest:     r.cursor(b.pruned),
		ServerTime: r.now().UTC().Truncate(time.Second),
	}
}

func (r *Relay) gap(b bounds) map[string]string {
	return map[string]string{"error": "gap", "epoch": r.j.epoch, "oldest": r.cursor(b.pruned)}
}

// answerGap answers 410 to a cursor of another journal, naming this one's epoch and oldest cursor.
func (r *Relay) answerGap(ctx context.Context, w http.ResponseWriter) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	b, err := r.j.bounds(ctx)
	if err != nil {
		r.log.Error("listing failed", "class", errorClass(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "journal"})
		return
	}
	r.log.Info("listing gap", "class", "epoch")
	writeJSON(w, http.StatusGone, r.gap(b))
}

// health is GET /healthz: whether the journal answers ({"ok":true}, else 503 {"ok":false}), for anyone (the
// container's HEALTHCHECK sends no token); its epoch, how many events it keeps and when the newest arrived
// only for an accepted bearer token, and never event data. It answers from a snapshot (healthNow), so however
// many checks come, the journal is read at most once each healthEvery and by one check at a time.
func (r *Relay) health(w http.ResponseWriter, req *http.Request) {
	s := r.healthNow(req.Context())
	if !s.ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
		return
	}
	auth := req.Header.Values("Authorization")
	if len(auth) == 0 || !r.authorized(auth) {
		if len(auth) > 0 {
			r.refusals.add(refusedHealthToken)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK             bool       `json:"ok"`
		Epoch          string     `json:"epoch"`
		Events         int64      `json:"events"`
		LastReceivedAt *time.Time `json:"last_received_at"`
	}{true, r.j.epoch, s.count, s.last})
}

// healthSnapshot is what one read of the journal's counters gave, and when.
type healthSnapshot struct {
	at    time.Time
	ok    bool
	count int64
	last  *time.Time
}

// healthNow is the snapshot a check answers from: the last one while it is fresh, or while another check is
// reading the journal; else this check reads it.
func (r *Relay) healthNow(ctx context.Context) *healthSnapshot {
	fresh := func(s *healthSnapshot) bool {
		age := r.now().Sub(s.at)
		return age >= 0 && age < healthEvery
	}
	s := r.healthSnap.Load()
	if fresh(s) || !r.healthMu.TryLock() {
		return s
	}
	defer r.healthMu.Unlock()
	if s := r.healthSnap.Load(); fresh(s) {
		return s
	}
	return r.refreshHealth(ctx)
}

// refreshHealth reads the journal's counters into a new snapshot (Open takes the first). The read outlives
// a caller that hung up, within healthTimeout: the snapshot is the journal's, never the caller's cancel,
// which would answer the checks of the next healthEvery 503.
func (r *Relay) refreshHealth(ctx context.Context) *healthSnapshot {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	r.healthReads.Add(1)
	count, last, err := r.j.stats(ctx)
	if err != nil {
		r.log.Error("health check failed", "class", errorClass(err))
	}
	s := &healthSnapshot{at: r.now(), ok: err == nil, count: count, last: last}
	r.healthSnap.Store(s)
	return s
}
