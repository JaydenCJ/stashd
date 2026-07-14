#!/usr/bin/env bash
# End-to-end smoke test for stashd: builds the binary, drives a full
# artifact lifecycle (put, dedup, tag, pin, gc, verify) against a temp
# store, and asserts on real CLI output. No network, idempotent, seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

# expect <text> <label>: assert the last captured $OUT contains <text>.
# Output is always captured into a variable first — piping the binary
# straight into `grep -q` would race pipefail against SIGPIPE.
expect() {
  echo "$OUT" | grep -q "$1" || fail "$2"
}

BIN="$WORKDIR/stashd"
export STASHD_DIR="$WORKDIR/store"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/stashd) || fail "go build failed"

echo "2. version matches manifest"
OUT="$("$BIN" version)"
[ "$OUT" = "stashd 0.1.0" ] || fail "version mismatch: $OUT"

echo "3. put stores, dedups, and prints ids"
printf 'agent run report\n' > "$WORKDIR/report.md"
printf 'fake png bytes'     > "$WORKDIR/login.png"
ID1="$("$BIN" put -q --run run-001 --tag kind=report "$WORKDIR/report.md")"
[ "${#ID1}" -eq 12 ] || fail "put -q should print a 12-char id"
OUT="$("$BIN" put --run run-002 "$WORKDIR/report.md")"
expect "dedup: blob already stored" "second put of identical content should dedup"
ID3="$("$BIN" put -q --run run-002 --tag kind=screenshot "$WORKDIR/login.png")"

echo "4. get round-trips content byte-for-byte"
"$BIN" get "$ID1" | diff - "$WORKDIR/report.md" || fail "get content mismatch"
"$BIN" get -o "$WORKDIR/out.png" "$ID3" 2>/dev/null
cmp -s "$WORKDIR/out.png" "$WORKDIR/login.png" || fail "get -o content mismatch"

echo "5. ls filters by tag and run"
OUT="$("$BIN" ls --tag kind=report)"
expect "report.md" "tag filter missed report"
OUT="$("$BIN" ls --run run-002)"
expect "2 artifacts" "run filter should find 2"

echo "6. stats shows the dedup win"
OUT="$("$BIN" stats)"
expect "artifacts  3" "stats artifact count wrong"
expect "blobs      2" "stats blob count wrong (dedup broken)"

echo "7. pin protects, rm respects references"
OUT="$("$BIN" pin "$ID3")"
expect "pinned" "pin failed"
if "$BIN" rm "$ID3" >/dev/null 2>&1; then
  fail "rm of a pinned artifact must refuse"
fi
OUT="$("$BIN" rm "$ID1")"
expect "blob kept, 1 other reference" "rm should keep the still-referenced blob"

echo "8. policy install + gc enforce retention"
cat > "$WORKDIR/policy.json" <<'EOF'
{
  "rules": [
    { "name": "everything", "max_age": "0s" }
  ]
}
EOF
OUT="$("$BIN" policy set "$WORKDIR/policy.json")"
expect "policy installed: 1 rule" "policy set failed"
OUT="$("$BIN" gc --dry-run)"
expect "gc (dry-run):" "dry-run summary missing"
OUT="$("$BIN" ls)"
expect "report.md" "dry-run must not delete anything"
OUT="$("$BIN" gc)"
expect 'rule "everything"' "gc should quote the rule"
expect "1 artifact expired" "gc should expire the unpinned artifact"
OUT="$("$BIN" ls)"
expect "login.png" "pinned artifact must survive gc"

echo "9. verify passes on a healthy store"
OUT="$("$BIN" verify)"
expect "0 corrupt, 0 missing" "verify reported problems"

echo "10. usage errors exit 2"
set +e
"$BIN" put >/dev/null 2>&1
[ $? -eq 2 ] || fail "put with no file should exit 2"
"$BIN" gc --max-age banana >/dev/null 2>&1
[ $? -eq 2 ] || fail "bad --max-age should exit 2"
set -e

echo "SMOKE OK"
