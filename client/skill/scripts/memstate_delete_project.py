#!/usr/bin/env python3
"""Soft-delete an entire project (local memstated)."""
import argparse
import sys

from _client import post


def main() -> int:
    ap = argparse.ArgumentParser(description="Soft-delete an entire project")
    ap.add_argument("--project", required=True)
    args = ap.parse_args()
    return post("/projects/delete", {"project_id": args.project})


if __name__ == "__main__":
    sys.exit(main())
