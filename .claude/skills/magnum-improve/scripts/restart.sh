#!/usr/bin/env bash
# restart.sh [--migration]
#
# Puts the daemon on master's build without cutting a round short.
#   no flag      build bin/magnum, then restart the daemon at the first
#                moment no round is in flight (daemon-restart --when-idle).
#   --migration  the build adds a migration, so a new binary cannot run CLI
#                commands (the board included) until the daemon restarts on
#                it: pause automation with the running binary, wait until no
#                round is in flight, build, restart, resume.
# Run it in the background; it can wait a long time.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../../../.." && pwd)
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
cd "$repo"

inflight() {
	sqlite3 -readonly "$db" "select count(*) from prs where state in ('claiming','reviewing','verifying')"
}

if [[ ${1:-} != --migration ]]; then
	make build >/dev/null
	bin/magnum daemon-restart --when-idle --timeout 120m 2>&1 | tail -1
	exit 0
fi

# The running binary pauses; a config key only the new build knows can make it
# refuse, and then the wait below still holds the restart until rounds end.
bin/magnum pause --for 60m --reason "restart onto a build with a migration" 2>&1 | tail -1 || true
for _ in $(seq 1 120); do
	[[ $(inflight) == 0 ]] && break
	sleep 30
done
if [[ $(inflight) != 0 ]]; then
	echo "restart: rounds still in flight after an hour; nothing restarted (magnum resume lifts the pause)" >&2
	exit 1
fi
make build >/dev/null
bin/magnum daemon-restart --when-idle --timeout 5m 2>&1 | tail -1
sleep 20
bin/magnum resume 2>&1 | tail -1
bin/magnum status 2>&1 | sed -n '1,2p;/^build/p;/pauses/,/^$/p'
