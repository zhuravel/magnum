package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (h *harness) deliverN(n int) {
	h.t.Helper()
	for i := 1; i <= n; i++ {
		if code := h.deliver("pull_request", fmt.Sprintf("d-%d", i), prBody(i, "opened")); code != http.StatusAccepted {
			h.t.Fatalf("delivery %d: %d", i, code)
		}
	}
}

func cursor(epoch string, seq int64) string { return epoch + ":" + strconv.FormatInt(seq, 10) }

func TestTheListingFollowsTheCursorInJournalOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(5)
	p := h.page("")
	if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("seqs from the start: %v", got)
	}
	if p.Epoch == "" || p.Next != cursor(p.Epoch, 5) || p.HighWater != cursor(p.Epoch, 5) || p.Oldest != cursor(p.Epoch, 0) {
		t.Fatalf("cursors: epoch %q next %q high %q oldest %q", p.Epoch, p.Next, p.HighWater, p.Oldest)
	}
	if p.ServerTime != h.clock.Now() {
		t.Fatalf("server_time %v, want %v", p.ServerTime, h.clock.Now())
	}
	e := p.Events[0]
	if e.DeliveryID != "d-1" || e.Event != "pull_request" || e.Action != "opened" || e.Repository != "talkable/example" ||
		e.RepositoryID != 2 || e.InstallationID != 1 || !reflect.DeepEqual(e.PRNumbers, []int{1}) || e.HeadSHA != headSHA ||
		e.BaseRef != "main" || e.Check != nil || !e.ReceivedAt.Equal(h.clock.Now()) {
		t.Fatalf("event: %+v", e)
	}
	p = h.page("after=" + url.QueryEscape(cursor(p.Epoch, 2)))
	if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{3, 4, 5}) {
		t.Fatalf("seqs after 2: %v", got)
	}
	p = h.page("after=" + url.QueryEscape(p.Next))
	if len(p.Events) != 0 || p.Next != cursor(p.Epoch, 5) {
		t.Fatalf("after the last: %+v", p)
	}
}

func TestTheListingAnswersRawJSONWithTheContractsFieldNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("check_run", "d-1", `{"action":"completed","check_run":{"name":"rspec","conclusion":"success","head_sha":"`+headSHA+`","pull_requests":[]},`+repoKey+`}`)
	code, body := h.get("", testToken)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"epoch", "events", "next", "high_water", "oldest", "server_time"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("answer lacks %q: %s", k, body)
		}
	}
	ev := raw["events"].([]any)[0].(map[string]any)
	var keys []string
	for k := range ev {
		keys = append(keys, k)
	}
	want := []string{"action", "base_ref", "check", "delivery_id", "event", "head_sha", "installation_id", "pr_numbers", "received_at", "repository", "repository_id", "seq"}
	slices.Sort(keys)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("event keys %v, want %v", keys, want)
	}
	if ev["received_at"] != "2026-10-20T12:00:00Z" {
		t.Fatalf("received_at %v", ev["received_at"])
	}
}

func TestTheListingTakesALimitFrom1To500(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(5)
	p := h.page("limit=2")
	if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{1, 2}) || p.Next != cursor(p.Epoch, 2) || p.HighWater != cursor(p.Epoch, 5) {
		t.Fatalf("limit 2: %v next %s high %s", got, p.Next, p.HighWater)
	}
	p = h.page("limit=2&after=" + url.QueryEscape(p.Next))
	if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatalf("second page: %v", got)
	}
	if p = h.page("limit=500"); len(p.Events) != 5 {
		t.Fatalf("limit 500: %d events", len(p.Events))
	}
	for _, q := range []string{"limit=0", "limit=501", "limit=-1", "limit=x", "wait=31s", "wait=31", "wait=-1s", "wait=-1", "wait=18446744074", "wait=99999999999999999999", "wait=forever", "after=nocolon", "after=e:-1", "after=e:x", "after=" + strings.Repeat("a", 200) + ":1"} {
		if code, body := h.get(q, testToken); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", q, code, body)
		}
	}
}

func TestTheDefaultLimitIs200(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 1; i <= 201; i++ {
		ev := Event{DeliveryID: fmt.Sprintf("d-%d", i), Event: "status", PRNumbers: []int{}, ReceivedAt: h.clock.Now()}
		if _, _, err := h.relay.j.insert(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if p := h.page(""); len(p.Events) != 200 || p.Next != cursor(p.Epoch, 200) {
		t.Fatalf("default page: %d events, next %s", len(p.Events), p.Next)
	}
}

func TestALongPollIsWokenByAnInsert(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(1)
	start := h.page("")
	done := make(chan Page, 1)
	go func() {
		code, body := h.get("wait=20s&after="+url.QueryEscape(start.Next), testToken)
		var p Page
		if code != http.StatusOK || json.Unmarshal([]byte(body), &p) != nil {
			t.Errorf("long poll: %d %s", code, body)
		}
		done <- p
	}()
	time.Sleep(150 * time.Millisecond)
	select {
	case p := <-done:
		t.Fatalf("the long poll answered before any insert: %+v", p)
	default:
	}
	began := time.Now()
	if code := h.deliver("pull_request", "d-2", prBody(2, "opened")); code != http.StatusAccepted {
		t.Fatalf("delivery: %d", code)
	}
	select {
	case p := <-done:
		if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{2}) {
			t.Fatalf("woken poll: %v", got)
		}
		if waited := time.Since(began); waited > 5*time.Second {
			t.Fatalf("the poll answered %v after the insert", waited)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll was not woken by the insert")
	}
}

func TestALongPollWithNothingNewAnswersEmptyWhenItsWaitRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(1)
	start := h.page("")
	began := time.Now()
	p := h.page("wait=300ms&after=" + url.QueryEscape(start.Next))
	waited := time.Since(began)
	if len(p.Events) != 0 || p.Next != start.Next {
		t.Fatalf("timed-out poll: %+v", p)
	}
	if waited < 250*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("the poll waited %v, want about 300ms", waited)
	}
	// Seconds without a unit are accepted as well, and an event already there answers at once.
	began = time.Now()
	if p = h.page("wait=30"); len(p.Events) != 1 || time.Since(began) > 5*time.Second {
		t.Fatalf("poll with an event waiting: %+v after %v", p, time.Since(began))
	}
}

func TestStopWaitingAnswersTheLongPollsAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	done := make(chan int, 1)
	go func() {
		code, _ := h.get("wait=30s", testToken)
		done <- code
	}()
	time.Sleep(150 * time.Millisecond)
	h.relay.StopWaiting()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("stopped poll: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopWaiting left the long poll waiting")
	}
}

func TestACursorOfAnotherEpochAnswers410(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(2)
	p := h.page("")
	code, body := h.get("after="+url.QueryEscape(cursor("0123456789abcdef0123456789abcdef", 1)), testToken)
	if code != http.StatusGone {
		t.Fatalf("foreign epoch: %d %s, want 410", code, body)
	}
	var gap map[string]string
	if err := json.Unmarshal([]byte(body), &gap); err != nil {
		t.Fatal(err)
	}
	if gap["error"] != "gap" || gap["epoch"] != p.Epoch || gap["oldest"] != cursor(p.Epoch, 0) {
		t.Fatalf("gap answer: %s", body)
	}
	// A cursor ahead of the journal (a restored file under the same epoch) is a gap too.
	if code, body := h.get("after="+url.QueryEscape(cursor(p.Epoch, 3)), testToken); code != http.StatusGone {
		t.Fatalf("cursor ahead of the journal: %d %s, want 410", code, body)
	}
}

func TestRetentionPrunesOldEventsAndAPrunedCursorAnswers410(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(3)
	h.clock.Add(10 * 24 * time.Hour)
	if code := h.deliver("pull_request", "d-4", prBody(4, "opened")); code != http.StatusAccepted {
		t.Fatal(code)
	}
	h.clock.Add(21 * 24 * time.Hour) // the first three are 31 days old, the fourth 21
	n, err := h.relay.Prune(t.Context())
	if err != nil || n != 3 {
		t.Fatalf("Prune: %d %v, want 3", n, err)
	}
	p := h.page("")
	if got := seqs(p.Events); !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("after the prune: %v", got)
	}
	if p.Oldest != cursor(p.Epoch, 3) || p.HighWater != cursor(p.Epoch, 4) {
		t.Fatalf("oldest %s high %s, want :3 and :4", p.Oldest, p.HighWater)
	}
	for _, seq := range []int64{0, 1, 2} {
		code, body := h.get("after="+url.QueryEscape(cursor(p.Epoch, seq)), testToken)
		if code != http.StatusGone || !strings.Contains(body, `"oldest":"`+cursor(p.Epoch, 3)+`"`) {
			t.Errorf("pruned cursor %d: %d %s, want 410 naming the oldest", seq, code, body)
		}
	}
	// A consumer that had read the pruned events lost nothing.
	if p := h.page("after=" + url.QueryEscape(cursor(p.Epoch, 3))); !reflect.DeepEqual(seqs(p.Events), []int64{4}) {
		t.Fatalf("after the oldest: %v", seqs(p.Events))
	}
	// Pruning everything keeps the high water: new events continue the sequence, and the old cursor still works.
	h.clock.Add(30 * 24 * time.Hour)
	if n, err := h.relay.Prune(t.Context()); err != nil || n != 1 {
		t.Fatalf("second Prune: %d %v", n, err)
	}
	p = h.page("")
	if len(p.Events) != 0 || p.Oldest != cursor(p.Epoch, 4) || p.Next != cursor(p.Epoch, 4) {
		t.Fatalf("empty journal: %+v", p)
	}
	h.deliver("pull_request", "d-5", prBody(5, "opened"))
	if p := h.page("after=" + url.QueryEscape(cursor(p.Epoch, 4))); !reflect.DeepEqual(seqs(p.Events), []int64{5}) {
		t.Fatalf("after a full prune: %v", seqs(p.Events))
	}
}

func TestTheRetentionDefaultsTo30Days(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(1)
	h.clock.Add(30*24*time.Hour - time.Second)
	if n, err := h.relay.Prune(t.Context()); err != nil || n != 0 {
		t.Fatalf("Prune before 30 days: %d %v", n, err)
	}
	h.clock.Add(2 * time.Second)
	if n, err := h.relay.Prune(t.Context()); err != nil || n != 1 {
		t.Fatalf("Prune after 30 days: %d %v", n, err)
	}
}

func TestTheJournalKeepsItsEpochAndSequenceAcrossARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(2)
	p := h.page("")
	h.relay.StopWaiting()
	h.srv.Close()
	if err := h.relay.Close(); err != nil {
		t.Fatal(err)
	}
	again := newHarness(t, func(o *Options) { o.DB = h.db })
	q := again.page("after=" + url.QueryEscape(cursor(p.Epoch, 1)))
	if q.Epoch != p.Epoch || !reflect.DeepEqual(seqs(q.Events), []int64{2}) {
		t.Fatalf("after a restart: epoch %s (was %s), %v", q.Epoch, p.Epoch, seqs(q.Events))
	}
	if code := again.deliver("pull_request", "d-1", prBody(1, "opened")); code != http.StatusOK {
		t.Fatalf("a delivery seen before the restart: %d, want 200", code)
	}
	// A new journal gets a new epoch, so a consumer's old cursor is a gap.
	fresh := newHarness(t)
	if code, _ := fresh.get("after="+url.QueryEscape(cursor(p.Epoch, 1)), testToken); code != http.StatusGone {
		t.Fatalf("an old cursor on a new journal: %d, want 410", code)
	}
}

func TestTheListingWantsAnAcceptedBearerToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) {
		o.TokenHashes = append(o.TokenHashes, tokenHash("second-relay-token-0123456789abcdef"))
	})
	h.deliverN(1)
	for name, token := range map[string]string{"missing": "", "wrong": "relay-pull-token-wrong", "a hash as the token": fmt.Sprintf("%x", tokenHash(testToken))} {
		code, body := h.get("", token)
		if code != http.StatusUnauthorized || strings.Contains(body, "talkable") {
			t.Errorf("%s token: %d %s, want 401 without events", name, code, body)
		}
	}
	for header, want := range map[string]int{
		"Basic " + testToken:         http.StatusUnauthorized,
		testToken:                    http.StatusUnauthorized,
		"Bearer  " + testToken:       http.StatusUnauthorized,
		"Bearer " + testToken + " x": http.StatusUnauthorized,
		"bearer " + testToken:        http.StatusOK,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", header)
		if code, _ := h.do(req); code != want {
			t.Errorf("Authorization %q: %d, want %d", strings.Replace(header, testToken, "<token>", 1), code, want)
		}
	}
	if code, _ := h.get("", "second-relay-token-0123456789abcdef"); code != http.StatusOK {
		t.Fatalf("second token: %d, want 200", code)
	}
}

func TestHealthzSaysTheJournalIsUpWithoutEventData(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	code, body := h.healthz(testToken)
	if code != http.StatusOK || body != `{"ok":true,"epoch":"`+h.relay.Epoch()+`","events":0,"last_received_at":null}`+"\n" {
		t.Fatalf("empty journal: %d %s", code, body)
	}
	h.deliverN(2)
	h.clock.Add(time.Minute)
	h.deliver("pull_request", "d-3", prBody(3, "opened"))
	code, body = h.healthz(testToken)
	if code != http.StatusOK || body != `{"ok":true,"epoch":"`+h.relay.Epoch()+`","events":3,"last_received_at":"2026-10-20T12:01:00Z"}`+"\n" {
		t.Fatalf("three events: %d %s", code, body)
	}
	h.clock.Add(healthEvery) // past the snapshot, so the next check reads the journal
	if err := h.relay.j.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", testToken} {
		if code, body = h.healthz(token); code != http.StatusServiceUnavailable || body != `{"ok":false}`+"\n" {
			t.Fatalf("closed journal: %d %s", code, body)
		}
	}
}

// Anyone may ask whether the relay is up (the container's HEALTHCHECK sends no token); how many events it
// keeps, since when and its epoch are for the token's holder.
func TestHealthzHidesItsCountersWithoutTheToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(2)
	h.clock.Add(healthEvery)
	for name, header := range map[string]string{
		"no token":           "",
		"a wrong token":      "Bearer relay-pull-token-wrong-0123456789abcdef",
		"a malformed token":  "Bearer x",
		"the token as Basic": "Basic " + testToken,
		"the token alone":    testToken,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if code, body := h.do(req); code != http.StatusOK || body != `{"ok":true}`+"\n" {
			t.Errorf("%s: %d %s, want 200 and ok alone", name, code, body)
		}
	}
	code, body := h.healthz(testToken)
	if code != http.StatusOK || body != `{"ok":true,"epoch":"`+h.relay.Epoch()+`","events":2,"last_received_at":"2026-10-20T12:00:00Z"}`+"\n" {
		t.Fatalf("with the token: %d %s, want the counters", code, body)
	}
}

func TestOpenRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := Options{DB: dir + "/relay.db", Secrets: [][]byte{[]byte(testSecret)}, TokenHashes: [][32]byte{tokenHash(testToken)}}
	for name, edit := range map[string]func(*Options){
		"no database":     func(o *Options) { o.DB = "" },
		"a query in path": func(o *Options) { o.DB = dir + "/relay.db?mode=memory" },
		"no secret":       func(o *Options) { o.Secrets = nil },
		"an empty secret": func(o *Options) { o.Secrets = [][]byte{{}} },
		"no token":        func(o *Options) { o.TokenHashes = nil },
		"short retention": func(o *Options) { o.Retention = time.Minute },
	} {
		o := good
		edit(&o)
		if r, err := Open(t.Context(), o); err == nil {
			r.Close()
			t.Errorf("%s: Open succeeded", name)
		}
	}
}
