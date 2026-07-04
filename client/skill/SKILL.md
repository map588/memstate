---
name: memstate
description: >
  Persistent, versioned memory via the memstated daemon. Data lives in a
  single SQLite file on your machine; no cloud, no API key. Use for storing
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

This skill talks to a local memstated daemon over HTTP loopback. Data is
stored in a SQLite file at `$MEMSTATE_DB` (default `~/.memstate/memstate.db`).
No API key. If `MEMSTATE_ADDR` is set the scripts attach to that daemon;
otherwise each script spawns a private daemon on a random port for the
duration of the script and shuts it down on exit.

Scripts live in this skill's `scripts/` directory (installed at
`~/.claude/skills/memstate/scripts/`). All examples below assume you run
them from there or by absolute path.

## Core Concepts

| Concept | Description |
|---|---|
| **Project** | Top-level container for memories, keyed by `project_id`. Auto-created on first write. |
| **Keypath** | Dot-separated path (`decisions.auth_provider`) unique within a project. Stored exactly as you write it — nothing is auto-prefixed except in heading-split mode (see below). |
| **Memory** | One fact or markdown section stored at a keypath, with full version history. Memory ids are integers. |
| **Versioning** | Writing an existing keypath supersedes the old value; the response returns the old version as `superseded`. Writes are synchronous — data is queryable the moment the script returns. |
| **Tombstone** | Deleting adds a tombstone version. History is never destroyed; rewriting the keypath resurrects it. |

## Naming conventions — follow EXACTLY

Every deviation fragments the store into disconnected near-duplicates.

- **project_id**: lowercase snake_case, ONE stable id per project
  (`my_app`). Before your first write, list existing projects
  (`memstate_get.py` with no arguments) and reuse the exact id. Never
  invent a variant — `my-app`, `myapp`, and `my_app_dev` are three
  different projects.
- **keypath segments**: lowercase snake_case only (`[a-z0-9_]`), joined by
  dots. Dates are `YYYY_MM_DD` inside a segment:
  `task.summary.2026_07_04` — never `2026-07-04`, camelCase, or spaces.
- **keypath shape**: `<area>.<topic>` or `<area>.<topic>.<detail>`.
  Preferred area segments: `decisions`, `todo`, `notes`, `gotchas`,
  `questions`, `files`, `config`, `arch`, `task.summary.<date>`.
- **One keypath = one fact.** To update a fact, write the SAME keypath
  with the new value; versioning keeps the old one. Do not create a
  sibling keypath for a new value of the same fact.
- **Git branches**: keypaths describe the main/default branch unless said
  otherwise. Facts only true on an unmerged branch go under
  `branches.<branch_slug>.<area>...`, branch name slugged to snake_case
  (`feature/foo-bar` → `branches.feature_foo_bar.todo`). When the branch
  merges, write the durable outcomes to normal top-level keypaths and
  recursively delete the `branches.<branch_slug>` subtree; if abandoned,
  just delete the subtree. Branch-independent knowledge (decisions taken,
  gotchas, architecture) always lives at the top level.
- **category**: kind of memory, ONE lowercase word from `decision`,
  `config`, `status`, `note`, `gotcha`, `reference`, `learning`.
  Filterable in search.
- **topics**: subject tags, lowercase snake_case (`auth,embeddings`).
  Search matches any listed topic.
- **source**: short provenance string, e.g. `claude-code session
  2026_07_04` or `user decision`. Shown in history.

## Workflows

### Before starting a task (recall)

```bash
# 0. List project ids — reuse the exact existing one
python3 scripts/memstate_get.py

# 1. Browse the project's keypath tree (names only, no content)
python3 scripts/memstate_get.py --project my_app

# 2. Read a subtree with content
python3 scripts/memstate_get.py --project my_app --keypath decisions --include-content

# 3. Search when you don't know the keypath
python3 scripts/memstate_search.py --project my_app --query "how is authentication configured"
python3 scripts/memstate_search.py --project my_app --query "auth setup" --mode semantic
```

### After completing a task (remember)

```bash
# One short fact at one keypath
python3 scripts/memstate_set.py \
  --project my_app --keypath config.port --value "8080" --category config

# Markdown summary, split by ## headings (one memory per section).
# Each section lands at <project_id>.<heading_slug>; ### headings nest
# one more dot segment; prose before the first ## lands at
# <project_id>.preamble. Pass --root "" to store sections at the top
# level instead, or --keypath to store ALL content as one memory.
python3 scripts/memstate_remember.py \
  --project my_app \
  --content "## Decisions\nSwitched JWT -> sessions.\n\n## Gotchas\nCookie must be SameSite=Lax." \
  --source "claude-code session 2026_07_04" --category note
```

Heading names `TODOs`, `Decisions`, `Open Questions`, `Files`, `Notes`,
`Gotchas` collapse to the canonical segments `todo`, `decisions`,
`questions`, `files`, `notes`, `gotchas`.

### History and cleanup

```bash
# How a fact changed over time (newest first, includes tombstones)
python3 scripts/memstate_history.py --project my_app --keypath config.port

# Tombstone one keypath (history preserved; rewriting resurrects it)
python3 scripts/memstate_delete.py --project my_app --keypath config.old_setting

# Tombstone a whole subtree, e.g. a merged branch's state
python3 scripts/memstate_delete.py --project my_app --keypath branches.feature_foo_bar --recursive

# Soft-delete a project (any later write to the same id revives it)
python3 scripts/memstate_delete_project.py --project my_app
```

## Script reference

### `memstate_set.py` — one fact at one keypath

```bash
python3 scripts/memstate_set.py \
  --project PROJECT_ID --keypath KEYPATH --value VALUE \
  [--source TEXT] [--category WORD] [--topics TAG1,TAG2]
```

**Response:** `action` (`created` | `superseded` | `unchanged`), `stored`
(the new memory), `superseded` (the prior version, if any). `unchanged`
means identical content AND metadata — no new version was written.

### `memstate_remember.py` — markdown or text

```bash
python3 scripts/memstate_remember.py \
  --project PROJECT_ID --content "MARKDOWN" \
  [--keypath KEYPATH]        # store everything as ONE memory here
  [--root PREFIX]            # heading-split mode: replace the <project_id>. prefix ("" = none)
  [--source TEXT] [--category WORD] [--topics TAG1,TAG2]
```

Splitting is a deterministic parse of `##`+ headings — no LLM, no job
queue, synchronous. **Response:** `method` (`explicit` | `headings`),
`items[]` each with `keypath`, `action`, `stored`, `superseded?`.

### `memstate_get.py` — browse and retrieve

```bash
python3 scripts/memstate_get.py                               # list project ids
python3 scripts/memstate_get.py --project ID                  # keypath tree (names only)
python3 scripts/memstate_get.py --project ID --keypath KP --include-content
python3 scripts/memstate_get.py --memory-id N                 # one memory by integer id
```

**Response:** projects → `projects[]`; tree → `domains[]`,
`total_memories`; subtree → `memories[]`, `total_count`. The tree gives
names only — pass a keypath (with `--include-content`) to read values.

### `memstate_search.py` — find current memories

```bash
python3 scripts/memstate_search.py --query "PLAIN WORDS" \
  [--project ID]             # omit to search ALL projects
  [--mode fts|semantic]      # fts = literal words (default); semantic = by meaning (needs Ollama)
  [--threshold 0.0-1.0]      # semantic only, default 0.5
  [--category WORD] [--topics TAG1,TAG2]   # topics = match any
  [--keypath-prefix KP]      # only this keypath or below, e.g. branches.feature_x
  [--limit N]                # default 20
```

Only the current version of each keypath is searchable; tombstoned
keypaths and soft-deleted projects never match. Query text is plain
words — punctuation is safe, no boolean syntax.
**Response:** `results[]`, `total_found`, `query`, `mode` (+ `score` per
result and `threshold`/`model` in semantic mode).

### `memstate_history.py` — version chain of one keypath

```bash
python3 scripts/memstate_history.py --project ID --keypath KP
python3 scripts/memstate_history.py --memory-id N
```

**Response:** `versions[]` (newest first, includes tombstones), `total_versions`.

### `memstate_delete.py` — tombstone keypath(s)

```bash
python3 scripts/memstate_delete.py --project ID --keypath KP [--recursive]
```

**Response:** `deleted_count`, `deleted_keypaths[]`.

### `memstate_delete_project.py` — soft-delete a project

```bash
python3 scripts/memstate_delete_project.py --project ID
```

**Response:** `project_id`, `deleted_count`. Reads fail afterwards; any
write to the same project_id revives it with all memories intact.

## Best practices

1. **Reuse ids.** List projects before your first write; never guess or
   re-slug a project_id from the directory name if a listed id exists.
2. **Update, don't duplicate.** Same keypath, new value. The version
   chain is the changelog.
3. **Search before browsing.** `memstate_search.py` beats walking the
   tree when you know roughly what you want; use `--mode semantic` when
   the stored wording probably differs from yours.
4. **Only current versions surface.** Search and get return the latest
   non-deleted version per keypath; use `memstate_history.py` to see the
   past. There is no `is_latest` flag to check.
5. **Categorize decisions.** `--category decision` on architecture
   choices makes them retrievable with one filtered search.
6. **Keep branch state quarantined.** In-flight branch facts under
   `branches.<slug>.*`; promote to top level on merge, then recursively
   delete the subtree. Scope a search to one branch with
   `--keypath-prefix branches.<slug>`.

## Connecting to the daemon

```bash
export MEMSTATE_ADDR="127.0.0.1:8765"     # attach to a shared daemon
export MEMSTATE_LOCAL_URL="http://127.0.0.1:9000/api/v1"  # or full URL override
```

With neither set, each script spawns its own daemon on a random port
against `$MEMSTATE_DB` and stops it on exit. A manually started
`memstated --addr HOST:PORT` probes the port first: if another memstated
already owns it, it exits quietly; if an unrelated process does, it exits
loudly (code 2).
