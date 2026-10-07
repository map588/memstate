#!/usr/bin/env python3
"""Store a markdown summary (memstated).

Two modes:
- explicit: pass --keypath to write the whole content at that path.
- extract:  omit --keypath; each `## heading` in the markdown becomes its
            own keypath (deeper headings nest via dot segments). Use --root
            to apply a common prefix to every extracted keypath.

--scope user writes to the reserved user scope. Only sections that land at
preferences.* or profile.* pass the daemon's allowlist there; host facts
need an explicit --keypath host.<host_slug>.env.* or .tools.*. One bad
section rejects the whole call, nothing is written.

Server response (both modes): { method, items: [{keypath, action, stored, superseded?}] }.
stored and superseded name the versions (id, keypath, version, ...) and
carry no content; superseded has a 40-word preview.
"""
import argparse
import sys

from _client import add_scope_args, add_write_args, check_write_target, post, resolve_project


def main() -> int:
    ap = argparse.ArgumentParser(description="Save a markdown summary")
    add_scope_args(ap)
    add_write_args(ap)
    ap.add_argument("--keypath", default=None,
                    help="optional — omit to extract keypaths from ## headings")
    ap.add_argument("--content", required=True)
    ap.add_argument("--source", default=None)
    ap.add_argument("--root", default=None,
                    help="optional prefix applied to every extracted keypath")
    ap.add_argument("--category", default=None,
                    help="optional label applied to every written section")
    ap.add_argument("--topics", default=None,
                    help="comma-separated tags applied to every written section")
    args = ap.parse_args()

    project = resolve_project(args)
    check_write_target(args, project)
    body = {
        "project_id": project,
        "content": args.content,
    }
    if args.keypath:
        body["keypath"] = args.keypath
    if args.source:
        body["source"] = args.source
    if args.root:
        body["root"] = args.root
    if args.category:
        body["category"] = args.category
    if args.topics:
        body["topics"] = args.topics.split(",")
    return post("/memories/remember", body)


if __name__ == "__main__":
    sys.exit(main())
