#!/usr/bin/env bash
# Simulates what an agent harness does with stashd: stamp a run ID, stash
# every output the run produces, then let retention do the cleanup.
#
# Usage: bash examples/agent-run.sh [store-dir]
# Builds stashd from the repo if it is not already on PATH. Everything
# happens inside a temp workdir plus the store dir you pass (or a temp one).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

if command -v stashd >/dev/null 2>&1; then
  BIN="stashd"
else
  BIN="$WORKDIR/stashd"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/stashd)
fi

export STASHD_DIR="${1:-$WORKDIR/store}"
export STASHD_RUN="run-$(date +%Y%m%d-%H%M%S)"
echo "# store: $STASHD_DIR   run: $STASHD_RUN"

# --- the "agent" produces outputs -------------------------------------
printf 'fake png for step 1' > "$WORKDIR/step1-login.png"
printf 'fake png for step 2' > "$WORKDIR/step2-form.png"
printf -- '--- a/app.go\n+++ b/app.go\n@@ +1 @@\n+// fix\n' > "$WORKDIR/fix.diff"
printf '# Run report\n\nAll checks passed.\n' > "$WORKDIR/report.md"

# --- stash them with lifecycle metadata --------------------------------
"$BIN" put --tag kind=screenshot --tag step=1 "$WORKDIR/step1-login.png"
"$BIN" put --tag kind=screenshot --tag step=2 "$WORKDIR/step2-form.png"
"$BIN" put --tag kind=diff "$WORKDIR/fix.diff"
REPORT_ID="$("$BIN" put -q --tag kind=report "$WORKDIR/report.md")"

# The final report is the artifact humans will want next month: pin it.
"$BIN" pin "$REPORT_ID"

echo
echo "# what this run stored:"
"$BIN" ls --run "$STASHD_RUN"

echo
echo "# install the sample policy and preview retention:"
"$BIN" policy set "$ROOT/examples/retention-policy.json"
"$BIN" gc --dry-run

echo
echo "# store totals:"
"$BIN" stats
