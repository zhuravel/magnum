package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// noDaemonClient is a reqClient whose kick finds no daemon (after hook runs,
// when set: a daemon or the user acting while the kick looks).
func noDaemonClient(t *testing.T, hook func(st *store.Store)) (reqClient, *store.Store) {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	return reqClient{st: st, now: time.Now, version: "test",
		sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		kick: func() (int, error) {
			if hook != nil {
				hook(st)
			}
			return 0, nil
		}}, st
}

func onlyRequest(t *testing.T, st *store.Store) store.Request {
	t.Helper()
	r, err := st.RequestByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A NeedDaemon send no daemon answered withdraws its request and fails with
// errNoDaemon: nothing stays queued for a daemon started later.
func TestANeedDaemonSendWithoutADaemonWithdrawsItsRequest(t *testing.T) {
	rc, st := noDaemonClient(t, nil)
	out, err := rc.send(context.Background(), "review", nil, reqSend{NeedDaemon: true})
	if !errors.Is(err, errNoDaemon) || !out.Failed() {
		t.Fatalf("send = %+v, %v; want failed with errNoDaemon", out.Req, err)
	}
	if r := onlyRequest(t, st); r.State != store.RequestFailed {
		t.Fatalf("request left %s, want failed (withdrawn)", r.State)
	}
}

// A withdrawal the registry refuses is not reported as "nothing was
// queued": the request stays pending and a daemon started later runs it, so
// the send says so and does not wrap errNoDaemon.
func TestAFailedWithdrawalSaysTheRequestMayStillRun(t *testing.T) {
	rc, st := noDaemonClient(t, func(st *store.Store) {
		if _, err := st.DB().ExecContext(context.Background(), `CREATE TRIGGER fail_withdraw BEFORE UPDATE ON requests
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
			t.Fatal(err)
		}
	})
	out, err := rc.send(context.Background(), "review", nil, reqSend{NeedDaemon: true})
	if err == nil || errors.Is(err, errNoDaemon) || !strings.Contains(err.Error(), "request 1 may still run when a daemon starts: withdrawing it failed") ||
		!strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("send error = %v, want the request named as still able to run", err)
	}
	if !out.Pending() {
		t.Fatalf("outcome %+v, want pending", out.Req)
	}
	if r := onlyRequest(t, st); r.State != store.RequestPending {
		t.Fatalf("request %s, want still pending", r.State)
	}
}

// A daemon that took the request while the kick looked for it answers it:
// the send reports that answer, not a withdrawal.
func TestAWithdrawalADaemonBeatReportsItsAnswer(t *testing.T) {
	rc, st := noDaemonClient(t, func(st *store.Store) {
		if err := st.CompleteRequest(context.Background(), 1, store.RequestDone, "reviewed"); err != nil {
			t.Fatal(err)
		}
	})
	out, err := rc.send(context.Background(), "review", nil, reqSend{NeedDaemon: true})
	if err != nil || out.Req.State != store.RequestDone || out.Result() != "reviewed" {
		t.Fatalf("send = %+v %q, %v; want the daemon's answer", out.Req, out.Result(), err)
	}
	if r := onlyRequest(t, st); r.State != store.RequestDone {
		t.Fatalf("request %s, want done", r.State)
	}
}

// A send interrupted while it looked for the daemon (ctrl-c) still
// withdraws its request: the withdrawal does not run on the ended context.
func TestAnInterruptedNeedDaemonSendStillWithdraws(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, st := noDaemonClient(t, func(*store.Store) { cancel() })
	_, err := rc.send(ctx, "review", nil, reqSend{NeedDaemon: true})
	if !errors.Is(err, errNoDaemon) {
		t.Fatalf("send error = %v, want errNoDaemon", err)
	}
	if r := onlyRequest(t, st); r.State != store.RequestFailed {
		t.Fatalf("request left %s, want withdrawn", r.State)
	}
}
