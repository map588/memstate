#!/usr/bin/env python3
"""Search memories (memstated).

Two modes:
- fts (default): SQLite FTS5 keyword match on content + keypath.
- semantic:      cosine similarity between the query and stored keypath
                 embeddings. Requires Ollama running locally with the
                 configured embed model (default nomic-embed-text).
"""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Search memories")
    ap.add_argument("--query", required=True)
    ap.add_argument("--project", default=None)
    ap.add_argument("--limit", type=int, default=20)
    ap.add_argument("--mode", choices=("fts", "semantic"), default="fts")
    ap.add_argument("--threshold", type=float, default=None,
                    help="semantic only: cosine floor for hits (default 0.5)")
    args = ap.parse_args()

    body = {"query": args.query, "limit": args.limit, "mode": args.mode}
    if args.project:
        body["project_id"] = args.project
    if args.threshold is not None:
        body["threshold"] = args.threshold
    return post("/memories/search", body)


if __name__ == "__main__":
    sys.exit(main())
