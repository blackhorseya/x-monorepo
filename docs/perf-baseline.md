# 效能 baseline（k6）

<sub>← [README](../README.md)</sub>

之後優化 CreateOrder 或 reservation batching 時的對照組（#263）。`tests/k6/orders.js` / `reservation.js` 是功能與閾值的 smoke，沒有可比較的數字；這裡是固定參數、跑 N 次、有離散程度的量測。**不進 PR 的 CI gate**（shared runner 的數字是雜訊）；要在乾淨環境取得 valid 數字，手動派送 `k6-bench` workflow（見下方「在 GitHub runner 上跑」）。

> **下面的數字量測於 server 定價之前**（#267 以前，CreateOrder 的單價由呼叫端給）。之後 `CreateOrder` 每張單多一次 catalog 批次查價（#347 以前是 rpc 內的函式呼叫、同一個 process；#347 起是對獨立的 `catalog-rpc` 的 Connect 呼叫；查價在 transaction 之外），`bench.js` 的 setup 也多一步 `UpsertProduct`：本機（開發機）的這些數字不能當作現在的對照組。server 定價之後的 valid baseline 見下方「CI baseline」（GitHub runner，run 41005829640）。

## 重跑

```bash
# 前置：本機 Postgres 在跑（task compose-up）、k6 / psql / python3 在 PATH 上、role 能 CREATE DATABASE
task k6-bench                      # 預設 DATABASE_URL=postgres://app:app@127.0.0.1:5432/app，輸出 bin/k6-bench/<stamp>/
task k6-bench -- /tmp/my-out       # 指定輸出目錄
RUNS=5 task k6-bench               # 次數（≥ 3）；參數都是環境變數，見 tests/k6/bench.sh 檔頭
```

- 每個 run 在 Postgres 建一個專屬資料庫（`k6_bench_<stamp>_<pid+random>_<n>`；只 drop 自己 CREATE 成功的，名稱已存在就失敗退出、不碰它，`task k6-bench-test` 用 sentinel 資料庫驗證）、用 `./bin/migrate up` 建 schema、起 `./bin/rpc` + `./bin/worker`（auth off、telemetry off、log warn；#347 起每個 attempt 另起一個 `./bin/catalog-rpc` 接同一個資料庫，rpc 以 `catalog.upstream` 問它，見 `tests/k6/catalog.sh`；v0.54.0 以前的 `bench.sh` 沒有這一步，catalog 在 rpc 內），跑完 drop（失敗或 ^C 也會）。所以每個 run 從空的 outbox 起跳，寫入量由參數決定（create：約 2.5–3 萬張單；reserve：100 × 30 = 3000 張），也不會留東西在你的資料庫裡（#238）。每個 run 另有獨立的 `RUN_ID`。
- 階段（`tests/k6/bench.js`，`PHASE=create|reserve`；setup、負載、排空各自獨立計時）：
  - **create**：20 VU 不停頓連打 CreateOrder 10 秒（closed model，飽和狀態的吞吐與延遲）。
  - **backlog drain**：create 結束的瞬間，worker 還欠一堆 reservation。`PENDING when create ended` = 那一刻仍是 PENDING 的訂單數；`backlog drain (orders/s)` = 這個數字 ÷ 到 PENDING 歸零的秒數（分子分母同一個時間窗；create 期間 worker 已處理的訂單兩邊都不算）。歸零後還要驗證每張訂單都是 CONFIRMED、RESERVED 的 reservation 數等於訂單總數，否則整個 bench 失敗（PENDING 消失不代表成功）。create 結束時沒有積壓則記為 0 / n/a，不會算出不存在的吞吐。
  - **reserve**：每秒 100 張單、30 秒（open model），每張 CreateOrder 後輪詢 GetReservation（`POLL`=50ms）到 RESERVED。兩個延遲，名稱即定義：`order → RESERVED` 從**送出 CreateOrder 之前**算到看到 RESERVED（端到端）；`CreateOrder returned → RESERVED` 從 CreateOrder 回傳後算起（worker 負責的那段）。輪詢間隔使每筆觀測值最多晚 50ms（平均約 25ms）看到 RESERVED，是量測誤差而非延遲的一部分；不要把兩個分布的 percentile 相加或相減當修正值。
- 吞吐的分母：第一個 iteration 開始到最後一次 CreateOrder 回傳（k6 腳本自己記的 `bench_iter_start_ms` / `bench_create_end_ms` 的 min / max），不用 k6 Counter 的 rate（它的分母是整個 test，包含 setup() 的 SetStock）。
- 可重跑的輸入：SKU 由 `SEED`（預設 1，記在 `result.json` 的 `parameters.seed`）和 iteration 序號決定的 PRNG（mulberry32）選，不用 `Math.random`；同參數、單 VU 時序列逐筆相同，多 VU 時交錯順序由排程決定。
- **哪些 attempt 算數，由工具依事前固定的規則判定**，不是看過數字的人：每個 attempt **開始前**先取樣 1 分鐘 load average（每 5 秒一次，寫進 `run<n>-baseline.csv`），最多等 `BASELINE_WAIT`（預設 180 秒）直到**最後一個樣本** ≤ `MAX_LOAD_PER_CORE`（預設 1.0）× 核心數，才開始量；等不到＝這個 attempt invalid（不執行，`run<n>-invalid.txt` 記 `environment not quiet`）。**量測進行中的 load 只記錄、不判定**（`run<n>-load.csv`，每 5 秒一筆，report 的 attempt 表「load while running」欄）：rpc、worker、Postgres、k6 都跑在同一台機器上，那是 bench 自己製造的 load（#288：4 核 runner 開跑時 1.70，bench 進行中爬到 5.39，舊規則因此把 5 次 attempt 全判 invalid）。另一種 invalid：**過載造成的失敗**——k6 exit 99（越過 threshold）時，工具讀該階段的 `--summary-export`，依 threshold **名稱白名單**分類（`bench_report.py` 的 `classify_threshold_failure`）；只有「能證明是過載」的才是無效 attempt（`run<n>-invalid.txt` 記哪些 threshold，後面的階段不再執行，算一次無效 attempt，最後 exit 3），**其餘一律是真正的失敗（fail-closed）**：

  | threshold | 被突破時 |
  |---|---|
  | `reservation_timeouts` | 過載 → invalid attempt |
  | `rpc_errors` | **只有每一個 error 都是逾時**（`rpc_errors{code:request.timeout}` + `rpc_errors{code:transport.timeout}` 的 count 等於 `rpc_errors` 的 count；`bench.js` 用永遠為真的 threshold 讓 k6 匯出這兩個子指標）才是過載；有任何非逾時的 error，或子指標不存在 → 失敗 |
  | `http_req_failed` | 只有同時有上一列的「全是逾時」`rpc_errors` 被突破才跟著算過載；單獨被突破 → 失敗 |
  | `reservation_wrong_status`、`rpc_ok{…}`、`rpc_duration{…}`、任何不在上表的 threshold | 失敗（正確性或無法解釋；**不是**「重試就好」） |
  | summary 讀不到／壞掉、或 exit 99 卻沒有任何 threshold 標為 crossed | 失敗（無法判斷） |

  同時突破過載類與非過載類 → 以失敗為準。失敗時 `bench.sh` 以 exit 99 結束（不是 0、不是 3），verdict 是 `INVALID: bench.sh failed (exit 99)…`，不能被當成無效 attempt 而重派送。
- attempt 一直跑到有 `RUNS`（3）個 valid 或達 `MAX_ATTEMPTS`（預設 6）；**所有 attempt 的檔案都留在輸出目錄**，統計只用 valid 的，invalid 的列在 `summary.md` / `result.json` 的 attempts 表（含原因）；湊不滿 3 個 valid 就在 summary 最上面印「NOT A VALID BASELINE」並以 exit 3 結束（全部 attempt 都被中止、沒有任何 k6 數字時只有 attempts 表）。門檻 1.0 的意思：開跑前 load average 已超過核心數，代表機器上有別的東西在搶；這個數字與 `BASELINE_WAIT` 都是預設值，不是由實測結果推導的，要換就在跑之前用環境變數指定（會記進 `result.json`）。真正的工具故障（找不到 binary、migrate 失敗、rpc / worker 起不來、k6 腳本錯誤、backlog 排空逾時或對帳不符、上表的非過載 threshold）維持非 0、非 3 的 exit code。
- **對帳**：reserve 階段每個 attempt 必須「CreateOrder 成功數 = 看到 RESERVED 的數量」（`reserved_ok`），`gracefulStop` 涵蓋 CreateOrder 逾時加等待的上限；k6 印出 `No script iterations fully finished` 也算失敗。已建立但沒完成的單不會被算成成功或延遲 0ms；reservation 的成功率 = 看到 RESERVED 的訂單 ÷ 建立成功的訂單（不是 CreateOrder 的成功率）。
- 輸出：`result.json`（機器可讀：每個指標的 median / min / max / CV / 各 run 數值，加上 commit、Go / Postgres / k6 版本、CPU、OS、每個 attempt 的 load 取樣統計與是否 valid、參數）、`summary.md`、以及每個 attempt 的 k6 `--summary-export`、load 取樣 csv 與 log。
- API key：腳本送一把假 key（auth off 會忽略），`bench.sh` 結束時 grep 輸出目錄所有檔案，找到就整個 bench 失敗；k6 腳本沿用 `lib.js` 的規則（不印 body / URL，`systemTags` 不含 `url`、`error`）。
- 量測工具本身的測試：`task k6-bench-test`（報告定義與有效性/對帳的單元測試（含 run 41000071677 的真實 load 樣本）、`bench_overload_test.sh`：假 k6 / psql 驗證 bench 自己的 load 不判無效、機器忙時等到安靜、等不到就 exit 3、k6 threshold 失敗 exit 3 而 k6 腳本錯誤仍是工具故障、drain 起算時間（fake psql）、對 stub server 的 fixture：seed 可重現、端到端延遲含 CreateOrder、吞吐不被 setup 稀釋、被中斷的 reservation 不會悄悄通過、sentinel 資料庫不會被 drop）；不進 CI。

## 在 GitHub runner 上跑（#280）

共用開發機拿不到 valid 數字（見下方 attempt 清單），所以有一個只靠 `workflow_dispatch` 觸發的 workflow：`.github/workflows/k6-bench.yml`，在乾淨的 GitHub-hosted `ubuntu-latest` 上跑**同一個** `task k6-bench`。不進 PR / main 的 check，不自動寫回任何東西。

```bash
GH_CONFIG_DIR=… gh workflow run k6-bench.yml --repo blackhorseya/x-monorepo --ref main   # 或 Actions 頁 → k6-bench → Run workflow
gh run list --workflow k6-bench.yml --repo blackhorseya/x-monorepo --limit 3
gh run download <run-id> --repo blackhorseya/x-monorepo -n k6-bench-<run-id>-1            # 原始檔 + summary
```

- **沒有 input**：有效性規則（`MAX_LOAD_PER_CORE` 1.0、`RUNS` 3、`MAX_ATTEMPTS` 6、負載參數）就是被派送的那個 commit 上 `tests/k6/bench.sh` 的預設值，workflow 不能放寬它。要換參數＝改腳本＝走 PR。`ref` 要選你想量的 commit；run 頁與 `runner.txt` 都會記 SHA。
- Postgres 是同一台 runner 上的 `postgres:16-alpine` service container（load average 也算它）；`k6`、`psql`、`python3` 在 runner 上。同一時間只跑一個（`concurrency: k6-bench`）。
- **判讀 run 狀態**（`task --exit-code` 把 `bench.sh` 的 exit code 原樣交給 `scripts/k6-bench-verdict.sh`；Task 預設會把任何失敗轉成 201，所以不能省；job 綠 = 工具判定 valid，其餘一律紅，不會有「綠但無效」）：

  | 結果 | 意思 | 該怎麼做 |
  |---|---|---|
  | job 綠，`verdict.txt` = `VALID` | `bench.sh` exit 0：有 `RUNS` 個 valid attempt | 取 artifact 的 `result.json` / `summary.md`，開另一支 docs PR 更新「Baseline」 |
  | job 紅，`verdict.txt` = `INVALID: NOT A VALID BASELINE` | `bench.sh` exit 3：valid attempt 不足 `RUNS`（開跑前機器不夠安靜，或過載造成 k6 threshold / timeout 失敗）；summary 最上面也有同樣的 banner，attempts 表逐個列原因 | **照實記錄，不寫入 baseline**；可在清單加一列 run URL + 每個 attempt 的 load 最大值。可再派送一次，但「再跑到過為止」要先講好上限，不要事後挑 |
  | job 紅，`verdict.txt` = `INVALID: bench.sh failed (exit N)` | 量測本身壞了（對帳失敗、API key 洩漏檢查、rpc / worker 起不來、k6 腳本錯誤、非過載類 threshold 被突破〔exit 99，例如 `reservation_wrong_status`〕…；**不含**證明是過載的 threshold 失敗，那是 exit 3） | 看 `bench.log`，修工具，不是 baseline |

- **artifact**（`k6-bench-<run-id>-<attempt>`，保留 90 天；invalid 也會上傳）：`runner.txt`（run URL、commit、runner 名稱 / image 版本、核心數、CPU、記憶體、kernel、Go / k6 / psql 版本、開跑時的 load）、`verdict.txt`、`bench.log`、`summary.md`、`result.json`，以及每個 attempt 的 `run<n>-baseline.csv`（開跑前）、`run<n>-load.csv`（進行中，每 5 秒一筆 1 分鐘 load average）、`run<n>-invalid.txt`（invalid 的原因，有才有）、`run<n>-create.json` / `run<n>-reserve.json`（k6 `--summary-export`）和 rpc / worker / k6 log。`result.json` 的 `attempts` 逐個列出開跑前 load、進行中 load 統計、valid 判定與原因；valid 的 attempt 才進統計，invalid 的留著但不算。
- **把 valid 結果寫回 docs**：另開 docs PR（本 workflow 不做），把 `summary.md` 的數字放進「Baseline」，並註明 run URL、被量的 commit SHA、runner 規格（`runner.txt`）與 attempt 清單。GitHub-hosted runner 的規格與鄰居會變，所以這個數字只能和**同一類 runner、同參數**的下一次比較，不能拿來和開發機的數字比。

## Baseline

### CI baseline（GitHub runner，run 41005829640）

目前唯一符合規則的 valid baseline，來自 `k6-bench` workflow 的一次 `workflow_dispatch`：<https://github.com/blackhorseya/x-monorepo/actions/runs/41005829640>（main @ `cffc8033d2df4b49e9a4e88d3a6b8de1c62d3cea`，attempt 1）。verdict：`VALID: bench.sh found enough valid attempts (exit 0)`。以下數字原樣抄自該 run 的 artifact（`k6-bench-41005829640-1` 的 `runner.txt`、`summary.md`），未重算、未四捨五入。

> **和下方本機數字的關係：環境不同，不能直接比較。** 這組是 GitHub-hosted `ubuntu-latest`（4 核 AMD EPYC 7763）上、server 定價之後（含 catalog 批次查價）的數字；下方「探索性結果」是 Apple M1 Max（10 核）、server 定價之前（#265 的本機 N=3）、非 valid 的數字。兩者的硬體、Postgres 版本與部署方式（runner 上是 `postgres:16-alpine` service container）、程式 commit 都不同，**不要拿來算改善或退步**。之後的優化只能和**同一類 runner、同參數**的下一次 CI 結果比。

**環境**（`runner.txt`）

| 項目 | 值 |
|---|---|
| runner | GitHub Actions 1000016430（github-hosted, Linux X64），`ubuntu-latest` |
| image | ubuntu26 20261026.180.1 |
| cores | 4 |
| CPU | AMD EPYC 7763 64-Core Processor |
| memory | 15983 MiB |
| kernel | Linux 7.0.0-1017-azure x86_64 |
| Go | go1.27.3 linux/amd64 |
| k6 | v2.3.0（commit/e088784614, go1.26.8, linux/amd64） |
| psql / Postgres | psql 18.6；server 為 Postgres 16.15（`postgres:16-alpine` service container，與 bench 同一台 runner） |
| 開跑時 load（1m/5m/15m） | 6.45 2.25 0.81 |

**參數**：create 20 VUs × 10s；reserve 100 orders/s × 30s；50 SKUs；poll 50ms；seed 1；3 個 valid attempt（實際跑 3 個）。

**有效性規則**（d-47 / #288）：每個 attempt 開跑前的 1 分鐘 load average ≤ 1.0 × 4 核 = 4.0（最多等 `BASELINE_WAIT`）；量測進行中的 load 只記錄、不判定；k6 越過 threshold（過載造成的 timeout / error）的 attempt 為 invalid。規則來自該 commit 上 `tests/k6/bench.sh` 的預設值，workflow 沒有 input、不能放寬。

**3 個 attempt 的 load**（開跑前＝判定依據；進行中＝只記錄）

| attempt | load before (judged) | load while running: max (recorded) | mean | valid | in statistics | why not valid |
|---|---|---|---|---|---|---|
| 1 | 3.99 | 6.1 | 4.87 | yes | yes |  |
| 2 | 3.51 | 7.57 | 5.68 | yes | yes |  |
| 3 | 3.77 | 6.75 | 5.43 | yes | yes |  |

**統計**（3 個 valid attempt 的 median、min–max、CV＝stdev/mean）

| metric | median | min–max | CV |
|---|---|---|---|
| CreateOrder throughput (req/s) | 1024 | 1023–1097 | 3.3% |
| CreateOrder success | 100.00% | 100.00–100.00% | 0.0% |
| CreateOrder p50 (ms) | 15.02 | 14.50–15.31 | 2.2% |
| CreateOrder p95 (ms) | 34.52 | 32.24–34.84 | 3.4% |
| CreateOrder p99 (ms) | 95.87 | 82.49–97.57 | 7.3% |
| PENDING when create ended (orders) | 9592 | 9581–10286 | 3.4% |
| backlog drain (orders/s) | 316 | 311–317 | 0.8% |
| backlog drain (s) | 30.4 | 30.2–33.1 | 4.1% |
| reserve: orders created (req/s) | 100.0 | 100.0–100.0 | 0.0% |
| reserve: created orders seen RESERVED | 100.00% | 100.00–100.00% | 0.0% |
| reserve: dropped iterations (never started) | 0 | 0–0 | 0.0% |
| order → RESERVED p50 (ms, from before CreateOrder) | 157 | 157–158 | 0.3% |
| order → RESERVED p95 (ms, from before CreateOrder) | 262 | 262–263 | 0.2% |
| order → RESERVED p99 (ms, from before CreateOrder) | 362 | 360–364 | 0.5% |
| CreateOrder returned → RESERVED p50 (ms) | 155 | 155–155 | 0.0% |
| CreateOrder returned → RESERVED p95 (ms) | 259 | 259–259 | 0.0% |
| CreateOrder returned → RESERVED p99 (ms) | 359 | 357–361 | 0.5% |

原始檔在該 run 的 artifact `k6-bench-41005829640-1`（保留 90 天）。

### catalog 拆出前後（#320 第 5 片）

同一個 `k6-bench` workflow、各派送一次（無 input），兩次都是 `VALID`（`bench.sh` exit 0；各 3 次 attempt、3 個 valid）。數字原樣抄自兩個 run 的 artifact `summary.md`，未重算：

| | 切換前 | 切換後 |
|---|---|---|
| run | <https://github.com/blackhorseya/x-monorepo/actions/runs/41162571838> | <https://github.com/blackhorseya/x-monorepo/actions/runs/41162587345> |
| ref / commit | tag `v0.54.0` / `fdf4602cf301bdcc7e8ecc6bf4eddcbb8eb6a7d6` | `main` / `9664a02934113d4dc137faefc1a58e0926567814` |
| catalog | `rpc` 內的函式呼叫（同一個 process） | 獨立的 `catalog-rpc`（同一台 runner、loopback，Connect + service key，#347） |
| runner | GitHub Actions 1000019021，`ubuntu-latest`，4 核，**Intel Xeon Platinum 8370C** | GitHub Actions 1000019024，`ubuntu-latest`，4 核，**AMD EPYC 7763** |
| image / kernel / Go / k6 / Postgres | ubuntu26 20261026.180.1 / 7.0.0-1017-azure / go1.27.3 / k6 v2.3.0 / 16.15 | 同左 |
| 參數（兩份 `result.json` 的 `parameters` 逐欄相同） | create 20 VUs × 10s；reserve 100 orders/s × 30s；50 SKUs；poll 50ms；seed 1；`runs` 3、`attempts` 3；`max_load_per_core` 1.0、`baseline_wait_s` 180 | 同左 |
| 開跑前 load（attempt 1/2/3，判定依據） | 1.67 / 3.83 / 3.86 | 2.0 / 3.96 / 3.93 |

**CreateOrder**（3 個 valid attempt 的 median、min–max、CV）

| metric | 切換前 | 切換後 |
|---|---|---|
| throughput (req/s) | 1300（1182–1327，CV 5.0%） | 729（657–750，CV 5.6%） |
| success | 100.00% | 100.00% |
| p50 (ms) | 12.05（11.69–12.97，CV 4.4%） | 20.85（20.55–22.11，CV 3.2%） |
| p95 (ms) | 30.11（29.35–38.59，CV 12.8%） | 75.93（72.05–81.27，CV 5.0%） |
| p99 (ms) | 70.20（68.34–74.42，CV 3.6%） | 118.26（112.08–127.01，CV 5.1%） |

其他階段（只列出，不做比較判讀；完整表在各自 artifact 的 `summary.md`）：

| metric（median） | 切換前 | 切換後 |
|---|---|---|
| PENDING when create ended (orders) | 12049 | 6689 |
| backlog drain (orders/s) | 438 | 320 |
| backlog drain (s) | 27.5 | 21.1 |
| reserve: created orders seen RESERVED | 100.00% | 100.00% |
| reserve: dropped iterations | 0 | 0 |
| order → RESERVED p50 / p95 / p99 (ms) | 156 / 260 / 311 | 158 / 262 / 362 |
| CreateOrder returned → RESERVED p50 / p95 / p99 (ms) | 154 / 257 / 309 | 155 / 259 / 359 |

**判定 CPU 是否相同的依據**是各 run artifact `runner.txt` 的 `cpu:` 欄，不是推測。切換前（Xeon 8370C）與切換後（EPYC 7763）**不同**，所以上表是「不同 CPU 的兩次量測」，**不能當成同條件比較**。

為了拿到與切換後同為 EPYC 7763 的切換前 run，在 `v0.54.0` 上又派送 3 次（循序，一次一個；`runs-on: ubuntu-latest` 無法指定 CPU，上限 3 次是事前定的）。**3 次都沒有派到 EPYC 7763**，所以沒有同 CPU 的配對。三次都是 VALID，CreateOrder median（min–max, CV）：

| run（`v0.54.0` @ `fdf4602`） | runner CPU | throughput (req/s) | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---|---|---|---|---|
| [41163673581](https://github.com/blackhorseya/x-monorepo/actions/runs/41163673581) | AMD EPYC 9V74 | 1017（988–1408，CV 16.8%） | 10.96（CV 2.2%） | 70.71（35.77–92.44，CV 35.2%） | 133.05（72.24–173.89，CV 33.0%） |
| [41164041016](https://github.com/blackhorseya/x-monorepo/actions/runs/41164041016) | AMD EPYC 9V45 | 1682（1480–1816，CV 8.3%） | 8.78（CV 7.2%） | 26.57（21.90–32.58，CV 16.2%） | 62.08（57.77–69.36，CV 7.6%） |
| [41164384315](https://github.com/blackhorseya/x-monorepo/actions/runs/41164384315) | AMD EPYC 9V74 | 1184（1145–1196，CV 1.8%） | 13.11（CV 0.6%） | 33.31（30.13–35.03，CV 6.2%） | 83.83（83.25–84.58，CV 0.6%） |

加上上面的 Xeon 8370C 那次，**同一個 commit（`v0.54.0`）在 4 個 run 上**：CreateOrder p50 8.78–13.11 ms、p95 26.57–70.71 ms、throughput 1017–1682 req/s。run 與 run 之間（不同 runner、不同 CPU）的差，和切換後 vs 切換前的差（p95 75.93 vs 30.11 ms）在同一個量級，所以這份資料**不能**把差異歸因給 catalog 拆出。

**這份比較的限制**（照實寫，不往外推）：

- 沒有同 CPU 的切換前／切換後配對。切換後只有一次（EPYC 7763，run 41162587345）；切換前沒有 EPYC 7763 的 run。
- 同 CPU 型號也不保證同條件：run 41162587345 與 run 41005829640（EPYC 7763，main @ `cffc8033d2df4b49e9a4e88d3a6b8de1c62d3cea`，catalog 仍在 rpc 內，上一節）是目前唯一同型號的一對，後者 CreateOrder p50 15.02 ms、p95 34.52 ms、p99 95.87 ms、1024 req/s（median）。但它的 commit 早於 `v0.54.0`，兩者之間有其他變更，只能當佐證。
- 每個 run 的 N = 3（3 個 valid attempt）；median 與 min–max 只描述那 3 次，沒有做顯著性檢定。run 內的離散程度也不小（上表 p95 CV 最高 35.2%）。
- **`v0.54.0` 與 `9664a02` 之間的 commit**（`git log v0.54.0..9664a02`，共 3 個，77 個檔案）：`721614d` chart 0.3.37（#346，只動 chart）、`081b0f3` inventory 測試（#342，只動測試）、`9664a02` rpc 改問 `catalog-rpc`（#347，拆分本身）。除了 #347，沒有其他 production 程式變更；#347 本身同時包含拆分以外的東西（`tests/k6/catalog.sh`、Helm、docs），bench 的啟動流程因此也不同（切換後每個 attempt 多起一個 `catalog-rpc` process）。
- 切換後的 `catalog-rpc` 與 rpc、worker、Postgres、k6 跑在同一台 4 核機器上（loopback，沒有跨機網路），它的 CPU 用量也算進切換後的數字；跨機部署的網路延遲不在量測範圍。
- #320 明寫「幅度要實測，不預設目標值」，這裡不下好壞結論，也不外推到其他硬體或 production。要得到可比的數字，得有可指定 CPU 的 runner（例如固定規格的 larger runner 或 self-hosted），或在同一型號 CPU 上各跑；目前沒有。

### 本機探索性結果（非 valid）

**開發機上沒有符合規則的 valid baseline。** 下面的表是探索性結果：在共用的開發機上、條件受限、而且挑選過程有事後成分（見 attempt 清單），**不是乾淨的 baseline**，只能當量級參考，不要拿來判定優化有沒有效。要得到 valid 的數字，在機器安靜時重跑 `task k6-bench`；工具會自己判定。

### 所有 attempt（都保留，副本在 `/Users/sean/.agend/workspace/x-monorepo-worker-45/`）

| 目錄 | 時間（UTC） | 程式 | load（1m） | 狀態 |
|---|---|---|---|---|
| `perf-baseline-20261029T213621Z` | 21:36–21:4x | PR head a4d6720 | 未取樣 | 舊定義（drain、延遲、吞吐的算法後來被 review 判定有誤），只供溯源，數字已作廢 |
| `perf-baseline-20261029T220610Z-DISCARDED` | 22:06–22:12 | 1675b0d | 當時未取樣；結束後 `uptime` 約 14 | **事後棄用**：先看到 reserve p95 約 7 秒、丟 446 個 iteration，才去查 load，理由是看過結果後才決定的 |
| `perf-baseline-20261029T221617Z` | 22:16–22:22 | 1675b0d | 只有起訖兩點：6.11 → 8.54 | 下表來源。跑之前我寫了「等 load < 6」的等待迴圈，**這個門檻是看過上一組之後才訂的**，而且實際開始時是 6.11；中間沒有取樣，無法證明整段符合任何規則 |
| `perf-baseline-r3-attempts/out`（attempt 1–6） | 22:48–22:59 | e0219ec（規則出現後的第一次） | 每個 attempt 的最大值 23.08 / 19.19 / 15.84 / 15.28 / 18.32 / 25.71（上限 10.0） | **6 組全部 invalid**，工具印「NOT A VALID BASELINE」 |

> 上表的 invalid 判定是舊規則（bench 進行中的 load ≤ 1.0 × 核心數）下做的；因為進行中的 load 含 bench 自己的 rpc / worker / Postgres / k6，這些判定可能是 bench 自己造成的（#288 起改為只看開跑前的 load）。數字仍然不能當 baseline，只是「機器忙」不再是唯一的解釋。

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
