#!/usr/bin/env python3
"""Install/uninstall the memstate hooks in ~/.claude/settings.json.

The name of a hook script selects its events (HOOK_EVENTS). Idempotent:
any existing entry of those events whose command references one of the
script names is replaced. Writes a .bak alongside.
"""
from __future__ import annotations
import json
import shutil
import sys
from pathlib import Path

SETTINGS = Path.home() / ".claude" / "settings.json"
# Script name -> the hook events that run it.
HOOK_EVENTS = {
    "memstate-persist-reminder": ("UserPromptSubmit",),
    "memstate-recall": ("UserPromptSubmit",),
    "memstate-compact": ("PreCompact", "PostCompact"),
}
EVENTS = sorted({event for events in HOOK_EVENTS.values() for event in events})


def is_memstate_hook(hook: dict) -> bool:
    command = hook.get("command", "")
    return any(marker in command for marker in HOOK_EVENTS)


def load() -> dict:
    if SETTINGS.exists():
        return json.loads(SETTINGS.read_text(encoding="utf-8") or "{}")
    return {}


def save(data: dict) -> None:
    SETTINGS.parent.mkdir(parents=True, exist_ok=True)
    if SETTINGS.exists():
        shutil.copy2(SETTINGS, SETTINGS.with_suffix(".json.bak"))
    tmp = SETTINGS.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    tmp.replace(SETTINGS)


def strip_memstate(cfg: dict) -> None:
    hooks = cfg.get("hooks")
    if not hooks:
        return
    for event in EVENTS:
        clean = []
        for entry in hooks.get(event, []):
            kept = [h for h in entry.get("hooks", []) if not is_memstate_hook(h)]
            if kept:
                entry["hooks"] = kept
                clean.append(entry)
        if clean:
            hooks[event] = clean
        else:
            hooks.pop(event, None)
    if not hooks:
        cfg.pop("hooks", None)


def events_for(script_path: str) -> tuple[str, ...]:
    for marker, events in HOOK_EVENTS.items():
        if marker in Path(script_path).name:
            return events
    sys.exit(f"unknown hook script: {script_path}")


def install(script_paths: list[str]) -> None:
    cfg = load()
    strip_memstate(cfg)
    hooks = cfg.setdefault("hooks", {})
    for script_path in script_paths:
        for event in events_for(script_path):
            hooks.setdefault(event, []).append(
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
        sys.exit("usage: configure-claude-hook.py install <script-path>... | uninstall")
    cmd = sys.argv[1]
    if cmd == "install":
        if len(sys.argv) < 3:
            sys.exit("install requires at least one script path")
        install(sys.argv[2:])
    elif cmd == "uninstall":
        uninstall()
    else:
        sys.exit(f"unknown command: {cmd}")


if __name__ == "__main__":
    main()
