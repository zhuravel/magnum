#!/usr/bin/env bash
# restart.sh [--migration] [--max-wait D]
#
# Puts the daemon on master's build without cutting a round short, and
# without new rounds starting while it waits for the ones in flight.
#   no flag       build bin/magnum, then `daemon-restart --drain`: the daemon
#                 starts no new round (requested ones included), the rounds in
#                 flight end, and it restarts on the build.
#   --migration   the build adds a migration, so a new binary cannot run CLI
#                 commands (the board included) until the daemon restarts on
#                 it: the running binary pauses automatic reviews and the
#                 script waits for the rounds in flight while the board still
#                 works; then it builds, and the new binary's `daemon-restart
#                 --drain` (which runs under the older daemon) holds every
#                 round, waits for one a request started meanwhile, restarts;
#                 then resume.
#   --max-wait D  how long to wait for the rounds in flight, in minutes or
#                 hours (default 30m); after it nothing is restarted.
# While it waits it prints the rounds in flight every 5 minutes. Run it in the
# background.
set -euo pipefail

usage="usage: restart.sh [--migration] [--max-wait D]"
migration=false
max_wait=30m
while (($#)); do
    case $1 in
    --migration) migration=true; shift ;;
    --max-wait) [[ $# -ge 2 ]] || { echo "$usage" >&2; exit 2; }; max_wait=$2; shift 2 ;;
    --max-wait=*) max_wait=${1#*=}; shift ;;
    *) echo "restart: unknown argument $1" >&2; echo "$usage" >&2; exit 2 ;;
    esac
done
if [[ $max_wait =~ ^([0-9]+)m$ ]]; then
    max_secs=$((10#${BASH_REMATCH[1]} * 60))
elif [[ $max_wait =~ ^([0-9]+)h$ ]]; then
    max_secs=$((10#${BASH_REMATCH[1]} * 3600))
else
    max_secs=0
fi
((max_secs > 0)) || { echo "restart: --max-wait takes minutes or hours, as 30m or 2h" >&2; exit 2; }

repo=$(cd "$(dirname "$0")/../../../.." && pwd)
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
cd "$repo"

# inflight lists the rounds in flight, "owner/repo#N (state), ...".
inflight() {
    sqlite3 -readonly "$db" "select coalesce(group_concat(x, ', '), '') from (select r.owner || '/' || r.name || '#' ||
        p.number || ' (' || p.state || ')' as x from prs p join repos r on r.id = p.repo_id
        where p.state in ('claiming', 'reviewing', 'verifying') order by x)"
}

# every5m passes the drain's output through, its progress lines (one every
# 15 s, each naming the rounds) once per 5 minutes.
every5m() {
    local line last=$SECONDS
    while IFS= read -r line; do
        if [[ $line == "draining: "* ]]; then
            ((SECONDS - last >= 300)) || continue
            last=$SECONDS
        fi
        printf '%s\n' "$line"
    done
}

# drain_restart restarts the daemon on bin/magnum once no round is in flight,
# holding new rounds meanwhile, for at most $1.
drain_restart() {
    bin/magnum daemon-restart --drain --timeout "$1" 2>&1 | every5m
}

if ! $migration; then
    make build >/dev/null
    drain_restart "$max_wait"
    exit 0
fi

# The running binary pauses; a config key only the new build knows can make it
# refuse, and then the wait below still holds the build until rounds end.
bin/magnum pause --for "$((max_secs / 60 + 30))m" --reason "restart onto a build with a migration" 2>&1 | tail -1 || true
start=$SECONDS
next=$((SECONDS + 300))
while :; do
    rounds=$(inflight) || rounds="(the registry is busy)"
    [[ -z $rounds ]] && break
    if ((SECONDS - start >= max_secs)); then
        echo "restart: rounds still in flight after $max_wait: $rounds; nothing built or restarted (magnum resume lifts the pause)" >&2
        exit 1
    fi
    if ((SECONDS >= next)); then
        echo "restart: waiting $(((SECONDS - start) / 60))m for $rounds"
        next=$((SECONDS + 300))
    fi
    sleep 30
done
make build >/dev/null
left=$((max_secs - (SECONDS - start)))
((left >= 300)) || left=300
if ! drain_restart "${left}s"; then
    echo "restart: the daemon still runs the older schema, so bin/magnum refuses other commands until it restarts:" \
        "run bin/magnum daemon-restart --drain, then bin/magnum resume" >&2
    exit 1
fi
sleep 20
bin/magnum resume 2>&1 | tail -1
bin/magnum status 2>&1 | sed -n '1,2p;/^build/p;/pauses/,/^$/p'
