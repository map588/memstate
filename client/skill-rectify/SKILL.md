---
name: memstate-rectify
description: >
  Bring the memstate memories of one project into the current form: one
  fact in one memory, no memory at the bare name of an area, lowercase
  snake_case keypaths, and text that starts with the question or the
  symptom. Use when the user asks to rectify, clean up, reformat or
  migrate the memstate of a project, or when memories from before the
  write rules of 2026-11-19 are in the way of a search.
license: MIT
tags:
  - memory
  - memstate
  - migration
---

# Rectify the memories of a project

A search finds a memory by its words and by its meaning. Memories from
before 2026-11-19 often have a form that a search does not find: a list
of 13 facts in one memory at `gotchas`, a keypath named after the day of
the discovery, a text that starts with the answer. The MCP tools refuse
or hint at such a write now. They do not change the memories that the
store holds already. You do that here.

**Target:** the project of this session (the cwd project, or the pinned
project). If the user named another project, pass its `project_id` on
each call. Do not change the shared scope unless the user asks for it.

The `memstate` skill ("Write a memory so that a search finds it") holds
the same rules with examples. This skill is complete without it.

## Rules for the work

- **Do not change what a memory says.** Change where the fact is and how
  the text starts. Do not drop a fact. Do not add a fact.
- **Do not check a fact against the code.** That is a different task. If
  you see that a fact is false or out of date, leave it and put it in
  your report.
- **Write the new memory first. Then delete the old one.** A delete keeps
  the history, and `memstate_history` shows each old version, so a
  mistake can be repaired.
- **Keep `category` and `topics`.** A read returns them. Pass them on
  each new memory that comes from the old one.
- **Do not ask the user about each memory.** Do the work and report. Ask
  only for a credential (see below) and for two memories that contradict.
- **Leave these alone:** `task.summary.*` and `task.compact.*` (records
  of one session, long by design), and the prefix `branches.<slug>` (the
  rules apply to the memories below it, and they stay below it).

## 1. Find the work

Run the audit. It reads each memory of the project, applies the write
rules, and writes nothing:

```bash
python3 ~/.claude/skills/memstate/scripts/memstate_audit.py             # the cwd project
python3 ~/.claude/skills/memstate/scripts/memstate_audit.py --project ID
```

Each finding has `keypath`, `bytes`, `rule` and `detail`. Without the
script, call `memstate_get()` and apply the table in step 2 by hand.

The audit sees the form and not the sense. Read each area for the rest
(`memstate_get` with `keypath="<area>"`; when the result is `truncated`,
read the keypaths one at a time). Note each memory that:

- starts with a name or with the answer, and not with the question or
  the symptom of a person who does not know the answer;
- has a keypath named after the time or the place of the discovery
  (`gotchas.faults_from_last_week`) and not after the fact;
- is a stub: "superseded by ...", "this keypath can be deleted";
- holds state that changes in each session (an installed version, a
  commit count, a test count) in `decisions` or `gotchas`;
- says "it", "this fix" or "the above" about a thing that the memory
  does not name;
- holds the same fact as another keypath;
- holds two or more facts, also when it is short.

In a large project, do one area, verify it, then do the next. The work
can stop at any point: a memory that is correct is not a finding in the
next audit.

## 2. Repair each memory

| Finding | Repair |
|---|---|
| `area_keypath`: a memory at `gotchas`, `decisions`, ... | Read it with `recursive=false`. Write each fact at `<area>.<topic>`. Then delete the area keypath with `recursive=false`. **`recursive=true` deletes every fact below the area.** |
| `keypath_format`: `Notes.Self-Test`, `task.summary.2026-07-03` | Read the old name. Write the text at the name in `use`. Delete the old name. Reads and deletes accept an old name. If the new name holds a memory already, read the two: for one fact, keep the text that says more; for two facts, give one of them another topic. |
| `preamble` | The text had no heading. Write it at `<area>.<topic>`, then delete `preamble`. If it only introduces the other sections, delete it. |
| `list_items`, `size` | If a search can ask for one item alone, write one memory for each item and delete the list. If the memory is one fact with a long reason, or steps that make sense only together, leave it. These two rules are hints, not faults. |
| `index`: a short memory that lists other keypaths | Make sure that each fact it names has its own keypath. Then delete it. The keypath tree is the index. |
| `credential` | Do not copy the text and do not repeat it. A delete is not sufficient, because the history keeps the text. Tell the user the keypath and the kind. Only the user can erase it: `memstate ui`, key `X`. Go on with the other memories. |
| Text that starts with the answer | Write the same keypath again. Start with the question or the symptom, then the fact, then the reason. For a gotcha: what went wrong, the cause, the fix. |
| Keypath named after a time or a place | Write the text at a keypath named after the fact. Delete the old keypath. |
| Stub | Delete it. A stub is still a search hit. |
| Session state in `decisions` or `gotchas` | Move it to `todo.<topic>`. If it is out of date, delete it and say so in the report. |
| Two keypaths with one fact | Keep the keypath with the better name and the text that says more. Delete the other. |

How to write the parts of a memory that you split:

- Give each part a keypath named after its fact:
  `gotchas.macos_key_event_skips_shortcuts`, not `gotchas.item_3`.
- Make each part complete alone. Name each thing in full, because a
  search returns one memory without the others.
- Use `memstate_set` for one part. For several parts of one area, one
  `memstate_remember` call is sufficient: `## Gotchas`, then one
  `### <topic>` heading for each fact, with no text between the area
  heading and the first topic.

The tools refuse a write in the wrong form, and the error says what to
change. A write result can carry `hint`: split the memory further, or
decide that it is one fact and go on.

## 3. Verify

1. Run the audit again. Each finding that stays must be one that you
   left on purpose: a long memory with one fact, or a credential that
   waits for the user.
2. Call `memstate_get()`. No area holds a memory at its bare name.
3. Take three facts that you rewrote. For each one, call
   `memstate_search` with the words of a person who has the problem and
   does not know the answer. Do not use words of the keypath. The fact
   must be in the first three hits. If it is not, its first sentence is
   still the answer and not the question: write it again.

## 4. Report

Tell the user, with numbers: memories split (and into how many), keypaths
renamed, texts rewritten, memories deleted, and findings left with the
reason for each. List separately what needs the user: each credential,
each pair of memories that contradict, each fact that looks false.

Then write one memory at `task.summary.<YYYY_MM_DD>` with the same
numbers and the findings that you left on purpose, so that the next run
of this skill does not judge them again.
