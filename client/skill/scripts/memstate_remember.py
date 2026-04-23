#!/usr/bin/env python3
"""Store a markdown summary (memstated).

Two modes:
- explicit: pass --keypath to write the whole content at that path.
- extract:  omit --keypath; each `## heading` in the markdown becomes its
            own keypath (deeper headings nest via dot segments). Use --root
            to apply a common prefix to every extracted keypath.

Server response (both modes): { method, items: [{keypath, action, stored, superseded?}] }.
"""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Save a markdown summary")
    ap.add_argument("--project", required=True)
    ap.add_argument("--keypath", default=None,
                    help="optional — omit to extract keypaths from ## headings")
    ap.add_argument("--content", required=True)
    ap.add_argument("--source", default=None)
    ap.add_argument("--root", default=None,
                    help="optional prefix applied to every extracted keypath")
    ap.add_argument("--context", default=None)
    args = ap.parse_args()

    body = {
        "project_id": args.project,
        "content": args.content,
    }
    if args.keypath:
        body["keypath"] = args.keypath
    if args.source:
        body["source"] = args.source
    if args.root:
        body["root"] = args.root
    if args.context:
        body["context"] = args.context
    return post("/memories/remember", body)


if __name__ == "__main__":
    sys.exit(main())
