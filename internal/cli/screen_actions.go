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
	"github.com/zhuravel/magnum/internal/tui"
)

// screenActions runs the keys of the dashboard and the PR board through the
// act commands' code paths (reviewMain, openMain, targetMain, stopMain, attentionMain). What they print is
// captured and returned as text: nothing may write to the terminal while the
// screen owns it. The act dependencies are built on the first action.
type screenActions struct {
	mu   sync.Mutex // one action at a time
	c    *Context   // the caller's Context printing into all and errs
	all  actCapture // stdout and stderr, in order
	errs actCapture // stderr only
	d    *actDeps
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
	s.d = d
	return d, nil
}

// do runs one command body. Exit 0 returns everything it printed (the
// dashboard shows the last line); a failure returns its last stderr line,
// without the "magnum <cmd>: " prefix, as the error.
func (s *screenActions) do(ctx context.Context, cmd string, body func(ctx context.Context, c *Context, d *actDeps) int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deps()
	if err != nil {
		return "", err
	}
	s.all.take()
	s.errs.take()
	code := body(ctx, s.c, d)
	all, errs := s.all.take(), s.errs.take()
	if code == 0 {
		return strings.TrimSpace(all), nil
	}
	msg := lastLine(errs)
	if msg == "" {
		msg = lastLine(all)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", code)
	}
	return "", errors.New(strings.TrimPrefix(msg, "magnum "+cmd+": "))
}

func (s *screenActions) Open(ctx context.Context, ref string) (string, error) {
	return s.do(ctx, "open", func(ctx context.Context, c *Context, d *actDeps) int {
		return openMain(ctx, c, d, ref, openOpts{timeout: 5 * time.Minute})
	})
}

func (s *screenActions) Review(ctx context.Context, ref string, o tui.ReviewOpts) (string, error) {
	return s.do(ctx, "review", func(ctx context.Context, c *Context, d *actDeps) int {
		return reviewMain(ctx, c, d, ref, reviewOpts{fresh: o.Fresh, simplify: o.Simplify})
	})
}

// target runs pin, unpin, release, mute or unmute. The dashboard asked
// before a release, so release does not ask again.
func (s *screenActions) target(ctx context.Context, name, ref string) (string, error) {
	k := targetKindByName(name)
	return s.do(ctx, name, func(ctx context.Context, c *Context, d *actDeps) int {
		return targetMain(ctx, c, d, k, ref, targetOpts{yes: true})
	})
}

func (s *screenActions) Pin(ctx context.Context, ref string) (string, error) {
	return s.target(ctx, "pin", ref)
}

func (s *screenActions) Unpin(ctx context.Context, ref string) (string, error) {
	return s.target(ctx, "unpin", ref)
}

func (s *screenActions) Release(ctx context.Context, ref string) (string, error) {
	return s.target(ctx, "release", ref)
}

func (s *screenActions) Mute(ctx context.Context, ref string) (string, error) {
	return s.target(ctx, "mute", ref)
}

func (s *screenActions) Unmute(ctx context.Context, ref string) (string, error) {
	return s.target(ctx, "unmute", ref)
}

// Approve posts the reviewer's APPROVE on the reviewed head; the screen
// asked first.
func (s *screenActions) Approve(ctx context.Context, ref string) (string, error) {
	return s.do(ctx, "approve", func(ctx context.Context, c *Context, d *actDeps) int {
		return verdictMain(ctx, c, d, "approve", engine.ReqApprove, ref, verdictOpts{})
	})
}

// RequestChanges posts the reviewer's REQUEST_CHANGES on the reviewed head;
// the screen asked first.
func (s *screenActions) RequestChanges(ctx context.Context, ref string) (string, error) {
	return s.do(ctx, "request-changes", func(ctx context.Context, c *Context, d *actDeps) int {
		return verdictMain(ctx, c, d, "request-changes", engine.ReqRequestChanges, ref, verdictOpts{})
	})
}

// Abort kills the PR's running review; the screen asked first.
func (s *screenActions) Abort(ctx context.Context, ref string) (string, error) {
	return s.stop(ctx, stopAbort, ref)
}

// Ignore aborts, mutes and frees the PR; the screen asked first.
func (s *screenActions) Ignore(ctx context.Context, ref string) (string, error) {
	return s.stop(ctx, stopIgnore, ref)
}

func (s *screenActions) stop(ctx context.Context, k stopKind, ref string) (string, error) {
	return s.do(ctx, k.name, func(ctx context.Context, c *Context, d *actDeps) int {
		return stopMain(ctx, c, d, k, ref, stopOpts{timeout: stopTimeout})
	})
}

// Attention focuses the pane that needs the user. "nothing needs you" (exit
// 1 for the herdr plugin's toast) is news here, not a failure.
func (s *screenActions) Attention(ctx context.Context) (string, error) {
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
