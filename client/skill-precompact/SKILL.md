---
name: memstate-precompact
description: >
  Run right before /compact. Saves this session's working state to memstate,
  then prints the /compact instruction line to paste, so the next context
  window starts from memstate instead of a lossy summary.
license: MIT
tags:
  - memory
  - memstate
  - compact
---

# Before a compact

A compact replaces this context with a summary. What the next context
window needs and memstate does not hold is lost. Do the two steps below
in order, then stop. Do not start new work after step 2.

## 1. Write the session state to memstate

Target: the session's project (the cwd project, or the pinned project).
Use `memstate_remember` for prose and `memstate_set` for one fact. Write
only what the code and the git history cannot answer.

Write these keypaths. Skip one only when it has nothing new.

| Keypath | Content |
|---|---|
| `task.summary.<YYYY_MM_DD>` | One paragraph: the task as the user stated it, what is done with its evidence (tests run, commits made), what is not done, and the exact next step. |
| `decisions.<topic>` | One keypath per choice made this session, with the reason. |
| `gotchas.<topic>` | One keypath per trap found: what failed, the cause, the fix. |
| `todo` | Unfinished items, the ones the user asked for and the ones you found. |
| `questions` | Open questions for the user, with the context an answer needs. |
| `notes.<topic>` | Facts you had to look up and would need again: how a subsystem works, the command that verified a step, a URL. |

Rules:

- Refer to code by symbol name, never by file and line.
- Update an existing keypath. Do not create a sibling for a new value of
  the same fact. The write response names the superseded version.
- State that holds only on an unmerged branch goes under
  `branches.<branch_slug>.*`.
- Never save a denied prompt. Never save a secret.
- A fact about the user or this machine goes to `scope="user"` only when
  it holds in every repository.
- Do not read anything back. The write response is the receipt.

## 2. Print the compact instructions

Print the line below, filled in, as the last thing in your reply, inside
one fenced block. The user pastes it as the argument of `/compact`. Keep
it under 120 words. Name the keypaths exactly as you wrote them.

```
/compact Focus on: project <id>; task "<one line>"; done so far: <one line>; next step: <one line>; open questions: <list or none>. The session state is in memstate under <keypath list>. In the new context, before any other work, call memstate_get on each of those keypaths. Drop tool output, file contents and reasoning from the summary. Keep only what this line says.
```
