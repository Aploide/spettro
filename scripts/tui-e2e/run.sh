#!/bin/sh
# End-to-end rendering check of the TUI: builds spettro, then drives it in a
# pseudo-terminal at several sizes against a fake OpenAI-compatible server
# (no API key, no network). See README.md in this directory.
#
# Usage: scripts/tui-e2e/run.sh [OUTDIR]
#   OUTDIR defaults to a new temporary directory; the snapshots and a
#   report.json per size are written there.
# Needs python3 with the pyte package (pip install pyte); set E2E_PYTHON to
# use a specific interpreter, such as a virtualenv's.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=${1:-$(mktemp -d)}
python=${E2E_PYTHON:-python3}

if ! "$python" -c 'import pyte' 2>/dev/null; then
	echo "pyte is not installed for $python (pip install pyte, or set E2E_PYTHON)" >&2
	exit 2
fi

mkdir -p "$out"
(cd "$repo" && go build -o "$out/spettro" ./cmd/spettro)

status=0
for size in 40x15 80x24 120x40 200x50; do
	w=${size%x*}
	h=${size#*x}
	rm -rf "${out:?}/$size"
	"$python" "$here/drive.py" "$out/spettro" "$out/$size" "$w" "$h" || status=1
done
echo "snapshots: $out"
exit $status
