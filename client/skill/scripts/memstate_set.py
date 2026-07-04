#!/usr/bin/env python3
"""Store a single fact at a keypath (memstated)."""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Set a single fact at a keypath")
    ap.add_argument("--project", required=True)
    ap.add_argument("--keypath", required=True)
    ap.add_argument("--value", required=True)
    ap.add_argument("--source", default=None)
    ap.add_argument("--category", default=None,
                    help="optional label, e.g. decision, config")
    ap.add_argument("--topics", default=None,
                    help="comma-separated tags for filtered search")
    args = ap.parse_args()

    body = {
        "project_id": args.project,
        "keypath": args.keypath,
        "content": args.value,
    }
    if args.source:
        body["source"] = args.source
    if args.category:
        body["category"] = args.category
    if args.topics:
        body["topics"] = args.topics.split(",")
    return post("/memories/store", body)


if __name__ == "__main__":
    sys.exit(main())
