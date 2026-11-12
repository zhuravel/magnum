package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
)

// `magnum doctor` says when the factory cannot read its pause.json or its
// reboot-drain.json, and what to do; with readable or absent files it says
// nothing of them.
func TestDoctorWarnsAboutAnUnreadableControlFile(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	if got := doctorControls(context.Background(), d); len(got) != 0 {
		t.Fatalf("checks %+v with no control file, want none", got)
	}
	if err := factory.WritePause(d.Layout, "{not a pause"); err != nil {
		t.Fatal(err)
	}
	if err := factory.WriteRebootDrain(d.Layout, "{not a drain"); err != nil {
		t.Fatal(err)
	}
	got := doctorControls(context.Background(), d)
	if len(got) != 2 {
		t.Fatalf("checks %+v, want a warning for each file", got)
	}
	for _, c := range got {
		if c.Name != "controls" || c.Status != doctorWarn || !strings.Contains(c.Fix, "magnum resume") {
			t.Errorf("check %+v, want a controls WARN with its fix", c)
		}
	}
	if !strings.Contains(got[0].Detail, "pause.json") || !strings.Contains(got[1].Detail, "reboot-drain.json") {
		t.Errorf("checks %+v, want each naming its file", got)
	}
}
