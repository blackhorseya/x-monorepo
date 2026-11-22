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
| runner、啟動 rpc／worker、平台的 steps、`registrations` | `apps/rpc/acceptance_test.go`（`TestAcceptance`、`startRPC`、`registrations`〔錨點 `new-bc:acceptance`〕、`registerPlatformSteps`） |
| BC 清單 vs 註冊清單、feature 掃描 | `apps/rpc/acceptance_registry_test.go`（`bcsFromWork`、`checkRegistered`、`scanFeatures`） |
| 每個 BC 的 feature 與 steps | `pkgs/<bc>/acceptance/features/<bc>.feature`、`pkgs/<bc>/acceptance/acceptance.go`（`task new-bc` 產生） |
| template／scenario DB、URL 檢查、身分斷言 | `apps/rpc/acceptance_db_test.go` |
| 密碼不外洩的測試（malformed／連不上／正常／子程序輸出） | `apps/rpc/acceptance_secret_test.go` |
| 子程序 helper（migrate、worker） | `apps/rpc/acceptance_proc_test.go` |
| `Env`、`Register`、`Eventually`、`ExpectRejected`、`Env.QueryText` | `pkgs/platform/acceptance/{acceptance,eventually,steps}.go` |

feature 檔用繁體中文，Go 程式碼與識別字用英文。zh-TW 的關鍵字由 gherkin 套件定義（`github.com/cucumber/gherkin/go/v42@v42.0.0/dialects_builtin.go`，`"zh-TW"`）：`功能`、`場景`、`假如`、`當`、`那麼`、`而且`、`但是`。

## 每個 scenario 怎麼跑

- **資料庫 URL**：`TEST_DATABASE_URL`（或 pgtest 自起的 container URL）必須是 `postgres://` 或 `postgresql://` 的 URL，query 的 key 只准 `sslmode`、`sslrootcert`、`sslcert`、`sslkey`、`connect_timeout`、`application_name`（`checkBaseURL`，`apps/rpc/acceptance_db_test.go`）；`dbname`、`database`、`service`、`options` 或 keyword/value 格式的 DSN 一律拒絕，因為 pgx 讓 query 的 `dbname` 蓋過 URL 的 path，scenario 會連到入口 DB 而不是自己的 clone。
- **資料庫**：`TestAcceptance` 先用 `pgtest.URL` 連到 server，建一個名稱帶隨機 run id 的 template DB，`go build` 並執行 `apps/migrate` 對它 `up`（migration 清單由 `apps/migrate` 持有、沒有匯出，做法同 `apps/worker/shutdown_test.go` 的 `migrateUp`）。要跑的 scenario 數由 gherkin 對 feature 檔算出（outline 每列算一個），測試一開始就用 `CREATE DATABASE … TEMPLATE` 複製出同樣數量的 DB（名稱 `acc_<run id>_<序號>`）；每個 scenario 的 Before hook 領一個。複製前 template 已設 `ALLOW_CONNECTIONS false`，並檢查沒有任何連線。只刪本次執行建立的 DB。scenario 結束時（After hook 在關完 `Env` 的 closer 之後）drop 前先等該 DB 的連線都消失，上限 5 秒（`dropWait`，用 `acceptance.Eventually` 輪詢 `pg_stat_activity`）：client 的 `Close` 先於 server 端 backend 結束，所以剛關掉的連線可能還列在裡面。逾時才失敗，訊息列出殘留連線的 pid、user、application_name、state（不含 query 文字），一般的 drop 不用 `FORCE`，只有 run 結束時清本次留下的 DB 才用。
- **有效身分斷言**：template 和每個 scenario DB 建好後，用 rpc／worker／migrate 會拿到的**同一個 URL 字串**、同樣的 driver（pgx）連一次，要求 `SELECT current_database()` 等於該 DB 的名稱，否則測試失敗；連線在檢查完就關閉。
- **不印 URL**：URL 帶密碼，處理它的錯誤與 log 只寫固定分類（`database url does not parse`、scheme 必須是 `postgres://`、`query key "dbname" is not accepted`），不 wrap 含 URL 的錯誤（`*url.Error`、pgx 的 `ParseConfigError` 會帶出 URL 與主機）；pgconn 連線失敗的訊息只有 `user=… database=…` 與位址、沒有密碼，子程序（apps/migrate、apps/worker）的輸出也沒有，這兩點由 `apps/rpc/acceptance_secret_test.go` 用合成密碼斷言。對 DB 下的語句出錯時（runner 的 `CREATE`／`DROP`／`ALTER`、`pg_stat_activity` 查詢、身分檢查，以及 steps 的 `Env.QueryText`）只用 `acceptance.DBFailure`：一句固定的話加上 server 回的 SQLSTATE，不 wrap、不印 driver 或 server 的文字——server 的訊息會帶出語句的參數（`SELECT ($1::text)::integer` 帶一個含密碼的 URL，訊息裡就有那個 URL）。context 已取消或逾時時，錯誤仍可用 `errors.Is(context.DeadlineExceeded)` 辨識。`apps/rpc/acceptance_secret_test.go` 的 `TestSecret_QueryTextServerErrorHasNoParameterAndHasTheSQLState` 對真的 Postgres 斷言（`pkgs/platform/acceptance` 自己的測試不需要 container）。
- **rpc**：runner 的 Before hook 在每個 scenario 都以 `Initialize` ＋ `httptest` 在測試程序內啟動（同 `TestPlatform_E2E`，`startRPC`），設定 `auth.mode=required` 與一把 admin API key，資料庫指向該 scenario 的 DB；`Env.RPCURL` 因此在所有 steps 前就備好。「假如 rpc 以 auth.mode=required 啟動…」這個 step 只斷言這個前提成立，每個 BC 的 feature 都用同一行文字當前提（它是平台的 step，BC 不重複定義）。
- **BC 清單與註冊**：測試開頭用 `go work edit -json` 加 `scripts/bcs.jq` 取得 BC 清單（與 `task gen-openapi`、`tools/newbc` 同一條規則；缺 `jq` 或 `go` 就失敗並說明，不會當成沒有 BC），跟 `registrations` 雙向比對：BC 沒註冊、註冊了卻不在 go.work、同一個 BC 註冊兩次、BC 沒有任何 scenario，都讓整個測試失敗並指名（`checkRegistered`）。沒有 BC（template 現況）時只跑平台的 feature。BC 的 feature 在 `pkgs/<bc>/acceptance/features/`。
- **worker 子程序**：只有 `@worker` tag 的 scenario 才起（`startScenarioWorker`），指向該 scenario 的 DB；worker binary 在測試開頭編一次（只有 feature 含 `@worker` 才編）。它登記到 `Env`，在 After hook 先於 DB 的 drop 被 SIGTERM 停掉，exit code 為 0 且有 `worker stopped` 都是該 scenario 結果的一部分。這個範圍比 #615 原文的「每個 scenario 都起」窄，由 lead 核可：沒有事件的 scenario 起 worker 沒有觀測價值。
- **事件 scenario 與 Eventually**：`--events` 的 BC 有一個 `@worker` scenario：Create 之後，先在 `<bc>.events_v1_<bc>created` outbox 表找出 payload 裡的 id 等於 Create 回傳的 id 的那一列（不是「有一列」），再用 `acceptance.Eventually` 等 `<bc>.inbox` 出現同一個 message id（`usecasex.Idempotent` 在 handler 的交易裡寫入，message id 就是 outbox 的 `uuid`），並斷言 `<bc>.events_poison` 沒有訊息。`Eventually(c, what, limit, check)` 每 100ms 輪詢，期限取 `limit` 與 `c` 的期限較近者，回傳實際等了多久（scenario 以 `godog.Logf` 印出）；逾時的錯誤帶 what、期限、最後一次的 `observed`（只放計數與 id，不放 URL）；check 回的錯誤若不是 context 錯誤（含與期限同時到的），原樣保留，`errors.Is` 認得出來，不會被當成「條件未成立」。steps 的 `c` 由 runner 的 Before hook 設期限：`t.Deadline()` 減去 20% 的剩餘時間（至少 5 秒），沒有 `-timeout` 時只受 `limit` 約束。查資料庫用 `Env.QueryText`：短命連線、查完就關，所有錯誤都是 `DBFailure` 的固定訊息（連不上、語句出錯、讀取出錯皆同）。
- **斷言**：用 Connect client 呼叫，斷言實際回來的 Connect code 與 `x-error-code`；授權成功則斷言 RPC 真的由 handler 回應，不看 HTTP status。
- **scenario 數**：實際跑的 scenario 數必須等於 feature 檔算出的數量，0 或不符都失敗（CI 的 `go test` 沒有 `-v`，這是讓綠燈代表都跑過的方式）。
- **worker**：以子程序執行（`go build` 到 `t.TempDir`），`APP_POSTGRES_URL` 指向該 DB，`APP_LOG_FORMAT=json`、`APP_LOG_LEVEL=info`。就緒訊號是 worker 自己在 stdout 寫的 `worker started`（`apps/worker/app.go` 的 `Run`）；結束驗證是送 SIGTERM 後 exit code 為 0 **且**看到 `worker stopped`。template 沒有 BC，worker 是 idle，所以這支測試只證明「能啟動、送出就緒訊號、能在期限內結束」，不涵蓋事件消費。
- **子程序期限**：取 `min(上限, t.Deadline 剩餘時間的一半)`；逾時就 kill 並等它真的結束。子程序「exit／failed／timed out」的判定只在 `waitChild`（`runStep`、`stop`、`killAndReap` 都走它）；worker 啟動時等就緒訊號是另一個 select（就緒、提前結束、逾時三者擇一）。worker 的 stdout／stderr 保存起來，失敗時印出。

## 加 BC 的 steps

`task new-bc -- <name> [--events]` 產生 `pkgs/<name>/acceptance/`（繁體中文 `features/<name>.feature` 與英文的 `Register`），並在 `apps/rpc/acceptance_test.go` 的 `registrations`（錨點 `new-bc:acceptance`，不要刪或複製）加一筆；`task new-bc-check` 在複本裡實際跑它們。BC 的 steps 實作 `acceptance.Register`（`func(sc *godog.ScenarioContext, env *acceptance.Env)`），`Env` 帶該 scenario 的 `DatabaseURL`、`RPCURL`、`AdminKey`，以及 `Defer`（登記要在 drop DB 之前關掉的資源）。不要在 `Register` 裡註冊 After hook。step 文字**每一條**都要帶 BC 名稱（含 `--events` 的 worker step：`<name> 的 worker 在 N 秒內…`）：同一個 scenario 的 context 裡有所有 BC 的 steps，文字必須只對到一個定義，否則 godog 回 `ambiguous step definition`（共用的前提 step 除外，它是平台的）。`tools/newbc` 的 `TestStepsAreUniquePerBC` 依序產生兩個 `--events` BC 與一個一般 BC，斷言每條 step 定義都含 BC 名、feature 的每條 step 只對到自己 BC 的定義。`pkgs/platform/acceptance` 與每個 BC 的 `acceptance` package 只能被測試檔 import：godog 不能進任何 binary（`task new-bc-check` 用 `go list -deps` 檢查 rpc、worker、migrate）。
