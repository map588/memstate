#!/usr/bin/env python3
"""
End-to-end test suite for the Memstate AI Skill.
Tests all 7 scripts against the live API.
"""
import json
import os
import subprocess
import sys
import time

SCRIPTS_DIR = os.path.dirname(os.path.abspath(__file__))
PROJECT_ID = f"skill-test-{int(time.time())}"

PASS = "\033[92m✅ PASS\033[0m"
FAIL = "\033[91m❌ FAIL\033[0m"
SKIP = "\033[93m⚠️  SKIP\033[0m"

results = []

def run_script(script, *args):
    cmd = [sys.executable, os.path.join(SCRIPTS_DIR, script)] + list(args)
    result = subprocess.run(cmd, capture_output=True, text=True)
    return result.returncode, result.stdout, result.stderr

def test(name, script, args, expect_key=None, expect_value=None):
    code, stdout, stderr = run_script(script, *args)
    passed = code == 0
    detail = ""
    if passed and expect_key:
        try:
            json_str = stdout
            if "Polling for completion..." in stdout:
                json_str = stdout.split("Polling for completion...")[1].strip()
            data = json.loads(json_str)
            actual = data.get(expect_key)
            if expect_value is not None and actual != expect_value:
                passed = False
                detail = f" (expected {expect_key}={expect_value!r}, got {actual!r})"
            elif actual is None:
                passed = False
                detail = f" (key '{expect_key}' missing from response)"
        except json.JSONDecodeError:
            passed = False
            detail = " (response was not valid JSON)"
    status = PASS if passed else FAIL
    print(f"{status} {name}{detail}")
    if not passed:
        if stderr:
            print(f"       stderr: {stderr.strip()}")
        if stdout:
            print(f"       stdout: {stdout.strip()[:300]}")
    results.append((name, passed))
    return passed, stdout

def main():
    print(f"\n{'='*60}")
    print(f"  Memstate AI Skill — End-to-End Test Suite")
    print(f"  Project: {PROJECT_ID}")
    print(f"{'='*60}\n")

    # 1. memstate_set — store a fact
    ok, out = test(
        "memstate_set: store a single fact",
        "memstate_set.py",
        ["--project", PROJECT_ID, "--keypath", "database.engine", "--value", "PostgreSQL", "--category", "decision"],
        expect_key="action", expect_value="created"
    )

    # 2. memstate_set — update the same keypath (should supersede)
    ok, out = test(
        "memstate_set: update same keypath (supersede)",
        "memstate_set.py",
        ["--project", PROJECT_ID, "--keypath", "database.engine", "--value", "PostgreSQL 16", "--category", "decision"],
        expect_key="version"
    )
    memory_id = None
    if ok:
        try:
            memory_id = json.loads(out).get("memory_id")
        except Exception:
            pass

    # 3. memstate_set — store a second fact
    test(
        "memstate_set: store a second fact",
        "memstate_set.py",
        ["--project", PROJECT_ID, "--keypath", "auth.method", "--value", "JWT with httpOnly cookies", "--category", "decision"],
        expect_key="action", expect_value="created"
    )

    # 4. memstate_get — list all projects (no args)
    test(
        "memstate_get: list all projects",
        "memstate_get.py",
        [],
        expect_key="projects"
    )

    # 5. memstate_get — project tree
    test(
        "memstate_get: project tree",
        "memstate_get.py",
        ["--project", PROJECT_ID],
        expect_key="domains"
    )

    # 6. memstate_get - subtree with content
    test(
        "memstate_get: subtree with --include-content",
        "memstate_get.py",
        ["--project", PROJECT_ID, "--keypath", "database", "--include-content"],
        expect_key="memories"
    )

    # 7. memstate_get — by memory_id
    if memory_id:
        test(
            "memstate_get: fetch by memory_id",
            "memstate_get.py",
            ["--memory-id", memory_id],
            expect_key="id", expect_value=memory_id
        )
    else:
        print(f"{SKIP} memstate_get: fetch by memory_id (no memory_id from previous step)")
        results.append(("memstate_get: fetch by memory_id", None))

    # 8. memstate_search — semantic search
    test(
        "memstate_search: semantic search",
        "memstate_search.py",
        ["--project", PROJECT_ID, "--query", "what database does the project use"],
        expect_key="results"
    )

    # 9. memstate_history - by keypath
    test(
        "memstate_history: by keypath",
        "memstate_history.py",
        ["--project", PROJECT_ID, "--keypath", "database.engine"],
        expect_key="versions"
    )

    # 10. memstate_remember — markdown ingestion (async)
    print(f"\n  [Note: memstate_remember polls for job completion, may take ~20s]")
    test(
        "memstate_remember: markdown ingestion",
        "memstate_remember.py",
        ["--project", PROJECT_ID,
         "--content", "## Architecture Summary\n- Backend: FastAPI\n- Database: PostgreSQL 16\n- Auth: JWT with httpOnly cookies\n- Deploy: Docker on AWS ECS",
         "--source", "agent"],
        expect_key="status", expect_value="complete"
    )

    # 11. memstate_delete — soft-delete a keypath
    test(
        "memstate_delete: soft-delete a keypath",
        "memstate_delete.py",
        ["--project", PROJECT_ID, "--keypath", "auth.method"],
        expect_key="deleted_count"
    )

    # 12. memstate_delete_project — cleanup
    test(
        "memstate_delete_project: cleanup test project",
        "memstate_delete_project.py",
        ["--project", PROJECT_ID],
        expect_key="project_id", expect_value=PROJECT_ID
    )

    # Summary
    passed = sum(1 for _, r in results if r is True)
    failed = sum(1 for _, r in results if r is False)
    skipped = sum(1 for _, r in results if r is None)
    total = len(results)

    print(f"\n{'='*60}")
    print(f"  Results: {passed}/{total} passed | {failed} failed | {skipped} skipped")
    print(f"{'='*60}\n")

    return 0 if failed == 0 else 1

if __name__ == "__main__":
    sys.exit(main())
