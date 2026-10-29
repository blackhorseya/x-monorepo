# 效能 baseline（k6）

<sub>← [README](../README.md)</sub>

之後優化 CreateOrder 或 reservation batching 時的對照組（#263）。`tests/k6/orders.js` / `reservation.js` 是功能與閾值的 smoke，沒有可比較的數字；這裡是固定參數、跑 N 次、有離散程度的量測。**不進 PR 的 CI gate**（shared runner 的數字是雜訊）。

## 重跑

```bash
# 前置：本機 Postgres 在跑（task compose-up）、k6 / psql / python3 在 PATH 上、role 能 CREATE DATABASE
task k6-bench                      # 預設 DATABASE_URL=postgres://app:app@127.0.0.1:5432/app，輸出 bin/k6-bench/<stamp>/
task k6-bench -- /tmp/my-out       # 指定輸出目錄
RUNS=5 task k6-bench               # 次數（≥ 3）；參數都是環境變數，見 tests/k6/bench.sh 檔頭
```

- 每個 run 在 Postgres 建一個專屬資料庫（`k6_bench_<stamp>_<n>`）、用 `./bin/migrate up` 建 schema、起 `./bin/rpc` + `./bin/worker`（auth off、telemetry off、log warn），跑完 drop（失敗或 ^C 也會）。所以每個 run 從空的 outbox 起跳，寫入量由參數決定（create：約 28k 張單；reserve：100 × 30 = 3000 張），也不會留東西在你的資料庫裡（#238）。每個 run 另有獨立的 `RUN_ID`。
- 兩個階段（`tests/k6/bench.js`，`PHASE=create|reserve`）：
  - **create**：20 VU 不停頓連打 CreateOrder 10 秒（closed model，飽和狀態的吞吐與延遲）。
  - **backlog drain**：create 之後 worker 還欠一大堆 reservation；計時到沒有 PENDING 的訂單為止。這是 worker 在持續有積壓時（會一次撈多筆的情況）的 reservation 吞吐。
  - **reserve**：每秒 100 張單、30 秒（open model），每張 CreateOrder 後輪詢 GetReservation（50ms）到 RESERVED；`order → RESERVED` 是 outbox 等待 + worker + handler 的端到端延遲，此時 worker 在閒置邊緣。
- 輸出：`result.json`（機器可讀：每個指標的 median / min / max / CV / 各 run 數值，加上 commit、Go / Postgres / k6 版本、CPU、OS、參數）、`summary.md`、以及每個 run 的 k6 `--summary-export` 與 log。
- API key：腳本送一把假 key（auth off 會忽略），`bench.sh` 結束時 grep 輸出目錄所有檔案，找到就整個 bench 失敗；k6 腳本沿用 `lib.js` 的規則（不印 body / URL，`systemTags` 不含 `url`、`error`）。

## Baseline

量測條件：commit `2cbf7ce`（= main `e47307b` 加上本 PR 的 `tests/k6`、`Taskfile.yml`，沒有 runtime 程式碼差異）、go version go1.27.1 darwin/arm64; Postgres 17.9; k6 v2.2.0 (commit/devel, go1.26.5, darwin/arm64)；Apple M1 Max (10 cores), Darwin 25.6.0 arm64。筆電，同時有其他程式在跑，絕對值只能在同一台機器上比。3 次 run，下表是中位數、min–max 與變異係數（stdev/mean）。

| metric | median | min–max | CV |
|---|---|---|---|
| CreateOrder throughput (req/s) | 2796 | 2558–2806 | 4.2% |
| CreateOrder success | 100.00% | 100.00–100.00% | 0.0% |
| CreateOrder p50 (ms) | 6.55 | 6.52–6.92 | 2.7% |
| CreateOrder p95 (ms) | 10.18 | 9.86–11.90 | 8.4% |
| CreateOrder p99 (ms) | 17.93 | 17.87–20.92 | 7.5% |
| backlog drain (orders/s) | 369 | 336–415 | 8.6% |
| backlog drain (s) | 70.0 | 68.0–84.0 | 9.6% |
| reserve: orders created (req/s) | 99.1 | 98.8–99.1 | 0.1% |
| reserve: success | 100.00% | 100.00–100.00% | 0.0% |
| reserve: dropped iterations | 0 | 0–8 | 141.4% |
| order → RESERVED p50 (ms) | 153 | 153–153 | 0.0% |
| order → RESERVED p95 (ms) | 260 | 255–305 | 8.2% |
| order → RESERVED p99 (ms) | 307 | 306–475 | 22.0% |

解讀：

- CreateOrder 的 p50 / p95 / p99 約 6.6 / 10 / 18 ms，吞吐 CV 約 4%；比較優化時，低於這個離散程度的差異不要當結論，必要時加大 `RUNS`。
- backlog drain 約 370 orders/s（CV 約 9%），遠低於 CreateOrder 的飽和吞吐（約 2800/s）：持續高負載下 outbox 積壓會線性成長，這是 reservation 路徑目前的瓶頸，也是 batching 優化的對照指標。
- order → RESERVED 在 100 orders/s 下 p50 約 153ms（`worker.poll_interval` 預設 200ms 的輪詢等待為主）、p95 約 260ms；p99 的離散較大（CV 22%）。
- reserve 的 `dropped iterations` 有一次 run 是 8（其餘 0）：到達率打滿預配置 VU 時的瞬間丟棄，不影響成功率（三次皆 100%）。

## 比較

優化前後用同一台機器、同一組參數各跑一次 `task k6-bench`，比 `result.json` 的 `median` 與 `cv_pct`。參數不同的結果不能比；`result.json` 的 `parameters` / `environment` 就是用來核對這件事。
