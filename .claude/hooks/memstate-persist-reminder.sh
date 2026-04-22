#!/usr/bin/env bash
# Nudge Claude to call memstate_remember when file edits have accumulated
# since the last persist. Emits a reminder to stdout, which the
# UserPromptSubmit hook injects as additional context.
#
# Threshold: 3 Edit/Write/NotebookEdit tool uses since the most recent
# mcp__memstate__memstate_remember or mcp__memstate__memstate_set entry
# in the session transcript.
set -u

event="$(cat)"
transcript=$(printf '%s' "$event" | jq -r '.transcript_path // empty' 2>/dev/null)
if [ -z "$transcript" ] || [ ! -f "$transcript" ]; then
  exit 0
fi

last_save=$(grep -nE '"name"[[:space:]]*:[[:space:]]*"mcp__memstate__memstate_(remember|set)"' "$transcript" | tail -1 | cut -d: -f1)
last_save=${last_save:-0}

edits=$(awk -v n="$last_save" 'NR > n' "$transcript" \
  | grep -cE '"name"[[:space:]]*:[[:space:]]*"(Edit|Write|NotebookEdit)"' \
  || true)

THRESHOLD=3
if [ "$edits" -ge "$THRESHOLD" ]; then
  cat <<EOF
<memstate-reminder>
$edits file edits have happened since the last memstate_remember/_set.
Before wrapping up this turn, consider calling memstate_remember at a
meaningful keypath (e.g. session.$(date +%Y-%m-%d).closing_state, or an
area-specific keypath) to persist non-obvious decisions or state.
Skip if the edits are trivial or already captured.
</memstate-reminder>
EOF
fi
exit 0
