package herdr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// schemaExcerpt is the subset of testdata/schema-excerpt.json the tests read.
type schemaExcerpt struct {
	Methods     map[string]string `json:"methods"` // method -> params $def name
	RequestDefs map[string]struct {
		Required []string `json:"required"`
		OneOf    []struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		} `json:"oneOf"`
	} `json:"request_defs"`
	SubscriptionEvent struct {
		Defs struct {
			Kind struct {
				Enum []string `json:"enum"`
			} `json:"SubscriptionEventKind"`
		} `json:"$defs"`
	} `json:"subscription_event"`
	EventKinds struct {
		Enum []string `json:"enum"`
	} `json:"event_kinds"`
	CLIRequests []struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	} `json:"cli_requests"`
}

func loadExcerpt(t *testing.T) schemaExcerpt {
	t.Helper()
	b, err := os.ReadFile("testdata/schema-excerpt.json")
	if err != nil {
		t.Fatal(err)
	}
	var x schemaExcerpt
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	return x
}

// TestTypedMethodsMatchSchema guards against drift: every method the client
// sends exists in the bundled herdr schema and carries its required params.
func TestTypedMethodsMatchSchema(t *testing.T) {
	x := loadExcerpt(t)
	cases := typedCases()
	methods := map[string]bool{"events.subscribe": true}
	for _, tc := range cases {
		methods[tc.method] = true
		def, ok := x.Methods[tc.method]
		if !ok {
			t.Errorf("%s: method %s not in herdr schema", tc.name, tc.method)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(tc.params), &params); err != nil {
			t.Fatal(err)
		}
		for _, req := range x.RequestDefs[def].Required {
			if _, ok := params[req]; !ok {
				t.Errorf("%s: %s params miss required field %q", tc.name, tc.method, req)
			}
		}
	}
	for m := range methods {
		if _, ok := x.Methods[m]; !ok {
			t.Errorf("method %s not in schema excerpt", m)
		}
	}
}

// TestTypedCasesMirrorCapturedCLI checks that the expected requests in
// typedCases are exactly what the real herdr CLI sends for the same command
// (captured against a fake socket). Fields the CLI sends with their schema
// default (right_click) are ignored; methods magnum does not use are skipped.
func TestTypedCasesMirrorCapturedCLI(t *testing.T) {
	x := loadExcerpt(t)
	unused := map[string]bool{"agent.wait": true, "workspace.list": true, "tab.list": true}
	cases := typedCases()
	for _, cr := range x.CLIRequests {
		if unused[cr.Method] {
			continue
		}
		var want map[string]any
		if err := json.Unmarshal(cr.Params, &want); err != nil {
			t.Fatal(err)
		}
		delete(want, "right_click")
		found := false
		for _, tc := range cases {
			if tc.method != cr.Method {
				continue
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(tc.params), &got); err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(got, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no typed case reproduces the CLI request %s %s", cr.Method, cr.Params)
		}
	}
}

// TestEventConstantsMatchSchema pins the subscription and streamed-event
// names to the bundled schema: Sub* are request Subscription types, the
// pane-scoped status event is spelled as subscription_event's kind enum, and
// the lifecycle events as event_kinds.
func TestEventConstantsMatchSchema(t *testing.T) {
	x := loadExcerpt(t)
	var subTypes []string
	for _, v := range x.RequestDefs["Subscription"].OneOf {
		subTypes = append(subTypes, v.Properties.Type.Const)
	}
	for _, sub := range []string{SubPaneAgentStatusChanged, SubPaneExited, SubPaneClosed, SubWorkspaceClosed} {
		if !slices.Contains(subTypes, sub) {
			t.Errorf("subscription type %q is not in the schema's Subscription types", sub)
		}
	}
	if kinds := x.SubscriptionEvent.Defs.Kind.Enum; !slices.Contains(kinds, EventPaneAgentStatusChanged) {
		t.Errorf("EventPaneAgentStatusChanged %q is not a SubscriptionEventKind %v", EventPaneAgentStatusChanged, kinds)
	}
	for _, ev := range []string{EventPaneExited, EventPaneClosed, EventWorkspaceClosed} {
		if !slices.Contains(x.EventKinds.Enum, ev) {
			t.Errorf("event %q is not in the schema's event_kinds", ev)
		}
	}
}

// TestRequiredMethodsCoverEveryCall keeps RequiredMethods (read by `magnum
// doctor`) in step with the methods this package sends; those must also be in
// the schema excerpt (plugin.list, sent by the CLI through Call, is not in it).
func TestRequiredMethodsCoverEveryCall(t *testing.T) {
	required := RequiredMethods()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?:method: |const method = )"([a-z_.]+)"`)
	sent := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			sent[m[1]] = true
		}
	}
	if len(sent) < 20 {
		t.Fatalf("found only %d method literals; the pattern no longer matches the sources", len(sent))
	}
	for m := range sent {
		if !slices.Contains(required, m) {
			t.Errorf("method %s is sent but missing from RequiredMethods", m)
		}
	}
	x := loadExcerpt(t)
	for _, m := range required {
		if _, ok := x.Methods[m]; !ok && sent[m] {
			t.Errorf("required method %s not in the schema excerpt", m)
		}
	}
	required[0] = "changed"
	if RequiredMethods()[0] == "changed" {
		t.Error("RequiredMethods must return a copy")
	}
}
