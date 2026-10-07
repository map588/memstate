#!/usr/bin/env python3
"""Search memories (memstated).

Three modes:
- hybrid (default): FTS match on any query word fused with the semantic
                 ranking (reciprocal rank fusion). When the embedder is
                 unavailable the FTS hits are returned alone and the
                 response carries a `degraded` reason.
- fts:           SQLite FTS5 keyword match on content + keypath. Every
                 query word must match.
- semantic:      cosine similarity between the query and embeddings of
                 the current content at each keypath. Requires Ollama
                 running locally with the configured embed model
                 (default nomic-embed-text; set MEMSTATE_EMBED_MODEL
                 or start memstated with --embed-model to change it).

--scope user searches the reserved user scope and drops hits that describe
another machine (host.<other_slug>.*).
"""
import argparse
import sys

from _client import add_scope_args, emit, fetch, is_other_host, resolve_project


def main() -> int:
    ap = argparse.ArgumentParser(description="Search memories")
    ap.add_argument("--query", required=True)
    add_scope_args(ap)
    ap.add_argument("--all-projects", action="store_true",
                    help="search every project instead of just this repo's")
    ap.add_argument("--limit", type=int, default=10)
    ap.add_argument("--include-content", action="store_true",
                    help="return the full content of each hit, not only a 40-word preview")
    ap.add_argument("--mode", choices=("hybrid", "fts", "semantic"), default="hybrid")
    ap.add_argument("--threshold", type=float, default=None,
                    help="semantic and hybrid: cosine floor for hits (default 0.5)")
    ap.add_argument("--category", default=None,
                    help="only return memories with this category")
    ap.add_argument("--topics", default=None,
                    help="comma-separated: match memories tagged with any of these")
    ap.add_argument("--keypath-prefix", default=None,
                    help="only memories at this keypath or below, e.g. branches.feature_x")
    args = ap.parse_args()

    body = {"query": args.query, "limit": args.limit, "mode": args.mode}
    if not args.all_projects:
        body["project_id"] = resolve_project(args)
    if args.include_content:
        body["include_content"] = True
    if args.threshold is not None:
        body["threshold"] = args.threshold
    if args.category:
        body["category"] = args.category
    if args.topics:
        body["topics"] = args.topics.split(",")
    if args.keypath_prefix:
        body["keypath_prefix"] = args.keypath_prefix

    def search():
        out = fetch("POST", "/memories/search", body)
        if args.scope == "user" and isinstance(out, dict) and isinstance(out.get("results"), list):
            out["results"] = [h for h in out["results"] if not is_other_host(h.get("keypath", ""))]
            out["total_found"] = len(out["results"])
        return out

    return emit(search)


if __name__ == "__main__":
    sys.exit(main())
