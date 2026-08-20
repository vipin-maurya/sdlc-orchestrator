#!/usr/bin/env bash
# Registers the sdlc engine as a systemd --user service on Linux.
#
# Unlike autoship — a scheduled one-shot, installed as a *timer* — `sdlc run`
# is a long-lived engine: it holds the SQLite database, the gradle slots and the
# device pool, and ticks itself every orchestrator.poll_interval. So this is a
# Type=simple service with Restart=always, not a timer: systemd's job is to
# start it and put it back if it dies, not to wake it up on a schedule.
#
# This installs a *user* unit, not a system one, deliberately: it only runs in a
# session for the invoking user, which is what the Windows S4U interactive-token
# principal and the macOS LaunchAgent buy on those platforms — the agent CLIs
# (claude, agy) read per-user credentials, and the builds need the user's Gradle
# and Android SDK caches. On a headless box, enable lingering so the unit runs
# without an active login (`loginctl enable-linger $USER`).
#
# Crash-safety is the engine's own (SPEC §7): every state entry records the
# worktree HEAD, and on restart the worktree is reconciled before the state
# re-runs. A Restart=always bounce is a normal event, not a recovery procedure.
#
# Usage:
#   scripts/install-systemd-service.sh --exe /path/to/sdlc --config /path/to/sdlc.yaml [options]
#
# Options:
#   --exe PATH             Path to the sdlc binary (required)
#   --config PATH          Path to sdlc.yaml (required)
#   --name NAME             Unit name. Default: sdlc
#   --restart MINUTES       Wait this long before restarting a crashed engine. Default: 5
#   --once                  Install a Type=oneshot unit running `sdlc run --once`
#                           (drain and exit) instead of the persistent engine.
#   --print-only            Print the unit file without installing it.

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
UNIT_DIR="$HOME/.config/systemd/user"
SERVICE_PATH="${UNIT_DIR}/${NAME}.service"
RESTART_SEC=$(( RESTART * 60 ))

# --config is a global flag on sdlc: it precedes the subcommand.
if [[ $ONCE -eq 1 ]]; then
  SHAPE="drain once and exit"
  EXEC="${EXE_ABS} --config ${CONFIG_ABS} run --once"
  SERVICE_BODY="Type=oneshot"
else
  SHAPE="persistent engine"
  EXEC="${EXE_ABS} --config ${CONFIG_ABS} run"
  # SIGTERM is what `sdlc run` already handles for a clean Ctrl-C, so the
  # default KillSignal is correct; give it room to finish the current tick.
  SERVICE_BODY="Type=simple
Restart=always
RestartSec=${RESTART_SEC}
TimeoutStopSec=120"
fi

SERVICE_CONTENT=$(cat <<UNIT
[Unit]
Description=sdlc: run the SDLC orchestrator engine for $(basename "$WORKDIR")
After=default.target

[Service]
${SERVICE_BODY}
WorkingDirectory=${WORKDIR}
ExecStart=${EXEC}

[Install]
WantedBy=default.target
UNIT
)

echo "Unit name:        ${NAME}.service"
echo "Command:          $EXEC"
echo "Working dir:      $WORKDIR"
echo "Shape:            $SHAPE"
if [[ $ONCE -eq 0 ]]; then
  echo "Restart policy:   always, ${RESTART} min apart"
fi
echo "Runs as:          your user session (systemd --user, not a system unit)"

if [[ $PRINT_ONLY -eq 1 ]]; then
  echo ""
  echo "--print-only: nothing was installed. Unit would be:"
  echo "--- ${NAME}.service ---"
  echo "$SERVICE_CONTENT"
  exit 0
fi

mkdir -p "$UNIT_DIR"
printf '%s\n' "$SERVICE_CONTENT" > "$SERVICE_PATH"

systemctl --user daemon-reload
systemctl --user enable --now "${NAME}.service"

echo ""
echo "Installed and started: $SERVICE_PATH"
echo "Inspect it with:       systemctl --user status ${NAME}.service"
echo "Follow its log with:   journalctl --user -u ${NAME}.service -f"
echo "Watch jobs with:       $EXE_ABS --config \"$CONFIG_ABS\" status"
echo "Remove it with:        systemctl --user disable --now ${NAME}.service && rm \"$SERVICE_PATH\""
echo ""
echo "On a headless server, the user session (and this unit) normally stops"
echo "when you log out. Keep it running unattended with:"
echo "  sudo loginctl enable-linger \$USER"
