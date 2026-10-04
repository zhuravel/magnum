package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// TestLooksLikeDaemon pins the ps check every signal to the daemon goes
// through: the executable (comm) must be magnum, not merely a word of the
// command line.
func TestLooksLikeDaemon(t *testing.T) {
	cases := []struct {
		comm, args string
		want       bool
	}{
		{"/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon", true},
		{"/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon --once", true},
		{"/Users/b/My Projects/magnum/bin/magnum", "/Users/b/My Projects/magnum/bin/magnum daemon", true},
		{"magnum", "magnum --config /x/config.toml daemon", true},
		{"magnum", "magnum --config=/x/config.toml daemon", true},
		{"magnum", "/Users/b/bin/magnum daemon", true}, // a bare (Linux-style) comm
		// An editor opening a file named magnum: the executable is vim.
		{"/usr/bin/vim", "/usr/bin/vim /tmp/magnum daemon", false},
		{"/usr/bin/vim", "/usr/bin/vim magnum daemon", false},
		{"vim", "vim /tmp/magnum daemon", false},
		{"/usr/bin/vim", "/usr/bin/vim notes.txt", false},
		// daemon must be the subcommand, not a later argument.
		{"/Users/b/bin/magnum", "/Users/b/bin/magnum status", false},
		{"/Users/b/bin/magnum", "/Users/b/bin/magnum logs daemon", false},
		{"/Users/b/bin/magnum", "/Users/b/bin/magnum --config daemon", false},
		{"/Users/b/bin/magnum-old", "/Users/b/bin/magnum-old daemon", false},
		{"/Users/b/bin/magnum", "/Users/b/bin/magnumx daemon", false},
		{"/Users/b/bin/magnum", "/Users/b/bin/magnum", false},
		{"", "/Users/b/bin/magnum daemon", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := LooksLikeDaemon(tc.comm, tc.args); got != tc.want {
			t.Errorf("LooksLikeDaemon(%q, %q) = %v, want %v", tc.comm, tc.args, got, tc.want)
		}
	}
}

func TestProcessCommandReadsCommThenArgs(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"ps", "-p", "250", "-o", "comm="}, Result: execx.Result{Stdout: []byte("/Users/b/My Tools/magnum\n")}},
		{Prefix: []string{"ps", "-p", "250", "-o", "args="}, Result: execx.Result{Stdout: []byte("/Users/b/My Tools/magnum daemon\n")}},
	}}
	comm, args, err := ProcessCommand(context.Background(), run, 250)
	if err != nil || comm != "/Users/b/My Tools/magnum" || args != "/Users/b/My Tools/magnum daemon" {
		t.Fatalf("comm %q args %q err %v", comm, args, err)
	}
	if !LooksLikeDaemon(comm, args) {
		t.Fatal("the real daemon must pass")
	}

	gone := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"ps"}, Err: errors.New("exit 1")}}}
	if _, _, err := ProcessCommand(context.Background(), gone, 250); err == nil {
		t.Fatal("a failed ps must be an error")
	}
	if len(gone.Calls) != 1 || !slices.Contains(gone.Calls[0].Args, "comm=") {
		t.Fatalf("calls %v", gone.Calls)
	}
}
