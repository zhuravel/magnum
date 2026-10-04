package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"reflect"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

func mustOtherKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestWatchedRepos(t *testing.T) {
	c := &config.Config{Watches: []config.Watch{
		{Owner: "talkable", Include: []string{"talkable", "web-*", "talkable"}, Identity: "talkable-app"},
		{Owner: "zhuravel", Include: []string{"*"}, Identity: "zhuravel"},
		{Owner: "acme", Include: []string{"api", "web"}, Exclude: []string{"web"}, Identity: "talkable-app"},
	}}
	if got, want := WatchedRepos(c, "talkable-app"), []string{"talkable/talkable", "acme/api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := WatchedRepos(c, "zhuravel"); len(got) != 0 {
		t.Fatalf("globs must not produce repos: %v", got)
	}
}

func TestReportString(t *testing.T) {
	r := Report{Lines: []string{"PASS a", "FAIL b"}}
	if r.String() != "PASS a\nFAIL b" {
		t.Fatalf("%q", r.String())
	}
}
