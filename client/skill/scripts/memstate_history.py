#!/usr/bin/env python3
"""Show the version chain for a keypath (memstated). --scope shared targets the shared scope."""
import argparse
import sys

from _client import add_scope_args, post, resolve_project


def main() -> int:
    ap = argparse.ArgumentParser(description="View version history for a keypath")
    add_scope_args(ap)
    ap.add_argument("--keypath")
    ap.add_argument("--memory-id", type=int)
    args = ap.parse_args()

    if args.memory_id is not None:
        body = {"memory_id": args.memory_id}
    elif args.keypath:
        body = {"project_id": resolve_project(args), "keypath": args.keypath}
    else:
        print("Error: provide --memory-id OR --keypath", file=sys.stderr)
        return 1
    return post("/memories/history", body)


if __name__ == "__main__":
    sys.exit(main())
