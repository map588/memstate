#!/usr/bin/env python3
"""Tombstone a keypath (local memstated)."""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Soft-delete a keypath")
    ap.add_argument("--project", required=True)
    ap.add_argument("--keypath", required=True)
    ap.add_argument("--recursive", action="store_true")
    args = ap.parse_args()

    return post("/memories/delete", {
        "project_id": args.project,
        "keypath": args.keypath,
        "recursive": args.recursive,
    })


if __name__ == "__main__":
    sys.exit(main())
