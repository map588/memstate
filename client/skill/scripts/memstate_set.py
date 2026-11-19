#!/usr/bin/env python3
"""Store a single fact at a keypath (memstated).

--scope shared writes to the reserved shared scope. The daemon accepts only
preferences.*, profile.*, host.<host_slug>.env.* and host.<host_slug>.tools.*
there; see SKILL.md, "Shared scope".

The script refuses a keypath that is the bare name of an area, a keypath
that is not lowercase snake_case, and a value that holds a credential. The
result carries "hint" when a search will not find the memory well; see
SKILL.md, "Write a memory so that a search finds it".
"""
import argparse
import sys

from _client import (add_scope_args, add_write_args, check_write, check_write_target, fact_hint,
                     host_topics, post_write, resolve_project)


def main() -> int:
    ap = argparse.ArgumentParser(description="Set a single fact at a keypath")
    add_scope_args(ap)
    add_write_args(ap)
    ap.add_argument("--keypath", required=True)
    ap.add_argument("--value", required=True)
    ap.add_argument("--source", default=None)
    ap.add_argument("--category", default=None,
                    help="optional label, e.g. decision, config")
    ap.add_argument("--topics", default=None,
                    help="comma-separated tags for filtered search")
    args = ap.parse_args()

    check_write(args, args.keypath, args.value)
    project = resolve_project(args)
    check_write_target(args, project)
    body = {
        "project_id": project,
        "keypath": args.keypath,
        "content": args.value,
    }
    if args.source:
        body["source"] = args.source
    if args.category:
        body["category"] = args.category
    topics = host_topics(args, args.topics.split(",") if args.topics else None)
    if topics:
        body["topics"] = topics
    return post_write("/memories/store", body, fact_hint(args.keypath, args.value))


if __name__ == "__main__":
    sys.exit(main())
