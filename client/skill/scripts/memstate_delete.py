#!/usr/bin/env python3
"""Tombstone a keypath (memstated). --scope shared targets the shared scope."""
import argparse
import sys

from _client import add_scope_args, post, resolve_project


def main() -> int:
    ap = argparse.ArgumentParser(description="Soft-delete a keypath")
    add_scope_args(ap)
    ap.add_argument("--keypath", required=True)
    ap.add_argument("--recursive", action="store_true")
    args = ap.parse_args()

    return post("/memories/delete", {
        "project_id": resolve_project(args),
        "keypath": args.keypath,
        "recursive": args.recursive,
    })


if __name__ == "__main__":
    sys.exit(main())
