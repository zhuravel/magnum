package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

func doctorHerdrChecks(ctx context.Context, d doctorDeps) []doctorCheck {
	socket := inspTilde(d.Config.Herdr.Socket)
	info, err := d.Herdr.Ping(ctx)
	var out []doctorCheck
	if err != nil {
		out = append(out, doctorFailed("herdr", "herdr socket "+socket+" does not answer: "+err.Error(),
			"start herdr (run `herdr` in a terminal), or point [herdr] socket in config.toml at the right socket"))
	} else {
		out = append(out, doctorOK("herdr", fmt.Sprintf("herdr server %s (protocol %d) at %s", info.Version, info.Protocol, socket)))
		if info.Protocol != 0 && info.Protocol != herdr.Protocol {
			out = append(out, doctorWarned("herdr protocol", fmt.Sprintf("herdr speaks protocol %d; magnum was verified against %d", info.Protocol, herdr.Protocol),
				"run magnum's tests against this herdr (go test ./internal/herdr) before trusting automation"))
		}
	}
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
	link := "herdr plugin link " + inspTilde(d.Layout.Home) + " (or `magnum install --plugin`)"
	if err := d.Herdr.Call(ctx, "plugin.list", map[string]any{}, &out); err != nil {
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
