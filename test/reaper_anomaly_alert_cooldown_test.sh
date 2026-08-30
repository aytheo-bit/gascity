#!/usr/bin/env bash
# Test: repeated fail-closed backup-guard anomalies do not flood operator mail.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAPER="$SCRIPT_DIR/../internal/bootstrap/packs/core/assets/scripts/reaper.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

FUNCTIONS=$(awk '
  /^should_emit_anomaly_alert\(\)/{copy=1}
  copy{print}
  copy && /^}/{
    if (++seen == 2) exit
  }
' "$REAPER")

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
CITY_ABS="$tmpdir/city"
mkdir -p "$CITY_ABS"
BACKUP_GUARD_ANOMALY_RECORDED=1
GC_REAPER_ANOMALY_ALERT_COOLDOWN_SECONDS=86400

eval "$FUNCTIONS"

ANOMALIES="gm: bulk prune skipped: backup stale or absent (source=$CITY_ABS/.beads/backup/backup_state.json age=90000s threshold=86400s)
"
if should_emit_anomaly_alert; then
    record_anomaly_alert_receipt
    pass "first backup-guard anomaly is emitted and receipted"
else
    fail "first backup-guard anomaly was unexpectedly suppressed"
fi

if should_emit_anomaly_alert; then
    fail "identical backup-guard anomaly was not suppressed"
else
    pass "identical backup-guard anomaly is suppressed during cooldown"
fi

ANOMALIES="gm: bulk prune skipped: backup stale or absent (source=$CITY_ABS/.beads/backup/backup_state.json age=90001s threshold=86400s)
"
if should_emit_anomaly_alert; then
    fail "advancing stale age bypassed the backup-guard cooldown"
else
    pass "advancing stale age is normalized for cooldown comparison"
fi

ANOMALIES="gm: bulk prune skipped: backup stale or absent (source=$CITY_ABS/.beads/backup/backup_state.json age=absent threshold=86400s)
"
if should_emit_anomaly_alert; then
    pass "a transition from stale-present to absent is emitted"
else
    fail "a transition from stale-present to absent was suppressed"
fi

# Keep the original stale receipt active for the unrelated-anomaly assertion.
ANOMALIES="gm: bulk prune skipped: backup stale or absent (source=$CITY_ABS/.beads/backup/backup_state.json age=90002s threshold=86400s)
"

ANOMALIES="${ANOMALIES}city: unrelated database failure
"
if should_emit_anomaly_alert; then
    pass "an unrelated concurrent anomaly bypasses the cooldown"
else
    fail "an unrelated concurrent anomaly was suppressed"
fi

ANOMALIES="gm: bulk prune skipped: backup stale or absent (source=$CITY_ABS/.beads/dolt-backup-state.json age=absent threshold=86400s)
"
if should_emit_anomaly_alert; then
    pass "a materially different backup source is emitted"
else
    fail "a materially different backup source was suppressed"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
