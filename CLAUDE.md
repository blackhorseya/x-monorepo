# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Protobuf-first Go monorepo：Clean Architecture + DDD（bounded context 優先）、`go.work` 多 module、
Connect 對外、Wire 組裝、Turborepo 前端（`web/`）。細節以 `README.md`（架構與程式碼規則）和 `docs/`（CI / release / 維運 / config / error codes / workflow）為準，這份只放「開工就要知道」的事。

## 常用指令

所有檢查的單一事實來源是 `Taskfile.yml`；hooks、lefthook、CI 都只是呼叫它。但「呼叫它」不等於「跑過」：AgEnD worktree 的 `core.hooksPath` 指向 `~/.agend/hooks`，lefthook 通常沒掛上。回報要分開寫，沒掛上就在 exact head 手動跑 `task gate-local`，寫「手動 gate 通過，hook 未執行」，不寫成 lefthook 通過；驗證與措辭見 `docs/ci.md`「本機 hook 證據」。

```bash
task tools                 # 固定版本工具鏈（buf / wire / golangci-lint / protoc-gen-*）→ ./bin，第一次必跑
task tidy                  # 每個 module go mod tidy + go work sync
task gen wire              # protos → pkgs/<bc>/contract（pb + Connect + openapi.yaml）；DI → apps/*/wire_gen.go
task lint                  # buf lint + go vet + golangci-lint（depguard 強制分層 / BC 邊界）
task test                  # go test -race，每個 module
task gate-fast FILE=a.go   # gofmt + vet + golangci-lint --fast-only + archtest，只跑該檔所屬 module（秒級）
task gate-local            # gen freshness + buf lint + affected modules 的 go test -race（~1 min）
task gate-ci               # 與 CI 等價的全量檢查
task check-gen-fresh-all   # gen freshness 含 TS（= CI gen-freshness；需要網路）；check-gen-fresh 只含 Go / OpenAPI / Wire
task breaking              # proto 對 main 做 buf breaking
task compose-up            # 本機 Postgres :5432 + Redis :6379 + grafana/otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up         # 跑每個 BC 的 migrations；service 啟動不會自己跑
task merge-ready -- <pr>   # merge 前必跑：PR 最新綠燈是不是對「現在的 main + 這個 head」跑的（列 main / merge / base / head SHA 與 check 數）；stale（main 已前進）exit 2，先 `-- <pr> --revalidate` 重驗，不要 update branch（head 一變就要重審）
task release-snapshot      # goreleaser dry run → bin/dist（真的 release 由 main 上的 CI 綠燈自動觸發，不手動打 tag；沒改到會進 artifact 的檔案就被 gate 跳過，task release-gate 先看：列 releasable 檔 + 預測版號；六個 image 與 checksums.txt 都由 cosign keyless 簽名、archive 另有 GitHub artifact attestation，發布後 verify job 跑 scripts/release-verify.sh 全驗一遍；snapshot 以 --skip=sign 跳過；CI 的 release-snapshot 另跑 scripts/rekor-v2-canary.sh——cosign 預設 signing config 出現非 v1 的 Rekor 就紅（#204 的 409 處理只適用 v1，見 #215），.github/workflows/rekor-canary.yml 每週再跑一次，紅了不要 rerun release）
task poison -- list        # 看各 BC 的 poison 表；requeue <bc> <offset>…|all 送回原 topic、drop 刪掉（worker poison 子命令）
task expire -- --older-than 24h --dry-run   # reservation TTL：RESERVED 超過的還庫存 + 發 OrderCancellationRequested（worker expire；Helm expire CronJob 每 15 分鐘；rpc 版 InventoryService.ExpireReservations 給 console Expire 頁，要 admin role；`InventoryService.SetStock` 同樣要 admin role，auth 開著時非 admin 是 `PERMISSION_DENIED`）
task prune -- --older-than 720h --dry-run   # 刪每個 group 都 ack 過的 outbox 列 + 消費過的 inbox 列（worker prune；先 dry-run；NOTE 列出每個落後的 group 與最老未讀年齡：held back by = 過了 retention、behind = 還沒，--forget-group 刪掉不會再跑的）
scripts/dex-token.sh [email] [--id-token]    # 本機 Dex（compose 的 `dex` 服務；kind 的 dex.yaml）：authorization code + PKCE 登入拿一顆真 JWT；issuer / 帳號 / 離線 /etc/hosts 見 docs/operations.md「Dex dev IdP」
task payment-worker        # build 後以同一份 configs/payment-rpc.yaml（PAYMENT_CONFIG 換檔；config 檔在命令裡 export，環境裡已有的 APP_CONFIG 不會蓋過）跑 ./bin/payment-worker --require-durable：payment 的事件 consumer（#410）。本機 `require_payment: true` 時要它和 `task payment-rpc` 一起跑，OrderConfirmed 才會變成付款單；先 `task migrate -- up`（ordering 與 payment schema）。**所有讀設定檔的 task（run／worker／poison／expire／bootstrap／prune／migrate／catalog-rpc／payment-rpc／payment-worker）都在命令裡 `export APP_CONFIG=<CONFIG|CATALOG_CONFIG|PAYMENT_CONFIG>`**，環境裡已有的 `APP_CONFIG` 不會蓋過；Task 的 `env:` 蓋不過 OS 環境（#358），不要再用 `env:` 設 `APP_CONFIG`。`scripts/task-config-test.sh`（CI 的 build job）從真 `task` 入口驗證這一點，路徑含空白也要成立
task bootstrap                              # identity 第一個 account + API key（空 store 才建；worker bootstrap；secret 只印一次）
task helm-check            # deployments/helm/x-monorepo：每個 ci/*.yaml profile 跑 helm lint + kubeconform + check.py（derived 數字：grace period、hook 順序、prune args、console 的 RPC_UPSTREAM、ServiceMonitor / NetworkPolicy 的選擇器、payment-rpc 的三個 workload 與 ingress 只有 webhook path）、ci-negative/ 全部要 render 失敗、check-observability.py（Payment 面板與 alert 的 metric 名稱是程式碼真的匯出的）
task k6-wire               # tests/k6/wire.sh：四支腳本打一個回壞掉 HTTP response（status code / version / header 行帶著請求的 X-API-Key、body 被截斷、連線直接關）的假 server；每個 run 必須失敗，且 key 不在 k6 輸出、summary、metrics tag 任何地方（不需要 Postgres；CI 的 k6-wire job）
task k6-bench              # tests/k6/bench.sh：效能 baseline（CreateOrder 吞吐 / 延遲、下單 → RESERVED，N≥3 次、每次獨立資料庫）；結果與重跑方法見 docs/perf-baseline.md；不進 CI（乾淨 runner 上用 `workflow_dispatch` 的 k6-bench workflow，不寫回 baseline）
task k6-smoke              # tests/k6：orders（load thresholds）/ ratelimit（429 + Retry-After）/ timeout（1ns → 全部 DEADLINE_EXCEEDED）/ reservation（rpc + worker，下單 → RESERVED 延遲）各自起一個 bin/rpc 打；task k6 -- <script> 打現成 server
task helm-kind             # 真裝：kind cluster + deployments/helm/kind（拋棄式 Postgres / Redis）+ tree build 的 image（含 console：web-build → web/apps/console/Dockerfile），install → smoke → upgrade → smoke → payment.sh（payment-rpc 三個 workload、ingress-nginx 前的 webhook 與限流、payment-rpc 停掉時訂單照常 CONFIRMED）→ payment-flow.sh（`ordering.require_payment` 開的完整付款流程：付款成功、延遲／重送／晚到的款項、TTL 序列、開關兩個方向、payment-rpc 停掉；用 `payment-rpc mock pay\|notify\|fail`，只在 `kind/payment-flow-values.yaml` 開）；helm-kind-down 刪 cluster
task helm-kind-keda        # 接著在同一個 cluster：KEDA + OTLP collector + Prometheus，k6 灌訂單看 worker 真的靠 eventbus_consumer_lag scale out / in
task run                   # build 後以 configs/local.yaml 跑 ./bin/rpc（Connect + gRPC + gRPC-Web 同 port；RPC 在 /api/<svc>/<Method> 與原生路徑都通，/api/docs 開）
task payment-rpc           # build 後以 configs/payment-rpc.yaml（gitignored，從 payment-rpc.example.yaml 複製）跑 ./bin/payment-rpc serve --require-durable，:8082；目前沒有人呼叫它（rpc 轉發是切片 4），要 `task migrate -- up` 先建 payment schema
```

跑單一測試：`go.work` 根目錄不是 module，`./...` 在那裡比不到任何東西，必須 `cd` 進 module：

```bash
cd pkgs/ordering && go test -race -run 'TestCreateGetCancel' ./application/usecase/
cd pkgs/ordering && go test -count=1 -run '^TestArchitecture$' .      # 只跑 archtest
go test -race ./infra/postgres/                                       # integration test：testcontainers 自起 Postgres（需 Docker/Podman）
TEST_DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable \
  go test -race ./infra/postgres/                                     # 或指定現成 DB；設了連不上是 FAIL 不是 skip
```

前端：`web/` 是 Turborepo（pnpm）。`packages/proto` 是 connect-es client（`task gen-ts` 需要網路，產出到那裡；先過 `tools/stripvalidate` 拿掉 `buf.validate` options 與 `validate.proto`，只留 `Violations`）；
`apps/console` 是 Vite + TanStack Router / Query + Tailwind v4 + shadcn/ui，`task web-dev`（proxy 到 :8080）/ `task web-typecheck` / `task web-build` /
`task web-e2e`（Playwright：`web/apps/console/e2e`，起 `bin/rpc` + vite preview 真打；`E2E_DATABASE_URL` 有設就先 `migrate up` 走 Postgres 並由 `e2e/worker.setup.ts` 多起一個 `bin/worker`（CI；`reservation.spec.ts` 驗 outbox → worker → RESERVED / RELEASED 與 Complete → COMMITTED，`reservations.spec.ts` 驗 /reservations 的 status filter 與 `?order=` pin），沒設是 in-memory，spec 用 `e2e/mode.ts` 的 `withDB` 分支；project `auth`（`e2e/auth/`）另起一個 `auth.mode=required` 的 rpc + preview 驗 credential 流程（API key 與 `jwt.ts` 現場簽的 HS256 JWT；`helpers.ts` / `jwt.ts` 不是 spec，project 的 testMatch 只收 `*.spec.ts`）；locator 用 `getByLabel` / `getByRole`，第一次先 `pnpm exec playwright install chromium`）；**e2e 的 port 都由 `E2E_RPC_PORT` / `E2E_WEB_PORT` 一處派生**（`e2e/ports.ts`，playwright.config 與 `e2e/assert-clean.ts` 共用；預設 18091／4173）：fixtures 不寫死位址——`oidc-backend` 的 rpc 設定是 `e2e/oidc-backend/rpc-server.ts` 從模板 `rpc.yaml` 依 IdP port 現場產生的暫存檔（SIGTERM 時刪掉，所以那個 webServer 設了 `gracefulShutdown`），logout／「console 自己的 origin」的 assertion 從 project 的 `baseURL` 算。換 port 跑一定要 `CI=1`（否則 `reuseExistingServer` 會接到別人的 server）；`task web-e2e-ports` 是非預設 port 的 smoke（只跑標 `@ports` 的案例，不含 RevokeSessions 的快取等待），跑完檢查所有派生 port 在 `127.0.0.1` 與 `::1` 都已關（只有 ECONNREFUSED 算關，逾時或其他錯誤算失敗；`localhost` 在 IPv6 優先的機器上是 `::1`，Vite preview 綁它）、暫存設定已刪（先跑 `e2e/assert-clean.selftest.ts` 證明 checker 抓得到 IPv4、IPv6 與真 `vite preview`（它自己選一組全部派生 port 都空閒的 pair，不用 caller 的 E2E pair、不寫死 port；`assert-clean.parallel.selftest.ts` 證明兩個 runner 帶不同 pair 同時跑都過）；CI 在 `task web-e2e` 之後跑它）；`task web-e2e-image` 把 chromium project 改打真的 console image（`e2e/image.sh`，`E2E_CONSOLE_URL` 取代 vite preview；`image.spec.ts` 只在這模式跑，驗 nginx 的 fallback / cache header / proxy）。`task web-e2e-payment` 是 payment 的 Console 真流程（`playwright.payment.config.ts`，自己的 config：預設專案的 worker 開關是關的；需要 `E2E_DATABASE_URL` 與 `psql`）：一個 wrapper 行程在那台 Postgres 上建專屬資料庫，跑真的 catalog-rpc、payment-rpc serve／consume、rpc、worker，spec 在 Console 建單、按 Pay、用 `payment-rpc mock pay` 付款到 `COMPLETED`，沒有 `page.route`；跑完同樣檢查派生 port 都已關（`derivePorts().all` 含它的 port）。CI 在 `task web-e2e-ports` 之後跑它。
console 每個頁面（`src/routes/*`）是 `router.tsx` 裡 `lazyRouteComponent` 拆出的獨立 chunk，layout 與 `client.ts` / `queries.ts` 留在首屏；頁面之間共用的東西（`Field`、`ErrorAlert`、`formatMoney` 等）放 `src/components/`，不要讓 layout 或別的頁面 import `routes/<page>` 的 runtime 值（會把那頁綁回首屏，型別用 `import type` 無妨）；載入中 / chunk 載入失敗由 `defaultPendingComponent` / `defaultErrorComponent`（`components/route-status.tsx`，含 Retry）顯示。
**OIDC 登入（#273，`docs/identity-login.md` §8）**：`src/oidc/`（`config.ts` 讀同源 `/config.json`、`pkce.ts`、`discovery.ts`、`session.ts` = 登入 / callback / refresh rotation / 登出、`unbound.ts`）+ `routes/login.tsx`、`routes/auth-callback.tsx`（redirect URI `/auth/callback`）。設定是 **runtime**：nginx image 的 `deploy/18-console-env.envsh` 驗證 `CONSOLE_OIDC_ISSUER` / `CONSOLE_OIDC_CLIENT_ID` / `CONSOLE_API_KEY_LOGIN`，envsubst（精確白名單）渲染 `/config.json` 與 CSP（`connect-src 'self'` + issuer 的精確 origin；兩個都沒設 = OIDC 關閉；只設一個、含控制字元或格式不合 = container 啟動失敗）；Helm 先走 `console.extraEnv`。access token 只在記憶體（`auth.ts` 的 `setSessionCredential` / `rotateSessionToken`，不碰 localStorage）、refresh token 在 sessionStorage 一個 key；PKCE verifier / state / nonce 在 callback 讀取當下就刪。refresh 失敗：IdP 拒絕（400/401/403）或 `expires_in` 不是 10s–24h 的整數 = 登出，沒回應 / 5xx / 429 = 保留 token + 全域 backoff（人按 Retry 可突破）+ Retry；access token 到期當下就撤下（refresh 在飛行中也一樣，晚到的成功回應仍接受）= 回 /login 顯示 Retry；期限取 `expires_in` 與 JWT `exp` 較早者（都沒有 = 60s，< 10s 拒絕）；對 IdP 的請求 10s timeout。discovery 的 token endpoint 必須與 issuer 同 origin（console 自己比對，CSP 擋不住 'self'）。API key 入口是 `/login` 下方的 break-glass，`apiKeyLogin=false` 時不在 DOM。`/accounts/$id` 的 Sign-in bindings 卡讀 `ListAccountIdentities`（admin only，只顯示 contract 欄位）。後端回 `auth.unbound_identity` 時 `components/claim-invite.tsx` 取代頁面。Playwright：`e2e/auth/oidc*.spec.ts` 用 `e2e/auth/oidc.ts` 以 `page.route()` 當 IdP（簽 HS256 token 給真的 auth project rpc），project `oidc-backend`（`e2e/oidc-backend/`：`idp-server.ts` 是 loopback ES256 IdP、`rpc.yaml` 開 `auth.oidc` 的真 rpc；驗 server 接受 token、真的 `auth.unbound_identity` → ClaimInvite 綁定、`invite.email_unverified`、`RevokeSessions` 後舊 token 被拒（等 30s resolver cache）；`oidc-dex`（`E2E_DEX=1` 才存在，`task web-e2e-dex`；`e2e/oidc-dex/`：真 Dex 登入表單 → 未綁定 → `CreateInvite` + `ClaimInvite` → 進 Console，每個測試自己起一個只信任 compose 的 Dex 的 rpc（`rpc.ts` fixture，retry 也重來；Dex 帳號固定、binding 跟 rpc 同壽）、console 在 port 4203——該 origin 與 `web.allowedOrigins` 都得登記在 `deployments/docker/dex/config.yaml`）；`e2e/image.spec.ts` 驗 image 的 `/config.json` 與 CSP（`image.sh` 另起 OIDC 設定的容器，並驗證各種錯誤 env 讓容器拒絕啟動）。
shadcn component 用 `pnpm dlx shadcn@latest add <name>` 加，產出的 `cn` import 要確認是 `@/lib/utils`。

**新 clone / 新 worktree 第一件事**：`task tools`。`./bin` 是 gitignored，而 `task gen`（也被 `gate-local` 間接呼叫）會**先刪掉**
所有 `*.pb.go` / `*.connect.go` 再呼叫 `bin/protoc-gen-*`；工具不在時會留下一個生成檔全被刪掉的 tree。
遇到了就 `git checkout -- pkgs` 還原，**不要**用 `git commit -a` 把刪除一起 commit 進去。
生成物（pb.go、connect.go、`wire_gen.go`、`contract/v1/openapi.yaml`）全部有 commit，gen-freshness 靠 `git diff` 比對它們。

## Hook harness（會擋你的東西）

`.claude/settings.json` 掛了兩個 hook，exit 2 時 stderr 會回到 agent：

- **PostToolUse（Edit / Write）**：對 `.go / .proto / go.mod / go.work / buf*.yaml` 跑 `task gate-fast`。
  用 Bash（sed、heredoc）改檔**不會**觸發，改完請手動 `task gate-fast FILE=<path>`。
- **Stop**：跑 `task gate-local`，失敗就不讓你宣告完成。gen-freshness 是和 HEAD 比 diff，
  改了 proto 或 wire provider 後要 `task gen wire` 並把產物一起 commit。

CI（`.github/workflows/ci.yml`）另外有 `work-sync`（go.mod 不得漂移，加依賴後 `task tidy`；Taskfile 的 `MODULES` 由 go.work 的 `use` 導出，新增 module 只加 `use`；`scripts/modules.jq` 是路徑唯一的規則——只收 `[A-Za-z0-9._/-]`、空的 `use` 清單也不行，每個 task 都會失敗；`scripts/check-modules.sh` 在 CI 跑同一份）、
`cover-gate`（所有 `*/domain/*` package 必須 100% 覆蓋）、`buf breaking`（只警告）。PR 上每個 job 只在輸入有變時跑（`scripts/ci-changes.sh` 決定；main 一律全跑），所以一支 PR 沒跑 helm-kind / k6 是正常的，不是壞掉；改 `scripts/ci-changes.sh` 的對照表要一起改 `scripts/test.sh`（`task scripts-test`）。

## 架構：誰依賴誰

```
pkgs/kernel      shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
pkgs/platform    技術共用：contextx、config、server（gin + Connect）、usecasex、otel、postgres、archtest；
                 admin（admin.v1.AdminService：poison 操作 + Prune RPC，與 worker 的 CLI 同一組 eventbus 函式；contract 在 protos/admin，不屬於任何 BC）
pkgs/<bc>        一個 bounded context = 完整六角形（示範 BC：ordering；inventory 是第二個，消費 ordering 的 event，ordering 再消費它的 `StockReserved` 把 order 變 CONFIRMED；`CompleteOrder` RPC 發 `OrderCompleted`，inventory 消費它把 reservation COMMITTED、扣 on_hand；`worker expire` 把 RESERVED 超過 TTL 的變 EXPIRED、還庫存、發 `OrderCancellationRequested`；sweep 與 `CompleteOrder` 競態時，`OrderCompleted` 遇 EXPIRED 會從 available 補扣成 COMMITTED，不夠就 `COMMIT_SHORT`（終態、不動庫存、alert `x-monorepo-commit-short`，營運手動處理）；
                 identity 是第三個：DB 管理的 account / API key，沒有 event；`adapters/auth` 實作 platform 的 `server.CredentialResolver`，auth interceptor 對 `X-API-Key` 先查 config `auth.api_keys` 再問它（TTL cache 30s）；`auth.oidc.enabled` 時 `Authorization: Bearer` 先交 `platform/oidc` verifier（`iss` 不認識才走 `auth.jwt`，其他失敗終局），驗過的 `(iss, sub)` 經 `adapters/auth.ExternalCredentials` 解成 account 的 principal（`Subject` = account id、roles 取自 account、不看 token claim；停用 / `iat` < `sessions_revoked_at` / DB 錯誤都拒絕且不 cache），未綁定的身分只能呼叫 `ClaimInvite`（其餘 `PERMISSION_DENIED` `auth.unbound_identity`），有 `X-API-Key` 時不看 Bearer——`docs/identity.md`「外部身分綁定」；key 通過驗證會節流、best-effort 地寫 `last_used_at`（`auth.last_used_interval`，預設 1m，寫入失敗不影響驗證）；`DisableAccount` / `AssignRoles` / `RevokeApiKey` 有 last-admin guard：會讓最後一個有效 admin（ACTIVE、有 `auth.admin_role`、至少一把未撤銷未過期 key）失格就回 `identity.last_admin`（`FAILED_PRECONDITION`），同一 transaction 內先 `port.AdminCensus.LockAdmins`（Postgres `FOR UPDATE` 鎖 admin 列、再另一條 statement 算 key；memory 靠 Transactor 串行）再載入 aggregate，「現在」也在拿到鎖之後才讀（`LockAdmins` 回傳，等鎖期間過期的 key 不算），config 的 static key 不算——`docs/identity.md`）
                 catalog 是第四個：商品價格的 server 端來源（`Product` = sku / name / price（`kernel.Money`，每個 product 各自幣別）/ active），沒有 event；`UpsertProduct` / `DeactivateProduct` 要 admin role（同 `SetStock`），`QuotePrices` 批次報價分得出不存在與已下架——**ordering 用它定價**：`CreateOrder` 只收 sku / quantity（`CreateOrderItem`，`unit_price` 已 reserved），`ordering/adapters/pricing` 經 catalog 的 contract 查價（#320：rpc **不載入 catalog**，`wire.go` 把 `PriceQuoter` 綁到 `pkgs/catalog/adapters/client` 的 Connect client，問另一個 process `apps/catalog-rpc`；`catalog.upstream` / `APP_CATALOG_API_KEY` / `catalog.timeout` 見 `docs/config.md`），寫成訂單明細的快照；sku 不存在 / 已下架 → `order.unknown_sku` / `order.inactive_sku`，兩種幣別 → `order.mixed_currency`；inventory 的 `SetStock` 也問它（`stock.unknown_sku` / `catalog.unavailable`，已下架放行）；catalog 沒答覆（連不上、逾時、5xx、service key 被拒）→ `UNAVAILABLE` + `catalog.unavailable`，沒寫入；同 key 重送不問 catalog；rpc 的 `readyz` 不依賴 catalog——`docs/catalog.md`
                 payment 是第五個（#367 切片 2、3A、3B，`docs/payment.md`）：訂單的單一付款單（`Payment` aggregate 帶完整狀態機：PENDING／CAPTURED／EXPIRED／CANCELLED／REFUNDING／REFUNDED／NEEDS_REVIEW，每個「狀態 × 事件」格都是一個 domain 方法，`domain/payment` 表驅動 100% 覆蓋），經 provider 的 hosted checkout 收款（卡片資料不進系統）；`PaymentProvider` port 第一版只有 `infra/provider/mock`。**`payment-rpc` 三個子命令**：`serve`（`StartPayment`／`GetPayment`＋ webhook route `POST /api/payment/webhook`，簽章驗原始 body、不認 API key，驗過才寫 `payment.webhook_events` 與 `WebhookReceived`）、`consume`（訂閱 ordering 的 `OrderConfirmed`／`OrderCancelled`／`OrderCompleted` 與 payment 自己的 `PaymentRefundRequested`／`WebhookReceived`，自己的 router 與 `payment.poison`，**不進 `apps/worker`**；#410 切片 2 起它是獨立 binary `apps/payment-worker`，`payment-rpc` 只剩 `serve`／`expire`）、`expire`（一輪 sweep：逾時 `PENDING` 先問 provider 再決定、`EXPIRED`／`CANCELLED` 在 `payment.reconcile_window` 內補查、順手刪逾期且已處理的 webhook 通知）。發布 `PaymentCaptured`／`PaymentExpired`／`PaymentRefunded`；ordering 在 `apps/worker` 訂閱前兩個（切片 3B，只 import payment 的 `contract/v1`，依訂單的 `payment_required` 快照與狀態決定 `PAID`→`COMPLETED`／取消／忽略／拒絕，不看目前的開關）。`worker poison`／`worker prune`／admin RPC 的 BC 清單與 worker 的 retention／poison gauge 都含 payment（schema 名用 `paymentv1.Schema`，不載入 payment 的 domain）；payment 的表不存在時 worker 拒絕啟動。caller 永遠只有 `apps/rpc`（service key `rpc`），所以 request 自帶 `customer_id` 與 `admin`，payment-rpc 相信它們（前提是請求通過了 auth interceptor）；非本人 `NOT_FOUND`、admin 可讀、admin 不可代付（`payment.permission_denied`）。provider 的錯誤文字不可信，任何 use case 都不轉述它（只回固定的 `port.ErrProviderUnavailable`）
  provider.go      wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorate() / decorateTx() 包起來
  contract/v1/     buf 生成的 wire format（pb + Connect + openapi.yaml），只有 adapters / apps 可 import
  domain/<agg>/    aggregate、VO、Repository interface、domain errors（手寫，不是 proto）
  application/     dto（手寫 application 邊界）、usecase（orchestration）、port、mapper
  infra/           driven：memory / postgres（gorm row model + embed migrations）/ system（clock、id）
  adapters/        driving：rpc（Connect handler，pb ↔ dto）、consumer（broker handler）
apps/rpc         composition root：main + wire.go，無業務、無 mapping；不載入 catalog（問 catalog-rpc，見下）；`rpc --wait-catalog[=60s]` 是 Helm initContainer 用的盡力等待（等不到只 WARN、exit 0，不是硬依賴）
apps/catalog-rpc  composition root：只掛 catalog 的 rpc（同 apps/rpc 裁成一個 BC）；沒有 identity，不接 CredentialResolver，認證只認 `auth.api_keys`（`auth.jwt` / `auth.oidc.enabled` 拒啟動）；本機 `task catalog-rpc` 讀自己的 `configs/catalog-rpc.yaml`（gitignored，從 `catalog-rpc.example.yaml` 複製；不讀 `local.yaml`）；`--require-durable`（Helm 以 container arg 傳）在 `postgres.url` 空、`auth.mode` 不是 required、或 `auth.api_keys` 不是恰好一個名為 `rpc` 的 key 時拒啟動；schema 歸 apps/migrate
apps/payment-rpc  composition root：只掛 payment 的 rpc（同 catalog-rpc 的形狀：不接 identity，認證只認 `auth.api_keys`，`auth.jwt`／`auth.oidc.enabled` 拒啟動；`--require-durable` 另要求 `postgres.url`、`auth.mode: required`、`auth.api_keys` 恰好一把名為 `rpc`）；子命令 `serve`／`consume`／`expire`（都可帶 `--require-durable`；`consume`、`expire` 沒有 listener 也沒有 auth，只要求 `postgres.url`，且 `consume` 要 ordering 的事件表已 migrate）；provider 由 `payment.provider` 在 `run` 裡先建好再交給 injector（`Initialize(cfg, provider)`），`payment.reconcile_window` 必須大於 provider 的 checkout 有效期，否則拒啟動；`/api/readyz` 只看 Postgres、不看 provider；本機 `task payment-rpc` 讀自己的 `configs/payment-rpc.yaml`
apps/payment-worker  composition root：payment 的事件 consumer（#410；切片 2 起 chart 的 `payment-consume` Deployment 跑它，`payment-rpc consume` 已移除，舊入口以非零 exit 指向本 binary）。`Consumer`、consumer group（`ConsumerGroup(cfg)` = `<worker.consumer_group>.payment`，不得改名）、`--require-durable` 的檢查都在 `pkgs/payment`（`consume.go`），兩個入口共用同一份、各自只給入口名（`payment-worker` / `payment-rpc consume`，用在拒絕訊息與 log）；wire 圖沒有 server，所以沒有 listener、auth、identity，也沒有 credential 來源要拒絕。唯一引數是 `--require-durable`（其他任何引數拒啟動）；拒啟動：`postgres.url` 空白（有無旗標都拒）、`worker.broker` 不是 postgres、`payment.*` 不合法（`BuildProvider`），加旗標時 mock provider 的 `payment.mock_webhook_secret` 空白。**會**帶 provider 憑證（`apps/worker` 才拒絕）。`service.name` 預設是執行檔名 `payment-worker`；本機 `task payment-worker` 讀 `configs/payment-rpc.yaml`；image / archive 與其他 app 一起發（非 root、唯讀 rootfs，`scripts/image-smoke.sh payment-worker`）；chart 的 `payment-consume` 跑它（切片 2）
apps/worker      事件 consumer：每個 BC 的 Subscriptions 掛同一個 Watermill router，各 BC 自己的 consumer group / poison；
                 worker.broker=redis 時自己當 forwarder 把 outbox 表轉進 Redis Streams 再讀（outbox 仍是寫入路徑）；
                 `worker poison list|show|requeue|drop` 操作各 BC 的 poison 表（requeue = 重新 insert 回原 topic 的 outbox 表）；
                 `worker prune --older-than <d>` 是 outbox / inbox 的 retention（poison 表不清）；`worker expire --older-than <d>` 是 inventory 的 reservation TTL（adapters/cli，第二個 wire injector）；`worker bootstrap` 是 identity 的第一個 account + key（adapters/cli，第三個 injector；identity 沒 event 所以不在 router 裡）；
                 有 DB 時 `eventbus.Lag` 另報每張 outbox 表 / inbox 的大小與最老一列年齡（`eventbus_outbox_*` / `eventbus_inbox_*`），alert 用它抓 prune 沒跑與 stale group；各 BC 的 `adapters/metrics` 另報 `ordering_orders{status}` / `inventory_reservations{status}` 與各 status 最老一筆的年齡（saga 漏斗，PENDING 太老有 alert）
apps/migrate     收集各 BC 的 Migration()，唯一能 import infra 的 app
deployments/helm Helm chart：rpc / worker Deployment、選配 payment-rpc（`paymentRpc.enabled` 與 `config.ordering.require_payment` 從 chart 0.3.53 起**預設 on**，全新安裝沒給 auth 與 payment secrets 會被拒絕並指名兩條出路，要關得兩個值都設 false：serve／consume Deployment ＋ expire CronJob，自己的 ConfigMap／Secret，只對外開 webhook 的 ingress ＋ ingress-nginx 限流）、migrate pre-install/pre-upgrade hook Job、prune CronJob、console（nginx，/api proxy 到 rpc Service，`console.enabled` 預設 off）、
                 選配 rpc ServiceMonitor 與 ingress-only NetworkPolicy（rpc 永遠放行 console；`ingressFrom` 空 = 任何來源）；terminationGracePeriodSeconds
                 從 rpc.shutdown / worker 的 drain 算出來（改 shutdown 行為要一起改 values 註解與 check.py）；CI 的 helm-kind job 在 kind 真裝 + smoke
```

**depguard（`.golangci.yml`）強制的 import 方向**：

| 套件 | 可以 import | 不可以 |
|---|---|---|
| domain | kernel | 其他全部 |
| application | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| infra | domain、application/port、platform | adapters / contract |
| adapters | application、contract、platform、其他 BC 的 `contract/` | 其他 BC 的 domain / application / infra / adapters |
| kernel、platform | 標準庫、第三方 | 任何 BC |
| apps/* | 各 BC 的 `provider.go`、platform | usecase（只透過 wire）、infra |
| 任何 BC 的 domain / application / infra | — | 其他 BC 的任何東西（走 port 或 event） |

**archtest（`pkgs/platform/archtest`，每個 BC 的 `arch_test.go`）強制的 DDD 形狀**：domain struct 欄位 private、
無 `Set*`、每個 struct 有 constructor（`*Params` 除外）、不呼叫 `time.Now()`、不 log；`*Repository` interface
只能在 domain；usecase 只有 `Execute(c, in) (out, error)` 一個公開方法；dto 無方法、不 import domain / proto；
port 只有 interface；adapter 持有 `usecasex.UseCase[I,O]`，不持有 `*usecase.X`。

幾個容易踩的設計點：

- **contract ≠ dto**：contract 是生成的 wire format，dto 是手寫的 application 邊界，只放 usecase 需要的欄位；
  同一個 usecase 被 rpc 和 consumer 共用時 dto 是共同語言。不要照 pb 抄 dto。
- **decorator 只在 `provider.go` 加**：`decorate()` = metrics + logging，給讀取；`decorateTx()` 再多 `usecasex.Transactional` + 最內層的 `usecasex.Idempotent`（consumer inbox：
  `<bc>.inbox` 表記 message id，重送在 usecase 之前以 `message.duplicate` drop；rpc path 沒 message id、沒 DB 時 inbox 是 nil，都是 pass-through），給寫入。
  rpc 與 consumer 自動都有。**usecase 不自己開 transaction**，也不持有 `port.Transactor`；直接用收到的 `c` 呼叫 repo / publisher，
  它們從 context 拿到 tx。唯一例外是 ordering 的 `CreateOrder`（#320）：它持有 `port.Transactor`，只用它包 `Save` + `Publish`，
  因為寫入前要先問 catalog（查價），不能佔著 DB 連線等；它走 `decorateNoTx()`（metrics + logging + Idempotent，無 `Transactional`）。
  限 rpc 路徑的 `CreateOrder`，其他寫入 usecase 與 consumer inbox 的 atomicity 規則不變。
  **第二個例外（payment，#367 切片 2）**：`StartPayment` 同樣先呼叫外部服務（provider 開 hosted checkout），不能佔著 DB 連線等：usecase 持有 `port.Transactor`，只包住寫入，provider 呼叫永遠在 transaction 之外，走 `decorateNoTx()`（payment 的版本只有 metrics + logging，沒有 inbox）。範圍：`StartPayment`、`ProcessRefund`（退款呼叫）、`ExpireSweep`（provider 查詢），三者都只用 `port.Transactor` 包住寫入；consume 路徑的 `RecordOrderConfirmed`／`RecordOrderEnded`／`ProcessWebhook` 是一般的 `decorateTx`（交易＋inbox），其中取消 provider checkout 是交易 **commit 之後**由一個 decorator 做的、best effort 的呼叫（provider.go 的 `cancelCheckoutAfterCommit`）；其他 payment usecase 仍照 `decorate()`。provider 的錯誤文字不可信，usecase 不轉述它（只回固定的 `port.ErrProviderUnavailable`）。
  inventory 的 `SetStock`（#326）同樣要先問 catalog，但**不是**例外：usecase 沒變、不持有 `Transactor`，檢查是 `provider.go` 組在 `Transactional` 外面的 decorator（`requireProduct`，Metrics / Logging 之內），所以 catalog 的拒絕也被計到、檢查時不佔連線。
- **Domain error** 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 Connect code，
  `Code` 放 `x-error-code` header。consumer 把 domain error 視為 drop（不重試），infra error 才 retry——所以「業務上的失敗結果」
  （例如 inventory 缺貨）要做成 aggregate 狀態 + event，不能做成 error；反過來 optimistic-lock 的 `stock.ErrStale` / `reservation.ErrStale` / `order.ErrStale` 刻意不是 `kernel.Error`：
  它們 wrap `kernel.ErrConcurrentUpdate`，consumer 會 retry，rpc 回 `Aborted`（`x-error-code: concurrent_update`），metrics 記 `kind=aborted`。
- **Optimistic lock**：aggregate 帶 `version`，`Save` 比對後 +1。存完後記憶體中的 aggregate 版本**不會**前進，要再存一次請先 reload。
- **CreateOrder 冪等**：`CreateOrderRequest.idempotency_key`（選填，1–64 個可見 ASCII）。範圍是 `unique(customer_id, key)`，customer 是 `ownership.ForCreate` 解析後的（admin 代客 = 被代下單的 customer）；
  key + `request_hash`（`order.FingerprintV2(lines)` = `"v2:"` + sha256(sku, quantity)，**不正規化**：順序、重複 SKU 都算內容；價格是 catalog 的，不在請求裡。沒有 `v2:` 前綴的是 v1 指紋〔含單價〕，`IsReplayOf` 用該訂單自己的快照價重算比對）存在 `ordering.orders`（migration `0008`，partial unique index），與訂單同壽、不另設 TTL。
  同 key 同內容 → 回原訂單**目前**狀態、不寫不發 event、不問 catalog；同 key 不同內容 → `ALREADY_EXISTS` / `order.idempotency_key_reused`（訊息固定，不帶任何資料）。
  併發靠 `INSERT … ON CONFLICT (customer_id, idempotency_key) DO NOTHING`（不 abort tx、不靠鎖）：`Save` 回 `ErrIdempotencyConflict`，use case 在**同一個 tx** 回讀勝出者；衝突路徑在 `Publish` 之前返回，所以 outbox 不會多一列。key 值不進 log / error / metric / span（R4）。空 key = 舊行為。
  結果計入 `ordering_create_order_total{result=created|replayed|key_reused}`（`ordering/adapters/metrics`，rpc 註冊，在 `ProvideCreateOrder` 的最外層 decorator，所以在 commit 之後數；`OrderOutput.Replayed` 是 use case 告訴它「這是重放」的唯一管道；label 只有 `result`；其他失敗刻意不計，看 `usecase_duration_seconds{kind}`）。
- **Order history（#427，`docs/order-history.md`）**：`ordering.order_history` 是只追加的轉換紀錄（誰／何時／from→to／cause）。寫入是結構性的：`ProvideEventPublisher` 把 outbox publisher 包進 `adapters/events.Recorder`，每個 publish 的 order event 在同一個 transaction 裡記一列，use case 不自己記。`from`／`to`／`cause` 跟著 domain event 走（`Cancel(now, cause)` 的來源由 adapter 在 `dto.CancelOrderInput.Source` 明說，零值被拒絕）；actor 由 `Recorder` 從 principal 取。cause 是固定列舉，`OrderCancellationRequested.reason` 的字串不進 history 也不進 log。`worker prune` 不碰這張表；migration 不加指向 `orders` 的 FK。
- **Validation**：request 形狀由 protovalidate interceptor 擋（`x-error-code: request.validation_failed` + `Violations` detail），
  consumer path 沒有它，所以 domain 的檢查不能拿掉。
- **Auth 只做 authentication**：`auth.mode` 預設 `off`（`task run`、E2E、console 都沒帶 credential）；開了之後 `server.NewAuthInterceptor`
  把 `X-API-Key`（config `auth.api_keys` 先、再問 `server.CredentialResolver` = identity 的 DB key；resolver 失敗是拒絕不是放行）/ `Authorization: Bearer <jwt>` 解析成 `contextx.Principal` 放進 context（identity key 的 `Subject` 是 account id、`CredentialID` 是 key id），usecase 用 `contextx.PrincipalFrom(c)` 讀，
  consumer path 永遠是 anonymous。`rpc` access line 的 `auth` / `sub` 與 span 的 `enduser.id` 由 auth interceptor 透過 logging interceptor 留在 context 的 slot 回報（`recordAuth`），不要把 principal 塞進 metrics label——它記的 `auth_decisions_total{result, method}` 只有 ok / public / anonymous / missing / invalid × api_key / jwt / none（Grafana 的 Auth row 與 `rpc-auth` alert 讀它；OIDC 的 `method` 仍是 `jwt`，未綁定放行 `ClaimInvite` 記 `ok`，未綁定被擋記 `invalid`，不加新值）。要不要做授權（role 檢查）是讀的人的事，不要在 interceptor 裡加；ordering 的 ownership 就在 application 層（`pkgs/ordering/application/ownership.Policy`，admin role 由 `auth.admin_role` 注入）：`CreateOrder` 的 customer 取自 principal（admin 可代客、anonymous 必填）、`ListOrders` 非 admin 只列自己的、`GetOrder` / `CancelOrder` / `CompleteOrder` 非 admin 動到別人的訂單都給 `NOT_FOUND`（Cancel / Complete 走 `FindByIDForCustomer`，狀態不變、不發 event；admin 與 anonymous 不受限，consumer 的 `OrderCancellationRequested` 沒有 principal 所以仍可取消任何訂單；`CompleteOrder` 的狀態語意沒改）。
  Interceptor chain 由外而內：`tracing, meta, logging, [rate limit], [auth], [timeout], validate`——rate limit 在 auth 之外（洪水先擋再驗簽章）、
  timeout 在 auth 之內（deadline 算 validate + usecase）；`http.timeout` 到期的失敗一律 `DEADLINE_EXCEEDED` + `request.timeout`，超過 `http.rate_limit` 是 `RESOURCE_EXHAUSTED` + `request.rate_limited` + `Retry-After`。
  所有 interceptor 都是 unary-only：哪天加了 streaming RPC，auth 要另外用 `WrapStreamingHandler` 包，不然 stream 沒有驗。
- **Graceful shutdown 是有測試保護的行為**：rpc 收到 SIGTERM 先把 `/api/readyz` 翻 503 再服務 `http.shutdown_delay`（預設 0），然後關 listener、
  in-flight request 有 `http.shutdown_timeout`；worker 的 `eventbus.Register` 把 subscriber 包進 `eventbus.Draining`，正在跑的 handler 跑完 + ack 才結束
  （上限 8s）。每次 handler attempt 另有 `worker.handler_timeout`（預設 10s，0 = 不限）：到期的 attempt 照常 retry，用完進 poison。改 `Server.Serve` / `Draining` 請一起跑 `platform/server` 與 `platform/eventbus` 的 shutdown tests。
- **Response meta 走 header**（`X-Request-Id`、`X-Trace-Id`、`traceparent`），不包進 proto message。
- **Persistence**：每個 BC 一個 Postgres schema（`ordering.orders`、`ordering.schema_migrations`）；SQL 放
  `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql` 由 BC embed（go.work 下 embed 不能跨 module）。
  `postgres.url` 空 → `*gorm.DB` 為 nil → `ProvideOrderRepository` 改用 in-memory，所以 `task run` 與 E2E test 不需要 DB。
- **`wire.go` 是 AppConfig → component config 的唯一 mapping 點**。

## Config

`platform/config.Load()`：defaults → YAML（`$APP_CONFIG`，否則 `./configs/config.yaml`）→ `APP_*` env，由低到高。
`configs/local.yaml` gitignored，`task run` / `task migrate` 第一次會從 `config.example.yaml` 複製。`task catalog-rpc` 另有 `configs/catalog-rpc.yaml`（從 `catalog-rpc.example.yaml` 複製，`CATALOG_CONFIG=<file>` 換檔）。
注意：viper 忽略空 env，`APP_POSTGRES_URL=` 清不掉 key，要「不接 DB」請改 local.yaml。完整 key 表見 `docs/config.md`。
`auth.*`（mode / api_keys / jwt / public）見 `docs/config.md`。

## 新增東西的步驟

**新增 RPC**：改 `protos/<bc>/v1/service.proto`（request 欄位規則用 `buf.validate` 寫在 proto，interceptor 會擋；domain 仍保留不變量）→ `task gen`（`task breaking` 檢查相容）→ domain 加行為 →
`application/dto` + `usecase` → `provider.go` 加 `ProvideXxx` 並用 `decorate()`（讀）或 `decorateTx()`（寫）包 → `adapters/rpc` 實作
（`var _ XxxServiceHandler` 會提醒）→ `task wire`。

**新增 Bounded Context**（照 `pkgs/inventory` 抄）：`protos/<newbc>/v1/{entity,service,event}.proto` + `openapi.base.yaml` 與
`buf.gen.yaml` 的 `go_package` override → `pkgs/<newbc>/go.mod`（replace 指向 kernel / platform / 要讀 contract 的 BC）+ `go.work`
加 `use`（`MODULES` 由它導出，不用另外加）+ `Taskfile.yml` 的 `BCS` + `apps/*/go.mod` 的 require / replace → `task tidy` → `contract/v1/openapi.go` →
`task gen` → 複製 `pkgs/ordering` 結構寫 `provider.go`（`Subscriptions` 自己的 named type）→ `.golangci.yml` 加該 BC 的 layer block 與
`bc-isolation-<bc>`，並把它加進其他 BC / shared / apps 的 deny（`pkg` 是 prefix match，不能用 `*`）→ `apps/rpc` 的 `NewRoutes` /
`NewDocs` / `NewPoisonService` 的 schema 清單加它、`apps/{rpc,worker}/wire.go` 加 `ProviderSet`、`apps/worker` 的 `NewWorker` 收它的 `Subscriptions`、`poison.go` 的 `schemas` 加它（沒有 event 的 BC 如 identity：router / poison 都不加，只有 operator 子命令才進 `apps/worker` 的獨立 injector；payment 是唯一例外，Subscriptions 掛在自己的 composition root `apps/payment-worker`〔與切片 2 前的 `payment-rpc consume`〕，因為 provider 憑證不能進 `apps/worker`）→ `task wire` →
有 DB 則加 migrations（含自己 topic 與 poison 的 events 表、`<bc>.inbox` 表）與 `apps/migrate/main.go` 的 `migrations`；`application/port` 加自己的 `Inbox` 與 `provider.go` 的 `ProvideInbox`（每個 BC 自己的型別，wire 才分得開）。

## Conventions（本 repo 特有）

- goimports local-prefix：`github.com/blackhorseya/x-monorepo`
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`；interface 參數 `c context.Context`，
  實作內 `ctx := contextx.FromContext(c)`
- PR title 遵循 Conventional Commits
- **Review contracts**（`docs/workflow.md#review-contracts`，PR template 有 checkbox）：proto 變更的驗證回報要明列 Go / OpenAPI / Wire / **TS** 的結果（`task check-gen-fresh` 不含 TS，用 `task check-gen-fresh-all`）；宣稱 migration 線上可用要用完整交易驗鎖；認證 generation／取消測試 reload 後同時驗 cookie 與 server session；公開 mutation 設計要有寫入量與清理上界的資源表
- **RED commit 要失敗在「目標」**：行為變更先提交失敗的測試，但「指令 exit 非零」不等於缺陷重現——typo、缺依賴、
  fixture 建立失敗、上游 use case 先拒絕都會讓新測試變紅。
  - behavioral fix（修 regression、改行為）：RED 必須失敗在**目標 assertion**（預期的 assertion 訊息）。
    setup / fixture 失敗、上游先拒絕、compile failure 都不算這個行為的 RED。
  - 新增 API / 型別的 feature：可以接受 compile-fail 型 RED，但要標明失敗類型（它只證明缺 API），
    並另有 assertion 型測試在 GREEN 驗證行為、能拒絕錯誤的實作。不為了湊 assertion 對新 API 造無意義的測試。
  - **RED evidence**（PR 描述或回報）：immutable SHA（完整 40 字元）、完整指令、target test、失敗類型
    （`compile` / `runtime assertion` / `setup`）、預期的 assertion 訊息、實際失敗位置（`file:line` 與輸出）、
    fixture 已成功的證據（失敗訊息在 seed 之後，或同 fixture 的另一個測試通過）、對應的 GREEN SHA。
  - 純文件、註解、無可測行為的預設值變更，以及沒有可測邏輯的 CI 修正，免 RED。已凍結的 RED 歷史不重寫。
  - `scripts/red-stage.sh -m '<預期訊息 regex>'` 讀 `go test` 輸出，分類成 `assertion` / `setup` / `compile` / `panic` / `pass`
    （只有 `assertion` exit 0）。它只是候選分類：訊息有印出不代表那是對的 assertion，語意由 reviewer 判。
    例（在固定 RED SHA 的 named worktree 裡跑，不要切換自己綁定 worktree 的 HEAD）：`cd <RED SHA 的 named worktree> && go test -run '^TestX$' ./pkgs/ordering/... 2>&1 | scripts/red-stage.sh -m 'want 1 call'`
