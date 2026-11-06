#!/usr/bin/env bash
# The backlog-drain measurement of bench.sh, on its own so that tests/k6/bench_drain_test.sh can run
# it against a fake psql. Called when the create phase has just ended:
#   bench_drain.sh <database url> <out json> [timeout seconds]
# Writes {"total_orders", "pending_at_end", "seconds"}. pending_at_end and total_orders are ONE
# snapshot (one statement); the clock starts BEFORE that statement, so what the worker finishes while
# the snapshot is being taken is inside the window. (Started after, it would be in the numerator but
# not in the denominator.) The bias left is the other way and bounded by one psql round trip: the
# window is a little long, the rate a little low. The end is the first poll that sees no PENDING, so
# the window is also up to one poll interval (0.2s) long.
# Then every order must be CONFIRMED with one RESERVED reservation each: an empty PENDING is not success.
set -euo pipefail
url=$1 out=$2 timeout=${3:-300}
now() { python3 -I -c 'import time; print(time.time())'; }
q() { psql "$url" -qAt -F'|' -c "$1"; }

t0=$(now) || exit $?
snap=$(q "select count(*) filter (where status = 'PENDING'), count(*) from ordering.orders") || exit $?
pending=${snap%%|*} total=${snap##*|}
# a read that fails is "not drained yet" (the loop retries, and running out of time fails below); the value tested is always the one a successful read returned
until left=$(q "select count(*) from ordering.orders where status = 'PENDING'") && [ "$left" = 0 ]; do
  tnow=$(now) || exit 1
  python3 -I -c "import sys; sys.exit(0 if $tnow - $t0 < $timeout else 1)" \
    || { echo "backlog not drained within ${timeout}s" >&2; exit 1; }
  sleep 0.2
done
t1=$(now) || exit 1
secs=$(python3 -I -c "print(round($t1 - $t0, 2))") || exit $?
notok=$(q "select count(*) from ordering.orders where status <> 'CONFIRMED'") || exit $?
reserved=$(q "select count(*) from inventory.reservations where status = 'RESERVED'") || exit $?
if [ "$notok" != 0 ] || [ "$reserved" != "$total" ]; then
  echo "drain ended with $notok order(s) not CONFIRMED and $reserved RESERVED reservation(s) for $total orders" >&2
  exit 1
fi
echo "{\"total_orders\": $total, \"pending_at_end\": $pending, \"seconds\": $secs}" > "$out"
