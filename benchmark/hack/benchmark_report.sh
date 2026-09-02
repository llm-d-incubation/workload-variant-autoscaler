#!/usr/bin/env bash
# Wrapper that runs benchmark_report.py in its own venv, creating/updating it
# as needed -- avoids PEP 668 "externally managed environment" pip errors on
# systems (e.g. Homebrew Python) that block plain `pip install`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV_DIR="$SCRIPT_DIR/.venv"

if [ ! -x "$VENV_DIR/bin/python3" ]; then
	echo "Setting up venv for benchmark_report.py at $VENV_DIR..." >&2
	python3 -m venv "$VENV_DIR"
fi
"$VENV_DIR/bin/pip" install --quiet -r "$SCRIPT_DIR/requirements.txt"

exec "$VENV_DIR/bin/python3" "$SCRIPT_DIR/benchmark_report.py" "$@"
