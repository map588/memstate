#!/usr/bin/env python3
"""
End-to-end test suite for the Memstate AI Skill.
Runs every script against a private child daemon on a temporary DB. It
never attaches to the shared daemon: MEMSTATE_CHILD=1 forces child mode,
MEMSTATE_ADDR and MEMSTATE_LOCAL_URL are dropped, and MEMSTATE_DB points
at a fresh temporary directory unless the caller sets it.
"""
import json
import os
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import _client  # noqa: E402

SCRIPTS_DIR = os.path.dirname(os.path.abspath(__file__))
# Project ids are snake_case; the scripts refuse any other shape.
PROJECT_ID = f"skill_test_{int(time.time())}"

# Hermetic environment for every script this suite spawns.
os.environ.pop("MEMSTATE_ADDR", None)
os.environ.pop("MEMSTATE_LOCAL_URL", None)
os.environ["MEMSTATE_CHILD"] = "1"
os.environ["MEMSTATE_CONFIG"] = "off"
os.environ["MEMSTATE_NO_UPDATE_CHECK"] = "1"
if not os.environ.get("MEMSTATE_DB"):
    _TMP = tempfile.TemporaryDirectory(prefix="memstate-skilltest-")
    os.environ["MEMSTATE_DB"] = os.path.join(_TMP.name, "memstate.db")

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
            data = json.loads(stdout)
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
        ["--project", PROJECT_ID, "--new-project", "--keypath", "database.engine", "--value", "PostgreSQL", "--category", "decision"],
        expect_key="action", expect_value="created"
    )

    # 2. memstate_set — update the same keypath (should supersede)
    ok, out = test(
        "memstate_set: update same keypath (supersede)",
        "memstate_set.py",
        ["--project", PROJECT_ID, "--keypath", "database.engine", "--value", "PostgreSQL 16", "--category", "decision"],
        expect_key="action", expect_value="superseded"
    )
    memory_id = None
    if ok:
        try:
            memory_id = json.loads(out)["stored"]["id"]
        except Exception:
            pass

    # 3. memstate_set — store a second fact
    test(
        "memstate_set: store a second fact",
        "memstate_set.py",
        ["--project", PROJECT_ID, "--keypath", "auth.method", "--value", "JWT with httpOnly cookies", "--category", "decision"],
        expect_key="action", expect_value="created"
    )

    # 3b. memstate_set --host — tags the write with host_<host_slug>
    code, out, err = run_script(
        "memstate_set.py",
        "--project", PROJECT_ID, "--host", "--keypath", "build.toolchain",
        "--value", "arm cross toolchain", "--topics", "build",
    )
    host_topic = "host_" + _client.host_slug()
    try:
        got = json.loads(out)["stored"].get("topics")
    except Exception:
        got = None
    passed = code == 0 and got == ["build", host_topic]
    print(f"{PASS if passed else FAIL} memstate_set: --host adds the host topic"
          + ("" if passed else f" (got {got!r}; {err.strip()})"))
    results.append(("memstate_set: --host adds the host topic", passed))

    # 4. memstate_get — list all projects
    test(
        "memstate_get: list all projects",
        "memstate_get.py",
        ["--list-projects"],
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
            ["--memory-id", str(memory_id)],
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

    # 10. memstate_remember — heading extraction, one memory per ## section
    test(
        "memstate_remember: markdown ingestion",
        "memstate_remember.py",
        ["--project", PROJECT_ID,
         "--content", "## Architecture Summary\n- Backend: FastAPI\n- Database: PostgreSQL 16\n- Auth: JWT with httpOnly cookies\n- Deploy: Docker on AWS ECS",
         "--source", "agent"],
        expect_key="method", expect_value="headings"
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

    # 13. shared scope — the daemon allows only the allowlisted shapes there.
    user_kp = f"preferences.skill_test_{int(time.time())}"
    test(
        "memstate_set: --scope shared accepts a preferences fact",
        "memstate_set.py",
        ["--scope", "shared", "--keypath", user_kp, "--value", "terse answers"],
        expect_key="action", expect_value="created"
    )
    code, stdout, stderr = run_script(
        "memstate_set.py", "--scope", "shared", "--keypath", "todo.skill_test", "--value", "nope")
    rejected = code == 1 and "HTTP 400" in stderr and "allowed" in stderr
    print(f"{PASS if rejected else FAIL} memstate_set: --scope shared rejects a todo keypath")
    if not rejected:
        print(f"       code={code} stderr={stderr.strip()[:300]}")
    results.append(("memstate_set: --scope shared rejects a todo keypath", rejected))
    test(
        "memstate_get: --scope shared tree carries shared.host",
        "memstate_get.py",
        ["--scope", "shared"],
        expect_key="host"
    )
    test(
        "memstate_get: project tree carries the shared scope",
        "memstate_get.py",
        ["--project", "skill_test_other"],
        expect_key="shared"
    )
    test(
        "memstate_delete: --scope shared cleans up",
        "memstate_delete.py",
        ["--scope", "shared", "--keypath", user_kp],
        expect_key="deleted_count"
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
