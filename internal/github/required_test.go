package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// The bodies below are in the shapes GitHub answered on 2026-10-04 (public
// repositories' rulesets; a 404 from the classic endpoint without admin
// access); names are placeholders.
const (
	rulesBody = `[{"type":"deletion","ruleset_source_type":"Organization","ruleset_source":"talkable","ruleset_id":1},
		{"type":"required_status_checks","parameters":{"do_not_enforce_on_create":false,"required_status_checks":[{"context":"Completion","integration_id":15368},{"context":"lint"}],"strict_required_status_checks_policy":false},"ruleset_source_type":"Repository","ruleset_source":"talkable/talkable","ruleset_id":2},
		{"type":"pull_request","parameters":{"required_approving_review_count":1},"ruleset_source_type":"Repository","ruleset_source":"talkable/talkable","ruleset_id":2},
		{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Completion"},{"context":"license/cla","integration_id":865473}]},"ruleset_source_type":"Organization","ruleset_source":"talkable","ruleset_id":3}]`
	classicBody  = `{"url":"u","strict":true,"contexts":["ci/build","rspec"],"checks":[{"context":"ci/build","app_id":null},{"context":"rspec","app_id":15368},{"context":"jest","app_id":15368}],"contexts_url":"u"}`
	upgradeBody  = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","documentation_url":"https://docs.github.com/rest","status":"403"}`
	notFoundBody = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`
)

// requiredFake answers the rules and the classic protection endpoints; a
// status other than 200 fails gh the way it fails for that HTTP status.
func requiredFake(t *testing.T, rulesStatus int, rules string, classicStatus int, classic string) *execx.Fake {
	answer := func(c execx.Cmd, status int, body string) (execx.Result, error) {
		if status == 200 {
			return okResult(compact(t, body))
		}
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(body), &msg)
		return failResult(c, compact(t, body), []byte("gh: "+msg.Message+" (HTTP "+strconv.Itoa(status)+")\n"))
	}
	return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		switch path := c.Args[1]; {
		case strings.Contains(path, "/rules/branches/"):
			return answer(c, rulesStatus, rules)
		case strings.HasSuffix(path, "/protection/required_status_checks"):
			return answer(c, classicStatus, classic)
		}
		t.Errorf("unexpected call %v", c.Args)
		return execx.Result{}, errors.New("unexpected call")
	}}}}
}

func TestRequiredChecks(t *testing.T) {
	cases := []struct {
		name          string
		rulesStatus   int
		rules         string
		classicStatus int
		classic       string
		want          []string
		known         bool
		calls         int
		wantErr       error // matched with errors.Is
		wantErrNonNil bool
	}{
		{name: "rulesets", rulesStatus: 200, rules: rulesBody, want: []string{"Completion", "lint", "license/cla"}, known: true, calls: 1},
		{name: "no ruleset checks: classic protection", rulesStatus: 200, rules: `[]`, classicStatus: 200, classic: classicBody,
			want: []string{"ci/build", "rspec", "jest"}, known: true, calls: 2},
		{name: "classic protection requires none", rulesStatus: 200, rules: `[]`, classicStatus: 200, classic: `{"strict":false,"contexts":[],"checks":[]}`,
			want: []string{}, known: true, calls: 2},
		{name: "free plan: 403 from both", rulesStatus: 403, rules: upgradeBody, classicStatus: 403, classic: upgradeBody, calls: 2},
		{name: "no rules, classic 404", rulesStatus: 200, rules: `[]`, classicStatus: 404, classic: notFoundBody, calls: 2},
		{name: "rules fail", rulesStatus: 502, rules: `{"message":"Server Error"}`, calls: 1, wantErrNonNil: true},
		{name: "rules rate limited", rulesStatus: 403, rules: `{"message":"API rate limit exceeded for installation"}`, calls: 1, wantErr: ErrRateLimited},
		{name: "classic fails", rulesStatus: 200, rules: `[]`, classicStatus: 500, classic: `{"message":"Server Error"}`, calls: 2, wantErrNonNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := requiredFake(t, tc.rulesStatus, tc.rules, tc.classicStatus, tc.classic)
			got, known, err := (&Client{Run: f}).RequiredChecks(context.Background(), "talkable", "talkable", "master")
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantErrNonNil:
				if err == nil {
					t.Fatal("want an error")
				}
			case err != nil:
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) || known != tc.known {
				t.Errorf("checks = %#v known %v, want %#v %v", got, known, tc.want, tc.known)
			}
			if len(f.Calls) != tc.calls {
				t.Fatalf("calls = %d, want %d", len(f.Calls), tc.calls)
			}
			want := []string{"api", "repos/talkable/talkable/rules/branches/master?per_page=100", "--hostname", "github.com"}
			if c := f.Calls[0]; !reflect.DeepEqual(c.Args, want) || c.Mutates {
				t.Errorf("rules call = %v mutates=%v", c.Args, c.Mutates)
			}
			if tc.calls == 2 {
				want := []string{"api", "repos/talkable/talkable/branches/master/protection/required_status_checks", "--hostname", "github.com"}
				if c := f.Calls[1]; !reflect.DeepEqual(c.Args, want) || c.Mutates {
					t.Errorf("classic call = %v mutates=%v", c.Args, c.Mutates)
				}
			}
		})
	}
}

func TestRequiredChecksInvalidInput(t *testing.T) {
	f := &execx.Fake{}
	for _, in := range [][3]string{{"talkable", "talkable", "a b"}, {"talkable", "talkable", "../x"}, {"talkable", "talkable", ""}, {"a b", "talkable", "main"}} {
		if _, _, err := (&Client{Run: f}).RequiredChecks(context.Background(), in[0], in[1], in[2]); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %v", f.Calls)
	}
}
