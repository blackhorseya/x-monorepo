"""One 1m load average sample as "<epoch>,<load>"; with an interval (seconds) as argument, one every interval until killed.

    python3 -I tests/k6/loadavg.py [interval]

BENCH_LOADAVG_FILE is the test seam: when set, the load is the number in that file instead of the
machine's, so tests/k6/bench_overload_test.sh can make "the machine" busy or quiet at will.
"""
import os
import sys
import time


def load1():
    path = os.environ.get("BENCH_LOADAVG_FILE")
    if path:
        with open(path) as f:
            return float(f.read().strip())
    return round(os.getloadavg()[0], 2)


if __name__ == "__main__":
    interval = float(sys.argv[1]) if len(sys.argv) > 1 else 0
    while True:
        print(f"{int(time.time())},{load1()}", flush=True)
        if not interval:
            break
        time.sleep(interval)
