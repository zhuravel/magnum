package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
)

// `magnum status` names the version of each agent CLI the daemon recorded
// (engine.KVKindVersion), in the order of the roles' kinds, and for a day
// after a change when it came ("(changed at 11:24)"), which `magnum doctor`
// alone said before.
func TestStatusNamesTheAgentCLIVersions(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	for kind, v := range map[string]engine.KindVersion{
		"codex": {Version: "codex-cli 0.163.0", Path: "/opt/codex/0.163.0/codex", SeenAt: now.Add(-36 * time.Minute),
			Previous: "codex-cli 0.162.1", ChangedAt: now.Add(-36 * time.Minute)},
		"claude": {Version: "2.1.292 (Claude Code)", Path: "/opt/claude/2.1.292", SeenAt: now.Add(-50 * time.Hour),
			Previous: "2.1.290 (Claude Code)", ChangedAt: now.Add(-50 * time.Hour)},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, engine.KVKindVersion(kind), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.CLIs) != 2 || r.CLIs[0].Kind != "codex" || r.CLIs[0].Version != "codex-cli 0.163.0" || r.CLIs[1].Kind != "claude" {
		t.Fatalf("clis = %+v", r.CLIs)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	want := "clis:     codex codex-cli 0.163.0 (changed at " + inspClock(now, now.Add(-36*time.Minute)) + "), claude 2.1.292 (Claude Code)\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("status lacks %q:\n%s", want, b.String())
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"clis":[{"kind":"codex","version":"codex-cli 0.163.0"`) {
		t.Fatalf("json %s", raw)
	}
}
