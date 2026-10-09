package tui

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A reload still in flight when the picker closes stops with it, as the
// other screens' loads do: RunPicker ends the context it hands Reload.
func TestAReloadStopsWhenThePickerCloses(t *testing.T) {
	w := scriptedInput(t)

	started, stopped := make(chan struct{}), make(chan struct{})
	var once, onceStopped sync.Once // a second ctrl+r may start a second reload
	reload := func(ctx context.Context) ([]PickEntry, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		onceStopped.Do(func() { close(stopped) })
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunPicker(context.Background(), pickEntries(), PickerOptions{Reload: reload})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for asked := false; !asked; {
		if _, err := w.Write([]byte("\x12")); err != nil { // ctrl+r, once the program reads
			t.Fatal(err)
		}
		select {
		case <-started:
			asked = true
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("ctrl+r started no reload")
		}
	}
	if _, err := w.Write([]byte("\x03")); err != nil { // ctrl+c
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPicker = %v", err)
		}
	case <-deadline:
		t.Fatal("RunPicker did not return")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the reload ran on after the picker closed")
	}
}
