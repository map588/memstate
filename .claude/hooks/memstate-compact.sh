#!/usr/bin/env bash
# Claude Code PreCompact and PostCompact hook: shape the compact summary,
# then file it in memstate. All logic lives in `memstated compact-hook`.
# It reads the hook event on stdin. Before a compact it prints the summary
# instructions and exits 0. After a compact it files each section of the
# summary as one memory through the shared daemon, which it starts when
# none runs. It exits 2 with the reason on stderr when it cannot store a
# part, which Claude Code shows to you.
command -v memstated >/dev/null 2>&1 || exit 0
# A memstated from before this verb takes the word as a daemon start and
# does not exit, which holds the compact until the hook times out. The
# help text says if the verb is there.
memstated --help 2>&1 | grep -q 'compact-hook' || exit 0
exec memstated compact-hook
