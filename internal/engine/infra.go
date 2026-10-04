package engine

// Infrastructure failures: a fetch, clone, checkout or dependency step that
// failed for a reason no PR caused (an SSH key the agent lost, DNS, the
// network, TLS, a toolchain step failing the same way for several PRs). One
// such failure pauses dispatch for everything, toasts once and is never
// charged to the PR; a probe (`git ls-remote` of a watched clone's origin)
// lifts the pause once the outside world answers again.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// Infrastructure pause timing.
const (
	infraBackoffFirst = 2 * time.Minute  // the first pause, and the first probe
	infraBackoffMax   = 30 * time.Minute // a failing probe doubles the wait up to this
	// depsWindow: the same dependency-step error on two different PRs within
	// this window is an infrastructure failure, not the PRs'.
	depsWindow       = 10 * time.Minute
	infraToastWindow = 24 * time.Hour
	infraProbeTime   = 30 * time.Second
	infraDetailRunes = 300
)

// infraPatterns are error texts (lower case) of failures outside any PR, with
// the cause shown in the pause and the toast.
var infraPatterns = []struct{ match, cause string }{
	{"permission denied (publickey", "SSH key refused"},
	{"could not resolve host", "DNS lookup failed"},
	{"temporary failure in name resolution", "DNS lookup failed"},
	{"nodename nor servname provided", "DNS lookup failed"},
	{"connection timed out", "network timeout"},
	{"operation timed out", "network timeout"},
	{"connection refused", "connection refused"},
	{"connection reset by peer", "connection reset"},
	{"network is unreachable", "network unreachable"},
	{"no route to host", "network unreachable"},
	{"ssl certificate problem", "TLS failure"},
	{"server certificate verification failed", "TLS failure"},
	{"ssl_error", "TLS failure"},
	{"ssl_connect", "TLS failure"},
	{"gnutls_handshake", "TLS failure"},
	{"tls handshake", "TLS failure"},
}

// infraCause names the infrastructure failure err reports ("" = none of the
// known patterns: SSH key refused, DNS, network timeouts and refusals, TLS).
func infraCause(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	for _, p := range infraPatterns {
		if strings.Contains(msg, p.match) {
			return p.cause
		}
	}
	return ""
}

// depsFailure is the last dependency-step failure of a checkout.
type depsFailure struct {
	sig  string
	prID int64
	at   time.Time
}

// depsStepMarker is how a failed pool deps step reads (slots' runLogged label
// "deps <slot>: <script>").
const depsStepMarker = "slots: deps "

// depsSignature is a dependency-step failure with the slot's own name and
// path taken out (exit status and last stderr line when the command ran), so
// the same failure in two slots compares equal; "" when err is not one.
func depsSignature(err error, slotName, slotPath string) string {
	if err == nil || !strings.Contains(err.Error(), depsStepMarker) {
		return ""
	}
	sig := err.Error()
	if i := strings.Index(sig, depsStepMarker); i >= 0 {
		sig = sig[i:]
	}
	var xe *execx.ExitError
	if errors.As(err, &xe) {
		last := ""
		for l := range strings.Lines(execx.Redact(xe.Stderr)) {
			if t := strings.TrimSpace(l); t != "" {
				last = t
			}
		}
		sig = fmt.Sprintf("exit %d: %s", xe.Code, last)
	}
	for _, s := range []string{slotPath, slotName} {
		if s != "" {
			sig = strings.ReplaceAll(sig, s, "<slot>")
		}
	}
	return sig
}

// checkoutInfra reports the infrastructure cause of a failed checkout of
// prID ("" = the PR's own failure): a known pattern, or a dependency step
// failing exactly as it did for another PR within depsWindow.
func (e *Engine) checkoutInfra(err error, prID int64, slotName, slotPath string) string {
	if cause := infraCause(err); cause != "" {
		return cause
	}
	sig := depsSignature(err, slotName, slotPath)
	if sig == "" {
		return ""
	}
	now := e.now()
	e.infraMu.Lock()
	defer e.infraMu.Unlock()
	prev := e.depsFail
	e.depsFail = depsFailure{sig: sig, prID: prID, at: now}
	if prev.sig == sig && prev.prID != prID && now.Sub(prev.at) <= depsWindow {
		return "dependency step fails for every PR"
	}
	return ""
}

// infraPauseRec is the infrastructure pause as kv holds it.
type infraPauseRec struct {
	Until  time.Time // the next probe
	Reason string
	Detail string
}

func (e *Engine) infraPause(ctx context.Context) (infraPauseRec, bool) {
	until, ok := e.kvTime(ctx, KVInfraPausedUntil)
	if !ok {
		return infraPauseRec{}, false
	}
	reason, _ := e.getKV(ctx, KVInfraPausedReason)
	detail, _ := e.getKV(ctx, KVInfraPausedDetail)
	return infraPauseRec{Until: until, Reason: reason, Detail: detail}, true
}

// pauseInfra closes dispatch after an infrastructure failure and returns
// when its probe runs. A pause already in force is kept as is: concurrent
// failures pause once and toast once. probeDir is a clone whose origin the
// probe asks (""= any watched clone).
func (e *Engine) pauseInfra(ctx context.Context, cause string, err error, probeDir string) time.Time {
	e.infraMu.Lock()
	defer e.infraMu.Unlock()
	if p, ok := e.infraPause(ctx); ok {
		return p.Until
	}
	d := infraBackoffFirst
	if v, ok := e.getKV(ctx, KVInfraBackoff); ok {
		if prev, perr := time.ParseDuration(v); perr == nil && prev > 0 {
			d = min(prev*2, infraBackoffMax)
		}
	}
	until := e.now().Add(d)
	detail := clipRunes(execx.Redact(err.Error()), infraDetailRunes)
	e.setKV(ctx, KVInfraPausedUntil, store.FormatTime(until))
	e.setKV(ctx, KVInfraPausedReason, cause)
	e.setKV(ctx, KVInfraPausedDetail, detail)
	e.setKV(ctx, KVInfraBackoff, d.String())
	if probeDir != "" {
		e.setKV(ctx, kvInfraProbeDir, probeDir)
	}
	e.event(ctx, "warn", "", "infra.paused", fmt.Sprintf("dispatch paused (%s); probing from %s: %s", cause, until.Local().Format("15:04"), detail), nil)
	e.urgent("infra", "magnum: reviews paused ("+cause+")",
		detail+"\nNo PR is charged; magnum probes `git ls-remote` and resumes by itself (magnum status).", infraToastWindow)
	return until
}

// probeInfra runs the infrastructure probe once the pause's wait is over:
// success lifts the pause (and its toast's dedup record), failure waits
// twice as long (up to infraBackoffMax). Without a probe or a clone to
// probe the pause lifts when its wait ends, keeping the backoff, so a
// failure right after pauses longer.
func (e *Engine) probeInfra(ctx context.Context) {
	p, ok := e.infraPause(ctx)
	if !ok || e.now().Before(p.Until) {
		return
	}
	if e.d.DryRun {
		e.rec.Record(ctx, "infra", "probe", "infrastructure pause ("+p.Reason+") would be probed")
		return
	}
	dir := e.infraProbeDir(ctx)
	if e.d.Probe == nil || dir == "" {
		e.clearInfraPause(ctx, false)
		e.event(ctx, "info", "", "infra.resumed", "infrastructure pause ("+p.Reason+") ended (nothing to probe)", nil)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, infraProbeTime)
	err := e.d.Probe(pctx, dir)
	cancel()
	if err != nil {
		e.infraMu.Lock()
		d := infraBackoffFirst
		if v, ok := e.getKV(ctx, KVInfraBackoff); ok {
			if prev, perr := time.ParseDuration(v); perr == nil && prev > 0 {
				d = min(prev*2, infraBackoffMax)
			}
		}
		e.setKV(ctx, KVInfraBackoff, d.String())
		e.setKV(ctx, KVInfraPausedUntil, store.FormatTime(e.now().Add(d)))
		if cause := infraCause(err); cause != "" {
			e.setKV(ctx, KVInfraPausedReason, cause)
		}
		e.setKV(ctx, KVInfraPausedDetail, clipRunes(execx.Redact(err.Error()), infraDetailRunes))
		e.infraMu.Unlock()
		e.log.Info("infrastructure probe failed; dispatch stays paused", "dir", dir, "next", d, "err", err)
		return
	}
	e.clearInfraPause(ctx, true)
	e.event(ctx, "info", "", "infra.resumed", "infrastructure pause ("+p.Reason+") ended: "+dir+" reaches its origin again", nil)
}

// clearInfraPause lifts the infrastructure pause; recovered (a probe
// succeeded) also resets the backoff.
func (e *Engine) clearInfraPause(ctx context.Context, recovered bool) {
	e.infraMu.Lock()
	defer e.infraMu.Unlock()
	e.forgetSend(ctx, "infra")
	e.delKV(ctx, KVInfraPausedUntil, KVInfraPausedReason, KVInfraPausedDetail, kvInfraProbeDir)
	if recovered {
		e.delKV(ctx, KVInfraBackoff)
	}
}

// infraProbeDir is the clone the probe asks: the one whose failure paused
// dispatch, else a pool's main clone, else any watched repository's clone
// ("" = none exists).
func (e *Engine) infraProbeDir(ctx context.Context) string {
	var dirs []string
	if v, ok := e.getKV(ctx, kvInfraProbeDir); ok {
		dirs = append(dirs, v)
	}
	for _, p := range e.cfg.Pools {
		dirs = append(dirs, p.MainClone)
	}
	if repos, err := e.st.ListRepos(ctx); err == nil {
		for _, r := range repos {
			dirs = append(dirs, deref(r.ClonePath))
		}
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return ""
}

// gitProbe is the real Deps.Probe: `git ls-remote <origin> HEAD` in dir,
// reaching origin the way the daemon's fetches do (gitx NetworkRemote: a
// github.com SSH origin over HTTPS through gh), so a locked ssh-agent cannot
// keep an infrastructure pause alive after the network is back.
func gitProbe(g *gitx.Client) func(ctx context.Context, dir string) error {
	return func(ctx context.Context, dir string) error {
		_, err := g.LsRemote(ctx, dir, infraProbeTime, "HEAD")
		return err
	}
}

// clipRunes shortens s to at most n runes with a trailing ellipsis.
func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
