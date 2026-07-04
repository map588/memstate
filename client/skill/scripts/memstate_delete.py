#!/usr/bin/env python3
"""Tombstone a keypath (memstated)."""
import argparse
import sys

from _client import default_project, post


def main() -> int:
    ap = argparse.ArgumentParser(description="Soft-delete a keypath")
    ap.add_argument("--project", default=None,
                    help="project id (default: derived from repo/dir name)")
    ap.add_argument("--keypath", required=True)
    ap.add_argument("--recursive", action="store_true")
    args = ap.parse_args()

    return post("/memories/delete", {
        "project_id": args.project or default_project(),
        "keypath": args.keypath,
        "recursive": args.recursive,
    })


if __name__ == "__main__":
    sys.exit(main())
