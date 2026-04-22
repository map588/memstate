#!/usr/bin/env python3
"""Show the version chain for a keypath (local memstated)."""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="View version history for a keypath")
    ap.add_argument("--project")
    ap.add_argument("--keypath")
    ap.add_argument("--memory-id", type=int)
    args = ap.parse_args()

    if args.memory_id is not None:
        body = {"memory_id": args.memory_id}
    elif args.project and args.keypath:
        body = {"project_id": args.project, "keypath": args.keypath}
    else:
        print("Error: provide --memory-id OR both --project and --keypath",
              file=sys.stderr)
        return 1
    return post("/memories/history", body)


if __name__ == "__main__":
    sys.exit(main())
