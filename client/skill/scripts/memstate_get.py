#!/usr/bin/env python3
"""Browse and retrieve memories from the memstated daemon.

Usage:
  memstate_get.py                                    # This repo's tree plus the shared scope (names only)
  memstate_get.py --scope shared                       # The shared scope tree, pruned to this host
  memstate_get.py --list-projects                    # List all project ids
  memstate_get.py --keypath db --include-content    # Subtree with content
  memstate_get.py --scope shared --keypath preferences # Shared-scope subtree with content
  memstate_get.py --project other_app --keypath db  # Another project's subtree
  memstate_get.py --memory-id 42                     # Single memory by numeric ID
"""
import argparse
import sys
import urllib.error
import urllib.parse

from _client import (SHARED_PROJECT, add_scope_args, count_values, emit, fetch,
                     get, host_slug, list_projects_visible, prune_other_hosts,
                     resolve_project)


def _tree(project: str) -> dict:
    return fetch("GET", f"/tree?project_id={urllib.parse.quote(project)}")


def _shared_tree() -> dict:
    """The shared scope, pruned to this machine's host.<slug> subtree. A
    soft-deleted _shared project (HTTP 409) yields an empty scope so the
    project tree still prints."""
    try:
        domains = _tree(SHARED_PROJECT).get("domains") or []
    except urllib.error.HTTPError:
        domains = []
    domains = prune_other_hosts(domains)
    return {"host": host_slug(), "domains": domains, "total_memories": count_values(domains)}


def main() -> int:
    ap = argparse.ArgumentParser(description="Browse and retrieve memories (server)")
    add_scope_args(ap)
    ap.add_argument("--list-projects", action="store_true",
                    help="list every project id in the store")
    ap.add_argument("--keypath")
    ap.add_argument("--memory-id", type=int)
    ap.add_argument("--include-content", action="store_true")
    args = ap.parse_args()

    if args.list_projects:
        return emit(list_projects_visible)

    if args.memory_id is not None:
        return get(f"/memories/{args.memory_id}")

    project = resolve_project(args)
    if args.keypath:
        body = {
            "project_id": project,
            "keypath": args.keypath,
            "recursive": True,
            "include_content": args.include_content,
        }
        return emit(lambda: fetch("POST", "/keypaths", body))

    if project == SHARED_PROJECT:
        return emit(_shared_tree)

    # The shared scope rides along with every project tree, like the proxy.
    def combined() -> dict:
        tree = _tree(project)
        tree["shared"] = _shared_tree()
        return tree

    return emit(combined)


if __name__ == "__main__":
    sys.exit(main())
