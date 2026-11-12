package cli

import (
	"context"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
)

// doctorControls warns about a factory control file its daemon cannot read
// (`magnum pause`'s pause.json, `magnum drain`'s reboot-drain.json): the
// daemon holds no pause for the one and holds the drain until `magnum
// resume` for the other, and says it once in an event. Nothing is printed
// while both read, or are absent.
func doctorControls(_ context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	if v, err := factory.ReadPause(d.Layout); err != nil {
		out = append(out, doctorWarned("controls", "the factory's pause.json cannot be read: "+err.Error(),
			"`magnum resume --factory` removes it; `magnum pause --factory` writes it again"))
	} else if _, ok := factory.ParsePause(v); v != "" && !ok {
		out = append(out, doctorWarned("controls", "the factory's pause.json is no pause `magnum pause` writes: no pause holds",
			"`magnum resume --factory` removes it; `magnum pause --factory` writes it again"))
	}
	if v, err := factory.ReadRebootDrain(d.Layout); err != nil {
		out = append(out, doctorWarned("controls", "the factory's reboot-drain.json cannot be read: "+err.Error(),
			"`magnum resume --factory` removes it"))
	} else if _, ok := engine.ParseRebootDrain(v); v != "" && !ok {
		out = append(out, doctorWarned("controls", "the factory's reboot-drain.json is no drain `magnum drain` writes: the factory "+
			"holds it until `magnum resume`", "`magnum resume --factory` removes it"))
	}
	return out
}
