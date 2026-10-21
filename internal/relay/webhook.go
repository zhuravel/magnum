package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const signaturePrefix = "sha256="

// receive is POST /github/webhook. Headers that cannot be a delivery are refused before the body is read
// or a turn taken; then the body is read whole within the body timeout, its signature checked over those
// exact bytes, and only then its routing fields read and stored (or ignored, for an installation the relay
// does not keep: keeps).
func (r *Relay) receive(w http.ResponseWriter, req *http.Request) {
	rc := http.NewResponseController(w)
	// The wait for a turn and the body share one deadline: a body that stalls is cut and frees its turn.
	deadline := time.Now().Add(r.bodyTimeout)
	_ = rc.SetReadDeadline(deadline)
	if req.ContentLength > r.maxBody {
		r.refuse(w, rc, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	mac, ok := signatureOf(req.Header.Values("X-Hub-Signature-256"))
	if !ok {
		r.refuse(w, rc, http.StatusUnauthorized, "signature")
		return
	}
	event, delivery := req.Header.Values("X-GitHub-Event"), req.Header.Values("X-GitHub-Delivery")
	if len(event) != 1 || len(delivery) != 1 || !eventRe.MatchString(event[0]) || !deliveryRe.MatchString(delivery[0]) {
		r.refuse(w, rc, http.StatusBadRequest, "headers")
		return
	}
	// Each delivery read holds its body in memory: a few at a time, and a short wait for a turn.
	wait := time.NewTimer(min(bodyQueueWait, time.Until(deadline)))
	select {
	case r.bodies <- struct{}{}:
		wait.Stop()
		defer func() { <-r.bodies }()
	case <-wait.C:
		r.refuse(w, rc, http.StatusServiceUnavailable, "busy")
		return
	case <-req.Context().Done():
		wait.Stop()
		return
	}
	body, err := readBody(w, req, r.maxBody)
	if err != nil {
		switch _, tooLarge := errors.AsType[*http.MaxBytesError](err); {
		case tooLarge:
			r.refuse(w, rc, http.StatusRequestEntityTooLarge, "too_large")
		case errors.Is(err, os.ErrDeadlineExceeded):
			r.refuse(w, rc, http.StatusRequestTimeout, "body_timeout")
		default:
			r.refuse(w, rc, http.StatusBadRequest, "read")
		}
		return
	}
	if !r.signed(body, mac) {
		r.refuse(w, rc, http.StatusUnauthorized, "signature")
		return
	}
	ev, err := Envelope(event[0], jsonOf(req, body))
	if err != nil {
		r.log.Warn("webhook refused", "delivery", delivery[0], "event", event[0], "class", "payload")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payload"})
		return
	}
	if !r.keeps(ev) {
		r.refusals.add(ignoredInstallation)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	ev.DeliveryID = delivery[0]
	ev.ReceivedAt = r.now().UTC().Truncate(time.Second)
	// The insert outlives a client that hung up: GitHub records the delivery as failed either way, and a
	// stored row spares the daemon's backstop a read.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), insertTimeout)
	defer cancel()
	seq, dup, err := r.j.insert(ctx, ev)
	switch {
	case err != nil:
		r.log.Error("webhook not stored", "delivery", ev.DeliveryID, "event", ev.Event, "class", errorClass(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "journal"})
	case dup:
		r.log.Info("webhook duplicate", "delivery", ev.DeliveryID, "event", ev.Event)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
	default:
		r.notify.wake()
		r.log.Info("webhook stored", "delivery", ev.DeliveryID, "event", ev.Event, "seq", seq)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "stored", "seq": seq})
	}
}

// refuse answers a delivery the relay does not store and stops reading its connection: the answer says
// Connection: close (so net/http does not read the rest of the body to reuse it) and the read deadline
// passes now. It is counted for the minute's log line (RunRefusalLog) by its status and class only, since
// nothing of a request whose signature was not found good is trusted.
func (r *Relay) refuse(w http.ResponseWriter, rc *http.ResponseController, code int, class string) {
	r.refusals.add(refusedWebhook(code, class))
	w.Header().Set("Connection", "close")
	_ = rc.SetReadDeadline(time.Now())
	writeJSON(w, code, map[string]string{"error": class})
}

// keeps says whether the relay stores a signed delivery: every one when no installation is listed, else one
// of a listed installation, or the App's ping, which names none (only the App's owner sends it, by saving
// the webhook). Anyone may install a public App, and GitHub signs that installation's deliveries with the
// App's secret too.
func (r *Relay) keeps(ev Event) bool {
	if len(r.installations) == 0 {
		return true
	}
	if ev.InstallationID == 0 {
		return ev.Event == "ping"
	}
	_, listed := slices.BinarySearch(r.installations, ev.InstallationID)
	return listed
}

// firstBodyBuffer is the most readBody reserves before the body arrives, whatever its Content-Length says.
const firstBodyBuffer = 1 << 20

// readBody reads the whole body, at most limit bytes. Its buffer starts at the Content-Length up to
// firstBodyBuffer and grows as the body comes: a sender's header alone reserves no more, while four turns
// at the 26 MiB limit would reserve over 100 MiB before any signature is checked.
func readBody(w http.ResponseWriter, req *http.Request, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if req.ContentLength > 0 {
		buf.Grow(int(min(req.ContentLength, firstBodyBuffer, limit)))
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, req.Body, limit))
	return buf.Bytes(), err
}

// signatureOf reads the one X-Hub-Signature-256 header, "sha256=" and 64 hex digits, into its MAC.
func signatureOf(headers []string) ([]byte, bool) {
	if len(headers) != 1 || !strings.HasPrefix(headers[0], signaturePrefix) {
		return nil, false
	}
	mac, err := hex.DecodeString(headers[0][len(signaturePrefix):])
	if err != nil || len(mac) != sha256.Size {
		return nil, false
	}
	return mac, true
}

// signed reports whether mac is the HMAC-SHA256 of body with one of the secrets.
func (r *Relay) signed(body, mac []byte) bool {
	ok := false
	for _, secret := range r.secrets {
		m := hmac.New(sha256.New, secret)
		m.Write(body)
		ok = hmac.Equal(m.Sum(nil), mac) || ok
	}
	return ok
}

// jsonOf is the payload's JSON: the body itself, or a form-encoded delivery's payload field (an App's hook
// can be set to deliver form-encoded payloads). A body that starts as a JSON object is JSON whatever its
// content type says. It is read only after the signature is checked.
func jsonOf(req *http.Request, body []byte) []byte {
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
		return body
	}
	mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return body
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil
	}
	return []byte(form.Get("payload"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
