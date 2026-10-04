package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A prompt blocked on a terminal returns when ctrl+c cancels the command's
// context, and the abandoned reader refuses later reads at once.
func TestPromptReadStopsOnCancel(t *testing.T) {
	pr, pw := io.Pipe() // nobody types
	defer pw.Close()
	p := newPromptIn(pr)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := p.line(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt ignored ctrl+c")
	}
	if _, err := p.key(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a later read on the abandoned reader: %v", err)
	}
	var w bytes.Buffer
	if p.confirm(context.Background(), &w, "go?") {
		t.Fatal("confirmed on an abandoned reader")
	}
}

func TestPromptLinesAndKeysShareOneBuffer(t *testing.T) {
	p := newPromptIn(bytes.NewBufferString("yes\nxreview9\nlast"))
	ctx := context.Background()
	var w bytes.Buffer
	if !p.confirm(ctx, &w, "go?") {
		t.Fatal("yes not confirmed")
	}
	if b, err := p.key(ctx); err != nil || b != 'x' {
		t.Fatalf("key %q %v", b, err)
	}
	if !p.confirmTyped(ctx, &w, "drop?", "review9") {
		t.Fatal("typed slug not confirmed")
	}
	if s, err := p.line(ctx); err != nil || s != "last" {
		t.Fatalf("last line without newline: %q %v", s, err)
	}
	if _, err := p.line(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF: %v", err)
	}
}

func TestReleaseConfirmationStopsOnCtrlC(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	pr, pw := io.Pipe()
	defer pw.Close()
	h.d.Stdin, h.d.StdinTTY, h.d.StdoutTTY = pr, true, true
	ctx, cancel := context.WithCancel(h.ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel() // what ctrl+c does to the command's signal context
	}()
	done := make(chan int, 1)
	go func() { done <- targetMain(ctx, h.c, h.d, targetKindByName("release"), "5", targetOpts{}) }()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("exit %d: %s", code, h.errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the release confirmation ignored ctrl+c")
	}
	if h.cleaner.applied != 0 {
		t.Fatal("released after ctrl+c")
	}
}
