#!/usr/bin/env python3
"""Full-text search across memories (local memstated, SQLite FTS5)."""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Full-text search (FTS5)")
    ap.add_argument("--query", required=True)
    ap.add_argument("--project", default=None)
    ap.add_argument("--limit", type=int, default=20)
    args = ap.parse_args()

    body = {"query": args.query, "limit": args.limit}
    if args.project:
        body["project_id"] = args.project
    return post("/memories/search", body)


if __name__ == "__main__":
    sys.exit(main())
