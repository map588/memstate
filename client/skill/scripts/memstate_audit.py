#!/usr/bin/env python3
"""List the memories of a project that break the write rules (memstated).

The MCP tools and the scripts refuse a bad write or give a hint. A memory
from before those rules is still in the store. This script reads each
current memory of one project and applies the same rules to it. It
writes nothing. The memstate-rectify skill uses it to find the work and
to verify the result.

Response: { project_id, total_memories, findings: [{keypath, bytes, rule,
detail, use?}], by_rule: {rule: count} }. A finding never holds the text
of a memory. Rules:

  area_keypath    a memory at the bare name of an area (decisions, gotchas, ...)
  keypath_format  a keypath that is not lowercase snake_case; "use" is the corrected one
  credential      a text with the shape of a credential
  preamble        text that a heading split stored with no heading
  list_items      5 or more list items in one memory
  index           a short memory that names 3 or more other keypaths
  size            more than 1500 bytes in one memory

The last three are hints, not faults: a memory that holds one fact can be
long. --scope shared reads the shared scope without the facts of other
machines.
"""

import argparse
import sys

from _client import (
    SHARED_PROJECT,
    add_scope_args,
    audit_memory,
    emit,
    fetch,
    is_other_host,
    resolve_project,
)


def audit(project: str) -> dict:
    shared = project == SHARED_PROJECT
    out = fetch("POST", "/keypaths", {"project_id": project, "include_content": True})
    memories = out.get("memories") or []
    if shared:
        memories = [m for m in memories if not is_other_host(m["keypath"])]
    findings, by_rule = [], {}
    for m in memories:
        content = m.get("content") or ""
        size = len(content.encode("utf-8"))
        for finding in audit_memory(m["keypath"], content, shared):
            findings.append({"keypath": m["keypath"], "bytes": size, **finding})
            by_rule[finding["rule"]] = by_rule.get(finding["rule"], 0) + 1
    return {
        "project_id": project,
        "total_memories": len(memories),
        "findings": findings,
        "by_rule": by_rule,
    }


def main() -> int:
    ap = argparse.ArgumentParser(
        description="List the memories that break the write rules"
    )
    add_scope_args(ap)
    args = ap.parse_args()
    project = resolve_project(args)
    return emit(lambda: audit(project))


if __name__ == "__main__":
    sys.exit(main())
