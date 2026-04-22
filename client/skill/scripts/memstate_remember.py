#!/usr/bin/env python3
"""Store a markdown summary at an explicit keypath (local memstated).

NOTE: auto-keypath-extraction is not supported in local mode; --keypath is required.
"""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Save a markdown summary at a keypath")
    ap.add_argument("--project", required=True)
    ap.add_argument("--keypath", required=True,
                    help="explicit keypath (auto-extraction is not supported)")
    ap.add_argument("--content", required=True)
    ap.add_argument("--source", default=None)
    ap.add_argument("--context", default=None)
    args = ap.parse_args()

    body = {
        "project_id": args.project,
        "keypath": args.keypath,
        "content": args.content,
    }
    if args.source:
        body["source"] = args.source
    if args.context:
        body["context"] = args.context
    return post("/memories/remember", body)


if __name__ == "__main__":
    sys.exit(main())
