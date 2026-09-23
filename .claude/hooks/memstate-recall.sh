#!/usr/bin/env bash
# Claude Code UserPromptSubmit hook: inject memories related to the prompt.
# All logic lives in `memstated recall`. It reads the hook event on stdin,
# needs a shared daemon (MEMSTATE_ADDR or ~/.memstate/daemon.addr), and
# always exits 0 so a failure never blocks the prompt.
command -v memstated >/dev/null 2>&1 || exit 0
exec memstated recall
