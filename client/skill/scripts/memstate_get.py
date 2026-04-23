#!/usr/bin/env python3
"""Browse and retrieve memories from the memstated daemon.

Usage:
  memstate_get.py                                    # List all projects
  memstate_get.py --project myapp                    # Full project tree
  memstate_get.py --project myapp --keypath db       # Subtree at keypath
  memstate_get.py --project myapp --keypath db --include-content
  memstate_get.py --memory-id 42                     # Single memory by numeric ID
"""
import argparse
import sys
import urllib.parse

from _client import get, post


def main() -> int:
    ap = argparse.ArgumentParser(description="Browse and retrieve memories (server)")
    ap.add_argument("--project")
    ap.add_argument("--keypath")
    ap.add_argument("--memory-id", type=int)
    ap.add_argument("--include-content", action="store_true")
    # accepted for compatibility; at-revision time-travel is not yet implemented
    ap.add_argument("--at-revision", type=int)
    args = ap.parse_args()

    if args.memory_id is not None:
        return get(f"/memories/{args.memory_id}")

    if args.project and args.keypath:
        body = {
            "project_id": args.project,
            "keypath": args.keypath,
            "recursive": True,
            "include_content": args.include_content,
        }
        if args.at_revision:
            body["at_revision"] = args.at_revision
        return post("/keypaths", body)

    if args.project:
        return get(f"/tree?project_id={urllib.parse.quote(args.project)}")

    return get("/projects")


if __name__ == "__main__":
    sys.exit(main())
