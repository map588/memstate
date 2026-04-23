#!/usr/bin/env python3
"""Install/uninstall the memstate UserPromptSubmit hook in ~/.claude/settings.json.

Idempotent: any existing UserPromptSubmit entry whose command references
`memstate-persist-reminder` is replaced. Writes a .bak alongside.
"""
from __future__ import annotations
import json
import shutil
import sys
from pathlib import Path

SETTINGS = Path.home() / ".claude" / "settings.json"
MARKER = "memstate-persist-reminder"


def load() -> dict:
    if SETTINGS.exists():
        return json.loads(SETTINGS.read_text() or "{}")
    return {}


def save(data: dict) -> None:
    SETTINGS.parent.mkdir(parents=True, exist_ok=True)
    if SETTINGS.exists():
        shutil.copy2(SETTINGS, SETTINGS.with_suffix(".json.bak"))
    tmp = SETTINGS.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, indent=2) + "\n")
    tmp.replace(SETTINGS)


def strip_memstate(cfg: dict) -> None:
    hooks = cfg.get("hooks")
    if not hooks:
        return
    ups = hooks.get("UserPromptSubmit", [])
    clean = []
    for entry in ups:
        kept = [h for h in entry.get("hooks", []) if MARKER not in h.get("command", "")]
        if kept:
            entry["hooks"] = kept
            clean.append(entry)
    if clean:
        hooks["UserPromptSubmit"] = clean
    else:
        hooks.pop("UserPromptSubmit", None)
    if not hooks:
        cfg.pop("hooks", None)


def install(script_path: str) -> None:
    cfg = load()
    strip_memstate(cfg)
    cfg.setdefault("hooks", {}).setdefault("UserPromptSubmit", []).append(
        {
            "matcher": "",
            "hooks": [{"type": "command", "command": script_path}],
        }
    )
    save(cfg)


def uninstall() -> None:
    if not SETTINGS.exists():
        return
    cfg = load()
    strip_memstate(cfg)
    save(cfg)


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit("usage: configure-claude-hook.py install <script-path> | uninstall")
    cmd = sys.argv[1]
    if cmd == "install":
        if len(sys.argv) < 3:
            sys.exit("install requires a script path")
        install(sys.argv[2])
    elif cmd == "uninstall":
        uninstall()
    else:
        sys.exit(f"unknown command: {cmd}")


if __name__ == "__main__":
    main()
