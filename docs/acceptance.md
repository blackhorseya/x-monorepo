# Acceptance（BDD）測試

用繁體中文 Gherkin 從真入口驗證行為：真的 rpc（測試程序內，經 HTTP）、真的 Postgres。runner 是 `github.com/cucumber/godog`（釘 `v0.16.0`，見 `apps/rpc/go.mod`）。

## 怎麼跑

```bash
task test                                                   # 每個 module 的 go test -race，含這裡的測試
cd apps/rpc && go test -count=1 -run '^TestAcceptance$' -v .   # 只跑 feature
cd apps/rpc && go test -count=1 -run '^TestAcceptance_WorkerProcess$' -v .   # 只驗 worker 子程序
```

它們是 `apps/rpc` 的一般測試，跟 CI 的 `test (apps/rpc)` job 一起跑，沒有另開 job。Postgres 由 `pgtest` 提供：沒設 `TEST_DATABASE_URL` 就自起 container；設了，該 role 要有 `CREATEDB`（`pkgs/platform/postgres/pgtest/pgtest.go` 的 package doc）。

## 檔案放哪裡

| 東西 | 位置 |
|---|---|
| feature（`# language: zh-TW`，繁體中文） | `apps/rpc/acceptance/features/*.feature` |
| runner、per-scenario DB、平台的 steps | `apps/rpc/acceptance_test.go`（`TestAcceptance`、`registers`、`registerPlatformSteps`） |
| 子程序 helper（migrate、worker） | `apps/rpc/acceptance_proc_test.go` |
| `Env`、`Register`（給之後的 BC 用） | `pkgs/platform/acceptance/acceptance.go` |

feature 檔用繁體中文，Go 程式碼與識別字用英文。zh-TW 的關鍵字由 gherkin 套件定義（`github.com/cucumber/gherkin/go/v42@v42.0.0/dialects_builtin.go`，`"zh-TW"`）：`功能`、`場景`、`假如`、`當`、`那麼`、`而且`、`但是`。

## 每個 scenario 怎麼跑

- **資料庫**：`TestAcceptance` 先用 `pgtest.URL` 連到 server，建一個名稱帶隨機 run id 的 template DB，`go build` 並執行 `apps/migrate` 對它 `up`（migration 清單由 `apps/migrate` 持有、沒有匯出，做法同 `apps/worker/shutdown_test.go` 的 `migrateUp`）。每個 scenario 的 Before hook 用 `CREATE DATABASE … TEMPLATE` 複製一個新 DB；After hook 刪掉它，錯誤會讓 scenario 失敗。只刪本次執行建立的 DB（名稱 `acc_<run id>_<序號>`）。複製前 template 已設 `ALLOW_CONNECTIONS false`，並檢查沒有任何連線。
- **rpc**：scenario 的「假如」step 以 `Initialize` ＋ `httptest` 在測試程序內啟動（同 `TestPlatform_E2E`），設定 `auth.mode=required` 與一把 admin API key，資料庫指向該 scenario 的 DB。scenario 結束時先關 rpc（含它的連線池），再刪 DB。
- **斷言**：用 Connect client 呼叫，斷言實際回來的 Connect code 與 `x-error-code`；授權成功則斷言 RPC 真的由 handler 回應，不看 HTTP status。
- **worker**：以子程序執行（`go build` 到 `t.TempDir`），`APP_POSTGRES_URL` 指向該 DB，`APP_LOG_FORMAT=json`、`APP_LOG_LEVEL=info`。就緒訊號是 worker 自己在 stdout 寫的 `worker started`（`apps/worker/app.go` 的 `Run`）；結束驗證是送 SIGTERM 後 exit code 為 0 **且**看到 `worker stopped`。template 沒有 BC，worker 是 idle，所以這支測試只證明「能啟動、送出就緒訊號、能在期限內結束」，不涵蓋事件消費。
- **子程序期限**：取 `min(上限, t.Deadline 剩餘時間的一半)`；逾時就 kill 並等它真的結束；等待只有 `waitChild` 一個 helper，回報是 exit、failed、timed out 哪一種。worker 的 stdout／stderr 保存起來，失敗時印出。

## 加 BC 的 steps

BC 的 steps 實作 `acceptance.Register`（`func(sc *godog.ScenarioContext, env *acceptance.Env)`），放進 `apps/rpc/acceptance_test.go` 的 `registers`。`Env` 帶該 scenario 的 `DatabaseURL`、`RPCURL`、`AdminKey`。`pkgs/platform/acceptance` 只能被測試檔 import：godog 不能進任何 binary。BC 的 feature 檔怎麼收集、`task new-bc` 產生骨架，不在這一版。
