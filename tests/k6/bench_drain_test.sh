#!/usr/bin/env bash
# bench_drain.sh against a fake psql (no Postgres): the reviewer's case — the snapshot statement takes
# 1s to answer, the worker finishes everything in the first 0.2s. The clock must have started before
# the snapshot, so the window covers the 1s and does not claim a rate from a window that began after
# the work was done. `task k6-bench-test`; not part of CI.
#   BENCH_DRAIN=<script>   test another copy (to see the check go red on the old order)
set -euo pipefail
script=${BENCH_DRAIN:-tests/k6/bench_drain.sh}
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat > "$work/bin/psql" <<'EOS'
#!/usr/bin/env bash
sql=${*: -1}
case $sql in
  *"filter"*) sleep 1; echo "10|10" ;;                       # snapshot: 10 PENDING of 10, slow to answer
  *"status = 'PENDING'"*) echo 0 ;;                            # the poll: the worker is long done
  *"<> 'CONFIRMED'"*) echo 0 ;;
  *"inventory.reservations"*) echo 10 ;;
  *) echo "fake psql: unexpected sql: $sql" >&2; exit 1 ;;
esac
EOS
chmod +x "$work/bin/psql"
PATH="$work/bin:$PATH" "$script" "postgres://fake/db" "$work/drain.json" 30
python3 -I - "$work/drain.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d["pending_at_end"] == 10 and d["total_orders"] == 10, d
assert d["seconds"] >= 0.95, f"window {d['seconds']}s: the clock started after the 1s snapshot, so 10 orders in {d['seconds']}s is a rate nobody measured"
print("--- PASS: drain window starts before the snapshot (%.2fs)" % d["seconds"])
PY
echo "ok  bench drain"
