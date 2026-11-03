package registry_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

func TestRequestsCompleteOnce(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	first, err := r.EnqueueRequest(ctx, "start", map[string]string{"text": "PR-12345 only the refund path"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.EnqueueRequest(ctx, "answer", nil)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := r.PendingRequests(ctx, 0)
	if err != nil || len(pending) != 2 || pending[0].ID != first || pending[1].ID != second {
		t.Fatalf("PendingRequests = %+v, %v; want both, oldest first", pending, err)
	}
	if pending[0].Kind != "start" || pending[0].State != registry.RequestPending || !pending[0].Created.Equal(t0) {
		t.Errorf("first request = %+v", pending[0])
	}
	var payload map[string]string
	if err := json.Unmarshal(pending[0].Payload, &payload); err != nil || payload["text"] != "PR-12345 only the refund path" {
		t.Errorf("payload %s, %v", pending[0].Payload, err)
	}
	if string(pending[1].Payload) != "{}" {
		t.Errorf("a nil payload is stored as %s, want {}", pending[1].Payload)
	}
	if limited, _ := r.PendingRequests(ctx, 1); len(limited) != 1 || limited[0].ID != first {
		t.Errorf("PendingRequests(1) = %+v", limited)
	}

	clk.Add(time.Minute)
	if err := r.CompleteRequest(ctx, first, registry.RequestDone, "task 7"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{registry.RequestDone, registry.RequestFailed} {
		if err := r.CompleteRequest(ctx, first, state, "again"); !errors.Is(err, registry.ErrConflict) {
			t.Errorf("completing a request twice (%s) = %v, want ErrConflict", state, err)
		}
	}
	got, err := r.RequestByID(ctx, first)
	if err != nil || got.State != registry.RequestDone || got.Result != "task 7" || !got.Handled.Equal(t0.Add(time.Minute)) {
		t.Errorf("RequestByID = %+v, %v", got, err)
	}
	if pending, _ := r.PendingRequests(ctx, 0); len(pending) != 1 || pending[0].ID != second {
		t.Errorf("PendingRequests after one completed = %+v", pending)
	}
}

func TestRequestsRefuseWhatTheDaemonCannotRead(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	if _, err := r.EnqueueRequest(ctx, "", nil); err == nil {
		t.Error("EnqueueRequest without a kind succeeded")
	}
	if _, err := r.EnqueueRequest(ctx, "start", make(chan int)); err == nil {
		t.Error("EnqueueRequest with an unmarshalable payload succeeded")
	}
	id, _ := r.EnqueueRequest(ctx, "start", nil)
	if err := r.CompleteRequest(ctx, id, registry.RequestPending, ""); err == nil {
		t.Error("CompleteRequest to pending succeeded")
	}
	if err := r.CompleteRequest(ctx, id+100, registry.RequestDone, ""); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("CompleteRequest of a missing request = %v, want ErrConflict (as magnum's)", err)
	}
	if _, err := r.RequestByID(ctx, id+100); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RequestByID of a missing request = %v, want ErrNotFound", err)
	}
}
