#!/usr/bin/env bash
# Start/stop voice-io without implicitly downloading model weights.
# `scripts/install` installs this executable in ~/.local/bin.
set -euo pipefail
umask 077  # dictation logs and child-created transcript files stay private

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$SCRIPT_DIR" == "${HOME}/.local/bin" ]]; then
    # The installed launcher must use its own installed package, not a
    # coincidental ~/.local/bin/.venv or a checkout in the current directory.
    PYTHON="${HOME}/.local/share/voice-io/venv/bin/python"
elif [[ -x "$SCRIPT_DIR/.venv/bin/python" ]]; then
    PYTHON="$SCRIPT_DIR/.venv/bin/python"
elif [[ -x "${HOME}/.local/share/voice-io/venv/bin/python" ]]; then
    PYTHON="${HOME}/.local/share/voice-io/venv/bin/python"
else
    PYTHON="${PYTHON:-python3}"
fi

if [[ "$(uname -s)" == Darwin ]]; then
    LOG_DIR="${HOME}/Library/Logs/voice-io"
else
    LOG_DIR="${XDG_STATE_HOME:-${HOME}/.local/state}/voice-io"
fi
LOG="$LOG_DIR/voice-io.log"

running() { pgrep -f '[p]ython.*-m voice_io live' >/dev/null 2>&1; }
control() {
    "$PYTHON" -c 'from voice_io.control import write; import sys; sys.exit(0 if write(sys.argv[1]) else 1)' "$1"
}
prepare_log() {
    mkdir -p "$LOG_DIR"
    chmod 700 "$LOG_DIR"
    if [[ -L "$LOG" ]]; then
        printf 'Refusing symlinked voice-io log: %s\n' "$LOG" >&2
        return 1
    fi
    if [[ -f "$LOG" ]]; then chmod 600 "$LOG"; fi
}
wait_gone() {
    for _ in {1..20}; do
        running || return 0
        sleep .25
    done
    return 1
}
stop() {
    if ! running; then return 0; fi
    if [[ "$(uname -s)" == Linux ]] && command -v systemctl >/dev/null 2>&1 \
            && systemctl --user is-active --quiet voice-io.service; then
        systemctl --user stop voice-io.service
    else
        control quit || true
    fi
    if ! wait_gone; then
        printf 'voice-io did not stop; check %s\n' "$LOG" >&2
        return 1
    fi
}
start() {
    prepare_log
    if running; then
        if control identify; then return 0; fi
        printf 'voice-io process exists but control FIFO is unavailable; check %s\n' "$LOG" >&2
        return 1
    fi
    if [[ "$(uname -s)" == Linux ]] && command -v systemctl >/dev/null 2>&1 \
            && systemctl --user cat voice-io.service >/dev/null 2>&1; then
        systemctl --user start voice-io.service
        for _ in {1..120}; do
            if running && control identify; then return 0; fi
            sleep .25
        done
        printf 'voice-io service did not become ready; check systemctl --user status voice-io.service\n' >&2
        return 1
    fi
    local log_offset=0
    if [[ -f "$LOG" ]]; then
        log_offset=$(wc -c < "$LOG")
    fi
    nohup "$PYTHON" -m voice_io live >> "$LOG" 2>&1 < /dev/null &
    local pid=$!
    for _ in {1..120}; do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" || true
            printf 'voice-io failed to start; see %s\n' "$LOG" >&2
            return 1
        fi
        if tail -c "+$((log_offset + 1))" "$LOG" | grep -qx 'voice-io daemon ready' \
                && control identify; then return 0; fi
        sleep .25
    done
    kill "$pid" 2>/dev/null || true
    printf 'voice-io startup timed out; see %s\n' "$LOG" >&2
    return 1
}

case "${1:-start}" in
    start) start ;;
    run) prepare_log; exec "$PYTHON" -m voice_io live ;;
    stop) stop ;;
    settings) exec "$PYTHON" -m voice_io settings ;;
    recover) exec "$PYTHON" -m voice_io recover ;;
    models) shift; exec "$PYTHON" -m voice_io models "$@" ;;
    *) printf 'usage: voice-io.sh [start|run|stop|settings|recover|models install|models verify]\n' >&2; exit 2 ;;
esac
