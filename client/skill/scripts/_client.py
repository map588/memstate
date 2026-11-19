"""Shared HTTP client for the memstate CLI scripts.

Three modes, mirroring the TS proxy:

  * attach  — MEMSTATE_ADDR is set: talk to a daemon someone else started.
              We never spawn, never kill.
  * shared  — the default: a shared daemon published its address in
              daemon.addr next to the DB and answers /health. We attach.
              The scripts never start a shared daemon; the MCP proxy does.
  * child   — no shared daemon, or MEMSTATE_CHILD=1: spawn
              `memstated --owner-pid=<us>`, read the "MEMSTATE_READY
              addr=..." banner from its stderr, SIGTERM on script exit.
              1:1 lifetime with this Python process.

The child is cached on the module, so a single script that imports this
module and makes several requests reuses one daemon.

Env:
  MEMSTATE_ADDR       attach to this host:port (attach mode)
  MEMSTATE_CHILD      1 skips daemon.addr and spawns a private daemon
  MEMSTATE_BIN        override the daemon path (default: sibling build / PATH)
  MEMSTATE_LOCAL_URL  full base URL override (for both modes)
  MEMSTATE_DB         the child's DB; the daemon log goes in the same directory
"""
import atexit
import json
import os
import re
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional

READY_RE = re.compile(r"MEMSTATE_READY addr=(\S+)")
_READY_TIMEOUT = 5.0


def _load_config_file() -> None:
    """Export ~/.memstate/config.env into os.environ for keys the environment
    does not already set: the daemon's rule (server/config.go), so the
    scripts, the proxy and the daemon see one configuration. MEMSTATE_CONFIG
    names another file; MEMSTATE_CONFIG=off skips it."""
    raw = os.environ.get("MEMSTATE_CONFIG", "")
    if raw == "off":
        return
    path = Path(os.path.expanduser(raw)) if raw else Path.home() / ".memstate" / "config.env"
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("export "):
            line = line[len("export "):].strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if key.startswith("MEMSTATE_") and not os.environ.get(key):
            os.environ[key] = value


_load_config_file()

_child: Optional[subprocess.Popen] = None
_base_url: Optional[str] = None
_started_lock = threading.Lock()


def _resolve_bin() -> str:
    explicit = os.environ.get("MEMSTATE_BIN")
    if explicit and Path(explicit).exists():
        return explicit
    # scripts/ → client/skill/scripts/ → ../../../server/memstated
    binary = "memstated.exe" if os.name == "nt" else "memstated"
    sibling = (Path(__file__).resolve().parent / ".." / ".." / ".." / "server" / binary).resolve()
    if sibling.exists():
        return str(sibling)
    return "memstated"  # fall through to PATH


def _memstate_dir() -> Path:
    """The directory of the DB. Mirrors memstateDir in the TS proxy."""
    db = os.environ.get("MEMSTATE_DB")
    if db:
        if db.startswith("~/"):
            db = str(Path.home() / db[2:])
        return Path(db).resolve().parent
    return Path.home() / ".memstate"


def _published_addr() -> str:
    """The address in daemon.addr next to the DB when a memstate daemon
    answers /health there, else ""."""
    try:
        addr = (_memstate_dir() / "daemon.addr").read_text().strip()
    except OSError:
        return ""
    if not addr:
        return ""
    try:
        with urllib.request.urlopen(f"http://{addr}/health", timeout=0.5) as resp:
            if json.load(resp).get("service") == "memstate":
                return addr
    except Exception:
        pass
    return ""


def _spawn_child() -> str:
    """Spawn memstated, read banner, wire atexit cleanup. Returns addr."""
    global _child
    bin_path = _resolve_bin()
    # The daemon log is next to the DB, so a daemon on a test DB does not
    # write to the log of the user's daemon.
    log_path = _memstate_dir() / "memstated.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_fd = open(log_path, "a")

    # NOT detached — keep the child in our process group so SIGINT on the
    # terminal propagates, and .terminate() is authoritative. --owner-pid is
    # the safety net if we get SIGKILLed.
    child = subprocess.Popen(
        [bin_path, "--owner-pid", str(os.getpid())],
        stdin=subprocess.DEVNULL,
        stdout=log_fd,
        stderr=subprocess.PIPE,
        start_new_session=False,
    )
    _child = child

    addr: Optional[str] = None
    deadline = time.monotonic() + _READY_TIMEOUT
    assert child.stderr is not None
    while time.monotonic() < deadline:
        line = child.stderr.readline()
        if not line:
            break
        try:
            text = line.decode("utf-8", errors="replace")
        except Exception:
            text = ""
        # Tee to log so we don't lose banner or subsequent lines.
        log_fd.write(text)
        log_fd.flush()
        m = READY_RE.search(text)
        if m:
            addr = m.group(1).strip()
            break

    if addr is None:
        child.kill()
        child.wait(timeout=2)
        raise RuntimeError(
            f"memstated never printed READY banner within {_READY_TIMEOUT}s; see {log_path}"
        )

    # Drain the rest of stderr in background so the child never blocks on a
    # full pipe buffer. Each line goes to the log.
    def _drain() -> None:
        try:
            assert child.stderr is not None
            for chunk in child.stderr:
                log_fd.write(chunk.decode("utf-8", errors="replace"))
                log_fd.flush()
        except Exception:
            pass

    threading.Thread(target=_drain, daemon=True).start()

    atexit.register(_cleanup_child)
    # SIGINT / SIGTERM → run atexit (Python default) then let the signal
    # actually terminate us. We intercept to make sure we reap the child.
    def _on_signal(signum: int, _frame) -> None:
        _cleanup_child()
        # Restore default and re-raise so exit code reflects the signal.
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)
    for s in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(s, _on_signal)
        except (ValueError, OSError):
            # Not main thread / not supported: rely on atexit.
            pass

    return addr


def _cleanup_child() -> None:
    global _child
    c = _child
    if c is None or c.poll() is not None:
        return
    try:
        c.terminate()
        try:
            c.wait(timeout=2)
        except subprocess.TimeoutExpired:
            c.kill()
            c.wait(timeout=1)
    except Exception:
        pass
    _child = None


def _base() -> str:
    global _base_url
    if _base_url is not None:
        return _base_url
    explicit_url = os.environ.get("MEMSTATE_LOCAL_URL")
    if explicit_url:
        _base_url = explicit_url.rstrip("/")
        return _base_url
    attach_addr = os.environ.get("MEMSTATE_ADDR")
    if attach_addr:
        _base_url = f"http://{attach_addr}/api/v1"
        return _base_url
    if os.environ.get("MEMSTATE_CHILD") != "1":
        published = _published_addr()
        if published:
            _base_url = f"http://{published}/api/v1"
            return _base_url
    # Child mode: spawn exactly once (thread-safe via the lock).
    with _started_lock:
        if _child is None:
            addr = _spawn_child()
        else:
            addr = ""  # unreachable
        _base_url = f"http://{addr}/api/v1"
    return _base_url


_HEADERS = {"Content-Type": "application/json"}


def fetch(method: str, path: str, body: Optional[dict] = None):
    """One daemon call. Returns the parsed JSON body (or the raw text when
    it is not JSON). Raises urllib errors; use emit() to turn them into an
    exit code."""
    url = f"{_base()}{path}"
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=_HEADERS, method=method)
    with urllib.request.urlopen(req) as resp:
        payload = resp.read().decode("utf-8")
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            return payload


def emit(call) -> int:
    """Run call(), print its result as JSON, and map daemon errors to exit
    codes: 1 for an HTTP error, 2 for an unreachable daemon."""
    try:
        result = call()
        if isinstance(result, str):
            print(result)
        else:
            print(json.dumps(result, indent=2))
        return 0
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", errors="replace")
        print(f"Error: HTTP {e.code} {detail}", file=sys.stderr)
        return 1
    except urllib.error.URLError as e:
        print(
            f"Error: could not reach memstated at {_base_url}: {e.reason}",
            file=sys.stderr,
        )
        return 2


def _cwd_base():
    """(name, in_repo): the git top-level directory name when the cwd is
    inside a repository, else the cwd name."""
    try:
        top = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            capture_output=True, text=True, timeout=5,
        )
        if top.returncode == 0 and top.stdout.strip():
            return Path(top.stdout.strip()).name, True
    except Exception:
        pass
    return Path.cwd().name, False


def default_project() -> str:
    """Project id derived from the git repo name (or cwd basename outside a
    repo), slugged to lowercase snake_case — same rule as the TS proxy, so
    scripts and MCP sessions land in the same project."""
    return slug_name(_cwd_base()[0])


def in_git_repo() -> bool:
    """True when the cwd is inside a git repository: its name is intent."""
    return _cwd_base()[1]


def is_home_dir() -> bool:
    """True when the cwd is the home directory, which names the user, not a
    project."""
    try:
        return Path.cwd().resolve() == Path.home().resolve()
    except Exception:
        return False


def system_dir_reason() -> str:
    """Why the cwd can name no project: a filesystem root, the Windows
    directory, or a Unix system tree. "" for an ordinary directory. Same
    rule as the TS proxy (systemDirReason)."""
    cwd = Path.cwd().resolve()
    if cwd == Path(cwd.anchor):
        return "a filesystem root"
    win = os.environ.get("SystemRoot") or os.environ.get("windir")
    if win and str(cwd).lower().startswith(str(Path(win).resolve()).lower()):
        return "the Windows system directory"
    if re.match(r"^/(usr|bin|sbin|etc|lib|lib64|opt|proc|sys|dev|boot)(/|$)", cwd.as_posix()):
        return "a system directory"
    return ""


def slug_name(name: str) -> str:
    """Shared id rule: lowercase, runs of other characters become "_",
    edge underscores trimmed. Same rule as the TS proxy and the Go daemon."""
    slug = re.sub(r"[^a-z0-9]+", "_", name.lower()).strip("_")
    return slug or "default"


# SHARED_PROJECT is the daemon's one reserved project for facts about the
# user and the host. The daemon rejects writes there outside a short
# allowlist of keypath shapes (see SKILL.md, "Shared scope").
SHARED_PROJECT = "_shared"


def host_slug() -> str:
    """This machine's segment under host.<slug> in SHARED_PROJECT: the first
    hostname label, slugged. Same rule as the TS proxy and the Go daemon."""
    return slug_name(socket.gethostname().split(".")[0])


def add_scope_args(ap) -> None:
    """Add the --project / --scope pair every script accepts."""
    ap.add_argument("--project", default=None,
                    help="project id (default: the cwd project, derived from the git "
                         "repository or directory name; ids that start with _ are reserved)")
    ap.add_argument("--scope", choices=("project", "shared"), default="project",
                    help="'shared' targets the reserved shared scope: facts that hold "
                         "in every repository (preferences, profile, practices, lessons, "
                         "this host's env and tools), never facts about this repo")


def resolve_project(args) -> str:
    """Project id for a call: the reserved shared project for --scope shared,
    else --project, else this repo's default."""
    if args.scope == "shared":
        if args.project:
            raise SystemExit("Error: pass --scope shared or --project, not both")
        return SHARED_PROJECT
    if args.project and is_reserved_id(args.project):
        raise SystemExit(
            f'Error: project ids that start with "_" are reserved ("{args.project}"); '
            "use --scope shared for the shared scope")
    return args.project or default_project()


def is_reserved_id(pid: str) -> bool:
    """Reserved ids start with "_". The daemon lists _shared among the
    projects, but for scripts it is --scope shared, never --project."""
    return pid.startswith("_")


def home_slug_name() -> str:
    """The project id the home directory would derive. It names the user,
    not a project, so writes never accept it."""
    return slug_name(Path.home().name)


def list_projects_visible() -> dict:
    """The daemon's project list without reserved ids."""
    out = fetch("GET", "/projects")
    if isinstance(out, dict):
        out = dict(out)
        out["projects"] = [p for p in (out.get("projects") or []) if not is_reserved_id(p["id"])]
    return out


def add_write_args(ap) -> None:
    """Add --new-project and --host, which the two write scripts accept."""
    ap.add_argument("--new-project", action="store_true",
                    help="allow this write to create a project that does not exist "
                         "yet (the git repository you are in never needs it)")
    ap.add_argument("--host", action="store_true",
                    help="tag every memory this call writes with the topic "
                         "host_<host_slug>: a project fact that holds only on "
                         "this machine")


def host_topic() -> str:
    """The topic that marks a project fact as true only on this machine.
    Same rule as the TS proxy (HOST_TOPIC)."""
    return "host_" + host_slug()


def host_topics(args, topics):
    """The topics a write sends: the caller's, plus host_topic() once when
    --host was given. Returns None when there are none."""
    topics = list(topics) if topics else []
    if args.host and host_topic() not in topics:
        topics.append(host_topic())
    return topics or None


# The near-duplicate rule folds the spellings that split one project into
# several: underscores, digits, a trailing dev/test/tmp/old/new, one id
# containing the other, or a small edit distance. Same rule as the TS proxy.
def normalize_id(pid: str) -> str:
    s = pid.replace("_", "")
    s = re.sub(r"[0-9]", "", s)
    return re.sub(r"(dev|test|tests|tmp|old|new)$", "", s)


def levenshtein(a: str, b: str) -> int:
    prev = list(range(len(b) + 1))
    for i in range(1, len(a) + 1):
        cur = [i] + [0] * len(b)
        for j in range(1, len(b) + 1):
            cost = 0 if a[i - 1] == b[j - 1] else 1
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + cost)
        prev = cur
    return prev[len(b)]


def near_duplicates(name: str, ids: list) -> list:
    """Existing ids that name looks like."""
    n = normalize_id(name)
    out = []
    for pid in ids:
        if pid == name:
            continue
        m = normalize_id(pid)
        if n == m or (len(n) >= 4 and len(m) >= 4 and (n in m or m in n)):
            out.append(pid)
            continue
        limit = 1 if min(len(n), len(m)) < 6 else 2
        if levenshtein(n, m) <= limit:
            out.append(pid)
    return out


def list_project_ids() -> list:
    """Live project ids from the daemon. Exits 2 when it is unreachable."""
    try:
        out = fetch("GET", "/projects")
    except urllib.error.URLError as e:
        print(f"Error: could not reach memstated at {_base_url}: {e.reason}", file=sys.stderr)
        raise SystemExit(2)
    projects = out.get("projects") if isinstance(out, dict) else None
    return [p["id"] for p in (projects or [])]


def check_write_target(args, project: str) -> None:
    """One rule for every write: it never creates a project unless it
    targets the git repository the script runs in, or --new-project is set
    and the id resembles no existing project. The home directory has no
    default project for writes. Same rule as the TS proxy; the memstate CLI
    is the human escape for a deliberate near-duplicate. Exits 1 with the
    way out on stderr; a refusal creates nothing."""
    if args.new_project and args.scope == "shared":
        raise SystemExit(
            "Error: --new-project has no effect with --scope shared: the shared scope always exists")
    if args.scope == "shared":
        return
    explicit = bool(args.project)
    if not explicit:
        if in_git_repo():
            if args.new_project:
                raise SystemExit(
                    "Error: --new-project has no effect here: the project of the git "
                    f'repository you are in ("{project}") is created without it')
            return
        why = "your home directory" if is_home_dir() else system_dir_reason()
        if why:
            raise SystemExit(
                f"Error: the working directory is {why}, which has no "
                "default project for writes. Pass --project ID (an id from "
                "memstate_get.py --list-projects, or a new id with --new-project), "
                "or --scope shared for facts about this machine")
    if explicit and project == home_slug_name():
        raise SystemExit(
            f'Error: "{project}" is the name of your home directory, which names the user, '
            "not a project; pass another --project, or --scope shared for facts about this machine")
    ids = list_project_ids()
    if project in ids:
        return
    if explicit:
        where = f'project "{project}" does not exist'
    else:
        where = ("the working directory is not a git repository and its project "
                 f'"{project}" does not exist')
    near = near_duplicates(project, ids)
    if near:
        listed = ", ".join(f'"{n}"' for n in near)
        raise SystemExit(
            f"Error: {where} but looks like existing project {listed}; use "
            f"--project {near[0]}. To create \"{project}\" as a separate project, "
            "use the memstate CLI")
    if not args.new_project:
        raise SystemExit(
            f"Error: {where}. Pass --project ID with an id from "
            "memstate_get.py --list-projects, or add --new-project to create "
            f'"{project}" (this also revives a soft-deleted project)')


def is_other_host(keypath: str) -> bool:
    """True when a shared-scope keypath describes another machine."""
    seg = keypath.split(".")
    return seg[0] == "host" and len(seg) >= 2 and seg[1] != host_slug()


def prune_other_hosts(domains: list) -> list:
    """Keep only this machine's subtree under `host`."""
    out = []
    for d in domains:
        if d.get("name") == "host":
            d = dict(d)
            d["children"] = [c for c in d.get("children", []) if c.get("name") == host_slug()]
        out.append(d)
    return out


def count_values(nodes: list) -> int:
    n = 0
    for node in nodes:
        if node.get("has_value"):
            n += 1
        n += count_values(node.get("children", []))
    return n


# ---- write rules -------------------------------------------------------
# The same three refusals and the same hints as the TS proxy
# (client/src/index.ts: checkKeypathFormat, checkNotArea, checkNoSecret,
# factHint, areaHint). This is a second implementation: change the two
# together. The regression suite has a check for each rule on each side.

AREAS = ["decisions", "todo", "notes", "gotchas", "questions", "files", "config", "arch"]
_KEYPATH_FORMAT = re.compile(r"[a-z0-9_]+(?:\.[a-z0-9_]+)*")
_KEYPATH_REF = re.compile(r"\b(?:" + "|".join(AREAS) + r")\.[a-z0-9_]+")
_LIST_ITEM = re.compile(r"^\s*(?:[-*]|\d+\.)\s+", re.MULTILINE)
HINT_LIST_ITEMS = 5
HINT_BYTES = 1500
HINT_INDEX_REFS = 3
HINT_INDEX_BYTES = 400

# Texts that are a credential with near certainty. The word "password" is
# not in the list: notes about a login flow hold it.
_SECRET_SHAPES = [
    ("a private key block", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----")),
    ("an API key (sk-...)", re.compile(r"\bsk-[A-Za-z0-9_-]{20,}")),
    ("a GitHub token", re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})")),
    ("an AWS access key id", re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
    ("a Slack token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}")),
    ("a JSON Web Token", re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}")),
]


def fixed_keypath(keypath: str) -> Optional[str]:
    """The keypath in the correct format, or None when no simple change
    gives one."""
    fixed = re.sub(r"[^a-z0-9_.]+", "_", keypath.lower())
    fixed = re.sub(r"\.{2,}", ".", fixed).strip("._")
    return fixed if _KEYPATH_FORMAT.fullmatch(fixed) else None


def check_keypath_format(keypath: str, field: str) -> None:
    """A keypath is lowercase snake_case segments joined by dots. Exits 1
    with the corrected keypath on stderr."""
    if _KEYPATH_FORMAT.fullmatch(keypath):
        return
    fixed = fixed_keypath(keypath)
    tail = f': use "{fixed}".' if fixed else "."
    raise SystemExit(
        f'Error: {field} "{keypath}" must be lowercase snake_case segments '
        f"[a-z0-9_] joined by dots{tail}")


def check_not_area(keypath: str) -> None:
    """A memory at the bare name of an area takes the place of the facts
    that belong below it, and comes before them in a search."""
    if keypath in AREAS:
        raise SystemExit(
            f'Error: "{keypath}" is an area, not a fact, and nothing was written. '
            f"Use {keypath}.<topic>, named after the fact.")


def check_no_secret(text: str) -> None:
    """The error names the kind of credential and never the text."""
    for kind, shape in _SECRET_SHAPES:
        if shape.search(text):
            raise SystemExit(
                f"Error: the text holds {kind}, and nothing was written. Memories are "
                "not a place for credentials: store where the secret is kept, not the secret.")


def check_write(args, keypath: Optional[str], text: str, root: Optional[str] = None) -> None:
    """The three refusals for one write. The shared scope has its own
    keypath rule in the daemon, so the area rule does not apply there."""
    check_no_secret(text)
    if keypath:
        check_keypath_format(keypath, "keypath")
        if args.scope != "shared":
            check_not_area(keypath)
    if root:
        check_keypath_format(root, "root")


def area_hint(keypaths: list) -> Optional[str]:
    """The hint for text that had no heading, and for a memory at the name
    of an area that a daemon from before deny_keypaths stored."""
    if any(k == "preamble" or k.endswith(".preamble") for k in keypaths):
        return ('The text before the first heading went to "preamble". Give it a '
                '"## <area>" and "### <topic>" heading, or pass keypath.')
    areas = [k for k in keypaths if k in AREAS]
    if not areas:
        return None
    names = ", ".join(f'"{k}"' for k in areas)
    return (f"{names} is an area, not a fact. Give each fact its keypath: <area>.<topic>, "
            'or a "### <topic>" heading below the area heading.')


def fact_rule(keypath: str, content: str) -> Optional[tuple]:
    """The name of the rule and the hint for one memory that a search will
    not find well. A task summary is long by design and gets none."""
    if keypath.startswith("task."):
        return None
    items = len(_LIST_ITEM.findall(content))
    if items >= HINT_LIST_ITEMS:
        return ("list_items", f"{items} list items in one memory. If a search can ask for one item "
                "alone, write one memory for each item.")
    size = len(content.encode("utf-8"))
    refs = len(set(_KEYPATH_REF.findall(content)))
    if refs >= HINT_INDEX_REFS and size <= HINT_INDEX_BYTES:
        return ("index", f"This memory names {refs} other keypaths and holds little else. The keypath "
                "tree is the index: a memory that lists other memories comes before them in a search.")
    if size > HINT_BYTES:
        return ("size", f"{size} bytes in one memory. A search finds a short memory with one fact: "
                "split this one if it holds more than one fact.")
    return None


def fact_hint(keypath: str, content: str) -> Optional[str]:
    """The hint text of fact_rule for a write result."""
    rule = fact_rule(keypath, content)
    return rule[1] if rule else None


def audit_memory(keypath: str, content: str, shared: bool = False) -> list:
    """The write rules applied to a memory that the store holds already:
    one finding for each rule that the memory breaks. memstate_audit.py
    prints them. A finding never holds the text of the memory."""
    found = []
    for kind, shape in _SECRET_SHAPES:
        if shape.search(content):
            found.append({"rule": "credential", "detail": f"The text holds {kind}."})
            break
    if not _KEYPATH_FORMAT.fullmatch(keypath):
        finding = {"rule": "keypath_format",
                   "detail": "The keypath is not lowercase snake_case segments joined by dots."}
        fixed = fixed_keypath(keypath)
        if fixed:
            finding["use"] = fixed
        found.append(finding)
    if not shared and keypath in AREAS:
        found.append({"rule": "area_keypath",
                      "detail": f'"{keypath}" is an area, not a fact. Its facts belong at '
                                f"{keypath}.<topic>, one memory for each fact."})
    if keypath == "preamble" or keypath.endswith(".preamble"):
        found.append({"rule": "preamble",
                      "detail": "Text that had no heading. Give it a keypath named after its fact."})
    rule = fact_rule(keypath, content)
    if rule:
        found.append({"rule": rule[0], "detail": rule[1]})
    return found


def post_write(path: str, body: dict, hint) -> int:
    """post() for a write. hint is a text, None, or a function of the
    daemon result that gives one; the hint goes into the printed result."""
    def call():
        result = fetch("POST", path, body)
        text = hint(result) if callable(hint) else hint
        if text and isinstance(result, dict):
            result["hint"] = text
        return result
    return emit(call)


def post_remember(body: dict) -> int:
    """A heading split: the daemon knows where each heading goes, so it
    refuses a section at the name of an area (deny_keypaths). A daemon from
    before that rule refuses the field; the script then writes without it
    and the result has the hint."""
    def call():
        try:
            result = fetch("POST", "/memories/remember", {**body, "deny_keypaths": AREAS})
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", errors="replace")
            if "deny_keypaths" not in detail:
                # The body of the error is read, so emit() cannot print it.
                raise SystemExit(f"Error: HTTP {e.code} {detail}") from None
            result = fetch("POST", "/memories/remember", body)
        if isinstance(result, dict):
            text = area_hint([item.get("keypath", "") for item in result.get("items", [])])
            if text:
                result["hint"] = text
        return result
    return emit(call)


def post(path: str, body: dict) -> int:
    return emit(lambda: fetch("POST", path, body))


def get(path: str) -> int:
    return emit(lambda: fetch("GET", path, None))
