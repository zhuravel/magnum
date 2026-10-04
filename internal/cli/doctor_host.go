package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/launchd"
)

// doctorStagingWarn is the ~/.codex/.tmp/marketplaces/.staging size worth a warning.
const doctorStagingWarn = 1 << 30

// doctorLoginShell checks that a non-interactive login shell sees mise's
// tools. Codex and Claude run their tool commands as `zsh -lc …`, which skips
// .zshrc, and macOS's /etc/zprofile puts /usr/bin first, so the Ruby, Node or
// Python a review needs must come from mise shims activated in ~/.zprofile.
//
// Without mise (not on PATH, no [[pool]], a LaunchAgent that does not use
// it) there is nothing to check and it reports SKIP.
func doctorLoginShell(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "login shell"
	if doctorMiseUse(ctx, d) == "" {
		return []doctorCheck{doctorSkipped(name, "mise is not on PATH and neither a [[pool]] nor the LaunchAgent uses it: no shims to check")}
	}
	shims := filepath.Join(d.UserHome, ".local", "share", "mise", "shims")
	if d.Getenv != nil {
		if v := d.Getenv("MISE_DATA_DIR"); v != "" {
			shims = filepath.Join(v, "shims")
		}
	}
	out, err := doctorExec(ctx, d, 15*time.Second, "", nil, "zsh", "-lc", "print -r -- $PATH")
	if err != nil {
		return []doctorCheck{doctorWarned(name, "`zsh -lc 'print -r -- $PATH'` failed: "+err.Error(), "")}
	}
	fix := "add `eval \"$(mise activate zsh --shims)\"` to ~/.zprofile (agent tool shells are `zsh -lc`: no .zshrc, /usr/bin first)"
	entries := strings.Split(strings.TrimSpace(inspFirstLine(out)), ":")
	shimsAt, usrBinAt := -1, len(entries)
	for i, e := range entries {
		switch {
		case e == shims && shimsAt < 0:
			shimsAt = i
		case e == "/usr/bin" && usrBinAt == len(entries):
			usrBinAt = i
		}
	}
	switch {
	case shimsAt < 0:
		return []doctorCheck{doctorFailed(name, "`zsh -lc` does not have "+inspTilde(shims)+" on PATH: agents' tool commands see the system Ruby/Node", fix)}
	case shimsAt > usrBinAt:
		return []doctorCheck{doctorFailed(name, "`zsh -lc` lists "+inspTilde(shims)+" after /usr/bin: agents' tool commands see the system Ruby/Node", fix)}
	}
	return []doctorCheck{doctorOK(name, "`zsh -lc` resolves mise shims before /usr/bin")}
}

func doctorDisk(_ context.Context, d doctorDeps) []doctorCheck {
	minGB := d.Config.Daemon.MinFreeDiskGB
	for _, p := range d.Config.Pools {
		minGB = max(minGB, p.MinFreeDiskGB)
	}
	path := d.UserHome
	if path == "" {
		path = d.Layout.Home
	}
	if d.DiskFree == nil {
		return nil
	}
	free, err := d.DiskFree(path)
	if err != nil {
		return []doctorCheck{doctorWarned("disk", "could not read free disk space: "+err.Error(), "")}
	}
	detail := fmt.Sprintf("%s free on the disk holding %s (min %d GB)", inspBytes(int64(free)), inspTilde(path), minGB)
	if minGB > 0 && free < uint64(minGB)<<30 {
		return []doctorCheck{doctorFailed("disk", detail+": provisioning is refused",
			"free space: `magnum cleanup`, `magnum cleanup --shrink`, or lower min_free_disk_gb in config.toml")}
	}
	return []doctorCheck{doctorOK("disk", detail)}
}

func doctorStaging(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "codex staging"
	if d.UserHome == "" {
		return nil
	}
	dir := filepath.Join(d.UserHome, ".codex", ".tmp", "marketplaces", ".staging")
	if _, err := os.Stat(dir); err != nil {
		return []doctorCheck{doctorOK(name, inspTilde(dir)+" is absent")}
	}
	out, err := doctorExec(ctx, d, 30*time.Second, "", nil, "du", "-sk", dir)
	if err != nil {
		return []doctorCheck{doctorWarned(name, "could not size "+inspTilde(dir)+": "+err.Error(), "")}
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return []doctorCheck{doctorWarned(name, "could not size "+inspTilde(dir), "")}
	}
	kb, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return []doctorCheck{doctorWarned(name, "could not size "+inspTilde(dir)+": "+err.Error(), "")}
	}
	detail := fmt.Sprintf("Codex marketplace staging %s is %s", inspTilde(dir), inspBytes(kb*1024))
	if kb*1024 >= doctorStagingWarn {
		return []doctorCheck{doctorWarned(name, detail+" (every Codex start clones into it)",
			"while no Codex runs: rm -rf "+inspTilde(dir))}
	}
	return []doctorCheck{doctorOK(name, detail)}
}

func doctorLaunchd(ctx context.Context, d doctorDeps) []doctorCheck {
	if d.Launchd == nil {
		return nil
	}
	info, err := d.Launchd(ctx)
	label := launchd.DefaultLabel
	switch {
	case err != nil:
		return []doctorCheck{doctorWarned("launchd", "could not ask launchctl about "+label+": "+err.Error(), "")}
	case info.State == launchd.Running:
		return []doctorCheck{doctorOK("launchd", fmt.Sprintf("launchd job %s running (pid %d)", label, info.PID))}
	case info.State == launchd.NotLoaded:
		return []doctorCheck{doctorWarned("launchd", "launchd job "+label+" is not loaded: the daemon does not run in the background",
			"magnum install (writes the LaunchAgent and starts it)")}
	}
	detail := fmt.Sprintf("launchd job %s is %s", label, info.State)
	if info.Exited {
		detail += fmt.Sprintf(" (last exit code %d)", info.LastExitCode)
	}
	return []doctorCheck{doctorWarned("launchd", detail, "read `magnum logs` and "+inspTilde(launchd.LogPath(d.Layout.Logs()))+
		" (launchd's capture of crashes), then `launchctl kickstart -k gui/"+strconv.Itoa(os.Getuid())+"/"+label+"`")}
}
