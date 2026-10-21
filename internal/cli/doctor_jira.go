package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/jira"
)

// doctorJira checks [jira]: off is a SKIP; on, the site's scheme and the
// token file (jira.ReadToken: it exists, only its owner can open it, it
// holds one token). A problem warns, since a round goes on without a
// ticket. Only MAGNUM_LIVE_JIRA=1 asks Jira whether it accepts the account
// (jira.Client.Myself, GET /rest/api/2/myself): doctor makes no network
// call otherwise, and the gate never sets it.
func doctorJira(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "jira"
	j := d.Config.Jira
	if !j.On() {
		return []doctorCheck{doctorSkipped(name, "Jira tickets are off ([jira] url is not set)")}
	}
	if !strings.HasPrefix(j.URL, "https://") {
		return []doctorCheck{doctorFailed(name, fmt.Sprintf("jira.url %q is not an https:// URL", j.URL), "set [jira] url to the site's https:// URL")}
	}
	if _, err := jira.ReadToken(j.TokenFile); err != nil {
		return []doctorCheck{doctorWarned(name, fmt.Sprintf("%v: rounds hand the judge no Jira ticket", err),
			fmt.Sprintf("write the Jira account's API token alone into %s, then chmod 600 %s", j.TokenFile, j.TokenFile))}
	}
	site := fmt.Sprintf("Jira tickets come from %s as %s", j.URL, j.User)
	if d.Getenv == nil || d.Getenv("MAGNUM_LIVE_JIRA") != "1" {
		return []doctorCheck{doctorOK(name, site+"; the token file is private (MAGNUM_LIVE_JIRA=1 also asks Jira whether it accepts it)")}
	}
	c := &jira.Client{Runner: d.Run, URL: j.URL, User: j.User, TokenFile: j.TokenFile}
	if err := c.Myself(ctx); err != nil {
		fix := "check [jira] url and user, and the token in " + j.TokenFile
		if je, ok := errors.AsType[*jira.Error](err); ok && je.Class == jira.ClassUnauthorized {
			fix = "create a new API token for " + j.User + " and write it into " + j.TokenFile
		}
		return []doctorCheck{doctorWarned(name, fmt.Sprintf("%s, but %v", site, err), fix)}
	}
	return []doctorCheck{doctorOK(name, site+"; Jira accepts the account's token")}
}
