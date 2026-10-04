package github

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// Fixtures under testdata/ were captured from the real API with read-only
// `gh api graphql --input -` / `gh api` calls (2026-10-02). Files ending in
// .stderr hold what gh printed when it exited 1.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// gqlReq is the JSON body the client writes to `gh api graphql --input -`.
type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func decodeReq(t *testing.T, c execx.Cmd) gqlReq {
	t.Helper()
	var r gqlReq
	if err := json.Unmarshal(c.Stdin, &r); err != nil {
		t.Fatalf("stdin is not a GraphQL request: %v (%q)", err, c.Stdin)
	}
	return r
}

// okResult is a successful gh run printing body.
func okResult(body []byte) (execx.Result, error) {
	return execx.Result{Stdout: body}, nil
}

// failResult mimics gh exiting 1: the JSON body on stdout, a message on stderr.
func failResult(c execx.Cmd, stdout, stderr []byte) (execx.Result, error) {
	res := execx.Result{Stdout: stdout, Stderr: stderr, Code: 1}
	return res, &execx.ExitError{Cmd: c, Code: 1, Stderr: string(stderr)}
}

// gqlRule routes every `gh api graphql` call to fn.
func gqlRule(t *testing.T, fn func(c execx.Cmd, req gqlReq) (execx.Result, error)) execx.Rule {
	return execx.Rule{
		Prefix: []string{"gh", "api", "graphql"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			return fn(c, decodeReq(t, c))
		},
	}
}

var aliasRe = regexp.MustCompile(`p(\d+): pullRequest\(number: (\d+)\)`)

// aliases lists the pN aliases of a batched query in order.
func aliases(q string) []string {
	var out []string
	for _, m := range aliasRe.FindAllStringSubmatch(q, -1) {
		out = append(out, m[1])
	}
	return out
}

func compact(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("bad JSON in test: %v", err)
	}
	return buf.Bytes()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
