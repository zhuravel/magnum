package relay

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASignedDeliveryIsStoredAndAnsweredAccepted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := prBody(45, "synchronize")
	if code := h.deliver("pull_request", "d-1", body); code != http.StatusAccepted {
		t.Fatalf("signed delivery: %d, want 202", code)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("rows: %d, want 1", n)
	}
}

func TestTheSignatureCoversTheExactBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(prBody(45, "opened"))
	sig := sign(testSecret, body)
	// The same JSON with one more space is another body: its signature does not match.
	reformatted := bytes.Replace(body, []byte(`"action":`), []byte(`"action": `), 1)
	if code, _ := h.post("pull_request", "d-1", reformatted, sig); code != http.StatusUnauthorized {
		t.Fatalf("reformatted body under the original signature: %d, want 401", code)
	}
	trailing := append(append([]byte{}, body...), '\n')
	if code, _ := h.post("pull_request", "d-2", trailing, sig); code != http.StatusUnauthorized {
		t.Fatalf("body with a trailing newline under the original signature: %d, want 401", code)
	}
	if n := h.count(); n != 0 {
		t.Fatalf("rows after refused deliveries: %d, want 0", n)
	}
	if code, _ := h.post("pull_request", "d-3", body, sig); code != http.StatusAccepted {
		t.Fatalf("exact body: %d, want 202", code)
	}
}

func TestAWrongMissingOrMalformedSignatureIsRefusedAndStoresNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(prBody(45, "opened"))
	good := sign(testSecret, body)
	for name, sig := range map[string]string{
		"missing":      "",
		"other secret": sign("not-the-webhook-secret-at-all-000", body),
		"sha1 prefix":  "sha1=" + strings.TrimPrefix(good, "sha256="),
		"no prefix":    strings.TrimPrefix(good, "sha256="),
		"short":        good[:len(good)-2],
		"long":         good + "00",
		"not hex":      "sha256=" + strings.Repeat("zz", 32),
		"upper prefix": "SHA256=" + strings.TrimPrefix(good, "sha256="),
		"space inside": "sha256= " + strings.TrimPrefix(good, "sha256="),
		"empty digest": "sha256=",
		"last digit":   good[:len(good)-1] + map[bool]string{true: "1", false: "0"}[good[len(good)-1] == '0'],
	} {
		if code, _ := h.post("pull_request", "d-"+strings.ReplaceAll(name, " ", "-"), body, sig); code != http.StatusUnauthorized {
			t.Errorf("%s signature: %d, want 401", name, code)
		}
	}
	// Two signature headers: refused, whichever one is right, even when both are.
	wrong := sign("not-the-webhook-secret-at-all-000", body)
	for name, pair := range map[string][2]string{"right second": {wrong, good}, "right first": {good, wrong}, "both right": {good, good}} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "d-two")
		req.Header.Add("X-Hub-Signature-256", pair[0])
		req.Header.Add("X-Hub-Signature-256", pair[1])
		if code, _ := h.do(req); code != http.StatusUnauthorized {
			t.Errorf("two signature headers, %s: %d, want 401", name, code)
		}
	}
	if n := h.count(); n != 0 {
		t.Fatalf("rows after refused deliveries: %d, want 0", n)
	}
}

func TestTheSignatureIsCheckedBeforeThePayloadIsParsed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wrong := func(b []byte) string { return sign("not-the-webhook-secret-at-all-000", b) }
	// A payload that does not parse answers 400 once signed: under a wrong signature it never gets that far.
	notJSON := []byte("not json at all")
	if code, _ := h.post("pull_request", "d-1", notJSON, wrong(notJSON)); code != http.StatusUnauthorized {
		t.Fatalf("an unparsable body under a wrong signature: %d, want 401", code)
	}
	if code, _ := h.post("pull_request", "d-2", notJSON, sign(testSecret, notJSON)); code != http.StatusBadRequest {
		t.Fatalf("the same body signed: %d, want 400", code)
	}
	form := []byte("payload=" + url.QueryEscape("{broken"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", bytes.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d-3")
	req.Header.Set("X-Hub-Signature-256", wrong(form))
	if code, _ := h.do(req); code != http.StatusUnauthorized {
		t.Fatalf("a form body under a wrong signature: %d, want 401", code)
	}
	if n := h.count(); n != 0 {
		t.Fatalf("rows: %d, want 0", n)
	}
}

func TestThePreviousSecretIsAcceptedDuringARotation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(prBody(45, "opened"))
	if code, _ := h.post("pull_request", "d-1", body, sign(testPrevSecret, body)); code != http.StatusAccepted {
		t.Fatalf("previous secret: %d, want 202", code)
	}
	single := newHarness(t, func(o *Options) { o.Secrets = o.Secrets[:1] })
	if code, _ := single.post("pull_request", "d-1", body, sign(testPrevSecret, body)); code != http.StatusUnauthorized {
		t.Fatalf("previous secret once it is dropped: %d, want 401", code)
	}
}

func TestABodyOverTheLimitIsRefusedWith413(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.MaxBody = 1024 })
	at := []byte(`{"action":"opened","pad":"` + strings.Repeat("x", 1024-len(`{"action":"opened","pad":""}`)) + `"}`)
	if len(at) != 1024 {
		t.Fatalf("fixture is %d bytes", len(at))
	}
	if code, _ := h.post("pull_request", "d-at", at, sign(testSecret, at)); code != http.StatusAccepted {
		t.Fatalf("a body at the limit: %d, want 202", code)
	}
	over := append([]byte(`{"pad":"x`), bytes.Repeat([]byte("x"), 1024)...)
	over = append(over, `"}`...)
	if code, _ := h.post("pull_request", "d-over", over, sign(testSecret, over)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the limit: %d, want 413", code)
	}
	// Without a Content-Length (chunked), the read stops at the limit too.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", io.MultiReader(bytes.NewReader(over)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d-chunked")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, over))
	if code, _ := h.do(req); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a chunked body over the limit: %d, want 413", code)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("rows: %d, want 1 (the body at the limit)", n)
	}
}

func TestTheDefaultBodyLimitIs26MiB(t *testing.T) {
	t.Parallel()
	if DefaultMaxBody != 26<<20 {
		t.Fatalf("DefaultMaxBody = %d, want 26 MiB", DefaultMaxBody)
	}
}

func TestARepeatedDeliveryAnswers200AndStoresOneRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := prBody(45, "synchronize")
	if code := h.deliver("pull_request", "d-1", body); code != http.StatusAccepted {
		t.Fatalf("first delivery: %d, want 202", code)
	}
	// A redelivery keeps the GUID, whatever it carries.
	if code := h.deliver("pull_request", "d-1", body); code != http.StatusOK {
		t.Fatalf("redelivery: %d, want 200", code)
	}
	if code := h.deliver("pull_request", "d-1", prBody(46, "closed")); code != http.StatusOK {
		t.Fatalf("another body under the same delivery id: %d, want 200", code)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("rows: %d, want 1", n)
	}
	p := h.page("")
	if len(p.Events) != 1 || p.Events[0].PRNumbers[0] != 45 {
		t.Fatalf("events: %+v, want the first delivery alone", p.Events)
	}
}

func TestASignedDeliveryWithoutItsHeadersOrJSONIsABadRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte(prBody(45, "opened"))
	sig := sign(testSecret, body)
	for name, c := range map[string]struct{ event, delivery string }{
		"no event":          {"", "d-1"},
		"no delivery":       {"pull_request", ""},
		"event with spaces": {"pull request", "d-2"},
		"delivery too long": {"pull_request", strings.Repeat("a", 65)},
		"delivery with a /": {"pull_request", "d/3"},
	} {
		if code, _ := h.post(c.event, c.delivery, body, sig); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	notJSON := []byte("not json at all")
	if code, _ := h.post("pull_request", "d-4", notJSON, sign(testSecret, notJSON)); code != http.StatusBadRequest {
		t.Errorf("a signed body that is not JSON: %d, want 400", code)
	}
	if n := h.count(); n != 0 {
		t.Fatalf("rows: %d, want 0", n)
	}
}

func TestAFormEncodedDeliveryIsReadFromItsPayloadField(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := []byte("payload=" + url.QueryEscape(prBody(45, "opened")))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d-form")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	if code, b := h.do(req); code != http.StatusAccepted {
		t.Fatalf("form-encoded delivery: %d %s, want 202", code, b)
	}
	// A JSON body under a form content type (a client's default) is still read as JSON.
	raw := []byte(prBody(46, "opened"))
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d-json-as-form")
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, raw))
	if code, b := h.do(req); code != http.StatusAccepted {
		t.Fatalf("JSON under a form content type: %d %s, want 202", code, b)
	}
	p := h.page("")
	if len(p.Events) != 2 || p.Events[0].Repository != "talkable/example" || p.Events[0].PRNumbers[0] != 45 || p.Events[1].PRNumbers[0] != 46 {
		t.Fatalf("events: %+v", p.Events)
	}
}

func TestADatabaseThatRefusesTheInsertAnswers503(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.relay.j.db.ExecContext(t.Context(), `CREATE TRIGGER refuse BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'full'); END`); err != nil {
		t.Fatal(err)
	}
	if code := h.deliver("pull_request", "d-1", prBody(45, "opened")); code != http.StatusServiceUnavailable {
		t.Fatalf("refused insert: %d, want 503", code)
	}
}

func TestOnlyTheWebhookPathTakesAPost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/github/webhook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := h.do(req); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /github/webhook: %d, want 405", code)
	}
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := h.do(req); code != http.StatusNotFound {
		t.Fatalf("POST /: %d, want 404", code)
	}
}

func TestNothingOfABodyASecretOrATokenReachesTheLogsOrTheJournal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliver("pull_request", "d-1", prBody(45, "opened"))
	h.deliver("issue_comment", "d-2", `{"action":"created","issue":{"number":7,"title":"SECRET-TITLE","pull_request":{"url":"x"}},
	  "comment":{"body":"SECRET-COMMENT","user":{"login":"rev-ann"}},"repository":{"id":2,"full_name":"talkable/example"},"installation":{"id":1}}`)
	h.deliver("check_run", "d-3", `{"action":"completed","check_run":{"name":"rspec","conclusion":"failure",
	  "head_sha":"0123456789abcdef0123456789abcdef01234567","output":{"title":"SECRET-OUTPUT","text":"SECRET-TEXT"},"pull_requests":[]},
	  "repository":{"id":2,"full_name":"talkable/example"},"installation":{"id":1}}`)
	// Refusals: a bad signature, a bad token, a body over the limit.
	bad := []byte(`{"comment":{"body":"SECRET-FORGED"}}`)
	h.post("issue_comment", "d-4", bad, sign("not-the-webhook-secret-at-all-000", bad))
	h.get("", "SECRET-WRONG-TOKEN")
	h.get("", testToken)
	h.get("after=bogus", testToken)

	h.relay.StopWaiting()
	h.srv.Close()
	if err := h.relay.Close(); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "d-1") || !strings.Contains(logs, "pull_request") {
		t.Fatalf("the logs name no delivery id and event: %s", logs)
	}
	stored := logs
	for _, p := range []string{h.db, h.db + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		stored += string(b)
	}
	for _, secret := range []string{"SECRET-", "alice", "rev-ann", "html_url", testSecret, testPrevSecret, testToken} {
		if strings.Contains(stored, secret) {
			t.Errorf("%q reached the logs or the journal", secret)
		}
	}
	if fi, err := os.Stat(h.db); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode: %v %v, want 0600", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Dir(h.db)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("journal directory mode: %v %v, want 0700", fi.Mode().Perm(), err)
	}
}

// The App is public, so anyone may install it and send deliveries GitHub signs with the App's secret. With
// Installations set, a delivery of another installation, or of none (but the App's own ping), answers 202
// and stores nothing; without it every installation's delivery is stored, as before.
func TestADeliveryOfAnotherInstallationIsIgnoredAndStoresNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.Installations = []int64{1, 7} })
	for name, c := range map[string]struct{ event, body string }{
		"another installation":     {"pull_request", otherInstallation(46)},
		"no installation":          {"github_app_authorization", `{"action":"revoked","sender":{"login":"alice"}}`},
		"a malformed installation": {"pull_request", strings.Replace(prBody(47, "opened"), `"installation":{"id":1}`, `"installation":{"id":"1"}`, 1)},
		"a ping of another":        {"ping", `{"zen":"Design for failure.","hook_id":5,"installation":{"id":99}}`},
	} {
		delivery := "d-" + strings.ReplaceAll(name, " ", "-")
		code, body := h.post(c.event, delivery, []byte(c.body), sign(testSecret, []byte(c.body)))
		if code != http.StatusAccepted || body != `{"status":"ignored"}`+"\n" {
			t.Errorf("%s: %d %s, want 202 ignored", name, code, body)
		}
		if strings.Contains(h.logs.String(), delivery) {
			t.Errorf("%s: logged one by one", name)
		}
	}
	if n := h.count(); n != 0 {
		t.Fatalf("rows after ignored deliveries: %d, want 0", n)
	}
	// A listed installation's delivery is stored, and so is the App's own ping, which names none.
	if code := h.deliver("pull_request", "d-mine", prBody(45, "opened")); code != http.StatusAccepted {
		t.Fatalf("a listed installation's delivery: %d, want 202", code)
	}
	if code := h.deliver("ping", "d-ping", `{"zen":"Keep it logically awesome.","hook_id":5}`); code != http.StatusAccepted {
		t.Fatalf("the App's ping: %d, want 202", code)
	}
	p := h.page("")
	if len(p.Events) != 2 || p.Events[0].DeliveryID != "d-mine" || p.Events[1].DeliveryID != "d-ping" {
		t.Fatalf("events: %+v, want the listed installation's and the ping", p.Events)
	}
	all := newHarness(t)
	if code := all.deliver("pull_request", "d-other", otherInstallation(46)); code != http.StatusAccepted || all.count() != 1 {
		t.Fatalf("without an allowlist, another installation's delivery: %d, %d rows; want it stored", code, all.count())
	}
}

func TestOpenRefusesAnInstallationIDBelow1(t *testing.T) {
	t.Parallel()
	for _, id := range []int64{0, -1} {
		o := Options{DB: filepath.Join(t.TempDir(), "relay.db"), Secrets: [][]byte{[]byte(testSecret)},
			TokenHashes: [][32]byte{tokenHash(testToken)}, Installations: []int64{1, id}}
		if r, err := Open(t.Context(), o); err == nil {
			r.Close()
			t.Errorf("installation %d: Open succeeded", id)
		}
	}
}

// A sender's Content-Length reserves nothing past the first MiB before the signature is checked: four turns
// of 26 MiB reserved about 104 MiB for bodies that never came.
func TestTheBodyBufferGrowsWithTheBodyNotTheContentLength(t *testing.T) {
	t.Parallel()
	small := []byte(prBody(45, "opened"))
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(small))
	req.ContentLength = DefaultMaxBody
	body, err := readBody(httptest.NewRecorder(), req, DefaultMaxBody)
	if err != nil || !bytes.Equal(body, small) {
		t.Fatalf("readBody: %v, %d bytes", err, len(body))
	}
	if cap(body) > 1<<20 {
		t.Fatalf("a %d-byte body under a Content-Length of %d took a buffer of %d bytes, want at most 1 MiB", len(small), req.ContentLength, cap(body))
	}
	// A body larger than the first buffer is read whole.
	big := bytes.Repeat([]byte("x"), 3<<20+17)
	req = httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(big))
	if body, err = readBody(httptest.NewRecorder(), req, DefaultMaxBody); err != nil || !bytes.Equal(body, big) {
		t.Fatalf("a 3 MiB body: %v, %d bytes read", err, len(body))
	}
}
