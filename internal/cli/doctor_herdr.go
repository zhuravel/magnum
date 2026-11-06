package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
)

// doctorHerdrChecks checks the herdr session of magnum's panes (PaneSession:
// a FAIL when it does not answer, as no round can run), yours while the
// panes are in another (a WARN: the toasts show with osascript, and the
// slot guards read no agent of yours or hold every slot), a move of the
// panes that waits or is due (doctorPanesMove), and the herdr CLI and
// server's version and methods.
func doctorHerdrChecks(ctx context.Context, d doctorDeps) []doctorCheck {
	panes := d.PaneSession
	if panes.Socket == "" {
		panes = d.Config.YourHerdr()
	}
	socket := inspTilde(panes.Socket)
	info, err := d.Herdr.Ping(ctx)
	var out []doctorCheck
	switch {
	case err != nil && d.Yours == nil:
		out = append(out, doctorFailed("herdr", "herdr socket "+socket+" does not answer: "+err.Error(),
			"start herdr (run `"+herdrStart(panes.Name)+"` in a terminal), or point [herdr] socket in config.toml at the right socket"))
	case err != nil:
		out = append(out, doctorFailed("herdr", fmt.Sprintf("herdr session %s of magnum's review panes (socket %s) does not answer: %s",
			panes.Name, socket, err), fmt.Sprintf("start it: run `%s` in a terminal (a running daemon opens it while no server runs there)",
			herdrStart(panes.Name))))
	default:
		where := socket
		if d.Yours != nil {
			where += " (session " + panes.Name + ", magnum's review panes)"
		}
		out = append(out, doctorOK("herdr", fmt.Sprintf("herdr server %s (protocol %d) at %s", info.Version, info.Protocol, where)))
		if info.Protocol != 0 && info.Protocol != herdr.Protocol {
			out = append(out, doctorWarned("herdr protocol", fmt.Sprintf("herdr speaks protocol %d; magnum was verified against %d", info.Protocol, herdr.Protocol),
				"run magnum's tests against this herdr (go test ./internal/herdr) before trusting automation"))
		}
	}
	if d.Yours != nil {
		out = append(out, doctorYourHerdr(ctx, d))
	}
	out = append(out, doctorPanesMove(ctx, d)...)
	cliOut, cerr := doctorExec(ctx, d, 5*time.Second, "", nil, "herdr", "--version")
	cliVersion := strings.TrimSpace(strings.TrimPrefix(cliOut, "herdr"))
	switch {
	case cerr != nil:
		out = append(out, doctorWarned("herdr version", "could not run `herdr --version`: "+cerr.Error(), "install herdr and put it on PATH"))
	case err == nil && cliVersion != "" && cliVersion != info.Version:
		out = append(out, doctorWarned("herdr version", fmt.Sprintf("herdr CLI %s differs from the running server %s", cliVersion, info.Version),
			"restart the herdr server when no agent is working so it runs the installed "+cliVersion+" (magnum talks to the server)"))
	default:
		out = append(out, doctorOK("herdr version", "herdr CLI "+doctorOrUnknown(cliVersion)+" matches the server"))
	}
	schema, serr := doctorExec(ctx, d, 10*time.Second, "", nil, "herdr", "api", "schema", "--json")
	if serr != nil {
		out = append(out, doctorWarned("herdr methods", "could not read `herdr api schema --json`: "+serr.Error(), "install herdr and put it on PATH"))
		return out
	}
	methods, proto, perr := doctorSchemaMethods(schema)
	if perr != nil {
		out = append(out, doctorWarned("herdr methods", "unreadable herdr API schema: "+perr.Error(), "upgrade herdr"))
		return out
	}
	var missing []string
	required := herdr.RequiredMethods()
	for _, m := range required {
		if !methods[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		out = append(out, doctorFailed("herdr methods", "herdr's API lacks methods magnum uses: "+strings.Join(missing, ", "),
			"upgrade herdr (`herdr update`) to 0.9.3 or later"))
	} else {
		out = append(out, doctorOK("herdr methods", fmt.Sprintf("all %d socket methods magnum uses exist (schema protocol %d)", len(required), proto)))
	}
	return out
}

// doctorYourHerdr checks your herdr session ([herdr] socket) while
// magnum's panes are in another: a WARN when it does not answer, as only
// the toasts and the slot guards read it.
func doctorYourHerdr(ctx context.Context, d doctorDeps) doctorCheck {
	yours := d.Config.YourHerdr()
	socket := inspTilde(yours.Socket)
	_, err := d.Yours.Ping(ctx)
	switch {
	case err == nil:
		return doctorOK("your herdr", fmt.Sprintf("your herdr session %s answers at %s (the toasts; the slot guards read your agents there)",
			yours.Name, socket))
	case herdr.NoServer(err):
		return doctorWarned("your herdr", fmt.Sprintf("no herdr server runs in your session %s (socket %s): the toasts show with "+
			"osascript, and the slot guards see only the agents herdr saved there, which it restores when it starts", yours.Name, socket),
			"start herdr (run `"+herdrStart(yours.Name)+"` in a terminal), or point [herdr] socket in config.toml at your session's socket")
	default:
		return doctorWarned("your herdr", fmt.Sprintf("your herdr session %s (socket %s) does not answer: %s; the toasts show with "+
			"osascript, and the slot guards hold every slot until it answers", yours.Name, socket, err),
			"check your herdr server (`herdr status server`); quit and start it again when no agent of yours is working")
	}
}

// doctorPanesMove warns of a move of magnum's panes to another herdr
// session: one that waits (engine.PanesMove: on what, and why), else one
// the config asks for (panesMoveDue). Either holds every new round.
func doctorPanesMove(ctx context.Context, d doctorDeps) []doctorCheck {
	if d.Store == nil || d.Config == nil {
		return nil
	}
	if mv, ok := engine.ReadPanesMove(ctx, d.Store); ok {
		return []doctorCheck{doctorWarned("herdr move", fmt.Sprintf("the move of magnum's panes from herdr session %s to %s %s "+
			"(since %s); no round starts until they move", mv.From.Name, mv.To.Name, panesMoveWait(mv), inspClock(time.Now(), mv.Since)),
			"the daemon tries again at each moment with nothing in flight: lift what holds it (`magnum logs` shows herdr.panes_move_waits)")}
	}
	if due := panesMoveDue(d.PaneSession, d.Config.ReviewHerdr()); due != "" {
		return []doctorCheck{doctorWarned("herdr move", due+": the daemon running this config moves them at its first moment "+
			"with nothing in flight, and starts no round until then", "a daemon started before the change: `magnum daemon-restart --drain`")}
	}
	return nil
}

// doctorSchemaMethods reads method names from `herdr api schema --json`.
func doctorSchemaMethods(s string) (map[string]bool, int, error) {
	var doc struct {
		Protocol int `json:"protocol"`
		Schemas  struct {
			Request struct {
				OneOf []struct {
					Properties struct {
						Method struct {
							Const string `json:"const"`
						} `json:"method"`
					} `json:"properties"`
				} `json:"oneOf"`
			} `json:"request"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, 0, err
	}
	m := map[string]bool{}
	for _, o := range doc.Schemas.Request.OneOf {
		if o.Properties.Method.Const != "" {
			m[o.Properties.Method.Const] = true
		}
	}
	if len(m) == 0 {
		return nil, 0, errors.New("no methods listed")
	}
	return m, doc.Protocol, nil
}

func doctorOrUnknown(s string) string {
	if s == "" {
		return "(unknown version)"
	}
	return s
}

func doctorPluginCheck(ctx context.Context, d doctorDeps) []doctorCheck {
	var out struct {
		Plugins []struct {
			PluginID string `json:"plugin_id"`
			Enabled  bool   `json:"enabled"`
		} `json:"plugins"`
	}
	link := "`magnum install --plugin`"
	h := d.Yours // the plugin is linked in your session
	if h == nil {
		h = d.Herdr
	}
	if err := h.Call(ctx, "plugin.list", map[string]any{}, &out); err != nil {
		return []doctorCheck{doctorWarned("plugin", "could not list herdr plugins: "+err.Error(), "start herdr, then "+link)}
	}
	for _, p := range out.Plugins {
		if p.PluginID == pluginID {
			if !p.Enabled {
				return []doctorCheck{doctorWarned("plugin", "herdr plugin "+pluginID+" is linked but disabled", "herdr plugin enable "+pluginID)}
			}
			return []doctorCheck{doctorOK("plugin", "herdr plugin "+pluginID+" is linked")}
		}
	}
	return []doctorCheck{doctorWarned("plugin", "herdr plugin "+pluginID+" is not linked (no popups, actions or PR link handler)", link)}
}
