---
name: memstate-local
description: >
  Persistent, versioned memory via a LOCAL memstated daemon. Data lives in a
  single SQLite file on your machine; no cloud, no API key. Use for storing
  facts, recalling memory, managing projects, and full-text search of agent
  summaries. Supports Markdown and direct keypath = value assignment.
license: MIT
tags:
  - memory
  - agent-memory
  - memstate
  - versioned-memory
  - keypath
  - local
---

# Memstate (local) Memory Management

This skill talks to a **local** memstated daemon running on loopback (default
`127.0.0.1:8765`). The daemon is a single Go binary; data is stored in a
SQLite file at `$MEMSTATE_DB` (default `~/.memstate/memstate.db`). No API key
is needed. If the daemon is not running, the TS MCP proxy auto-starts it;
when calling these scripts directly, start it yourself with `memstated` or
have an MCP session running first.

## Core Concepts

| Concept | Description |
|---|---|
| **Project** | Top-level container for memories (e.g., `my_app`, `backend_api`). Auto-created on first write. |
| **Keypath** | Dot-separated hierarchical path (e.g., `auth.method`). Auto-prefixed with `project.{project_id}.` |
| **Memory** | A single fact or markdown summary stored at a keypath with full version history. |
| **Versioning** | Writing to an existing keypath supersedes the old value. History is always preserved. |
| **Tombstone** | Deleting a keypath creates a tombstone version — history is never destroyed. |

## Input Formats

### Direct keypath = value assignment
```
config.port = 8080
database.engine = PostgreSQL 16
auth.method = JWT with httpOnly cookies
status.deployment = production
```

### Markdown (preferred for task summaries)
```markdown
## Architecture Decision
- Database: PostgreSQL 16
- Auth: JWT with httpOnly cookies
- Deploy: Docker on AWS ECS
- API style: REST with OpenAPI 3.1
```

## Workflows

### Before Starting a Task (Recall)

Always check what already exists before making decisions or modifying code.

```bash
# 1. Semantic search — find relevant facts by meaning
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_search.py \
  --project "my_app" \
  --query "how is authentication configured"

# 2. Browse the full project tree (all domains and keypaths)
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py \
  --project "my_app"

# 3. Get a specific subtree with full content
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py \
  --project "my_app" --keypath "database" --include-content
```

### After Completing a Task (Remember)

```bash
# Store a single fact (config, status, version numbers)
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_set.py \
  --project "my_app" \
  --keypath "config.port" \
  --value "8080" \
  --category "fact"

# Store a rich markdown summary (AI extracts keypaths automatically)
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_remember.py \
  --project "my_app" \
  --content "## Auth Migration\n- Changed from JWT to server-side sessions\n- Added MFA via TOTP\n- Files: auth.go, middleware.go" \
  --source "agent"
```

### Manage History and Cleanup

```bash
# View how a fact changed over time
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_history.py \
  --project "my_app" --keypath "database.engine"

# Soft-delete an outdated keypath (history preserved)
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_delete.py \
  --project "my_app" --keypath "config.old_setting"

# Soft-delete an entire project
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_delete_project.py \
  --project "my_app"
```

## Script Reference

### `memstate_set.py` — Set a single keypath value

Stores one fact at a specific keypath. Synchronous, immediately available.
Supersedes the previous value if the keypath already exists.

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_set.py \
  --project PROJECT_ID \
  --keypath KEYPATH \
  --value VALUE \
  [--category CATEGORY]  # decision | fact | config | requirement | note | code | learning
  [--topics TAG1,TAG2]
```

**Response keys:** `action` (created|superseded), `memory_id`, `version`

---

### `memstate_remember.py` — Ingest markdown or text

Preferred for task summaries, meeting notes, or any multi-fact content.
The AI engine automatically extracts structured keypaths from your text.
Processing is async (~15–18 s); the script polls until completion.

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_remember.py \
  --project PROJECT_ID \
  --content "MARKDOWN_OR_TEXT" \
  [--source agent|readme|docs|meeting|code] \
  [--context "optional hint for extraction"]
```

**Response keys:** `status` (completed|failed), `job_id`, `memories_created`

---

### `memstate_get.py` — Browse and retrieve memories

```bash
# List all projects
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py

# Full project tree (returns domains and keypaths)
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py --project PROJECT_ID

# Subtree at a keypath
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py \
  --project PROJECT_ID --keypath KEYPATH [--include-content] [--at-revision N]

# Single memory by UUID
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_get.py --memory-id UUID
```

**Response keys (project tree):** `domains`, `total_memories`
**Response keys (subtree):** `memories`, `total_count`
**Response keys (list projects):** `projects`

---

### `memstate_search.py` — Semantic search

Find memories by meaning when you don't know the exact keypath.

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_search.py \
  --query "NATURAL_LANGUAGE_QUERY" \
  [--project PROJECT_ID] \
  [--limit N]  # default 20, max 100
```

**Response keys:** `results` (array), `total_found`, `query`

---

### `memstate_history.py` — Version history

View all versions of a keypath or memory chain.

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_history.py \
  --project PROJECT_ID --keypath KEYPATH
# or
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_history.py \
  --memory-id UUID
```

**Response keys:** `versions` (array), `total_versions`

---

### `memstate_delete.py` — Soft-delete a keypath

Creates a tombstone version. History is always preserved.

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_delete.py \
  --project PROJECT_ID \
  --keypath KEYPATH \
  [--recursive]  # delete entire subtree
```

**Response keys:** `deleted_count`, `deleted_keypaths`

---

### `memstate_delete_project.py` — Soft-delete a project

```bash
python3 /home/ubuntu/skills/memstate-ai/scripts/memstate_delete_project.py \
  --project PROJECT_ID
```

**Response keys:** `project_id`, `deleted_count`

---

## Best Practices

1. **One keypath = one fact.** Use `api.style` not `api`. Be specific.
2. **Update, don't duplicate.** When a fact changes, call `memstate_set.py` with the SAME keypath and the NEW value. Do not create a new keypath.
3. **Trust `is_latest: true`.** Search results may show multiple versions. Only trust results where `is_latest` is `true`.
4. **Use Markdown for summaries.** `memstate_remember.py` is the recommended API for agents to store memories. It excels at parsing Markdown lists, headings, and key-value pairs into structured keypaths automatically using custom-trained AI models.
5. **Search before browsing.** `memstate_search.py` is faster than browsing the tree when you know what you're looking for.
6. **Use categories.** Setting `--category decision` on architecture choices makes them easier to filter later.

## Connecting to the daemon

All scripts speak HTTP to `MEMSTATE_LOCAL_URL` (default
`http://127.0.0.1:8765/api/v1`). Override with either:

```bash
# Full URL override
export MEMSTATE_LOCAL_URL="http://127.0.0.1:9000/api/v1"

# Or just the host:port
export MEMSTATE_ADDR="127.0.0.1:9000"
```

Start the daemon manually (or let the TS MCP proxy auto-spawn it):

```bash
# if `memstated` is on PATH
memstated

# or run the in-repo build
../../server/memstated
```

The daemon probes `:8765` on start; if something else is already there, it
exits loudly instead of fighting for the port.

## Local mode caveats vs the hosted skill

- `memstate_remember` REQUIRES `--keypath` — the local daemon does not run the
  keypath-extraction model. Pick an explicit keypath (`task.summary.auth`,
  `decisions.2026-04-deploy`, etc.).
- `memstate_search` uses SQLite FTS5 (substring / phrase / boolean), not
  embeddings. Semantic search via Ollama is planned.
- `--category` and `--topics` are accepted for compatibility but not yet
  queried. `--at-revision` is accepted but not yet implemented.
- Memory IDs are integers (not UUIDs).
