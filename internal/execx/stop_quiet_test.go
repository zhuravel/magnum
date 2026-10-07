package execx

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// A command ended because the caller's context was canceled (the daemon is
// stopping) is not a failure worth a warning: its "signal: terminated" line
// was half of the log's warnings at every stop. A command that outlived its own
// timeout, one that outlived the caller's deadline and one that fails on its
// own still warn.
func TestCanceledCommandLogsAtDebugButTimeoutsAndFailuresStillWarn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		run     func(r *Real) error
		want    slog.Level
		wantErr error
	}{
		{"canceled by the caller", func(r *Real) error {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			_, err := r.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
			return err
		}, slog.LevelDebug, context.Canceled},
		{"canceled before it started", func(r *Real) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := r.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
			return err
		}, slog.LevelDebug, context.Canceled},
		{"its own timeout", func(r *Real) error {
			_, err := r.Run(context.Background(), Cmd{Name: "sleep", Args: []string{"30"}, Timeout: 200 * time.Millisecond})
			return err
		}, slog.LevelWarn, context.DeadlineExceeded},
		{"the caller's deadline", func(r *Real) error {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := r.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
			return err
		}, slog.LevelWarn, context.DeadlineExceeded},
		{"a failure of its own", func(r *Real) error {
			_, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 4"}})
			return err
		}, slog.LevelWarn, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []slog.Level
			r := &Real{Log: levelRecorder(func(l slog.Level) { got = append(got, l) })}
			err := tc.run(r)
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("levels %v, want [%v]", got, tc.want)
			}
		})
	}
}
