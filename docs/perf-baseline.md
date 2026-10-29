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

- 每個 run 在 Postgres 建一個專屬資料庫（`k6_bench_<stamp>_<pid+random>_<n>`；只 drop 自己 CREATE 成功的，名稱已存在就失敗退出、不碰它，`task k6-bench-test` 用 sentinel 資料庫驗證）、用 `./bin/migrate up` 建 schema、起 `./bin/rpc` + `./bin/worker`（auth off、telemetry off、log warn），跑完 drop（失敗或 ^C 也會）。所以每個 run 從空的 outbox 起跳，寫入量由參數決定（create：約 2.5–3 萬張單；reserve：100 × 30 = 3000 張），也不會留東西在你的資料庫裡（#238）。每個 run 另有獨立的 `RUN_ID`。
- 階段（`tests/k6/bench.js`，`PHASE=create|reserve`；setup、負載、排空各自獨立計時）：
  - **create**：20 VU 不停頓連打 CreateOrder 10 秒（closed model，飽和狀態的吞吐與延遲）。
  - **backlog drain**：create 結束的瞬間，worker 還欠一堆 reservation。`PENDING when create ended` = 那一刻仍是 PENDING 的訂單數；`backlog drain (orders/s)` = 這個數字 ÷ 到 PENDING 歸零的秒數（分子分母同一個時間窗；create 期間 worker 已處理的訂單兩邊都不算）。歸零後還要驗證每張訂單都是 CONFIRMED、RESERVED 的 reservation 數等於訂單總數，否則整個 bench 失敗（PENDING 消失不代表成功）。create 結束時沒有積壓則記為 0 / n/a，不會算出不存在的吞吐。
  - **reserve**：每秒 100 張單、30 秒（open model），每張 CreateOrder 後輪詢 GetReservation（`POLL`=50ms）到 RESERVED。兩個延遲，名稱即定義：`order → RESERVED` 從**送出 CreateOrder 之前**算到看到 RESERVED（端到端）；`CreateOrder returned → RESERVED` 從 CreateOrder 回傳後算起（worker 負責的那段）。輪詢間隔使每筆觀測值最多晚 50ms（平均約 25ms）看到 RESERVED，是量測誤差而非延遲的一部分；不要把兩個分布的 percentile 相加或相減當修正值。
- 吞吐的分母：第一個 iteration 開始到最後一次 CreateOrder 回傳（k6 腳本自己記的 `bench_iter_start_ms` / `bench_create_end_ms` 的 min / max），不用 k6 Counter 的 rate（它的分母是整個 test，包含 setup() 的 SetStock）。
- 可重跑的輸入：SKU 由 `SEED`（預設 1，記在 `result.json` 的 `parameters.seed`）和 iteration 序號決定的 PRNG（mulberry32）選，不用 `Math.random`；同參數、單 VU 時序列逐筆相同，多 VU 時交錯順序由排程決定。
- **哪些 attempt 算數，由工具依事前固定的規則判定**，不是看過數字的人：量測期間每 5 秒取樣一次 1 分鐘 load average，任何一個樣本超過 `MAX_LOAD_PER_CORE`（預設 1.0）× 核心數，該 attempt 就是 invalid。attempt 一直跑到有 `RUNS`（3）個 valid 或達 `MAX_ATTEMPTS`（預設 6）；**所有 attempt 的檔案都留在輸出目錄**，統計只用 valid 的，invalid 的列在 `summary.md` / `result.json` 的 attempts 表；湊不滿 3 個 valid 就在 summary 最上面印「NOT A VALID BASELINE」並以 exit 3 結束。門檻 1.0 的意思：load average 含 bench 自己的 rpc / worker / k6 / Postgres，超過核心數代表可執行的工作比核心還多；這個數字是預設值，不是由實測結果推導的，要換就在跑之前用環境變數指定（會記進 `result.json`）。
- **對帳**：reserve 階段每個 attempt 必須「CreateOrder 成功數 = 看到 RESERVED 的數量」（`reserved_ok`），`gracefulStop` 涵蓋 CreateOrder 逾時加等待的上限；k6 印出 `No script iterations fully finished` 也算失敗。已建立但沒完成的單不會被算成成功或延遲 0ms；reservation 的成功率 = 看到 RESERVED 的訂單 ÷ 建立成功的訂單（不是 CreateOrder 的成功率）。
- 輸出：`result.json`（機器可讀：每個指標的 median / min / max / CV / 各 run 數值，加上 commit、Go / Postgres / k6 版本、CPU、OS、每個 attempt 的 load 取樣統計與是否 valid、參數）、`summary.md`、以及每個 attempt 的 k6 `--summary-export`、load 取樣 csv 與 log。
- API key：腳本送一把假 key（auth off 會忽略），`bench.sh` 結束時 grep 輸出目錄所有檔案，找到就整個 bench 失敗；k6 腳本沿用 `lib.js` 的規則（不印 body / URL，`systemTags` 不含 `url`、`error`）。
- 量測工具本身的測試：`task k6-bench-test`（報告定義與有效性/對帳的單元測試、drain 起算時間（fake psql）、對 stub server 的 fixture：seed 可重現、端到端延遲含 CreateOrder、吞吐不被 setup 稀釋、被中斷的 reservation 不會悄悄通過、sentinel 資料庫不會被 drop）；不進 CI。

## Baseline

**目前沒有符合規則的 valid baseline。** 下面的表是探索性結果：在共用的開發機上、條件受限、而且挑選過程有事後成分（見 attempt 清單），**不是乾淨的 baseline**，只能當量級參考，不要拿來判定優化有沒有效。要得到 valid 的數字，在機器安靜時重跑 `task k6-bench`；工具會自己判定。

### 所有 attempt（都保留，副本在 `/Users/sean/.agend/workspace/x-monorepo-worker-45/`）

| 目錄 | 時間（UTC） | 程式 | load（1m） | 狀態 |
|---|---|---|---|---|
| `perf-baseline-20261029T213621Z` | 21:36–21:4x | PR head a4d6720 | 未取樣 | 舊定義（drain、延遲、吞吐的算法後來被 review 判定有誤），只供溯源，數字已作廢 |
| `perf-baseline-20261029T220610Z-DISCARDED` | 22:06–22:12 | 1675b0d | 當時未取樣；結束後 `uptime` 約 14 | **事後棄用**：先看到 reserve p95 約 7 秒、丟 446 個 iteration，才去查 load，理由是看過結果後才決定的 |
| `perf-baseline-20261029T221617Z` | 22:16–22:22 | 1675b0d | 只有起訖兩點：6.11 → 8.54 | 下表來源。跑之前我寫了「等 load < 6」的等待迴圈，**這個門檻是看過上一組之後才訂的**，而且實際開始時是 6.11；中間沒有取樣，無法證明整段符合任何規則 |
| `perf-baseline-r3-attempts/out`（attempt 1–6） | 22:48–22:59 | e0219ec（規則出現後的第一次） | 每個 attempt 的最大值 23.08 / 19.19 / 15.84 / 15.28 / 18.32 / 25.71（上限 10.0） | **6 組全部 invalid**，工具印「NOT A VALID BASELINE」 |

最後一列是第一次在規則之下跑：這台機器當時的 load 15–26，其他程式同時在跑，一組都沒達標。它的統計表（不是 baseline）顯示 load 高時尾端會差多遠：reserve 端到端 p95 的中位數 4360ms（min–max 277–14365ms），而下表（load 較低的一組）是 308ms。

### 探索性結果（`perf-baseline-20261029T221617Z`，3 次 run）

程式 commit `1675b0d`（= main `e47307b` 加上本 PR 的 `tests/k6`、`Taskfile.yml`，沒有 runtime 程式碼差異；之後的修改只動工具）、Go 1.27.1、Postgres 17.9、k6 v2.2.0、Apple M1 Max（10 核）、Darwin 25.6.0。當時 1 分鐘 load average 起訖 6.11、8.54（10 核）。3 次 run，中位數、min–max 與變異係數（stdev/mean）。這一組的 drain 起算時間還在快照之後（後來修正為在快照之前，只會讓窗口略長、速率略低），且當時沒有對帳 `reserved_ok`，所以 drain 與 reserve 的數字重跑後可能略有不同。

| metric | median | min–max | CV |
|---|---|---|---|
| CreateOrder throughput (req/s) | 2668 | 2289–2832 | 8.8% |
| CreateOrder success | 100.00% | 100.00–100.00% | 0.0% |
| CreateOrder p50 (ms) | 6.83 | 6.55–7.58 | 6.2% |
| CreateOrder p95 (ms) | 10.91 | 9.72–14.23 | 16.4% |
| CreateOrder p99 (ms) | 19.32 | 17.74–26.53 | 18.1% |
| PENDING when create ended (orders) | 25890 | 22168–27417 | 8.8% |
| backlog drain (orders/s) | 372 | 342–380 | 4.5% |
| backlog drain (s) | 69.6 | 64.8–72.1 | 4.4% |
| reserve: orders created (req/s) | 100.0 | 100.0–100.0 | 0.0% |
| reserve: success（舊定義：CreateOrder 成功率，非對帳） | 100.00% | 100.00–100.00% | 0.0% |
| reserve: dropped iterations | 0 | 0–1 | 141.4% |
| order → RESERVED p50 (ms, from before CreateOrder) | 155 | 155–156 | 0.3% |
| order → RESERVED p95 (ms, from before CreateOrder) | 308 | 308–309 | 0.2% |
| order → RESERVED p99 (ms, from before CreateOrder) | 365 | 358–365 | 0.9% |
| CreateOrder returned → RESERVED p50 (ms) | 153 | 153–153 | 0.0% |
| CreateOrder returned → RESERVED p95 (ms) | 305 | 305–306 | 0.2% |
| CreateOrder returned → RESERVED p99 (ms) | 359 | 356–361 | 0.6% |

解讀（僅限這一組探索性結果）：

- CreateOrder 的 p50 / p95 / p99 約 6.8 / 11 / 19 ms，吞吐約 2700 req/s、CV 約 9%。
- backlog drain 約 370 orders/s，遠低於 CreateOrder 的飽和吞吐：持續高負載下 outbox 積壓會線性成長，這是 reservation 路徑目前的瓶頸，也是 batching 優化的對照指標。
- 100 orders/s 下，端到端 order → RESERVED p50 / p95 / p99 約 155 / 308 / 365 ms，其中 CreateOrder 回傳後的部分約 153 / 305 / 359 ms。
- reviewer 在另一台 Postgres 17.11、load 11→15 的環境重跑，CreateOrder 約 1950–4462 req/s、端到端 p95 約 253–264 ms：同一量級，但絕對值隨環境與 load 變動很大，這也是為什麼需要工具判定的 valid 條件。

## 比較

優化前後用同一台機器、同一組參數（含 `SEED`），各取得一份**工具判定為 valid** 的 `task k6-bench` 結果（不是 NOT A VALID BASELINE 的），比 `result.json` 的 `median` 與 `cv_pct`。參數、seed 或 load 差很多的結果不能比；`result.json` 的 `parameters` / `environment` 就是用來核對這件事。
