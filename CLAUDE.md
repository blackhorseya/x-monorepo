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
task release-snapshot      # goreleaser dry run → bin/dist（真的 release 由 main 上的 CI 綠燈自動觸發，不手動打 tag；沒改到會進 artifact 的檔案就被 gate 跳過，task release-gate 先看：列 releasable 檔 + 預測版號；五個 image 與 checksums.txt 都由 cosign keyless 簽名、archive 另有 GitHub artifact attestation，發布後 verify job 跑 scripts/release-verify.sh 全驗一遍；snapshot 以 --skip=sign 跳過；CI 的 release-snapshot 另跑 scripts/rekor-v2-canary.sh——cosign 預設 signing config 出現非 v1 的 Rekor 就紅（#204 的 409 處理只適用 v1，見 #215），.github/workflows/rekor-canary.yml 每週再跑一次，紅了不要 rerun release）
task poison -- list        # 看各 BC 的 poison 表；requeue <bc> <offset>…|all 送回原 topic、drop 刪掉（worker poison 子命令）
task expire -- --older-than 24h --dry-run   # reservation TTL：RESERVED 超過的還庫存 + 發 OrderCancellationRequested（worker expire；Helm expire CronJob 每 15 分鐘；rpc 版 InventoryService.ExpireReservations 給 console Expire 頁，要 admin role；`InventoryService.SetStock` 同樣要 admin role，auth 開著時非 admin 是 `PERMISSION_DENIED`）
task prune -- --older-than 720h --dry-run   # 刪每個 group 都 ack 過的 outbox 列 + 消費過的 inbox 列（worker prune；先 dry-run；NOTE 列出每個落後的 group 與最老未讀年齡：held back by = 過了 retention、behind = 還沒，--forget-group 刪掉不會再跑的）
scripts/dex-token.sh [email] [--id-token]    # 本機 Dex（compose 的 `dex` 服務；kind 的 dex.yaml）：authorization code + PKCE 登入拿一顆真 JWT；issuer / 帳號 / 離線 /etc/hosts 見 docs/operations.md「Dex dev IdP」
task bootstrap                              # identity 第一個 account + API key（空 store 才建；worker bootstrap；secret 只印一次）
task helm-check            # deployments/helm/x-monorepo：每個 ci/*.yaml profile 跑 helm lint + kubeconform + check.py（derived 數字：grace period、hook 順序、prune args、console 的 RPC_UPSTREAM、ServiceMonitor / NetworkPolicy 的選擇器）
task k6-wire               # tests/k6/wire.sh：四支腳本打一個回壞掉 HTTP response（status code / version / header 行帶著請求的 X-API-Key、body 被截斷、連線直接關）的假 server；每個 run 必須失敗，且 key 不在 k6 輸出、summary、metrics tag 任何地方（不需要 Postgres；CI 的 k6-auth job）
task k6-bench              # tests/k6/bench.sh：效能 baseline（CreateOrder 吞吐 / 延遲、下單 → RESERVED，N≥3 次、每次獨立資料庫）；結果與重跑方法見 docs/perf-baseline.md；不進 CI（乾淨 runner 上用 `workflow_dispatch` 的 k6-bench workflow，不寫回 baseline）
task k6-smoke              # tests/k6：orders（load thresholds）/ ratelimit（429 + Retry-After）/ timeout（1ns → 全部 DEADLINE_EXCEEDED）/ reservation（rpc + worker，下單 → RESERVED 延遲）各自起一個 bin/rpc 打；task k6 -- <script> 打現成 server
task helm-kind             # 真裝：kind cluster + deployments/helm/kind（拋棄式 Postgres / Redis）+ tree build 的 image（含 console：web-build → web/apps/console/Dockerfile），install → smoke → upgrade → smoke；helm-kind-down 刪 cluster
task helm-kind-keda        # 接著在同一個 cluster：KEDA + OTLP collector + Prometheus，k6 灌訂單看 worker 真的靠 eventbus_consumer_lag scale out / in
task run                   # build 後以 configs/local.yaml 跑 ./bin/rpc（Connect + gRPC + gRPC-Web 同 port；RPC 在 /api/<svc>/<Method> 與原生路徑都通，/api/docs 開）
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
`task web-e2e`（Playwright：`web/apps/console/e2e`，起 `bin/rpc` + vite preview 真打；`E2E_DATABASE_URL` 有設就先 `migrate up` 走 Postgres 並由 `e2e/worker.setup.ts` 多起一個 `bin/worker`（CI；`reservation.spec.ts` 驗 outbox → worker → RESERVED / RELEASED 與 Complete → COMMITTED，`reservations.spec.ts` 驗 /reservations 的 status filter 與 `?order=` pin），沒設是 in-memory，spec 用 `e2e/mode.ts` 的 `withDB` 分支；project `auth`（`e2e/auth/`）另起一個 `auth.mode=required` 的 rpc + preview 驗 credential 流程（API key 與 `jwt.ts` 現場簽的 HS256 JWT；`helpers.ts` / `jwt.ts` 不是 spec，project 的 testMatch 只收 `*.spec.ts`）；locator 用 `getByLabel` / `getByRole`，第一次先 `pnpm exec playwright install chromium`）；`task web-e2e-image` 把 chromium project 改打真的 console image（`e2e/image.sh`，`E2E_CONSOLE_URL` 取代 vite preview；`image.spec.ts` 只在這模式跑，驗 nginx 的 fallback / cache header / proxy）。
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

CI（`.github/workflows/ci.yml`）另外有 `work-sync`（go.mod 不得漂移，加依賴後 `task tidy`）、
`cover-gate`（所有 `*/domain/*` package 必須 100% 覆蓋）、`buf breaking`（只警告）。PR 上每個 job 只在輸入有變時跑（`scripts/ci-changes.sh` 決定；main 一律全跑），所以一支 PR 沒跑 helm-kind / k6 是正常的，不是壞掉；改 `scripts/ci-changes.sh` 的對照表要一起改 `scripts/test.sh`（`task scripts-test`）。

## 架構：誰依賴誰

```
pkgs/kernel      shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
pkgs/platform    技術共用：contextx、config、server（gin + Connect）、usecasex、otel、postgres、archtest；
                 admin（admin.v1.AdminService：poison 操作 + Prune RPC，與 worker 的 CLI 同一組 eventbus 函式；contract 在 protos/admin，不屬於任何 BC）
pkgs/<bc>        一個 bounded context = 完整六角形（示範 BC：ordering；inventory 是第二個，消費 ordering 的 event，ordering 再消費它的 `StockReserved` 把 order 變 CONFIRMED；`CompleteOrder` RPC 發 `OrderCompleted`，inventory 消費它把 reservation COMMITTED、扣 on_hand；`worker expire` 把 RESERVED 超過 TTL 的變 EXPIRED、還庫存、發 `OrderCancellationRequested`；sweep 與 `CompleteOrder` 競態時，`OrderCompleted` 遇 EXPIRED 會從 available 補扣成 COMMITTED，不夠就 `COMMIT_SHORT`（終態、不動庫存、alert `x-monorepo-commit-short`，營運手動處理）；
                 identity 是第三個：DB 管理的 account / API key，沒有 event；`adapters/auth` 實作 platform 的 `server.CredentialResolver`，auth interceptor 對 `X-API-Key` 先查 config `auth.api_keys` 再問它（TTL cache 30s）；`auth.oidc.enabled` 時 `Authorization: Bearer` 先交 `platform/oidc` verifier（`iss` 不認識才走 `auth.jwt`，其他失敗終局），驗過的 `(iss, sub)` 經 `adapters/auth.ExternalCredentials` 解成 account 的 principal（`Subject` = account id、roles 取自 account、不看 token claim；停用 / `iat` < `sessions_revoked_at` / DB 錯誤都拒絕且不 cache），未綁定的身分只能呼叫 `ClaimInvite`（其餘 `PERMISSION_DENIED` `auth.unbound_identity`），有 `X-API-Key` 時不看 Bearer——`docs/identity.md`「外部身分綁定」；key 通過驗證會節流、best-effort 地寫 `last_used_at`（`auth.last_used_interval`，預設 1m，寫入失敗不影響驗證）；`DisableAccount` / `AssignRoles` / `RevokeApiKey` 有 last-admin guard：會讓最後一個有效 admin（ACTIVE、有 `auth.admin_role`、至少一把未撤銷未過期 key）失格就回 `identity.last_admin`（`FAILED_PRECONDITION`），同一 transaction 內先 `port.AdminCensus.LockAdmins`（Postgres `FOR UPDATE` 鎖 admin 列、再另一條 statement 算 key；memory 靠 Transactor 串行）再載入 aggregate，「現在」也在拿到鎖之後才讀（`LockAdmins` 回傳，等鎖期間過期的 key 不算），config 的 static key 不算——`docs/identity.md`）
                 catalog 是第四個：商品價格的 server 端來源（`Product` = sku / name / price（`kernel.Money`，每個 product 各自幣別）/ active），沒有 event；`UpsertProduct` / `DeactivateProduct` 要 admin role（同 `SetStock`），`QuotePrices` 批次報價分得出不存在與已下架——**ordering 用它定價**：`CreateOrder` 只收 sku / quantity（`CreateOrderItem`，`unit_price` 已 reserved），`ordering/adapters/pricing` 經 catalog 的 contract（`CatalogServiceHandler.QuotePrices`，apps/rpc `wire.Bind` 到 catalog 自己的 service）查價，寫成訂單明細的快照；sku 不存在 / 已下架 → `order.unknown_sku` / `order.inactive_sku`，兩種幣別 → `order.mixed_currency`——`docs/catalog.md`
  provider.go      wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorate() / decorateTx() 包起來
  contract/v1/     buf 生成的 wire format（pb + Connect + openapi.yaml），只有 adapters / apps 可 import
  domain/<agg>/    aggregate、VO、Repository interface、domain errors（手寫，不是 proto）
  application/     dto（手寫 application 邊界）、usecase（orchestration）、port、mapper
  infra/           driven：memory / postgres（gorm row model + embed migrations）/ system（clock、id）
  adapters/        driving：rpc（Connect handler，pb ↔ dto）、consumer（broker handler）
apps/rpc         composition root：main + wire.go，無業務、無 mapping
apps/catalog-rpc  composition root：只掛 catalog 的 rpc（同 apps/rpc 裁成一個 BC）；沒有 identity，不接 CredentialResolver，認證只認 `auth.api_keys`（`auth.jwt` / `auth.oidc.enabled` 拒啟動）；`--require-durable`（Helm 以 container arg 傳）在 `postgres.url` 空、`auth.mode` 不是 required、或 `auth.api_keys` 不是恰好一個名為 `rpc` 的 key 時拒啟動；schema 歸 apps/migrate
apps/worker      事件 consumer：每個 BC 的 Subscriptions 掛同一個 Watermill router，各 BC 自己的 consumer group / poison；
                 worker.broker=redis 時自己當 forwarder 把 outbox 表轉進 Redis Streams 再讀（outbox 仍是寫入路徑）；
                 `worker poison list|show|requeue|drop` 操作各 BC 的 poison 表（requeue = 重新 insert 回原 topic 的 outbox 表）；
                 `worker prune --older-than <d>` 是 outbox / inbox 的 retention（poison 表不清）；`worker expire --older-than <d>` 是 inventory 的 reservation TTL（adapters/cli，第二個 wire injector）；`worker bootstrap` 是 identity 的第一個 account + key（adapters/cli，第三個 injector；identity 沒 event 所以不在 router 裡）；
                 有 DB 時 `eventbus.Lag` 另報每張 outbox 表 / inbox 的大小與最老一列年齡（`eventbus_outbox_*` / `eventbus_inbox_*`），alert 用它抓 prune 沒跑與 stale group；各 BC 的 `adapters/metrics` 另報 `ordering_orders{status}` / `inventory_reservations{status}` 與各 status 最老一筆的年齡（saga 漏斗，PENDING 太老有 alert）
apps/migrate     收集各 BC 的 Migration()，唯一能 import infra 的 app
deployments/helm Helm chart：rpc / worker Deployment、migrate pre-install/pre-upgrade hook Job、prune CronJob、console（nginx，/api proxy 到 rpc Service，`console.enabled` 預設 off）、
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
`configs/local.yaml` gitignored，`task run` / `task migrate` 第一次會從 `config.example.yaml` 複製。
注意：viper 忽略空 env，`APP_POSTGRES_URL=` 清不掉 key，要「不接 DB」請改 local.yaml。完整 key 表見 `docs/config.md`。
`auth.*`（mode / api_keys / jwt / public）見 `docs/config.md`。

## 新增東西的步驟

**新增 RPC**：改 `protos/<bc>/v1/service.proto`（request 欄位規則用 `buf.validate` 寫在 proto，interceptor 會擋；domain 仍保留不變量）→ `task gen`（`task breaking` 檢查相容）→ domain 加行為 →
`application/dto` + `usecase` → `provider.go` 加 `ProvideXxx` 並用 `decorate()`（讀）或 `decorateTx()`（寫）包 → `adapters/rpc` 實作
（`var _ XxxServiceHandler` 會提醒）→ `task wire`。

**新增 Bounded Context**（照 `pkgs/inventory` 抄）：`protos/<newbc>/v1/{entity,service,event}.proto` + `openapi.base.yaml` 與
`buf.gen.yaml` 的 `go_package` override → `pkgs/<newbc>/go.mod`（replace 指向 kernel / platform / 要讀 contract 的 BC）+ `go.work`
加 `use` + `Taskfile.yml` 的 `MODULES` 與 `BCS` + `apps/*/go.mod` 的 require / replace → `task tidy` → `contract/v1/openapi.go` →
`task gen` → 複製 `pkgs/ordering` 結構寫 `provider.go`（`Subscriptions` 自己的 named type）→ `.golangci.yml` 加該 BC 的 layer block 與
`bc-isolation-<bc>`，並把它加進其他 BC / shared / apps 的 deny（`pkg` 是 prefix match，不能用 `*`）→ `apps/rpc` 的 `NewRoutes` /
`NewDocs` / `NewPoisonService` 的 schema 清單加它、`apps/{rpc,worker}/wire.go` 加 `ProviderSet`、`apps/worker` 的 `NewWorker` 收它的 `Subscriptions`、`poison.go` 的 `schemas` 加它（沒有 event 的 BC 如 identity：router / poison 都不加，只有 operator 子命令才進 `apps/worker` 的獨立 injector）→ `task wire` →
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
