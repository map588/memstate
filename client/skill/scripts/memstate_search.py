#!/usr/bin/env python3
"""Search memories (memstated).

Two modes:
- fts (default): SQLite FTS5 keyword match on content + keypath.
- semantic:      cosine similarity between the query and embeddings of
                 the current content at each keypath. Requires Ollama
                 running locally with the configured embed model
                 (default nomic-embed-text).
"""
import argparse
import sys

from _client import default_project, post


def main() -> int:
    ap = argparse.ArgumentParser(description="Search memories")
    ap.add_argument("--query", required=True)
    ap.add_argument("--project", default=None,
                    help="project id (default: derived from repo/dir name)")
    ap.add_argument("--all-projects", action="store_true",
                    help="search every project instead of just this repo's")
    ap.add_argument("--limit", type=int, default=20)
    ap.add_argument("--mode", choices=("fts", "semantic"), default="fts")
    ap.add_argument("--threshold", type=float, default=None,
                    help="semantic only: cosine floor for hits (default 0.5)")
    ap.add_argument("--category", default=None,
                    help="only return memories with this category")
    ap.add_argument("--topics", default=None,
                    help="comma-separated: match memories tagged with any of these")
    ap.add_argument("--keypath-prefix", default=None,
                    help="only memories at this keypath or below, e.g. branches.feature_x")
    args = ap.parse_args()

    body = {"query": args.query, "limit": args.limit, "mode": args.mode}
    if not args.all_projects:
        body["project_id"] = args.project or default_project()
    if args.threshold is not None:
        body["threshold"] = args.threshold
    if args.category:
        body["category"] = args.category
    if args.topics:
        body["topics"] = args.topics.split(",")
    if args.keypath_prefix:
        body["keypath_prefix"] = args.keypath_prefix
    return post("/memories/search", body)


if __name__ == "__main__":
    sys.exit(main())
