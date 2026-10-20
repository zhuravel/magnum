package relay

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	headSHA = "0123456789abcdef0123456789abcdef01234567"
	repoKey = `"repository":{"id":2,"full_name":"talkable/example","name":"example","private":true},"installation":{"id":1}`
)

func TestTheEnvelopeOfEachEventKeepsOnlyItsRoutingFields(t *testing.T) {
	t.Parallel()
	prObject := `{"number":45,"title":"SECRET","body":"SECRET","head":{"sha":"` + headSHA + `","ref":"feature"},"base":{"ref":"main"}}`
	for _, c := range []struct {
		name, event, body string
		want              Event
	}{
		{
			name: "pull_request", event: "pull_request",
			body: `{"action":"synchronize","number":45,"before":"` + strings.Repeat("a", 40) + `","pull_request":` + prObject + `,` + repoKey + `}`,
			want: Event{Action: "synchronize", PRNumbers: []int{45}, HeadSHA: headSHA, BaseRef: "main"},
		},
		{
			name: "pull_request_review", event: "pull_request_review",
			body: `{"action":"submitted","review":{"body":"SECRET","state":"approved","commit_id":"` + strings.Repeat("b", 40) + `"},"pull_request":` + prObject + `,` + repoKey + `}`,
			want: Event{Action: "submitted", PRNumbers: []int{45}, HeadSHA: headSHA, BaseRef: "main"},
		},
		{
			name: "pull_request_review_comment", event: "pull_request_review_comment",
			body: `{"action":"created","comment":{"body":"SECRET","path":"app.rb"},"pull_request":` + prObject + `,` + repoKey + `}`,
			want: Event{Action: "created", PRNumbers: []int{45}, HeadSHA: headSHA, BaseRef: "main"},
		},
		{
			name: "pull_request_review_thread", event: "pull_request_review_thread",
			body: `{"action":"resolved","thread":{"comments":[{"body":"SECRET"}]},"pull_request":` + prObject + `,` + repoKey + `}`,
			want: Event{Action: "resolved", PRNumbers: []int{45}, HeadSHA: headSHA, BaseRef: "main"},
		},
		{
			name: "issue_comment on a pull request", event: "issue_comment",
			body: `{"action":"created","issue":{"number":7,"title":"SECRET","pull_request":{"url":"https://api.github.com/x"}},"comment":{"body":"SECRET"},` + repoKey + `}`,
			want: Event{Action: "created", PRNumbers: []int{7}},
		},
		{
			name: "issue_comment on an issue", event: "issue_comment",
			body: `{"action":"created","issue":{"number":8,"title":"SECRET"},"comment":{"body":"SECRET"},` + repoKey + `}`,
			want: Event{Action: "created", PRNumbers: []int{}},
		},
		{
			name: "issue_comment with a null pull_request", event: "issue_comment",
			body: `{"action":"edited","issue":{"number":9,"pull_request":null},` + repoKey + `}`,
			want: Event{Action: "edited", PRNumbers: []int{}},
		},
		{
			name: "check_run with pull requests", event: "check_run",
			body: `{"action":"completed","check_run":{"name":"rspec (3)","status":"completed","conclusion":"failure","head_sha":"` + headSHA + `",
			  "output":{"title":"SECRET","summary":"SECRET"},"pull_requests":[{"number":46,"base":{"ref":"main"}},{"number":45},{"number":46}]},` + repoKey + `}`,
			want: Event{Action: "completed", PRNumbers: []int{45, 46}, HeadSHA: headSHA, Check: &Check{Name: "rspec (3)", Conclusion: "failure"}},
		},
		{
			name: "check_run from a fork, before it completes", event: "check_run",
			body: `{"action":"created","check_run":{"name":"lint","status":"queued","conclusion":null,"head_sha":"` + headSHA + `","pull_requests":[]},` + repoKey + `}`,
			want: Event{Action: "created", PRNumbers: []int{}, HeadSHA: headSHA, Check: &Check{Name: "lint"}},
		},
		{
			name: "check_suite", event: "check_suite",
			body: `{"action":"completed","check_suite":{"head_sha":"` + headSHA + `","status":"completed","conclusion":"success","app":{"name":"GitHub Actions","slug":"github-actions"},
			  "pull_requests":[{"number":45}],"head_commit":{"message":"SECRET"}},` + repoKey + `}`,
			want: Event{Action: "completed", PRNumbers: []int{45}, HeadSHA: headSHA, Check: &Check{Name: "GitHub Actions", Conclusion: "success"}},
		},
		{
			name: "check_suite from a fork", event: "check_suite",
			body: `{"action":"completed","check_suite":{"head_sha":"` + headSHA + `","conclusion":"success","app":{"name":"CI"},"pull_requests":[]},` + repoKey + `}`,
			want: Event{Action: "completed", PRNumbers: []int{}, HeadSHA: headSHA, Check: &Check{Name: "CI", Conclusion: "success"}},
		},
		{
			name: "status", event: "status",
			body: `{"sha":"` + headSHA + `","state":"pending","context":"ci/build","description":"SECRET","target_url":"https://ci.example.com/SECRET",
			  "commit":{"commit":{"message":"SECRET"}},"branches":[{"name":"feature"}],` + repoKey + `}`,
			want: Event{PRNumbers: []int{}, HeadSHA: headSHA, Check: &Check{Name: "ci/build", Conclusion: "pending"}},
		},
		{
			name: "installation_repositories", event: "installation_repositories",
			body: `{"action":"added","installation":{"id":1},"repository_selection":"selected","repositories_added":[{"id":3,"full_name":"talkable/other"}],"repositories_removed":[]}`,
			want: Event{Action: "added", PRNumbers: []int{}},
		},
		{
			name: "ping", event: "ping",
			body: `{"zen":"SECRET","hook_id":12,"hook":{"config":{"url":"https://relay.example.com/github/webhook"}}}`,
			want: Event{PRNumbers: []int{}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Envelope(c.event, []byte(c.body))
			if err != nil {
				t.Fatalf("Envelope: %v", err)
			}
			want := c.want
			want.Event = c.event
			if strings.Contains(c.body, `"installation":{"id":1}`) {
				want.InstallationID = 1
			}
			if strings.Contains(c.body, `"full_name":"talkable/example"`) {
				want.RepositoryID, want.Repository = 2, "talkable/example"
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("envelope:\n got %+v\nwant %+v", got, want)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "feature") {
				t.Fatalf("the envelope keeps text: %s", b)
			}
			if !strings.Contains(string(b), `"pr_numbers":[`) {
				t.Fatalf("pr_numbers is not an array: %s", b)
			}
			if hasCheck := strings.Contains(string(b), `"check":`); hasCheck != (c.want.Check != nil) {
				t.Fatalf("check object present = %v: %s", hasCheck, b)
			}
		})
	}
}

func TestTheEnvelopeDropsRoutingValuesThatAreNotWellFormed(t *testing.T) {
	t.Parallel()
	body := `{"action":"Opened; DROP","pull_request":{"number":-3,"head":{"sha":"ABCDEF"},"base":{"ref":"main\u0000x"}},
	  "repository":{"id":-2,"full_name":"talkable/example/extra"},"installation":{"id":"one"}}`
	got, err := Envelope("pull_request", []byte(body))
	if err != nil {
		t.Fatalf("Envelope: %v", err)
	}
	want := Event{Event: "pull_request", PRNumbers: []int{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envelope:\n got %+v\nwant %+v", got, want)
	}
}

func TestACheckNameIsCutTo120RunesAndCleanedOfControls(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 130)
	body := `{"action":"completed","check_run":{"name":"\u001b[31mred\u202e ` + long + `","conclusion":"Not a conclusion","head_sha":"` + headSHA + `","pull_requests":[]}}`
	got, err := Envelope("check_run", []byte(body))
	if err != nil {
		t.Fatalf("Envelope: %v", err)
	}
	if got.Check == nil {
		t.Fatal("no check")
	}
	if n := utf8.RuneCountInString(got.Check.Name); n != 120 {
		t.Fatalf("name has %d runes, want 120: %q", n, got.Check.Name)
	}
	if !strings.HasPrefix(got.Check.Name, "[31mred é") {
		t.Fatalf("controls kept: %q", got.Check.Name)
	}
	if got.Check.Conclusion != "" {
		t.Fatalf("conclusion %q, want it dropped", got.Check.Conclusion)
	}
}

func TestTheEnvelopeKeepsAtMost100PullRequestNumbers(t *testing.T) {
	t.Parallel()
	var prs []string
	for i := 1; i <= 150; i++ {
		prs = append(prs, `{"number":`+jsonInt(i)+`}`)
	}
	body := `{"action":"completed","check_suite":{"head_sha":"` + headSHA + `","pull_requests":[` + strings.Join(prs, ",") + `]}}`
	got, err := Envelope("check_suite", []byte(body))
	if err != nil {
		t.Fatalf("Envelope: %v", err)
	}
	if len(got.PRNumbers) != 100 || got.PRNumbers[0] != 1 || got.PRNumbers[99] != 100 {
		t.Fatalf("pr_numbers: %d of them, %v...", len(got.PRNumbers), got.PRNumbers[:3])
	}
}

func TestAPayloadThatIsNotJSONHasNoEnvelope(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "not json", `{"action":`, `{"a":1} trailing`} {
		if _, err := Envelope("pull_request", []byte(body)); err == nil {
			t.Errorf("Envelope(%q): no error", body)
		}
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
