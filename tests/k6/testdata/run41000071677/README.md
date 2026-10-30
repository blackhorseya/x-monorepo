run 41000071677 (k6-bench workflow, 4-core ubuntu runner, commit 5fd7a7c1, #288): the `run<n>-load.csv` files and `run6-create.json` (k6 summary of the overloaded attempt: rpc_errors crossed, 20 x request.timeout) are the real
bench-time samples of attempts 1, 2 and 6. `baseline-at-start.csv` is the runner's load before attempt 1 ("load at start" in
that run's runner.txt: 1.70). Used by tests/k6/bench_report_test.py.
