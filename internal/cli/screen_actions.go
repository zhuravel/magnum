package cli

// The actions the status dashboard and the PR board share: both screens'
// keys run the act commands' own code paths.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenActions runs the keys of the dashboard and the PR board through the
// act commands' code paths (reviewMain, openMain, targetMain, stopMain, attentionMain). What they print is
// captured and returned as text, with the daemon requests they queued:
// nothing may write to the terminal while the screen owns it. The act
// dependencies are built on the first action.
type screenActions struct {
	mu   sync.Mutex // one action at a time
	c    *Context   // the caller's Context printing into all and errs
	all  actCapture // stdout and stderr, in order
	errs actCapture // stderr only
	d    *actDeps
	sent []tui.Request // the requests the action in flight queued

	stMu sync.Mutex
	st   *store.Store // the registry the actions queue into, once built
}

var _ tui.DashboardActions = (*screenActions)(nil)

func newScreenActions(c *Context) *screenActions {
	s := &screenActions{}
	cc := *c
	cc.Stdout, cc.Stderr = &s.all, io.MultiWriter(&s.all, &s.errs)
	s.c = &cc
	return s
}

// Close releases the act dependencies.
func (s *screenActions) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d == nil {
		return nil
	}
	return s.d.Close()
}

func (s *screenActions) deps() (*actDeps, error) {
	if s.d != nil {
		return s.d, nil
	}
	d, err := actNewDeps(s.c, actFull)
	if err != nil {
		return nil, err
	}
	// The screen owns the terminal: no prompt, confirmation or key wait.
	d.Stdin, d.in, d.StdinTTY, d.StdoutTTY = strings.NewReader(""), nil, false, false
	d.sent = func(o reqOutcome) { s.sent = append(s.sent, screenRequest(o.Req)) } // under s.mu, as the action runs
	s.d = d
	s.stMu.Lock()
	s.st = d.Store
	s.stMu.Unlock()
	return d, nil
}

// screenRequest is a request as the screens follow it.
func screenRequest(q store.Request) tui.Request {
	return tui.Request{ID: q.ID, Kind: q.Kind, State: q.State, Result: strings.TrimSpace(store.Deref(q.Result))}
}

// Requests re-reads the requests the screens follow. It does not wait for
// the action in flight: that one may wait half a minute for its own answer.
func (s *screenActions) Requests(ctx context.Context, ids []int64) ([]tui.Request, error) {
	s.stMu.Lock()
	st := s.st
	s.stMu.Unlock()
	switch {
	case len(ids) == 0:
		return nil, nil
	case st == nil: // no action opened the registry: say so rather than call the requests gone
		return nil, errors.New("the registry is not open yet")
	}
	out := make([]tui.Request, 0, len(ids))
	for _, id := range ids {
		q, err := st.RequestByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return out, err
		}
		out = append(out, screenRequest(q))
	}
	return out, nil
}

// do runs one command body and returns everything it printed (the footer
// shows the last line, the action log all of it) with the requests it
// queued; a failure's error is its last stderr line, without the
// "magnum <cmd>: " prefix.
func (s *screenActions) do(ctx context.Context, cmd string, body func(ctx context.Context, c *Context, d *actDeps) int) (tui.ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deps()
	if err != nil {
		return tui.ActionResult{}, err
	}
	s.all.take()
	s.errs.take()
	s.sent = nil
	code := body(ctx, s.c, d)
	all, errs := s.all.take(), s.errs.take()
	res := tui.ActionResult{Text: strings.TrimSpace(all), Requests: s.sent}
	s.sent = nil
	if code == 0 {
		return res, nil
	}
	msg := lastLine(errs)
	if msg == "" {
		msg = lastLine(all)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", code)
	}
	return res, errors.New(strings.TrimPrefix(msg, "magnum "+cmd+": "))
}

func (s *screenActions) Open(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.do(ctx, "open", func(ctx context.Context, c *Context, d *actDeps) int {
		return openMain(ctx, c, d, ref, openOpts{timeout: 5 * time.Minute})
	})
}

func (s *screenActions) Review(ctx context.Context, ref string, o tui.ReviewOpts) (tui.ActionResult, error) {
	return s.do(ctx, "review", func(ctx context.Context, c *Context, d *actDeps) int {
		return reviewMain(ctx, c, d, ref, reviewOpts{fresh: o.Fresh, simplify: o.Simplify})
	})
}

// target runs pin, unpin, release, mute or unmute. The dashboard asked
// before a release, so release does not ask again.
func (s *screenActions) target(ctx context.Context, name, ref string) (tui.ActionResult, error) {
	k := targetKindByName(name)
	return s.do(ctx, name, func(ctx context.Context, c *Context, d *actDeps) int {
		return targetMain(ctx, c, d, k, ref, targetOpts{yes: true})
	})
}

func (s *screenActions) Pin(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.target(ctx, "pin", ref)
}

func (s *screenActions) Unpin(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.target(ctx, "unpin", ref)
}

func (s *screenActions) Release(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.target(ctx, "release", ref)
}

func (s *screenActions) Mute(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.target(ctx, "mute", ref)
}

func (s *screenActions) Unmute(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.target(ctx, "unmute", ref)
}

// Approve posts the reviewer's APPROVE on the reviewed head; the screen
// asked first.
func (s *screenActions) Approve(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.do(ctx, "approve", func(ctx context.Context, c *Context, d *actDeps) int {
		return verdictMain(ctx, c, d, "approve", engine.ReqApprove, ref, verdictOpts{})
	})
}

// RequestChanges posts the reviewer's REQUEST_CHANGES on the reviewed head;
// the screen asked first.
func (s *screenActions) RequestChanges(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.do(ctx, "request-changes", func(ctx context.Context, c *Context, d *actDeps) int {
		return verdictMain(ctx, c, d, "request-changes", engine.ReqRequestChanges, ref, verdictOpts{})
	})
}

// Abort kills the PR's running review; the screen asked first.
func (s *screenActions) Abort(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.stop(ctx, stopAbort, ref)
}

// Ignore aborts, mutes and frees the PR; the screen asked first.
func (s *screenActions) Ignore(ctx context.Context, ref string) (tui.ActionResult, error) {
	return s.stop(ctx, stopIgnore, ref)
}

func (s *screenActions) stop(ctx context.Context, k stopKind, ref string) (tui.ActionResult, error) {
	return s.do(ctx, k.name, func(ctx context.Context, c *Context, d *actDeps) int {
		return stopMain(ctx, c, d, k, ref, stopOpts{timeout: stopTimeout})
	})
}

// Attention focuses the pane that needs the user. "nothing needs you" (exit
// 1 for the herdr plugin's toast) is news here, not a failure.
func (s *screenActions) Attention(ctx context.Context) (tui.ActionResult, error) {
	return s.do(ctx, "attention", func(ctx context.Context, c *Context, d *actDeps) int {
		code := attentionMain(ctx, c, d, attentionOpts{})
		if code == 1 && s.errs.empty() {
			return 0
		}
		return code
	})
}

func (s *screenActions) OpenBrowser(ctx context.Context, url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deps()
	if err != nil {
		return err
	}
	return actOpenURL(ctx, d, url)
}
