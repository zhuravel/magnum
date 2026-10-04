package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// A running daemon whose prompt snapshot is behind the files on disk shows
// a prompts line; with nothing changed, or no daemon, there is none.
func TestStatusShowsPromptsChangedSinceTheSnapshot(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	d.DaemonPID = func() (int, error) { return 4242, nil }
	loaded := now.Add(-2 * time.Hour)
	if err := st.SetKV(ctx, engine.KVPromptsLoadedAt, store.FormatTime(loaded)); err != nil {
		t.Fatal(err)
	}
	render := func() (statusReport, string) {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		return r, b.String()
	}

	// Nothing changed on disk: no line.
	if r, out := render(); strings.Contains(out, "prompts:") || r.Daemon.PromptsLoadedAt == nil || r.Daemon.PromptsChanged != 0 {
		t.Fatalf("unchanged: daemon %+v\n%s", r.Daemon, out)
	}

	for k, v := range map[string]string{
		engine.KVPromptsChanged:      "2",
		engine.KVPromptsChangedFiles: "judge-rereview.md,SKILL.md",
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	r, out := render()
	if r.Daemon.PromptsChanged != 2 || strings.Join(r.Daemon.PromptsChangedFiles, "|") != "judge-rereview.md|SKILL.md" {
		t.Fatalf("daemon = %+v", r.Daemon)
	}
	want := "prompts:  " + engine.PromptsLine(loaded, 2) + " (judge-rereview.md, SKILL.md); they take effect at the next restart\n"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in\n%s", want, out)
	}
	if strings.Index(out, "prompts:") < strings.Index(out, "daemon:") {
		t.Errorf("the prompts line comes before the daemon line:\n%s", out)
	}

	// No daemon runs: the next one loads the files afresh, so no line.
	d.DaemonPID = func() (int, error) { return 0, nil }
	if _, out := render(); strings.Contains(out, "prompts:") {
		t.Fatalf("no daemon:\n%s", out)
	}
}
