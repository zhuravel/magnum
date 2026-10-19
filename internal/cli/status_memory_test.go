package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// `magnum status` shows the machine's memory as the daemon last sampled it
// (engine.KVMachineMemory) on a memory line right after the disk line: the
// pressure level, the swap, magnum's live agents counted from the
// registry's live sessions (those not working are idle) and the sample's
// age; a part the daemon could not read says so with its cause, and before
// any sample was recorded there is no line. --json has memory with the same
// fields.
func TestStatusShowsTheMachinesMemory(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if strings.Contains(b.String(), "memory:") || r.Memory != nil {
		t.Fatalf("a memory line without a sample:\n%s", b.String())
	}
	if raw, _ := json.Marshal(r); strings.Contains(string(raw), `"memory"`) {
		t.Fatalf("json has memory without a sample: %s", raw)
	}

	// The fixture's judge is live with no status; two more live agents, one
	// working, and a parked one that does not count.
	live, err := st.LiveSessions(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("live sessions = %+v, %v", live, err)
	}
	for role, status := range map[string]herdr.Status{store.RoleCodexReview: herdr.StatusWorking, store.RoleSimplify: herdr.StatusIdle} {
		if _, err := st.CreateSession(ctx, store.Session{PRID: live[0].PRID, Role: role, State: store.SessionLive, AgentStatus: new(string(status))}); err != nil {
			t.Fatal(err)
		}
	}

	const mib = 1 << 20
	swap := &engine.MemorySwap{UsedBytes: int64(math.Round(50281.62 * mib)), TotalBytes: 51200 * mib}
	sampled := now.Add(-20 * time.Second)
	for _, tc := range []struct {
		name   string
		sample engine.MemorySample
		line   string
	}{
		{"read", engine.MemorySample{Level: engine.MemoryWarn, Pressure: "warn", Swap: swap, SampledAt: sampled},
			"memory:   pressure warn, swap 49.1 of 50.0 GB used, 3 agents live (1 working), sampled 20s ago\n"},
		{"level unknown", engine.MemorySample{Pressure: "unknown", Swap: swap, SampledAt: sampled,
			Cause: "sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'"},
			"memory:   pressure unknown (sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'), " +
				"swap 49.1 of 50.0 GB used, 3 agents live (1 working), sampled 20s ago\n"},
		{"swap unknown", engine.MemorySample{Level: engine.MemoryCritical, Pressure: "critical", SampledAt: sampled,
			Cause: `vm.swapusage printed "garbage", not a swap reading`},
			`memory:   pressure critical, swap unknown (vm.swapusage printed "garbage", not a swap reading), 3 agents live (1 working), sampled 20s ago` + "\n"},
		{"unknown", engine.MemorySample{Pressure: "unknown", SampledAt: now.Add(-3 * time.Hour),
			Cause: "sysctl kern.memorystatus_vm_pressure_level failed: no\x1b[31m such file"},
			"memory:   unknown (sysctl kern.memorystatus_vm_pressure_level failed: no [31m such file), 3 agents live (1 working), sampled 3h ago\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.sample)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetKV(ctx, engine.KVMachineMemory, string(raw)); err != nil {
				t.Fatal(err)
			}
			r, err := statusGather(ctx, d, statusOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			statusRender(&b, r)
			out := b.String()
			i := strings.Index(out, "\ndisk:")
			if i < 0 {
				t.Fatalf("no disk line:\n%s", out)
			}
			_, rest, _ := strings.Cut(out[i+1:], "\n")
			if !strings.HasPrefix(rest, tc.line) {
				t.Fatalf("the line after the disk line is not %q:\n%s", tc.line, out)
			}
			js, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Memory map[string]any `json:"memory"`
			}
			if err := json.Unmarshal(js, &got); err != nil {
				t.Fatal(err)
			}
			m := got.Memory
			if m["level"] != float64(tc.sample.Level) || m["pressure"] != tc.sample.Pressure || m["sampled_at"] == nil ||
				m["agents_live"] != float64(3) || m["agents_working"] != float64(1) || (m["cause"] != nil) != (tc.sample.Cause != "") ||
				(m["swap"] != nil) != (tc.sample.Swap != nil) {
				t.Fatalf("json memory = %v", m)
			}
			if tc.sample.Swap != nil {
				sw := m["swap"].(map[string]any)
				if sw["used_bytes"] != float64(swap.UsedBytes) || sw["total_bytes"] != float64(swap.TotalBytes) {
					t.Fatalf("json swap = %v", sw)
				}
			}
		})
	}
}
