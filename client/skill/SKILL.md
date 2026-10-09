---
name: memstate
description: >
  Persistent, versioned memory via the memstated daemon. Data lives in a
  single SQLite file on your machine. No cloud, no API key. Use for storing
  facts, recalling memory, managing projects, and full-text or semantic
  search of agent summaries. Supports Markdown and direct keypath = value
  assignment.
license: MIT
tags:
  - memory
  - agent-memory
  - memstate
  - versioned-memory
  - keypath
---

# Memstate Memory Management

This skill talks to a local memstated daemon over HTTP loopback. The
daemon stores data in a SQLite file at `$MEMSTATE_DB` (default
`~/.memstate/memstate.db`). No API key. If `MEMSTATE_ADDR` is set, the
scripts attach to that daemon. Otherwise each script spawns a private
daemon on a random port for the duration of the script and stops it on
exit.

Scripts live in this skill's `scripts/` directory (installed at
`~/.claude/skills/memstate/scripts/`). All examples below assume you
run them from there or by absolute path.

## Core Concepts

| Concept | Description |
|---|---|
| **Project** | Top-level container for memories, keyed by `project_id`. Auto-created on first write. |
| **Keypath** | Dot-separated path (`decisions.auth_provider`) unique within a project. Stored exactly as you write it. Nothing is auto-prefixed. |
| **Memory** | One fact or markdown section stored at a keypath, with full version history. Memory ids are integers. |
| **Versioning** | A write to an existing keypath supersedes the old value. The response names the old version as `superseded` with a 40-word preview; `memstate_history.py` returns it in full. Writes are synchronous. Data is queryable when the script returns. |
| **Tombstone** | A delete adds a tombstone version. History is never destroyed. A new write to the keypath revives it. |

## Naming conventions: follow EXACTLY

Every deviation fragments the store into disconnected near-duplicates.

- **project_id**: Omit `--project`. Every script derives the default
  from the git repository name (or the directory basename), slugged to
  snake_case, so all sessions in one repository share one project
  automatically. Pass `--project` only to reach a DIFFERENT project,
  and then only an id that `memstate_get.py --list-projects` lists.
  Never invent a variant. `my-app`, `myapp`, and `my_app_dev` are three
  different projects.
- **keypath segments**: Lowercase snake_case only (`[a-z0-9_]`), joined
  by dots. Dates are `YYYY_MM_DD` inside a segment:
  `task.summary.2026_07_04`. Never `2026-07-04`, camelCase, or spaces.
- **keypath shape**: `<area>.<topic>` or `<area>.<topic>.<detail>`.
  Preferred area segments: `decisions`, `todo`, `notes`, `gotchas`,
  `questions`, `files`, `config`, `arch`, `task.summary.<date>`.
- **One keypath = one fact.** To update a fact, write the SAME keypath
  with the new value. Versioning keeps the old one. Do not create a
  sibling keypath for a new value of the same fact.
- **Git branches**: Keypaths describe the main/default branch unless
  stated otherwise. Facts that are only true on an unmerged branch go
  under `branches.<branch_slug>.<area>...`, with the branch name
  slugged to snake_case (`feature/foo-bar` →
  `branches.feature_foo_bar.todo`). When the branch merges, write the
  durable outcomes to normal top-level keypaths, then recursively
  delete the `branches.<branch_slug>` subtree. If the branch is
  abandoned, delete the subtree. Branch-independent knowledge
  (decisions taken, gotchas, architecture) always lives at the top
  level.
- **category**: The kind of memory. ONE lowercase word from `decision`,
  `config`, `status`, `note`, `gotcha`, `reference`, `learning`.
  Filterable in search.
- **topics**: Subject tags, lowercase snake_case (`auth,embeddings`).
  Search matches any listed topic.
- **source**: A short provenance string, for example `claude-code
  session 2026_07_04` or `user decision`. Shown in history.

## User scope: facts that are not about this project

Every memory lives in a project by default. Some facts are about the
user or the machine and hold in every repository: an installed tool, a
path, the Ollama URL, a working preference, the user's role. These go
in the reserved project `_user`. Every script reaches it with
`--scope user` (never with `--project`).

A fact belongs in the user scope only when ALL three hold:

1. It stays true if this repository is deleted. It is not about any code.
2. It is true in every repository, for this user or for this machine.
3. It describes the user or the host, not work. Never a decision, todo,
   task summary, note, or gotcha about code. Those stay in the project.

The daemon enforces this line with a keypath allowlist. A write outside
it returns HTTP 400 and nothing is stored. Allowed shapes:

| Shape | Meaning | Example |
|---|---|---|
| `preferences.<topic>` | stated global working preferences | `preferences.commit_style` |
| `profile.<topic>` | who the user is; never credentials | `profile.role` |
| `host.<host_slug>.env.<topic>` | OS, shell, paths, runtimes, ports | `host.mbp.env.go_bin` |
| `host.<host_slug>.tools.<topic>` | how an installed tool is configured | `host.mbp.tools.ollama_url` |

`<host_slug>` is the first label of the hostname, slugged like a project
id (`Matts-MBP.local` → `matts_mbp`). `memstate_get.py` prints it as
`user.host`. Reads prune other machines' `host.*` subtrees; searches with
`--scope user` drop them.

```bash
python3 scripts/memstate_set.py --scope user --keypath preferences.commit_style \
  --value "short subjects, no trailers" --category config
python3 scripts/memstate_set.py --scope user --keypath host.matts_mbp.tools.ollama_url \
  --value "http://127.0.0.1:11434, model qwen3-embedding:4b" --category config
python3 scripts/memstate_get.py --scope user --keypath preferences --include-content
```

Heading extraction (`memstate_remember.py --scope user` without
`--keypath`) fits only `## Preferences` and `## Profile`. Host facts need
an explicit `--keypath`. One bad section rejects the whole call.

Never store secrets, tokens, or credentials in any scope. The
denied-prompt rule applies to the user scope too.

## Session project (MCP sessions only)

The MCP proxy defaults every call to the repository of its working
directory, and that is almost always right. When the prompt is clearly
about another subject (for example "set up my nginx config" from the home
directory), the agent pins its session once with `project_name` on the
first tool call. An existing id pins freely. A new id also needs
`new_project=true` and is refused when it looks like an existing id
(`regress_tests` vs `regress_test`). One pin per session.

One rule for creating projects, enforced by the proxy and by these
scripts alike: a write never creates a project unless it targets the git
repository you are in, or the call carries `new_project=true` (scripts:
`--new-project`). This holds for the cwd project outside a repository, for
an explicit project id, and for a soft-deleted project, which the flag
revives. A name that resembles an existing project is refused outright;
use the existing project, or create the new one with the `memstate` CLI,
which is the human escape. The home directory has no default project for
writes, and its name is never accepted as a project: pin a project or use
the user scope. A system directory (a filesystem root, `C:\WINDOWS\...`, or
`/usr`, `/etc` and the like) is treated the same way; a desktop app that is
not running in a project starts its MCP servers from one, so a session that
sees "no default project" in a write error passes `project_name` once, using
the project the recall hook's `<memstate-scope cwd_project>` names. Ids that start with `_` are reserved; the user scope is
`--scope user`, never `--project`, and `--list-projects` does not show it.
`--new-project` is an error where nothing can be created (`--scope user`,
or the git repository you are in). Reads are never gated. These scripts
are one-shot and have no session: pass `--project` instead of
`project_name`. A pinned MCP session publishes its pin for the recall hook
as `<db dir>/recall/pins/<pid>`, removed when the proxy exits.

## Workflows

### Before starting a task (recall)

```bash
# 1. Browse this repo's keypath tree (names only, no content)
python3 scripts/memstate_get.py

# 2. Read a subtree with content
python3 scripts/memstate_get.py --keypath decisions --include-content

# 3. Search when you do not know the keypath
python3 scripts/memstate_search.py --query "how is authentication configured"
python3 scripts/memstate_search.py --query "auth setup" --mode semantic

# Other projects: --list-projects shows ids. --project targets one.
python3 scripts/memstate_get.py --list-projects
```

### After completing a task (remember)

```bash
# One short fact at one keypath
python3 scripts/memstate_set.py \
  --keypath config.port --value "8080" --category config

# Markdown summary, split by ## headings (one memory per section).
# Each section lands at the top-level <heading_slug> keypath. ###
# headings nest one more dot segment. Prose before the first ## lands
# at `preamble`. Pass --root notes to nest sections under a prefix, or
# --keypath to store ALL content as one memory.
python3 scripts/memstate_remember.py \
  --content "## Decisions\nSwitched JWT -> sessions.\n\n## Gotchas\nCookie must be SameSite=Lax." \
  --source "claude-code session 2026_07_04" --category note
```

Heading names `TODOs`, `Decisions`, `Open Questions`, `Files`,
`Notes`, `Gotchas` collapse to the canonical segments `todo`,
`decisions`, `questions`, `files`, `notes`, `gotchas`.

### History and cleanup

```bash
# How a fact changed over time (newest first, includes tombstones)
python3 scripts/memstate_history.py --project my_app --keypath config.port

# Tombstone one keypath. History is kept. A new write revives it.
python3 scripts/memstate_delete.py --keypath config.old_setting

# Tombstone a whole subtree, e.g. a merged branch's state
python3 scripts/memstate_delete.py --keypath branches.feature_foo_bar --recursive

# Soft-delete a project (any later write to the same id revives it)
python3 scripts/memstate_delete_project.py --project my_app
```

## Script reference

### `memstate_set.py`: one fact at one keypath

```bash
python3 scripts/memstate_set.py \
  --keypath KEYPATH --value VALUE \
  [--project ID] [--new-project] [--source TEXT] [--category WORD] [--topics TAG1,TAG2]
```

**Response:** `action` (`created` | `superseded` | `unchanged`),
`stored` (id, keypath, version, metadata; no content), `superseded` (the
prior version, if any, with a 40-word `preview`).
`unchanged` means identical content AND metadata. No new version was
written.

### `memstate_remember.py`: markdown or text

```bash
python3 scripts/memstate_remember.py \
  --content "MARKDOWN" \
  [--project ID] [--new-project]
  [--keypath KEYPATH]        # store everything as ONE memory here
  [--root PREFIX]            # heading-split mode: nest sections under this prefix (default: none)
  [--source TEXT] [--category WORD] [--topics TAG1,TAG2]
```

The split is a deterministic parse of `##`+ headings. No LLM, no job
queue. The operation is synchronous. **Response:** `method`
(`explicit` | `headings`), `items[]` each with `keypath`, `action`,
`stored`, `superseded?` (the same shapes as `memstate_set.py`).

### `memstate_get.py`: browse and retrieve

```bash
python3 scripts/memstate_get.py                               # this repo's tree (names only)
python3 scripts/memstate_get.py --keypath KP --include-content
python3 scripts/memstate_get.py --list-projects               # all project ids
python3 scripts/memstate_get.py --project ID --keypath KP     # another project
python3 scripts/memstate_get.py --memory-id N                 # one memory by integer id
```

**Response:** the projects list returns `projects[]`. The tree returns
`domains[]` and `total_memories`. A subtree returns `memories[]` and
`total_count`. The tree gives names only. Pass a keypath (with
`--include-content`) to read values.

### `memstate_search.py`: find current memories

```bash
python3 scripts/memstate_search.py --query "PLAIN WORDS" \
  [--project ID]             # default: this repo's project
  [--all-projects]           # search the whole store
  [--mode hybrid|fts|semantic]  # hybrid (default) = any word + meaning, fused; fts = every word; semantic = meaning only (needs Ollama)
  [--threshold 0.0-1.0]      # semantic and hybrid, default 0.5
  [--category WORD] [--topics TAG1,TAG2]   # topics = match any
  [--keypath-prefix KP]      # only this keypath or below, e.g. branches.feature_x
  [--limit N]                # default 10
  [--include-content]        # full content per hit instead of a 40-word preview
```

Only the current version of each keypath is searchable. Tombstoned
keypaths and soft-deleted projects never match. Query text is plain
words. Punctuation is safe, and there is no boolean syntax.
Each hit carries `preview` (its first 40 words) instead of `content`;
read the keypaths that matter with `memstate_get.py`, or pass
`--include-content` when a script needs the text. **Response:**
`results[]`, `total_found`, `query`, `mode` (+ `score`
per result and `threshold`/`model` in semantic and hybrid modes; hybrid
adds `sources` per result and `degraded` when the embedder was
unavailable and only FTS hits are present).

### `memstate_history.py`: version chain of one keypath

```bash
python3 scripts/memstate_history.py --keypath KP [--project ID]
python3 scripts/memstate_history.py --memory-id N
```

**Response:** `versions[]` (newest first, includes tombstones),
`total_versions`.

### `memstate_delete.py`: tombstone keypath(s)

```bash
python3 scripts/memstate_delete.py --keypath KP [--project ID] [--recursive]
```

**Response:** `deleted_count`, `deleted_keypaths[]`.

### `memstate_delete_project.py`: soft-delete a project

```bash
python3 scripts/memstate_delete_project.py --project ID
```

**Response:** `project_id`, `deleted_count`. Reads fail afterwards.
Any write to the same project_id revives it with all memories intact.

## Best practices

1. **Let the default id work.** Omit `--project`. The repo-derived
   default keeps every session in one repo on one project. Only pass
   an explicit id that `--list-projects` shows.
2. **Update, do not duplicate.** Same keypath, new value. The version
   chain is the changelog.
3. **Search before browsing.** `memstate_search.py` beats a walk of
   the tree when you know roughly what you want. The default hybrid
   mode already matches by meaning; use `--mode fts` when you need every
   word to match.
4. **Only current versions surface.** Search and get return the latest
   non-deleted version per keypath. Use `memstate_history.py` to see
   the past. There is no `is_latest` flag to check.
5. **Categorize decisions.** `--category decision` on architecture
   choices makes them retrievable with one filtered search.
6. **Keep branch state quarantined.** Put in-flight branch facts under
   `branches.<slug>.*`. Promote them to the top level on merge, then
   recursively delete the subtree. Scope a search to one branch with
   `--keypath-prefix branches.<slug>`.
7. **Never save a denied prompt.** A denied prompt is a tool call that the
   user or a permission check denied. Do not save it under any keypath, in
   any category, or in any summary. Do not save it as a warning for a later
   session. No reason overrides this rule.
8. **Keep user facts out of projects, and work out of the user scope.**
   A tool path or a global preference goes to `--scope user`; a
   decision, todo, or summary never does. The daemon rejects the latter.

## Connecting to the daemon

```bash
export MEMSTATE_ADDR="127.0.0.1:8765"     # attach to a shared daemon
export MEMSTATE_LOCAL_URL="http://127.0.0.1:9000/api/v1"  # or full URL override
```

With neither set, each script spawns its own daemon on a random port
against `$MEMSTATE_DB` and stops it on exit. A manually started
`memstated --addr HOST:PORT` probes the port first. If another
memstated already owns the port, it exits quietly. If an unrelated
process owns the port, it exits loudly (code 2).
