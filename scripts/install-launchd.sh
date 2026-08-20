#!/usr/bin/env bash
# Registers the sdlc engine as a macOS LaunchAgent.
#
# Unlike autoship — a scheduled one-shot that exits between ticks — `sdlc run`
# is a long-lived engine: it holds the SQLite database, the gradle slots and the
# device pool, and ticks itself every orchestrator.poll_interval. So launchd's
# job here is not "wake it up every N minutes", it is "start it at login and put
# it back if it dies": RunAtLoad + KeepAlive, not StartCalendarInterval.
#
# A LaunchAgent (not a LaunchDaemon) is used deliberately, for the same reason
# autoship uses one and the Windows script uses an S4U interactive-token
# principal: the agent CLIs (claude, agy) read per-user credentials, and the
# builds need the user's Gradle and Android SDK caches.
#
# Crash-safety is the engine's own (SPEC §7): every state entry records the
# worktree HEAD, and on restart the worktree is reconciled before the state
# re-runs. A KeepAlive restart is a normal event, not a recovery procedure.
#
# Usage:
#   scripts/install-launchd.sh --exe /path/to/sdlc --config /path/to/sdlc.yaml [options]
#
# Options:
#   --exe PATH             Path to the sdlc binary (required)
#   --config PATH          Path to sdlc.yaml (required)
#   --name NAME             Agent name suffix. Default: sdlc
#   --restart MINUTES       Wait this long before restarting a crashed engine. Default: 5
#   --once                  Run `sdlc run --once` at load (drain and exit) instead of
#                           the persistent engine. For the soak in docs/running.md.
#   --print-only            Print the plist without installing it.

set -euo pipefail

EXE=""
CONFIG=""
NAME="sdlc"
RESTART=5
ONCE=0
PRINT_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --exe) EXE="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --restart) RESTART="$2"; shift 2 ;;
    --once) ONCE=1; shift ;;
    --print-only) PRINT_ONLY=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$EXE" || -z "$CONFIG" ]]; then
  echo "usage: $0 --exe PATH --config PATH [--name NAME] [--restart MIN] [--once] [--print-only]" >&2
  exit 2
fi
if [[ ! -x "$EXE" ]]; then
  echo "sdlc binary not found or not executable at $EXE" >&2
  exit 1
fi
if [[ ! -f "$CONFIG" ]]; then
  echo "sdlc.yaml not found at $CONFIG" >&2
  exit 1
fi

EXE_ABS="$(cd "$(dirname "$EXE")" && pwd)/$(basename "$EXE")"
CONFIG_ABS="$(cd "$(dirname "$CONFIG")" && pwd)/$(basename "$CONFIG")"
WORKDIR="$(dirname "$CONFIG_ABS")"
LABEL="dev.sdlc.${NAME}"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST_PATH="${PLIST_DIR}/${LABEL}.plist"
LOG_DIR="$HOME/Library/Logs/sdlc"
THROTTLE=$(( RESTART * 60 ))

# --config is a global flag on sdlc: it precedes the subcommand.
ONCE_ARG=""
if [[ $ONCE -eq 1 ]]; then
  ONCE_ARG="
        <string>--once</string>"
  KEEPALIVE="<false/>"
  SHAPE="drain once and exit"
else
  KEEPALIVE="<true/>"
  SHAPE="persistent engine"
fi

PLIST_CONTENT=$(cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${EXE_ABS}</string>
        <string>--config</string>
        <string>${CONFIG_ABS}</string>
        <string>run</string>${ONCE_ARG}
    </array>
    <key>WorkingDirectory</key>
    <string>${WORKDIR}</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    ${KEEPALIVE}
    <key>ThrottleInterval</key>
    <integer>${THROTTLE}</integer>
    <key>StandardOutPath</key>
    <string>${LOG_DIR}/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>${LOG_DIR}/stderr.log</string>
</dict>
</plist>
PLIST
)

ARGS_ECHO="--config \"$CONFIG_ABS\" run"
if [[ $ONCE -eq 1 ]]; then ARGS_ECHO="$ARGS_ECHO --once"; fi

echo "Label:            $LABEL"
echo "Command:          $EXE_ABS $ARGS_ECHO"
echo "Working dir:      $WORKDIR"
echo "Shape:            $SHAPE"
if [[ $ONCE -eq 0 ]]; then
  echo "Restart policy:   KeepAlive, no sooner than every ${RESTART} min"
fi
echo "Runs as:          the logged-in user (LaunchAgent, not LaunchDaemon)"
echo "launchd logs:     ${LOG_DIR}/{stdout,stderr}.log"

if [[ $PRINT_ONLY -eq 1 ]]; then
  echo ""
  echo "--print-only: nothing was installed. Plist would be:"
  echo "$PLIST_CONTENT"
  exit 0
fi

mkdir -p "$PLIST_DIR" "$LOG_DIR"
printf '%s\n' "$PLIST_CONTENT" > "$PLIST_PATH"

launchctl unload "$PLIST_PATH" >/dev/null 2>&1 || true
launchctl load -w "$PLIST_PATH"

echo ""
echo "Installed and loaded: $PLIST_PATH"
echo "Inspect it with:      launchctl list | grep $LABEL"
echo "Watch it with:        $EXE_ABS --config \"$CONFIG_ABS\" status"
echo "Remove it with:       launchctl unload \"$PLIST_PATH\" && rm \"$PLIST_PATH\""
