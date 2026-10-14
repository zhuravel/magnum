package config

import "testing"

// The review status comment is off in the defaults, so a distributable
// config posts nothing new; [pipeline] status_comment switches it on, a
// watch's own key wins either way, and the note is read as written.
func TestStatusCommentKeysResolvePerWatch(t *testing.T) {
	cfg, _, err := loadUser(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatusCommentFor(nil) || cfg.Pipeline.StatusCommentNote != "" {
		t.Fatalf("defaults: status_comment %v, note %q; want off and none", cfg.StatusCommentFor(nil), cfg.Pipeline.StatusCommentNote)
	}

	cfg, _, err = loadUser(t, `
[pipeline]
status_comment = true
status_comment_note = "Need it sooner? Ping me on Slack."

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[watch]]
owner = "quiet"
include = ["*"]
identity = "z"
status_comment = false

[[watch]]
owner = "busy"
include = ["*"]
identity = "z"
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatusCommentFor(cfg.WatchFor("quiet/x")) {
		t.Error("a watch with status_comment = false still has the comment on")
	}
	if !cfg.StatusCommentFor(cfg.WatchFor("busy/x")) || !cfg.StatusCommentFor(nil) {
		t.Error("the pipeline's status_comment = true did not reach a watch without the key")
	}
	if got := cfg.Pipeline.StatusCommentNote; got != "Need it sooner? Ping me on Slack." {
		t.Errorf("note = %q", got)
	}
	on := true
	cfg.Pipeline.StatusComment = false
	if !cfg.StatusCommentFor(&Watch{Owner: "x", StatusComment: &on}) {
		t.Error("a watch's status_comment = true did not win over the pipeline's false")
	}
	if (&Config{}).StatusCommentFor(nil) {
		t.Error("a config built without the keys has the comment on")
	}
}
