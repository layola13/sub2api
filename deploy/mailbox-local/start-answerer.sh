#!/bin/bash
set -a
DIR="$(cd "$(dirname "$0")" && pwd)"
export OPENCODE_MODEL="${OPENCODE_MODEL:-hy4-flash-free}"
export OPENCODE_PROVIDER="${OPENCODE_PROVIDER:-opencode}"
export MAILBOX_ANSWER_MODE=cli
export OPENCODE_CONFIG="${OPENCODE_CONFIG:-$DIR/opencode.json}"
export MAILBOX_OPENCODE_AGENT="${MAILBOX_OPENCODE_AGENT:-mailbox-relay}"
export MAILBOX_ANSWER_TIMEOUT_MS="${MAILBOX_ANSWER_TIMEOUT_MS:-110000}"
export OPENCODE_BIN="${OPENCODE_BIN:-opencode}"
export MAILBOX_SPOOL_DIR="${MAILBOX_SPOOL_DIR:-/tmp/mailbox-spool}"
set +a
cd "$DIR"
exec bun adapter.js "$MAILBOX_SPOOL_DIR" 500
