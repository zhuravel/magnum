#!/usr/bin/env bash
# evidence.sh <since> <run-dir>
#
# Writes what magnum did since <since> (an RFC 3339 UTC time such as
# 2026-10-04T00:00:00Z, or "3d" for three days ago) into <run-dir>:
#   evidence.md   the registry's numbers and lists, read-only
#   reports.txt   the review report directories written since then
#   gitlog.txt    magnum's own commits since then
#   claude-review-origin.txt   each rejected finding claude-review alone raised, with whether it matches a
#                 finding of the reviewer's report, a bullet of its Rejected list, or neither, and the text it
#                 matched
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

# miss_from says where a miss row came from: a declined or argued finding of magnum's own that the
# retro judged missing_context at repo scope is also stored as a miss (so the notes curation takes it)
# with the declines row's source_url. Those are magnum's own threads, not what a human reviewer
# raised, and the Misses section counts them apart.
miss_from="case when exists (select 1 from declines d where d.source_url = misses.source_url)
	then 'declined finding' else 'comment' end"

# origin says where each rejected finding that claude-review alone raised came from. A claude-review.md report
# ends with a "Rejected" list: candidates the reviewer dropped itself, with its reason, and the judge re-judges them like
# any claim (skills/magnum-review/SKILL.md), so the ledger holds each one as a rejected finding with the
# reviewer as its only source. In one window about two thirds of the rejected rows of claude-review alone were
# such bullets, and the findings tables then read as a reviewer far less precise than it is. Neither the ledger
# (codex-judge.json) nor the registry marks them, so the row's title is matched against the reviewer's report:
# the cosine of their word stems, a title against a bullet's claim (its words before the first colon) or
# against a finding's heading ("Finding 2", "3.", "P2:"); the judge words its titles freely:
#   self       the best match is a bullet of the Rejected list, at 0.2 or more (a tie goes to the bullet);
#   reported   the best match is a finding of the report, at 0.3 or more;
#   unmatched  neither (a bullet the judge reworded past recognition, or something of the judge's own);
#   no report  the claude-review.md of the row's report directory is gone.
# It is a heuristic, not a fact: claude-review-origin.txt lists each row with the text it matched, to check by
# eye. A posted finding is not classified (its title matches a bullet about the same code as often as not). The
# first column is findings.id; the classes go to the SQL below as a VALUES list.
origin=$(mktemp "$dir/.origin.XXXXXX")
python3 - "$db" "$since" "$dir/claude-review-origin.txt" >"$origin" <<'PY' || {
import json, os, re, subprocess, sys

db, since, audit = sys.argv[1:4]
SELF_MIN, REPORTED_MIN = 0.2, 0.3
STOP = set("""the and for with without that this these those from into than then but not does did can could would
should may might will has have had are was were been being its their there here when while which what who whom also
only still new now more most other any all each such via per use used uses using make makes made get gets got one
two rather instead already even just""".split())
ITEM = re.compile(r"^\s*(?:[-*]|\d+[.)])\s+")
FINDING = re.compile(r"^(?:findings?\b|\d+[.):]\s|[A-Z]?\d+[.)]\s|P[0-3]\b|issue\s+\d)", re.I)


def sql(q):
    r = subprocess.run(["sqlite3", "-readonly", "-json", db, q], capture_output=True, text=True, check=True)
    return json.loads(r.stdout or "[]")


def stem(w):
    for suf in ("ing", "ed", "es", "s"):
        if w.endswith(suf) and len(w) - len(suf) >= 4:
            return w[: -len(suf)]
    return w


def toks(s):
    words = re.sub(r"[^a-z0-9]+", " ", s.lower()).split()
    return {stem(w) for w in words if len(w) > 2 and not w.isdigit() and w not in STOP}


def cos(a, b):
    return len(a & b) / ((len(a) * len(b)) ** 0.5) if a and b else 0.0


def lead(text):
    m = re.search(r":\s", text[:260])
    return text[: m.start()] if m else text[:260]


def parse(text):
    """The bullets of the report's Rejected section, and its findings: a section whose heading names one
    (Finding 2, 3., P2:), with its paragraphs, down to a heading of its own level or higher."""
    bullets, chunks = [], []
    in_rej, rej_level, root, root_level, para, cur = False, 0, "", 0, [], None

    def flush_para():
        nonlocal para
        if para and root:
            chunks.append((root + " " + " ".join(para))[:900])
        para = []

    def flush_bullet():
        nonlocal cur
        if cur is not None:
            bullets.append(cur)
        cur = None

    for line in text.splitlines():
        h = re.match(r"^(#{1,6})\s+(.*)$", line)
        if h:
            flush_para()
            flush_bullet()
            level, title = len(h.group(1)), h.group(2)
            if re.match(r"rejected\b", title, re.I):
                in_rej, rej_level, root = True, level, ""
                continue
            if in_rej and level <= rej_level:
                in_rej = False
            if in_rej:
                continue
            if FINDING.match(title):
                root, root_level = title, level
                chunks.append(title)
            elif root and level > root_level:
                pass  # a subsection of the finding
            elif level >= 2 and re.match(r"findings?\b", title, re.I):
                root, root_level = title, level
            else:
                root, root_level = "", 0
        elif in_rej:
            if ITEM.match(line):
                flush_bullet()
                cur = ITEM.sub("", line).strip()
            elif cur is not None and line.strip():
                cur += " " + line.strip()
        elif not line.strip():
            flush_para()
        else:
            para.append(line.strip())
    flush_para()
    flush_bullet()
    return bullets, chunks


def best(t, base, items, view):
    top, text = 0.0, ""
    for x in items:
        score = view(t, x) + (0.1 if base and base in x else 0.0)
        if score > top:
            top, text = score, x
    return top, text


def clip(s, n):
    return " ".join((s or "").split())[:n]


rows = sql(f"""select f.id, f.run_id, f.finding_id, f.verdict, coalesce(f.severity, '-') as pri, f.path, f.title,
                      r.report_path
               from findings f join runs r on r.id = f.run_id
               where f.sources_json = '["claude-review"]' and f.verdict = 'rejected' and f.created_at >= '{since}'
               order by f.id""")
reports, values, lines = {}, [], []
for r in rows:
    path = os.path.join(os.path.dirname(r["report_path"] or ""), "claude-review.md")
    if path not in reports:
        try:
            with open(path, encoding="utf-8", errors="replace") as f:
                reports[path] = parse(f.read())
        except OSError:
            reports[path] = None
    cls, score, text = "no report", 0.0, ""
    if reports[path] is not None:
        bullets, chunks = reports[path]
        t, base = toks(r["title"] or ""), os.path.basename(r["path"] or "")
        sb, tb = best(t, base, bullets, lambda t, x: max(cos(t, toks(lead(x))), 0.8 * cos(t, toks(x))))
        sr, tr = best(t, base, chunks, lambda t, x: cos(t, toks(lead(x))) if len(x) < 200 else 0.8 * cos(t, toks(x[:400])))
        if sb >= SELF_MIN and sb >= sr:
            cls, score, text = "self", sb, tb
        elif sr >= REPORTED_MIN:
            cls, score, text = "reported", sr, tr
        else:
            cls, score, text = "unmatched", max(sb, sr), tb if sb >= sr else tr
    values.append("(%d,'%s')" % (r["id"], cls))
    lines.append("\t".join([str(r["id"]), cls, "%.2f" % score, r["run_id"], r["finding_id"], r["verdict"], r["pri"],
                            clip(r["path"], 80), clip(r["title"], 100), clip(text, 160)]))
with open(audit, "w") as f:
    f.write("id\tclass\tscore\trun\tfinding\tverdict\tpri\tpath\ttitle\tmatched\n")
    f.write("\n".join(lines) + ("\n" if lines else ""))
print(",".join(values))
PY
	echo "evidence.sh: could not tell where claude-review's findings came from; the self columns stay empty" >&2
	: >"$origin"
}
cls=$(<"$origin")
rm -f "$origin"
cls=${cls:-"(0,'')"}
# with_cls prefixes a query with the class (cls.c) of each rejected finding claude-review alone raised (cls.id =
# findings.id).
with_cls="with cls(id, c) as (values $cls)"

{
	echo "# magnum since $since"
	echo
	echo "## Rounds by kind and outcome (judge runs)"
	q "select kind, coalesce(outcome, state) as outcome, count(*) as runs
	   from runs where role like '%judge%' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## Posted reviews"
	echo
	echo "A nudge run whose review id another run here carries has no row of its own."
	echo
	q "select substr(r.created_at, 1, 16) as at, rp.owner || '/' || rp.name || '#' || p.number as pr, r.round, r.kind,
	          r.review_event as event, json_extract(r.result_json, '\$.verdict') as verdict,
	          json_extract(r.result_json, '\$.findings') as findings, r.review_url as url
	   from runs r join prs p on p.id = r.pr_id join repos rp on rp.id = p.repo_id
	   where r.role like '%judge%' and r.outcome = 'posted' and r.created_at >= '$since'
	     and not (r.kind = 'nudge' and exists (select 1 from runs o
	           where o.review_id = r.review_id and o.id <> r.id and o.role like '%judge%' and o.outcome = 'posted'
	             and o.created_at >= '$since'))
	   order by r.created_at"
	echo
	echo "## Findings: verdict, reason and priority"
	echo
	echo "\`self\`: of those rejected rows, the ones claude-review alone raised that its report's own Rejected list"
	echo "holds (claude-review-origin.txt; a heuristic match, see evidence.sh). The reviewer dropped those itself and"
	echo "the judge only re-checked them: leave them out when you judge a reviewer's precision."
	echo
	q "$with_cls select f.verdict, coalesce(f.reason_code, '-') as reason, coalesce(f.severity, '-') as pri,
	          count(*) as n, coalesce(sum(f.verdict = 'rejected' and cls.c = 'self'), 0) as self
	   from findings f left join cls on cls.id = f.id
	   where f.created_at >= '$since' group by 1, 2, 3 order by 1, 4 desc"
	echo
	echo "## Posted findings by source"
	q "select sources_json as sources, coalesce(severity, '-') as pri, count(*) as n
	   from findings where verdict = 'posted' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## claude-review alone: what its rejected findings are (priority)"
	echo
	echo "The findings whose only source is claude-review. A rejected one is \`reported\` when it matches a finding"
	echo "of the reviewer's report (the judge's rejection of something the reviewer reported), \`self\` when it"
	echo "matches a bullet of the report's Rejected list, \`unmatched\` when neither (a bullet reworded past"
	echo "recognition, or the judge's own), \`no report\` when the report is gone. The reviewer's precision lies"
	echo "between posted / (posted + reported + unmatched) and posted / (posted + reported)."
	echo
	q "$with_cls select coalesce(f.severity, '-') as pri,
	          sum(f.verdict = 'posted') as posted,
	          sum(f.verdict = 'rejected' and cls.c = 'reported') as \"rejected (reported)\",
	          sum(f.verdict = 'rejected' and cls.c = 'self') as \"rejected (self)\",
	          sum(f.verdict = 'rejected' and cls.c = 'unmatched') as \"rejected (unmatched)\",
	          sum(f.verdict = 'rejected' and cls.c = 'no report') as \"rejected (no report)\"
	   from findings f left join cls on cls.id = f.id
	   where f.sources_json = json_array('claude-review') and f.created_at >= '$since' group by 1 order by 1"
	echo
	echo "## Role minutes (submitted to ended)"
	q "select role, kind, count(*) as runs,
	          round(avg((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as avg_min,
	          round(max((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as max_min
	   from runs where submitted_at is not null and ended_at is not null and created_at >= '$since'
	   group by 1, 2 order by 1, 2"
	echo
	echo "## Misses (retro)"
	q "select class, coalesce(scope, '-') as scope, coalesce(severity, '-') as pri, raised, state, $miss_from as \"from\",
	          count(*) as n
	   from misses where created_at >= '$since' group by 1, 2, 3, 4, 5, 6 order by 7 desc"
	q "select substr(created_at, 1, 10) as at, severity as pri, scope, $miss_from as \"from\", title, lesson
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
# the running daemon's rounds name: the newest judge run since the daemon
# loaded its prompts (kv daemon.prompts_loaded_at) tells which. Right after a
# restart no judge has run yet; then, when nothing changed on disk since the
# daemon loaded (kv daemon.prompts_changed absent), the hash is the first 12
# hex characters of the SHA-256 of $repo/skills/magnum-review/SKILL.md, the
# name the daemon gave its copy, and the summary says so. "net" is the score
# the author's last reply gives the fix ("Net +3", "Net: -2", "net 0", "Low
# priority (Net 0)"; a minus may be U+2212), as "+3", "-2", "0" or "+0.5",
# empty when the reply names none. "by" says who replied, in order: "fixer"
# for magnum's fixer, which posts as the PR's author and ends every reply
# with <!-- magnum:fix --> (skills/magnum-fix/SKILL.md), "own" for magnum's
# reviewer, "person" for anyone else, and "person?" for a reply the file cut
# at 600 characters without the marker in what it kept, which may still be
# the fixer's. A marked reply counts as an answer even when the file calls
# it own (a watch that posts as the fixer's login).
python3 - "$dir/reports.txt" "$db" "$repo" >>"$out" <<'PY'
import hashlib, json, os, re, subprocess, sys, collections
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

NET = re.compile(r"\bnet\b:?\s*([-+−]?)\s*(\d+(?:\.\d+)?)(?!\w)", re.I)

def net(body):
    m = NET.search(body or "")
    if not m:
        return ""
    num = m.group(2)
    if float(num) == 0:
        return "0"
    return ("-" if m.group(1) in ("-", "−") else "+") + num

frag = "case when instr(prompt_text, '/skill/') > 0 then substr(prompt_text, instr(prompt_text, '/skill/') + 7, 21) end"
posted = {}  # review id -> [run id, round, kind, skill]: the first run that names the review
for r in sql(f"select id, review_id, round, kind, {frag} as skill from runs where review_id is not null order by id"):
    p = posted.setdefault(r["review_id"], [r["id"], r["round"], r["kind"], None])
    p[3] = p[3] or skill(r["skill"])
cur = sql(f"""select {frag} as skill from runs where role like '%judge%' and instr(prompt_text, '/skill/') > 0
              and created_at >= (select value from kv where key = 'daemon.prompts_loaded_at') order by id desc limit 1""")
current, from_file = (skill(cur[0]["skill"]) if cur else None), False
changed = sql("select count(*) as n from kv where key = 'daemon.prompts_changed'")
if not cur and changed and changed[0]["n"] == 0:
    try:
        with open(os.path.join(sys.argv[3], "skills", "magnum-review", "SKILL.md"), "rb") as f:
            current, from_file = hashlib.sha256(f.read()).hexdigest()[:12], True
    except OSError:
        pass

FIX = "<!-- magnum:fix -->"

def who(r):
    body = r.get("body") or ""
    if FIX in body:
        return "fixer"
    if r.get("own"):
        return "own"
    return "person?" if r.get("truncated") else "person"

guess, by, rows, unanswered, under_current = collections.Counter(), collections.Counter(), [], 0, 0
for pr, f in sorted(latest.items()):
    try:
        threads = json.load(open(f))
    except Exception:
        continue
    for t in threads if isinstance(threads, list) else threads.get("threads", []):
        replies = t.get("replies") or []
        theirs = [r for r in replies if who(r) != "own"]
        own = len(replies) - len(theirs)
        if not theirs:
            unanswered += 1
            continue
        last = theirs[-1]
        repliers = []
        for r in replies:
            if not repliers or repliers[-1] != who(r):
                repliers.append(who(r))
        guess[last.get("class") or "-"] += 1
        at = (t.get("created_at") or "-").replace("T", " ")[:16]
        run = posted.get(t.get("review_id"))
        where = f"run {run[0]}, r{run[1]} {run[2]}" if run else "-"
        if run and current and run[3] == current:
            at += ", current skill"
            under_current += 1
        text = " ".join((last.get("body") or "").split())[:400].replace("|", "/")
        by[who(last)] += 1
        rows.append(f"| {last.get('class') or '-'} | {net(last.get('body'))} | {len(theirs)}/{own} | {', '.join(repliers)} | {at} | {where} | {t.get('finding','')[:90]} | {t.get('location','')} | {t.get('url','')} | {text} |")
print("\n## Replies to magnum's threads (latest round per PR)\n")
skill_note = f", {under_current} of them posted under the current skill" if current else ""
if from_file:
    skill_note += f" ({current}, from SKILL.md: no judge ran since the restart)"
print(f"{len(rows)} answered threads{skill_note}, {unanswered} without a reply. The class is a keyword guess made at round time, not a verdict:")
print("read every reply below and decide what it says.\n")
print("| class at round time | threads |\n|---|---|")
for k, v in guess.most_common():
    print(f"| {k} | {v} |")
print("\n| last reply by | threads |\n|---|---|")
for k, v in by.most_common():
    print(f"| {k} | {v} |")
print("\n### Every answered thread\n")
print("| class at round time | net | replies theirs/own | by | posted (UTC) | round | finding | where | thread | last reply (first 400 chars) |")
print("|---|---|---|---|---|---|---|---|---|---|")
print("\n".join(rows) or "| - | - | - | - | - | - | - | - | - | - |")
PY

git -C "$repo" log --since="$since" --format='%h %ad %<(150,trunc)%s' --date=short >"$dir/gitlog.txt"

echo "$out ($(wc -l <"$out" | tr -d ' ') lines), $(wc -l <"$dir/reports.txt" | tr -d ' ') report dirs, $(wc -l <"$dir/gitlog.txt" | tr -d ' ') commits"
