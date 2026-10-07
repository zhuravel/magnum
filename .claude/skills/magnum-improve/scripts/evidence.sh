#!/usr/bin/env bash
# evidence.sh <since> <run-dir>
#
# Writes what magnum did since <since> (an RFC 3339 UTC time such as
# 2026-10-04T00:00:00Z, or "3d" for three days ago) into <run-dir>:
#   evidence.md   the registry's numbers and lists, read-only
#   reports.txt   the review report directories written since then
#   gitlog.txt    magnum's own commits since then
# It reads the registry with sqlite3 -readonly and never writes to it.
set -euo pipefail

since=${1:?usage: evidence.sh <since> <run-dir>}
dir=${2:?usage: evidence.sh <since> <run-dir>}
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
reviews=${MAGNUM_REVIEWS:-$HOME/.local/share/magnum/reviews}
repo=$(cd "$(dirname "$0")/../../../.." && pwd)

if [[ $since =~ ^([0-9]+)d$ ]]; then
	since=$(date -u -v-"${BASH_REMATCH[1]}"d +%Y-%m-%dT%H:%M:%SZ)
fi
mkdir -p "$dir"
out=$dir/evidence.md

q() { sqlite3 -readonly -header -markdown "$db" "$1"; }

{
	echo "# magnum since $since"
	echo
	echo "## Rounds by kind and outcome (judge runs)"
	q "select kind, coalesce(outcome, state) as outcome, count(*) as runs
	   from runs where role like '%judge%' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## Posted reviews"
	q "select substr(r.created_at, 1, 16) as at, rp.owner || '/' || rp.name || '#' || p.number as pr, r.round, r.kind,
	          r.review_event as event, json_extract(r.result_json, '\$.verdict') as verdict,
	          json_extract(r.result_json, '\$.findings') as findings, r.review_url as url
	   from runs r join prs p on p.id = r.pr_id join repos rp on rp.id = p.repo_id
	   where r.role like '%judge%' and r.outcome = 'posted' and r.created_at >= '$since' order by r.created_at"
	echo
	echo "## Findings: verdict, reason and priority"
	q "select verdict, coalesce(reason_code, '-') as reason, coalesce(severity, '-') as pri, count(*) as n
	   from findings where created_at >= '$since' group by 1, 2, 3 order by 1, 4 desc"
	echo
	echo "## Posted findings by source"
	q "select sources_json as sources, coalesce(severity, '-') as pri, count(*) as n
	   from findings where verdict = 'posted' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## Role minutes (submitted to ended)"
	q "select role, kind, count(*) as runs,
	          round(avg((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as avg_min,
	          round(max((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as max_min
	   from runs where submitted_at is not null and ended_at is not null and created_at >= '$since'
	   group by 1, 2 order by 1, 2"
	echo
	echo "## Misses (retro)"
	q "select class, coalesce(scope, '-') as scope, coalesce(severity, '-') as pri, raised, state, count(*) as n
	   from misses where created_at >= '$since' group by 1, 2, 3, 4, 5 order by 6 desc"
	q "select substr(created_at, 1, 10) as at, severity as pri, scope, title, lesson
	   from misses where class = 'miss' and created_at >= '$since' order by created_at"
	echo
	echo "## Warn and error events by kind"
	q "select level, kind, count(*) as n, max(substr(at, 1, 16)) as last
	   from events where level in ('warn', 'error') and at >= '$since' group by 1, 2 order by 3 desc limit 40"
	echo
	echo "## Operator requests (what the operator did by hand)"
	q "select kind, state, count(*) as n from requests where created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## PRs that need attention or retry now"
	q "select rp.owner || '/' || rp.name || '#' || p.number as pr, p.state, p.attempts,
	          substr(coalesce(p.last_error, ''), 1, 160) as last_error
	   from prs p join repos rp on rp.id = p.repo_id
	   where p.gh_state = 'OPEN' and (p.state = 'needs_attention' or p.attempts > 0 or p.last_error is not null)
	   order by p.state, p.attempts desc"
} >"$out"

# Report directories touched since then, newest first.
stamp=$(mktemp "$dir/.since.XXXXXX")
touch -t "$(date -j -u -f %Y-%m-%dT%H:%M:%SZ "$since" +%Y%m%d%H%M.%S)" "$stamp"
find "$reviews" -mindepth 4 -maxdepth 4 -type d -newer "$stamp" -print 2>/dev/null |
	xargs -I{} stat -f '%m %N' {} | sort -rn | cut -d' ' -f2- >"$dir/reports.txt" || true
rm -f "$stamp"

# Every reply to magnum's threads: the newest review-threads.json of each PR
# reviewed since then. The class is magnum's keyword guess only, made when the
# round wrote the file (an older round, an older classifier); people answer in
# free form (a score such as "Net: -3", a deferral, an argument), so the
# declined-findings lens reads each reply and decides its meaning itself. A
# thread says when magnum posted its finding (created_at) and in which review
# (review_id, joined to the run that posted it); "current skill" marks a
# finding whose run named the skill copy (<state>/skill/<hash>/SKILL.md) that
# the running daemon's rounds name.
python3 - "$dir/reports.txt" "$db" >>"$out" <<'PY'
import json, os, re, subprocess, sys, collections
latest = {}
for d in open(sys.argv[1]).read().split():
    pr = os.path.dirname(d)
    if pr not in latest and os.path.exists(os.path.join(d, "review-threads.json")):
        latest[pr] = os.path.join(d, "review-threads.json")

def sql(q):
    try:
        res = subprocess.run(["sqlite3", "-readonly", "-json", sys.argv[2], q], capture_output=True, text=True, check=True)
        return json.loads(res.stdout or "[]")
    except Exception:
        return []

def skill(fragment):
    m = re.match(r"([0-9a-f]{12})/SKILL\.md", fragment or "")
    return m.group(1) if m else None

frag = "case when instr(prompt_text, '/skill/') > 0 then substr(prompt_text, instr(prompt_text, '/skill/') + 7, 21) end"
posted = {}  # review id -> [run id, round, kind, skill]: the first run that names the review
for r in sql(f"select id, review_id, round, kind, {frag} as skill from runs where review_id is not null order by id"):
    p = posted.setdefault(r["review_id"], [r["id"], r["round"], r["kind"], None])
    p[3] = p[3] or skill(r["skill"])
cur = sql(f"""select {frag} as skill from runs where role like '%judge%' and instr(prompt_text, '/skill/') > 0
              and created_at >= (select value from kv where key = 'daemon.prompts_loaded_at') order by id desc limit 1""")
current = skill(cur[0]["skill"]) if cur else None

guess, rows, unanswered, under_current = collections.Counter(), [], 0, 0
for pr, f in sorted(latest.items()):
    try:
        threads = json.load(open(f))
    except Exception:
        continue
    for t in threads if isinstance(threads, list) else threads.get("threads", []):
        theirs = [r for r in t.get("replies") or [] if not r.get("own")]
        own = len(t.get("replies") or []) - len(theirs)
        if not theirs:
            unanswered += 1
            continue
        last = theirs[-1]
        guess[last.get("class") or "-"] += 1
        at = (t.get("created_at") or "-").replace("T", " ")[:16]
        run = posted.get(t.get("review_id"))
        where = f"run {run[0]}, r{run[1]} {run[2]}" if run else "-"
        if run and current and run[3] == current:
            at += ", current skill"
            under_current += 1
        text = " ".join((last.get("body") or "").split())[:400].replace("|", "/")
        rows.append(f"| {last.get('class') or '-'} | {len(theirs)}/{own} | {at} | {where} | {t.get('finding','')[:90]} | {t.get('location','')} | {t.get('url','')} | {text} |")
print("\n## Replies to magnum's threads (latest round per PR)\n")
skill_note = f", {under_current} of them posted under the current skill" if current else ""
print(f"{len(rows)} answered threads{skill_note}, {unanswered} without a reply. The class is a keyword guess made at round time, not a verdict:")
print("read every reply below and decide what it says.\n")
print("| class at round time | threads |\n|---|---|")
for k, v in guess.most_common():
    print(f"| {k} | {v} |")
print("\n### Every answered thread\n")
print("| class at round time | replies theirs/own | posted (UTC) | round | finding | where | thread | last reply (first 400 chars) |")
print("|---|---|---|---|---|---|---|---|")
print("\n".join(rows) or "| - | - | - | - | - | - | - | - |")
PY

git -C "$repo" log --since="$since" --format='%h %ad %<(150,trunc)%s' --date=short >"$dir/gitlog.txt"

echo "$out ($(wc -l <"$out" | tr -d ' ') lines), $(wc -l <"$dir/reports.txt" | tr -d ' ') report dirs, $(wc -l <"$dir/gitlog.txt" | tr -d ' ') commits"
