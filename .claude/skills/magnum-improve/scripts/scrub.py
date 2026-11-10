"""The scrub the factory runs before it keeps text, for the improvement loop's evidence.sh.

It is internal/factory/runtime/scrub.go after execx.Redact: GitHub tokens, JWTs, Authorization values and private keys;
AWS, Slack, OpenAI, Anthropic, GitLab, Stripe and Google keys; the values of password=, token: and the like (the key
stays); emails; host names and IPv4 addresses. Each pattern is the Go one word for word, which selftest.sh checks, and
go() compiles it with Go's meaning. It masks every byte the Go scrub masks, and more only where a run of host
characters is longer than a host name may be; FuzzEvidenceScrubMasksEveryByteScrubMasks (internal/factory/runtime)
feeds both the same texts and fails on a byte it shows that the Go scrub masked.

Python's engine, unlike Go's, backtracks: the JWT pattern on an eyJ repeated, the email pattern on a long run of the
characters an address is made of and the host pattern on a dotted run take minutes on a long word. So the JWT and the
email patterns skip the starts that must fail as an earlier one in the same run failed, which is no change to what
they match, and the host pattern runs on each run of host characters, a run longer than a host name (253 characters)
masked whole. No text is dropped or cut before the masks run: a word dropped for speed took with it a quote, a key, a
-----BEGIN or an Authorization: that opens a secret going on past the word, and the rest of that secret showed.

The secret, token and key-value patterns run on the whole text, in the Go scrub's order, since a private key, an
Authorization value and a key's value (on the next line, or quoted over lines) cross a line break; emails and hosts,
which cannot, run on each line kept.

python3 scrub.py --serve is the fuzz's side: records of a 4-byte big-endian length and a text's UTF-8 bytes in, the
same for the scrubbed text out, until its input ends.
"""
import re
import sys

# Go's \s; Python's has the vertical tab too, and Go's \S takes it.
GO_SPACE = "\\t\\n\\f\\r "
# The letters Go's (?i) folds to a rune Python's ASCII mode does not: s to the long s, k to the Kelvin sign.
GO_FOLDS = {"s": "\u017f", "k": "\u212a"}


def go(pattern):
    """A Go pattern (RE2 syntax) in Python's, compiled with re.ASCII, to match what Go's regexp matches: \\s and \\S
    are Go's (no vertical tab), $ is the end of the text, and (?i) folds s and k as Go does. It knows the constructs
    the scrub's patterns use, and fails on another flag, a named group, a letter's escape in a class but \\s (and
    [\\s\\S]), or a negated class under (?i) that holds s or k."""
    out, i, fold, groups = [], 0, False, []
    if pattern.startswith("(?i)"):
        out.append("(?i)")
        i, fold = 4, True
    while i < len(pattern):
        c = pattern[i]
        if c == "\\":
            e = pattern[i : i + 2]
            out.append({"\\s": "[" + GO_SPACE + "]", "\\S": "[^" + GO_SPACE + "]", "\\z": "\\Z"}.get(e, e))
            i += 2
        elif c == "[":
            j = i + 1 + pattern.startswith("[^", i)
            j += pattern[j] == "]"
            while pattern[j] != "]":
                j += 2 if pattern[j] == "\\" else 1
            out.append(go_class(pattern[i : j + 1], fold))
            i = j + 1
        elif c == "(":
            groups.append(fold)
            for head in ("(?i:", "(?:", "("):
                if pattern.startswith(head, i):
                    break
            if head == "(" and pattern.startswith("(?", i):
                raise ValueError("go: a group go() does not know in %r" % pattern)
            fold = fold or head == "(?i:"
            out.append(head)
            i += len(head)
        elif c == ")":
            fold = groups.pop()
            out.append(c)
            i += 1
        elif c == "$":
            out.append("\\Z")
            i += 1
        elif fold and c.lower() in GO_FOLDS:
            out.append("[" + c.lower() + GO_FOLDS[c.lower()] + "]")
            i += 1
        else:
            out.append(c)
            i += 1
    return re.compile("".join(out), re.ASCII)


def go_class(cls, fold):
    """A Go character class in Python's: \\s as Go's, and under (?i) the long s and the Kelvin sign beside s and k."""
    if cls == "[\\s\\S]":
        return cls
    neg = cls.startswith("[^")
    body, members, k = cls[2 if neg else 1 : -1], [], 0
    while k < len(body):
        if body[k] == "\\":
            e = body[k : k + 2]
            if e == "\\S" or e[1:].isalpha() and e != "\\s":
                raise ValueError("go: %s in a class" % e)
            members.append(GO_SPACE if e == "\\s" else e)
            k += 2
        elif k + 2 < len(body) and body[k + 1] == "-":
            members.append(body[k : k + 3])
            k += 3
        else:
            members.append(body[k])
            k += 1

    def holds(letter):
        return any(m == letter or len(m) == 3 and m[0] <= letter <= m[2] for m in members)

    extra = "".join(f for c, f in GO_FOLDS.items() if fold and (holds(c) or holds(c.upper())))
    if extra and neg:
        raise ValueError("go: a negated class under (?i) that holds s or k: %s" % cls)
    return ("[^" if neg else "[") + extra + "".join(members) + "]"


# execx.Redact's four patterns, in its order. The JWT one runs apart (jwt_from), so that it skips the starts that fail;
# the other three run as one pattern, each in a group of its own, so that a tie goes to the first in Go's order.
REDACT_GO = [
    r"(?:gho_|ghs_|ghp_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+",
    r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}",
    r"(?i:authorization:\s*(?:bearer|token|basic)\s+)\S+",
    r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)",
]
REDACT = go("|".join("(%s)" % p for p in REDACT_GO[:1] + REDACT_GO[2:]))
JWT = go(REDACT_GO[1])
JWT_RUN = re.compile(r"[A-Za-z0-9_-]*", re.ASCII)
TOKENS = go("|".join([
    r"\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b",
    r"\bxox[abposr]-[A-Za-z0-9-]{10,}",
    r"\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}",
    r"\bglpat-[A-Za-z0-9_-]{16,}",
    r"\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}",
    r"\bAIza[0-9A-Za-z_-]{35}\b",
]))
PAIRS = go(r'''(?i)\b((?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)''')
EMAIL = go(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# an email that starts a run of the characters of its first part, after the place the search starts
EMAIL_RUN = re.compile(r"(?<![A-Za-z0-9._%+-])" + EMAIL.pattern, re.ASCII)
HOST = go(r"(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b")
# The characters of a host name, the word characters and the long s and the Kelvin sign Go's (?i) folds to: a host
# never leaves such a run, and the characters around a run are no word characters, so the hosts of a run are the
# same as on the whole line.
HOST_RUN = re.compile("[A-Za-z0-9_.\u017f\u212a-]+", re.ASCII)
HOST_MAX = 253


def redacted(m):
    s = m.group(0)
    i = s.find(":")
    return s[: i + 1] + " <redacted>" if i >= 0 and s.lower().startswith("authorization") else "<redacted>"


def jwt_from(text, pos):
    """The JWT pattern's first match at pos or after, as Go finds it. When it fails at an eyJ, it fails at every later
    eyJ of the same run of token characters: the first part runs to the end of that run from any of them, and is
    only shorter. So the search goes on from the run's end, and a run of eyJ costs its length, not its square."""
    while True:
        at = text.find("eyJ", pos)
        if at < 0:
            return None
        m = JWT.match(text, at)
        if m:
            return m
        pos = JWT_RUN.match(text, at).end()


def redact_order(m):
    """Where a match of Redact's patterns stands: its start, then its pattern's place in Go's order."""
    return m.start(), 1 if m.re is JWT else 0 if m.lastindex == 1 else 2


def redact(text):
    """execx.Redact: the leftmost match of its four patterns, the first of them in Go's order where two start at one
    place, and on from its end."""
    out, pos, other, jwt = [], 0, None, None
    while True:
        if other is not False and (other is None or other.start() < pos):
            other = REDACT.search(text, pos) or False
        if jwt is not False and (jwt is None or jwt.start() < pos):
            jwt = jwt_from(text, pos) or False
        m = min((x for x in (other, jwt) if x), key=redact_order, default=None)
        if m is None:
            out.append(text[pos:])
            return "".join(out)
        out.append(text[pos : m.start()])
        out.append(redacted(m))
        pos = m.end()


def emails(line):
    """The email pattern's matches masked, as Go finds them. When it fails at a place, it fails at every later place of
    the same run of the characters of an address's first part (the run ends where it ended, at the same @ or none),
    so after the place the search starts only a run's start is tried."""
    out, pos = [], 0
    while True:
        m = EMAIL.match(line, pos) or EMAIL_RUN.search(line, pos + 1)
        if not m:
            out.append(line[pos:])
            return "".join(out)
        out.append(line[pos : m.start()])
        out.append("<email>")
        pos = m.end()


def host(m):
    run = m.group(0)
    if "." not in run:
        return run
    return "<host>" if len(run) > HOST_MAX else HOST.sub("<host>", run)


def masked(text):
    """The text with the secret, token and key-value patterns masked, in the Go scrub's order."""
    return PAIRS.sub(r"\1\2<redacted>", TOKENS.sub("<redacted>", redact(text)))


def line_masked(line):
    """A line of masked text with its emails and hosts masked too."""
    return HOST_RUN.sub(host, emails(line))


def scrub_lines(text, keep=0):
    """The lines of text scrubbed, the last keep of them (0: all), and how many it left out."""
    lines = masked(text or "").splitlines()
    left = max(0, len(lines) - keep) if keep else 0
    return [line_masked(line) for line in lines[left:]], left


def scrub(s):
    return "\n".join(scrub_lines(s)[0])


def scrub_text(text):
    """The whole text scrubbed, every line and line break kept: what the fuzz compares with the Go scrub."""
    return "".join(line_masked(line) for line in masked(text).splitlines(keepends=True))


def serve(stdin, stdout):
    while True:
        head = stdin.read(4)
        if len(head) < 4:
            return
        text = stdin.read(int.from_bytes(head, "big")).decode("utf-8", "surrogateescape")
        out = scrub_text(text).encode("utf-8", "surrogateescape")
        stdout.write(len(out).to_bytes(4, "big") + out)
        stdout.flush()


if __name__ == "__main__":
    if sys.argv[1:] != ["--serve"]:
        sys.exit("usage: scrub.py --serve")
    serve(sys.stdin.buffer, sys.stdout.buffer)
