package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

type fakeIdentity struct {
	name, kind, login string
	rep               identity.Report
	err               error
	checked           int
}

func (f *fakeIdentity) Name() string  { return f.name }
func (f *fakeIdentity) Login() string { return f.login }
func (f *fakeIdentity) Kind() string  { return f.kind }
func (f *fakeIdentity) Env(context.Context) (map[string]string, error) {
	return nil, nil
}
func (f *fakeIdentity) Check(context.Context) (identity.Report, error) {
	f.checked++
	return f.rep, f.err
}

func identitiesFixture() (*fakeIdentity, *fakeIdentity) {
	gh := &fakeIdentity{name: "zhuravel", kind: "gh", login: "zhuravel",
		rep: identity.Report{Pass: true, Lines: []string{"PASS gh api user: zhuravel"}}}
	appID := &fakeIdentity{name: "talkable-app", kind: "app", login: "talkable[bot]",
		rep: identity.Report{Pass: false, Lines: []string{"PASS GET /app", "FAIL pull_requests permission is read",
			"     fix: grant Pull requests: read & write and accept it on installation 2"}}}
	return gh, appID
}

func TestIdentitiesCheckRecordsVerdicts(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	gh, appID := identitiesFixture()
	if err := st.SetKV(ctx, engine.KVIdentityCheck("zhuravel"), "fail"); err != nil {
		t.Fatal(err)
	}
	// The daemon toasted zhuravel's failure; the toast is deduplicated.
	if ok, err := st.ShouldSend(ctx, "identity:zhuravel", time.Hour); err != nil || !ok {
		t.Fatal("seed toast")
	}
	// The app's failure toast stays deduplicated while it keeps failing.
	if ok, err := st.ShouldSend(ctx, "identity:talkable-app", time.Hour); err != nil || !ok {
		t.Fatal("seed toast")
	}
	oldKick := identitiesKick
	identitiesKick = func(paths.Layout) (int, error) {
		return 0, errors.New("pid 9 in daemon.pid is \"vim\", not a magnum daemon")
	}
	t.Cleanup(func() { identitiesKick = oldKick })
	code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh, appID}, "", false)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	out := f.Out.String()
	for _, want := range []string{"== zhuravel (gh, zhuravel)", "PASS gh api user", "== talkable-app (app, talkable[bot])",
		"FAIL pull_requests permission is read", "fix: grant Pull requests", "1 of 2 identities failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if v, _, _ := st.GetKV(ctx, engine.KVIdentityCheck("zhuravel")); v != "pass" {
		t.Fatalf("zhuravel kv = %q", v)
	}
	if v, _, _ := st.GetKV(ctx, engine.KVIdentityCheck("talkable-app")); v != "fail" {
		t.Fatalf("app kv = %q", v)
	}
	if v, _, _ := st.GetKV(ctx, engine.KVIdentityError("talkable-app")); v != "pull_requests permission is read" {
		t.Fatalf("app error kv = %q", v)
	}
	evs, _ := st.EventsBySubject(ctx, "identity:talkable-app", 0)
	if len(evs) != 1 || evs[0].Kind != "identity.checked" {
		t.Fatalf("events = %+v", evs)
	}
	// fail → pass forgets the toast, so the next failure notifies at once.
	if ok, err := st.ShouldSend(ctx, "identity:zhuravel", time.Hour); err != nil || !ok {
		t.Errorf("zhuravel's toast dedup not reset: %v %v", ok, err)
	}
	if ok, err := st.ShouldSend(ctx, "identity:talkable-app", time.Hour); err != nil || ok {
		t.Errorf("a failing identity's toast dedup was reset: %v %v", ok, err)
	}
	if !strings.Contains(f.Err.String(), "not a magnum daemon") {
		t.Errorf("the refused kick is not reported: %s", f.Err.String())
	}
}

func TestIdentitiesCheckNameAndJSON(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	gh, appID := identitiesFixture()
	appID.err = errors.New("network down")
	if code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh, appID}, "zhuravel", true); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if appID.checked != 0 {
		t.Fatal("--name must check only that identity")
	}
	var rows []identitiesResult
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil || len(rows) != 1 || !rows[0].Pass || rows[0].Name != "zhuravel" {
		t.Fatalf("json %s err %v", f.Out.String(), err)
	}
	f.Out.Reset()
	if code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh, appID}, "talkable-app", false); code != 1 ||
		!strings.Contains(f.Out.String(), "could not finish: network down") {
		t.Fatalf("code %d out %s", code, f.Out.String())
	}
	if v, _, _ := st.GetKV(ctx, engine.KVIdentityError("talkable-app")); v != "network down" {
		t.Fatalf("error kv = %q", v)
	}
	if code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh}, "nobody", false); code != 1 ||
		!strings.Contains(f.Err.String(), "configured: zhuravel") {
		t.Fatalf("unknown name code %d err %s", code, f.Err.String())
	}
}

func TestIdentitiesCommand(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	if err := st.SetKV(context.Background(), engine.KVIdentityCheck("talkable-app"), "fail"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code := f.run("identities"); code != 0 {
		t.Fatalf("list code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	for _, want := range []string{"NAME", "zhuravel", "talkable-app", "talkable[bot]", "talkable/talkable", "fail"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if code := f.run("identities", "bogus"); code != 2 {
		t.Fatalf("bogus code %d", code)
	}
	_ = store.RequestDone
}

// While a daemon runs, the verdicts go to it (engine.ReqIdentityVerdict)
// instead of being written by the CLI next to the daemon's own checks.
func TestIdentitiesCheckHandsVerdictsToTheDaemon(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	gh, appID := identitiesFixture()
	oldPID, oldKick := identitiesDaemonPID, identitiesKick
	t.Cleanup(func() { identitiesDaemonPID, identitiesKick = oldPID, oldKick })
	identitiesDaemonPID = func(paths.Layout) (int, error) { return 4242, nil }
	identitiesKick = func(paths.Layout) (int, error) { return 4242, nil }

	// The daemon: completes each verdict request (its handler records it).
	stop := make(chan struct{})
	daemonDone := make(chan []engine.IdentityVerdictPayload)
	go func() {
		var got []engine.IdentityVerdictPayload
		defer func() { daemonDone <- got }()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			reqs, err := st.PendingRequests(ctx, 10)
			if err != nil {
				return
			}
			for _, r := range reqs {
				var p engine.IdentityVerdictPayload
				if r.Kind != engine.ReqIdentityVerdict || json.Unmarshal(r.Payload, &p) != nil {
					continue
				}
				got = append(got, p)
				_ = st.CompleteRequest(ctx, r.ID, store.RequestDone, "identities check: recorded")
			}
		}
	}()
	code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh, appID}, "", false)
	close(stop)
	got := <-daemonDone
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	want := []engine.IdentityVerdictPayload{{Name: "zhuravel", Pass: true}, {Name: "talkable-app", Reason: "pull_requests permission is read"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("verdicts handed to the daemon = %+v, want %+v", got, want)
	}
	if v, ok, _ := st.GetKV(ctx, engine.KVIdentityCheck("zhuravel")); ok {
		t.Fatalf("the CLI wrote the verdict itself (%q) while a daemon runs", v)
	}
	if !strings.Contains(f.Out.String(), "(the daemon, pid 4242, recorded the new verdicts)") {
		t.Fatalf("out:\n%s", f.Out.String())
	}
}

// A pidfile whose process is not a daemon: nobody takes the requests, so they
// are withdrawn and the CLI records the verdicts itself.
func TestIdentitiesCheckRecordsItselfWhenNoDaemonAnswers(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	gh, _ := identitiesFixture()
	oldPID, oldKick := identitiesDaemonPID, identitiesKick
	t.Cleanup(func() { identitiesDaemonPID, identitiesKick = oldPID, oldKick })
	identitiesDaemonPID = func(paths.Layout) (int, error) { return 4242, nil }
	identitiesKick = func(paths.Layout) (int, error) {
		return 0, errors.New("pid 4242 in daemon.pid is \"vim\", not a magnum daemon")
	}
	if code := identitiesCheck(ctx, f.Ctx, st, []identity.Source{gh}, "", false); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if v, _, _ := st.GetKV(ctx, engine.KVIdentityCheck("zhuravel")); v != "pass" {
		t.Fatalf("kv = %q, want the CLI's own record", v)
	}
	if reqs, err := st.PendingRequests(ctx, 10); err != nil || len(reqs) != 0 {
		t.Fatalf("requests left for a later daemon to replay: %+v %v", reqs, err)
	}
}
