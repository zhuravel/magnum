package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// `magnum doctor` has one memory line: the machine's pressure level and
// swap read now, as the daemon samples them, a WARN at critical pressure,
// and a SKIP when the level cannot be read (no such sysctl off macOS).
func TestDoctorSaysTheMachinesMemory(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	base := fake.Rules
	const swap = "total = 51200.00M  used = 50281.62M  free = 918.38M  (encrypted)\n"
	answers := func(level execx.Rule) {
		fake.Rules = append([]execx.Rule{level,
			{Prefix: []string{"/usr/sbin/sysctl", "-n", "vm.swapusage"}, Result: execx.Result{Stdout: []byte(swap)}}}, base...)
	}
	prints := func(level string) execx.Rule {
		return execx.Rule{Prefix: []string{"/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level"}, Result: execx.Result{Stdout: []byte(level)}}
	}
	check := func() doctorCheck {
		t.Helper()
		var found []doctorCheck
		for _, c := range doctorRun(context.Background(), d) {
			if c.Name == "memory" {
				found = append(found, c)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d memory checks: %+v", len(found), found)
		}
		return found[0]
	}

	for level, want := range map[string]string{"1\n": "memory pressure normal, swap 49.1 of 50.0 GB used", "2\n": "memory pressure warn, swap 49.1 of 50.0 GB used"} {
		answers(prints(level))
		if c := check(); c.Status != doctorPass || c.Detail != want || c.Fix != "" {
			t.Errorf("level %q = %+v, want PASS %q", level, c, want)
		}
	}

	answers(prints("4\n"))
	if c := check(); c.Status != doctorWarn || c.Detail != "memory pressure critical, swap 49.1 of 50.0 GB used" || !strings.Contains(c.Fix, "magnum status") {
		t.Errorf("critical = %+v, want a WARN with a fix", c)
	}

	answers(execx.Rule{Prefix: []string{"/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level"},
		Result: execx.Result{Code: 1, Stderr: []byte("sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'\n")}})
	if c := check(); c.Status != doctorSkip ||
		c.Detail != "memory pressure unknown (sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'), swap 49.1 of 50.0 GB used" {
		t.Errorf("unreadable level = %+v, want a SKIP with the cause", c)
	}
}
