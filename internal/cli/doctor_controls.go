package cli

import (
	"context"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
)

// doctorControls warns about a factory control file its daemon cannot read
// or parse (`magnum pause`'s pause.json, `magnum drain`'s
// reboot-drain.json), with what the daemon holds and a remedy that works:
// either file holds (a pause, a drain) until it is removed, and `magnum
// resume --factory` removes each, an unreadable one too. Nothing is printed
// while both read, or are absent.
func doctorControls(_ context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	if v, err := factory.ReadPause(d.Layout); err != nil {
		out = append(out, doctorWarned("controls", "the factory's pause.json cannot be read: "+err.Error()+
			": the factory holds a pause until it is removed", "`magnum resume --factory` removes it"))
	} else if _, ok := factory.ParsePause(v); v != "" && !ok {
		out = append(out, doctorWarned("controls", "the factory's pause.json is no pause `magnum pause` writes: the factory holds "+
			"a pause until it is removed", "`magnum resume --factory` removes it; `magnum pause --factory` writes a pause of yours over it"))
	}
	if v, err := factory.ReadRebootDrain(d.Layout); err != nil {
		out = append(out, doctorWarned("controls", "the factory's reboot-drain.json cannot be read: "+err.Error()+
			": the factory holds a drain until it is removed", "`magnum resume --factory` removes it"))
	} else if _, ok := engine.ParseRebootDrain(v); v != "" && !ok {
		out = append(out, doctorWarned("controls", "the factory's reboot-drain.json is no drain `magnum drain` writes: the factory "+
			"holds it until `magnum resume`", "`magnum resume --factory` removes it"))
	}
	return out
}
