package tui

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"
)

// slowWidths is a ColumnWidths whose SaveWidths waits for release, so a
// test can hold a save in the middle of its store write.
type slowWidths struct {
	started chan struct{} // a save reached the store
	release chan struct{} // let the saves finish

	mu    sync.Mutex
	saved []map[string]int // in the order the store wrote them
}

func (s *slowWidths) LoadWidths(context.Context, string) (map[string]int, error) { return nil, nil }

func (s *slowWidths) SaveWidths(_ context.Context, _ string, w map[string]int) error {
	s.started <- struct{}{}
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, maps.Clone(w))
	return nil
}

// A drag that ends while an earlier save is still writing to the store
// numbers its save at once: the event loop never waits for a slow store
// write. The saves still write one at a time, and one older than the last
// written is skipped.
func TestASlowWidthSaveNeverHoldsUpTheNextOne(t *testing.T) {
	store := &slowWidths{started: make(chan struct{}, 4), release: make(chan struct{})}
	s := newWidthSaver(store, widthsBoard)
	first := s.save(context.Background(), map[string]int{"title": 30})
	firstDone := make(chan struct{})
	go func() { first(); close(firstDone) }()
	select {
	case <-store.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first save never reached the store")
	}

	made := make(chan func(), 1)
	go func() {
		cmd := s.save(context.Background(), map[string]int{"title": 40})
		made <- func() { cmd() }
	}()
	var second func()
	select {
	case second = <-made:
	case <-time.After(2 * time.Second):
		close(store.release)
		t.Fatal("save waited for the store write of the save before it")
	}
	newest := s.save(context.Background(), map[string]int{"title": 50})
	close(store.release)
	<-firstDone
	newest() // the newest: written
	second() // older than the one written: skipped
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saved) != 2 || store.saved[0]["title"] != 30 || store.saved[1]["title"] != 50 {
		t.Fatalf("the store wrote %v, want the first save and the newest", store.saved)
	}
}
