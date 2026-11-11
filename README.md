# x-monorepo

Protobuf-first Go monorepo：**Clean Architecture + DDD（BC-first）**，go.work 多 module，
Connect 對外，Wire 組裝，Turborepo 前端。

## Use this template

這是 GitHub template repo。按 **Use this template** 建好自己的 repo 後：

```bash
task tools                                  # 固定版本的工具鏈 → ./bin
task rename -- github.com/<you>/<repo>      # 改 module path（go.mod / imports / buf.gen.yaml）並重新 gen
git commit -am "chore: rename module path"  # gen-freshness 是跟 HEAD 比，先 commit
task gate-ci                                # 跟 CI 一樣的檢查，應該全綠
```

目前只保留 `identity` 一個 BC，沒有 event；新增 BC 用 `task new-bc -- <name> [--events]`（流程見 [`docs/workflow.md`](docs/workflow.md)）。有 event 的完整版（含 ordering、inventory、payment、catalog）保存在 https://github.com/blackhorseya/x-monorepo-reference（tree 與 main `87ebfba1` 相同）；移除的經過見 issue #518。

設計的來源與取捨：

| 來源 | 借來的 | 刻意不借的 |
|---|---|---|
| DDD / go-ddd | aggregate、VO、repository in domain、BC-first、ACL、event | — |
| go-kit | Service / Endpoint / Transport 三層；endpoint middleware → `usecasex` decorator | 手寫 transport、`any` 型別擦除、god service interface |
| go-micro | 一個 service 可同時掛多種 transport；broker / store 抽象為 port | 框架本身、自家 codegen、內建 registry（K8s 已有） |

## Quick start

```bash
task tools          # buf / protoc-gen-go / protoc-gen-connect-go / wire / golangci-lint → ./bin
task tidy           # 每個 module go mod tidy + go work sync（產生 go.sum）
task new-bc -- <name> [--events]   # 新增 BC：產生 pkgs/<name>、protos/<name>、migration 並接進共用檔，再 gen / tidy / wire
task gen wire       # protos → pkgs/*/contract、DI graph → apps/*/wire_gen.go
task lint test      # 分層與 BC 邊界由 archtest 強制（`go test`）
task compose-up     # 本機 Postgres + otel-lgtm（Grafana http://localhost:3000）
task migrate -- up  # 跑每個 BC 的 migrations（service 啟動時不會自己跑）；第一次會把 config.example.yaml 複製成 configs/local.yaml（gitignored）
task run            # ./bin/rpc 讀 configs/local.yaml：Connect + gRPC + gRPC-Web 同一個 port、text log、/api/docs 開
                    # 不想接 DB：把 local.yaml 的 postgres.url 清空（= in-memory repository）

# identity 的管理 RPC 要 admin role（auth 關著就直接打）
curl -X POST localhost:8080/api/identity.v1.IdentityService/ListAccounts \
  -H 'Content-Type: application/json' \
  -d '{}'
```

## Layout

```
.
├── go.work                        # 每個 pkgs/* 和 apps/* 各一個 go.mod
├── buf.yaml / buf.gen.yaml        # proto module；go_package 由 override 決定輸出位置
├── buf.gen.ts.yaml                # TS 輸出到 web/packages/proto
├── protos/
│   ├── common/v1/money.proto      # 跨 BC 的 wire 型別
│   ├── admin/v1/                  # platform 的 poison / prune 操作面（不屬於任何 BC）
│   └── identity/v1/               # account / API key（entity / service，沒有 event）
│       ├── entity.proto           # aggregate 的 state schema（跨語言契約，不是 Go domain model）
│       └── service.proto          # RPC
├── pkgs/
│   ├── kernel/                    # shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
│   ├── platform/                  # 技術共用：contextx、config、server（gin）、usecasex、otel（metrics / traces / logs）、eventbus、postgres（gorm + migrate runner）、archtest（import 規則）等
│   └── identity/                  # ── Bounded Context（完整六角形）：DB 管理的 account + API key，給 auth interceptor 解 X-API-Key（docs/identity.md）──
│       ├── provider.go            # wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorator 包起來
│       ├── contract/v1/           # buf 生成：pb + Connect + openapi.yaml（只有 adapters / apps 可 import）
│       ├── domain/                # 手寫 aggregate、VO、Repository interface、domain errors
│       │   ├── account/           # Account aggregate（id = principal subject、roles、ACTIVE / DISABLED）
│       │   ├── apikey/            # APIKey aggregate（只存 sha256、prefix；ACTIVE / REVOKED、expires_at）
│       │   ├── externalidentity/  # 外部 OIDC 的 (iss, sub) 綁定（docs/identity-login.md）
│       │   └── invite/            # 邀請
│       ├── application/
│       │   ├── dto/               # application 邊界（go-kit endpoint request/response）
│       │   ├── usecase/           # orchestration（go-kit service）
│       │   ├── port/              # 需要外界提供的能力
│       │   └── mapper/            # domain → dto
│       ├── infra/                 # driven：repository / port 實作
│       │   ├── memory/            # in-memory repository（APP_POSTGRES_URL 未設時）
│       │   ├── postgres/          # gorm row model + SQL + 錯誤轉譯；migrations/*.sql 以 embed 隨 BC 走
│       │   └── system/            # clock / id
│       └── adapters/              # driving
│           ├── rpc/               # Connect handler；pb ↔ dto
│           ├── auth/              # 實作 platform 的 server.CredentialResolver（TTL cache），apps/rpc 以 wire.Bind 接上
│           └── cli/               # `worker bootstrap` 用的 injector
├── deployments/docker/            # docker-compose.yaml：本機 infra（Postgres），app 不進容器；goreleaser 用的 Dockerfile
├── deployments/helm/x-monorepo/   # Helm chart：rpc / worker Deployment、migrate hook Job、prune CronJob（task helm-check）
├── deployments/helm/kind/         # task helm-kind：kind cluster 真裝 chart（拋棄式 Postgres / Redis + smoke.sh）
├── configs/config.example.yaml    # `task migrate` 第一次會複製成 configs/local.yaml（gitignored）
├── scripts/                       # CI、gate、release 用的 shell / jq 腳本（docs/ci.md、docs/release.md）
├── tools/
│   ├── newbc/                     # `task new-bc` 的產生器與樣板（docs/workflow.md）
│   └── stripvalidate/             # `task gen-ts` 的一步：buf image 去掉 validate 規則後再產 TS
├── .claude/skills/new-bc/         # 新增 BC 的 skill（SKILL.md）
├── .github/                       # workflows、composite actions、PR template
├── tests/k6/                      # k6：ratelimit、timeout（打 `identity.v1.IdentityService/ListAccounts`）；task k6 -- <script>、task k6-smoke、task k6-auth-matrix（各 auth 組合）、task k6-wire
├── apps/
│   ├── rpc/                       # composition root：main + wire，無業務、無 mapping
│   ├── worker/                    # 事件 consumer process：同一份 ProviderSet + Watermill router（目前沒有 BC 有 consumer，沒有 handler，啟動後只等 signal；`poison` / `prune` / `bootstrap` 子命令照常）
│   └── migrate/                   # 收集各 BC 的 embed migrations，cobra：up / down / version；唯一能 import infra 的 app
└── web/                           # Turborepo（pnpm）
    ├── packages/proto/            # @repo/proto：buf 生成的 connect-es client（有 commit）
    └── apps/console/              # Vite + React + TanStack Router / Query + Tailwind v4 + shadcn/ui
```

## Docs

README 放架構與程式碼層面的規則；流程、維運與參考表在 `docs/`：

| 檔 | 內容 |
|---|---|
| [`docs/ci.md`](docs/ci.md) | gates：hooks / lefthook / CI 各跑什麼、CI 的 job 與依路徑的 gating、相關 scripts |
| [`docs/release.md`](docs/release.md) | release-drafter + goreleaser 的 release 鏈、release gate、簽名 / attestation 與驗證 |
| [`docs/operations.md`](docs/operations.md) | Compose 本機環境（Grafana dashboards / alerts）、k6 load test、Helm 部署與 kind 實測 |
| [`docs/observability.md`](docs/observability.md) | OTel → LGTM：metrics、traces、logs、每個 metric 與 alert 的意義 |
| [`docs/config.md`](docs/config.md) | 所有 config key、預設值、env 覆寫規則、`auth.*` |
| [`docs/error-codes.md`](docs/error-codes.md) | `x-error-code` 總表 |
| [`docs/workflow.md`](docs/workflow.md) | 新增 RPC / Bounded Context / consumer、拆 BC 成獨立服務、rename module |
| [`docs/identity.md`](docs/identity.md) | identity BC：DB 管理的 account / API key、`server.CredentialResolver`、principal 語意、cache TTL、帳密 / issuer 的擴充路徑 |
| [`docs/identity-login.md`](docs/identity-login.md) | 外部 OIDC 登入的現行規格（Dex 作為開發 IdP；多 issuer verifier、JWKS、`(iss, sub)` 綁定與邀請、Console PKCE、撤銷、拆 PR #270–#274；未實作）；自建帳密 issuer 為可選模組（未排程） |

## Dependency rules（`pkgs/platform/archtest` 強制）

| 套件 | 可以 import | 不可以 import |
|---|---|---|
| `domain` | `kernel` | application / infra / adapters / contract / platform |
| `application` | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| `infra` | domain、application/port、platform | adapters / contract |
| `adapters` | application、contract、platform、**其他 BC 的 `contract/`**（consumer 解它的 event、events adapter 發它擁有的 request） | 其他 BC 的 domain / application / infra / adapters |
| `kernel`、`platform` | 標準庫、第三方 | 任何 BC |
| `apps/*` | 各 BC 的 `provider.go`、platform | 直接呼叫 usecase（只透過 wire）、infra |
| `apps/migrate` | 各 BC 的 `infra/postgres`（只為了拿 `Migration()`）、platform | domain / application / adapters |
| 任何 BC 的 domain / application / infra | — | 其他 BC 的任何東西（含 contract；走 port 或 event） |

**contract 和 dto 的差別**：contract 是 wire format（有版本、跨語言、生成），dto 是 application 邊界（手寫、
只放 usecase 需要的欄位）。同一個 usecase 被 rpc 和 consumer 兩個 adapter 呼叫時，dto 就是它們的共同語言。
dto 不要照著 pb 抄。

## server：gin + Connect

`platform/server` 用 gin 當 HTTP router：每個 BC 的 Connect handler 以 `gin.WrapH` 掛在 `/api/<pkg>.<Service>/*procedure`
（`http.StripPrefix`，給 Connect / gRPC-Web client 用 base URL `…/api`）**以及**原生路徑 `/<pkg>.<Service>/*procedure`
（gRPC 協定把路徑寫死，grpc-go / grpcurl 加不了前綴）；`/api/healthz`（liveness：process 活著就 200，不看依賴）、`/api/readyz`（readiness：跑 app 給的 `server.Readiness` checks，
rpc 是 Postgres ping；任一失敗或 server 正在 drain 回 503 + JSON 明細）、`/api/version`（`{version, commit, date, go}`，不用驗證：port-forward 就知道 pod 跑哪版；console 的 footer 顯示「console X · server Y」——X 是 release.yml build 時用 `VITE_CONSOLE_VERSION` 烘進去的 tag，本機 build 是 `dev`；兩邊都是真版號又不同就標 version mismatch）、`/api/metrics`、`/api/docs` 是普通 gin route。
**Graceful shutdown**（`Server.Serve`，`apps/rpc/main.go` 以 `signal.NotifyContext` 接 SIGINT / SIGTERM）：先把 `/api/readyz` 翻成 503 `"server":"draining"`
但其他路徑照常服務 `http.shutdown_delay`（預設 0：本機 Ctrl-C 立刻關；k8s 設 ≥ 一個 probe period，LB 才來得及停止派流量——Helm chart 的 `rpc.shutdown` 把這個關係算死），
再關 listener、給 in-flight request `http.shutdown_timeout`（預設 15s）跑完；超時才切斷並回 error。liveness 全程 200。
HTTP/1.1 與 h2c 各一個 in-flight request 的 drain 有真 socket 的測試（`platform/server/shutdown_test.go`），SIGTERM → exit 0 有 `apps/rpc/shutdown_test.go`。
整台 server 都在 `/api` 下，ingress 一條規則。`RedirectTrailingSlash` / `RedirectFixedPath` 關閉（gRPC client
不跟 redirect）；gin 只做 routing + `Recovery()`，RPC log 留在 Connect interceptor。HTTP/1.1 與 h2c 同一個 port，
Connect / gRPC / gRPC-Web 三種協定都有 E2E test（`apps/rpc/main_test.go`）。

**Response meta 走 header，不包進 proto message**：transport 層本來就有 status / header / trailer，在 body 包 `{data, meta}`
等於重做一次、每個 Response 都要重複寫、client 多剝一層；錯誤的 `x-error-code` 也已經是 header。`server.NewMetaInterceptor`
在成功與錯誤兩條路都設。錯誤的 metadata（`x-error-code`、`Retry-After` 與下表的 header）由 `server.WithMeta` 掛在 `*connect.Error` 上，
離開 server 時 Connect unary 寫進 HTTP response header，gRPC / gRPC-Web / Connect streaming 寫進 trailer（`pkgs/platform/server/meta.go`；
`TestMeta_ErrorMetadataFollowsTheProtocol`、`apps/rpc/main_test.go` 的三協定表）：

| header | 內容 |
|---|---|
| `X-Request-Id` | client 有帶就原樣回（≤128 字），沒帶就 server 產 UUID v7；`server.RequestID(c)` 拿得到，request log 也有 `request_id` |
| `X-Trace-Id` | OTel trace id（有 span 才有；`otel.endpoint` 空時沒有），直接貼到 Tempo / Loki `| trace_id="…"` |
| `traceparent` | W3C trace context。**rpc 不延續請求帶進來的 `traceparent`**（開 SDK：新 root trace，呼叫端的 span 只留成 link，查 trace 用上一列的 `X-Trace-Id`；`otel.endpoint` 空：不讀也不寫 trace header，回應沒有 `X-Trace-Id`）。rpc 沒有下游呼叫，所以沒有轉送 `traceparent` 的問題（`docs/observability.md`）。回應這一份：`otelconnect.WithPropagateResponseHeader()` 回給瀏覽器端 tracing（只在成功回應帶：otelconnect 在 handler 跑之前就寫，`server.New` 在呼叫失敗時用 `dropHeadersOnError` 拿掉它，見 `TestTrace_TraceparentOnSuccessOnly`；錯誤看 `X-Trace-Id`） |

分頁之類「該 RPC 自己的資料」（`next_page_token`、`total_size`）才放進 message，不做通用 `ResponseMeta` 欄位。

## Validation：protovalidate 在邊界擋 request 形狀

| 層 | 擋什麼 | 位置 |
|---|---|---|
| proto | `buf.validate` rule 直接寫在 request message 上（`min_len`、`gt`、`gte`、`pattern`、`required`、`min_items`） | `protos/<bc>/v1/service.proto`、`common/v1/money.proto` |
| server | `NewAuthInterceptor`（`auth.mode` 不是 `off` 才掛，在 logging 之內、validate 之外）：`X-API-Key` 先對 `auth.api_keys`（static，break-glass）再問 `server.CredentialResolver`（`pkgs/identity` 的 DB key，見 `docs/identity.md`；resolver 失敗 = 拒絕，不放行；一次查詢受 `auth.resolver_timeout` 限制），否則 `Authorization: Bearer` 對 `auth.jwt`，解析成 `contextx.Principal{Subject, Name, Roles, Method, CredentialID}` 放進 context；`rpc` access line 帶 `auth`（`api_key` / `jwt` / `anonymous` / `public`）與 `sub`（auth 在 logging 之內，透過 context 裡的 slot 回報）、usecase 的 log 帶 `sub`、span 帶 `enduser.id` / `enduser.roles` / `auth.method`——Loki 與 Tempo 都查得到誰呼叫的，被拒絕的呼叫也有一行（`code=unauthenticated auth=anonymous`）；每個決定計入 `auth_decisions_total{result, method}`（dashboard 的 Auth row 與 `rpc-auth` alert 看它）；name 與 credential 本身不記；usecase 用 `contextx.PrincipalFrom(c)` 讀（consumer path / mode off 是 anonymous zero value）。只做 authentication，授權是讀的人的事（usecase 用 principal 判斷，例如 `auth.admin_role` 的檢查，見 `docs/identity.md`）。沒帶 → `UNAUTHENTICATED` + `x-error-code: auth.unauthenticated`，帶了驗不過 → `auth.invalid_credential`。probes / `/api/metrics` / `/api/docs` 是 gin route，不在 RPC chain 裡，永遠開著 | `pkgs/platform/server/auth.go`、`pkgs/platform/contextx/principal.go` |
| server | `NewRateLimitInterceptor`（`http.rate_limit.rps` > 0 才掛，在 logging 之內、auth 之外——洪水要在付 JWT 簽章驗證成本之前就擋掉）：token bucket，`by: global` 一桶 / `peer` 每個 client IP 一桶（idle 1 分鐘回收）。沒 token → `RESOURCE_EXHAUSTED`、`x-error-code: request.rate_limited`、`Retry-After: <秒>`，不排隊等 | `pkgs/platform/server/limits.go` |
| server | `NewTimeoutInterceptor`（`http.timeout` > 0 才掛，在 auth 之內、validate 之外）：usecase 拿到會到期的 context，慢 query 被 cancel 而不是吃掉整個 shutdown drain 窗口。到期後失敗的 RPC 一律回 `DEADLINE_EXCEEDED` + `x-error-code: request.timeout`（不管 driver 的 error 被 `ToConnectError` 映成什麼）；到期後仍成功的照常回 response（工作已 commit）。client 的 deadline 較短時一樣適用 | `pkgs/platform/server/limits.go` |
| server | `NewValidateInterceptor`（最內層 interceptor）：違反 → `INVALID_ARGUMENT`、header `x-error-code: request.validation_failed`、error detail `buf.validate.Violations`（欄位路徑 + rule id + message）；rule 本身編不過 → `INTERNAL` | `pkgs/platform/server/validate.go` |
| domain | 業務不變量照舊（`NewMoney`…）：consumer path 沒有 interceptor，domain 是唯一防線 | `pkgs/<bc>/domain` |

- 規則故意和 domain 重複：edge 給 client 好讀的錯（哪個欄位、哪條 rule），domain 給所有入口一致的保證。
- `buf.yaml` 的 `deps` 帶進 `buf.build/bufbuild/protovalidate`（`buf.lock` 有 commit）；`buf.gen.yaml` 用 `managed.disable` 讓它保留自己的 Go package。
- OpenAPI 會把 rule 轉成 `minLength` / `minimum` / `pattern`，所以 `/api/docs` 看得到。
- TS client 不帶 rule：`task gen-ts` 經 `tools/stripvalidate` 把 options（含 `buf.validate.*`，也含其他 unknown option field）和 `validate.proto` 拿掉，只留 `Violations` / `Violation` / `FieldPath` / `FieldPathElement`（`buf/validate/violations.proto`，package 與 type URL 不變），console bundle 因此少 ~70 kB raw / ~13 kB gzip。代價：瀏覽器端用不了 protovalidate 規則（驗證只在 server 與 domain）；wire 相容由 `tools/stripvalidate` 的測試鎖住。詳見 `web/packages/proto/README.md`。

## Pagination：cursor（keyset）+ opaque page token

列表 RPC 走 [AIP-158](https://google.aip.dev/158) 的欄位名（`page_size` / `page_token` / `next_page_token`），不做 offset：
`OFFSET n` 深頁是線性掃描，且中途插入或刪除會漏／重複；keyset 每頁都是 index range scan。`ListAccounts` 是範例：

| 層 | 負責 | 位置 |
|---|---|---|
| domain | 排序鍵固定（`ListAccounts` 是 id 升冪），`ListParams` 帶 `Limit` / `After`。新 ID 是 UUID v7，舊資料仍是 v4，兩者並存且都當不透明字串，不得只靠 `id` 排序、也不保證 ID 單調遞增——跨 pod 的 v7 只有毫秒級順序。ID 不是憑證：v7 前 48 bit 是建立時間，API key secret 由 `SecretGenerator` 另產 256 bit 全隨機值 | `pkgs/identity/domain/account/repository.go` |
| platform | `pagination.Clamp`（0 → 預設 20、上限 100、負數 → `InvalidArgument`）與 `EncodeToken` / `DecodeToken`（base64url(JSON)，不透明但不是秘密） | `platform/pagination` |
| usecase | token 在這裡鑄造與驗證；`ListAccounts` 的 payload 只有最後一筆的 id（沒有 filter）；有 filter 的列表要把 filter 一併編進 payload，換了 filter 還帶舊 token → `pagination.invalid_token` | `usecase/account.go` |
| infra | SQL `WHERE id > ? ORDER BY id ASC LIMIT n+1`：多抓一筆判斷有沒有下一頁，不 COUNT；memory 版做一樣的比較 | `infra/postgres/account_repository.go` |

- `total_size` 不回：大表 COUNT 是另一筆成本，要時再加 optional 欄位不算 breaking。
- 不開放 `order_by`：動態排序會讓 cursor 與 index 都要跟著變，有需求再加。

## Events：domain event → transactional outbox（Watermill）→ worker

**目前狀態**：這套機制（platform 的 outbox / inbox / poison / eventbus、`worker poison|prune|bootstrap`、console 的 Poison / Prune 頁）都還在，但現在沒有任何 BC 發 event 或有 consumer：`worker` 沒有 handler，啟動後只等 signal；poison 服務的 schema 清單是空的。下面以 `<bc>` 表示「有 event 的 BC」；有 event 的完整範例保存在 reference repo（見上方）。

x-brain pitfall `best-effort-publish-loses-one-shot-events`：「存完再 best-effort publish」掉一次就是永久掉。這裡的做法：

| 層 | 做什麼 | 位置 |
|---|---|---|
| kernel | `kernel.Event`（`AggregateID()`、`OccurredAt()`）是所有 BC 事件的 base interface | `pkgs/kernel/event.go` |
| domain | aggregate 在行為裡 `record()`，`PullEvents()` 取出並清空；BC 的 `Event` embed `kernel.Event`，實作共用 embedded `base` struct | `domain/<aggregate>/events.go` |
| application | usecase 只依序呼叫 `repo.Save` 與 `port.EventPublisher.Publish`；`provider.go` 的 `decorateTx()` 用 `usecasex.Transactional` 把整個 usecase 包進 `port.Transactor.Do`，兩者同一個交易 | `provider.go` |
| adapters/events | domain event → `<bc>.v1.<Event>` proto bytes → `message.Publisher`；topic = proto full name，metadata 帶 `key`（aggregate id）與 `traceparent` | `adapters/events/publisher.go` |
| platform/eventbus | Watermill：`PostgresPublisher` 從 message context 取出 use case 的 `*sql.Tx` 寫 `<bc>.events_<topic>`（**outbox 就是 commit 本身**）；`NewPostgresSubscriber` + `NewRouter`（Recoverer → tracing → 把帶 topic / message_id / trace_id 的 logger 放進 context，usecase log 才會進 Loki）+ `Register`（每個 BC：PoisonQueue → Retry → drop → access → timeout：每則訊息一行 Info `message`，對應 rpc 的 `rpc` access log；`worker.handler_timeout` 給每次 attempt 一個 deadline）；`ProvidePublisher` 由 app 提供一次給所有 BC；沒 DB 時用 GoChannel（at most once、同 process） | `pkgs/platform/eventbus` |
| infra | `postgres.Transactor` 把 gorm tx 放進 context，repository / publisher 用 `platformpg.TxOrDB` 拿；memory 版直接呼叫 fn | `infra/postgres/transactor.go` |
| apps/worker | 另一個 process：wire 每個 BC 的 `ProviderSet`，把各 BC 的 `Subscriptions`（`ProvideSubscriptions`，named type 讓 wire 分得開）掛上同一個 router；每個 BC 自己的 subscriber / consumer group（`<worker.consumer_group>.<bc>`）與 poison topic。`task worker`，需要 `postgres.url`；同 `worker.consumer_group` 的 replica 分工。`worker poison …`（`task poison -- …`）是 poison 表的操作面、`worker prune`（`task prune -- …`）是 outbox / inbox 的 retention | `apps/worker`；`worker bootstrap`（`task bootstrap`）是 identity 的第一個 account + API key（空 store 才建，secret 印一次；identity 沒 event，只掛這個 injector，不進 router） |

- Topic → table：`<bc>.v1.<Event>` → `<bc>.events_v1_<event>`（+ `_offsets`），第一段就是 BC，**DDL 由該 BC 的 migration 出**，
  service 不在啟動時建表；`InitializeSchema` 關閉。新 topic = 新 migration。
- 語意：at-least-once。handler 回 `eventbus.ErrDrop`（壞 payload、domain 說不行）= ack 不重試也不進 poison；
  其他 error → Retry（3 次、指數退避）→ `<bc>.poison` 表（`events_poison`）。
- 冪等有兩層：**inbox**（`<bc>.inbox` 表，`usecasex.Idempotent` 在 `decorateTx` 的 transaction 之內把 message id 記進去，重送（broker at-least-once、
  `poison requeue`、ack 遺失）在載入任何 aggregate 之前就以 `message.duplicate` drop；失敗的 attempt 連記錄一起 rollback，retry 會重記）
  加上 domain 狀態機（重送的事件碰到已經轉移過的 aggregate，由 domain 擋下）。
  沒 DB（in-memory）時沒有 inbox：GoChannel 本來就 at most once。
- **Retention**：outbox 表與 inbox 表都只會長，`task prune -- --older-than 720h [--dry-run] [--unconsumed] [<bc>]`（`worker prune`）刪掉
  (1) 比 `--older-than` 老、且**每個**有 cursor 的 consumer group 都 ack 過的 outbox 列（判斷式就是 lag gauge / subscriber「還沒讀」的反面；
  redis 模式下 cursor 是 forwarder 的）——沒有任何 group 有 cursor 的表（只發不收的 topic、worker 從沒跑過）預設跳過，`--unconsumed` 才清；
  (2) 比 `--older-than` 老的 inbox 列，但任何 BC 的 poison 表還停著的 message id 留著（requeue 用原 id 重發，inbox 列是擋重做的憑據）。
  一列只要還有**一個**有 cursor 的 group 沒讀就留著，NOTE 欄列出每個還有未讀列的 group、幾列、最老一列多老——到 retention 的是 `held back by`（真的擋住 prune），還沒到的是 `behind`（live group 兩次 poll 之間也會短暫出現；stale group 不用等到 retention 就看得到，和 held gauge / alert 同步）；不會再跑的 group（對這個 DB 跑過的 E2E 留下的 `test-*`、
  改過名的 `worker.consumer_group`、broker 改 redis 前的 BC group）用 `--forget-group <name>` 先刪掉它的 cursor（真回來就當新 group，照 `worker.start_from`）。
  poison 表不在此列（`poison drop` 才是）；redis stream 的修剪是 Redis 端的 XTRIM 政策。先 `--dry-run` 看數字再跑。
  同一件事也有 RPC：`AdminService.Prune`（`older_than` / `dry_run` / `unconsumed` / `forget_groups` / `batch_size`，回每張表的列數、skipped 原因、
  擋住的 group），console 的 **Prune** 頁就是打它——alert 響了之後不用進 pod：dry run 看 NOTE、填 forget、再跑；
bc / days / forget 在 URL 上（`/prune?bc=<bc>&forget=<group>.<bc>`，Poison 頁的 BC badge 與 alert 說明都連過來），dry run 永遠從開著開始。它跑在 `http.timeout` 底下，
  所以是 dry-run / forget-group / 例行小量的入口；第一次清大 backlog 用 CLI / CronJob（逾時只是回應掉了，已刪的 batch 都已 commit，重跑無害）。
- Poison / prune 操作面有兩個入口，底層同一組 `eventbus.ListPoison / Requeue / Drop / PruneSchema`：
  **RPC** `admin.v1.AdminService`（`protos/admin/v1`，platform 的 contract，不屬於任何 BC；`pkgs/platform/admin` 是 handler，
  `apps/rpc` 掛上並給它 BC schema 清單）— `ListPoison / GetPoison / RequeuePoison / DropPoison / Prune`，auth 開著時要 `auth.admin_role`
  （API key 用 `name=key:admin`，JWT 用 `roles` claim），不然 `PERMISSION_DENIED` + `x-error-code: auth.permission_denied`；
  `auth.mode: off` 時全開（跟其他 RPC 一樣——`off` 只給本機開發，任何外面連得到的環境都要 `required` + `admin_role`）。console 的 **Poison** 頁就是打它。
  **CLI** `task poison -- list [-n 50] [<bc>]` 列出各 BC 停在 poison 表的訊息（offset、uuid、原 topic、最後一次 error），
  `show <bc> <offset>` 看完整 metadata 與 payload（proto bytes，印 base64；用 `buf convert` 解），`requeue <bc> <offset>…|all`
  把訊息**重新 insert 回原 topic 的 outbox 表**（同一個 message id、拿掉 poison metadata；每個 consumer group、含 redis 模式的 forwarder
  都會再收到）並同一個 transaction 刪掉 poison row，`drop <bc> <offset>…|all` 直接刪。同一個 binary（cobra：沒有子命令 = 跑 consumer）：
  `worker poison …`，image 也是 `docker run …/worker poison list`，`--help` 有完整說明。`eventbus_poison_messages` 不為 0 就來這裡；修好 handler 再 requeue，再失敗會再進 poison。
- Trace：publish 時把 `traceparent` 寫進 metadata，router 的 tracing middleware 還原成 CONSUMER span，Tempo 看得到 rpc → publish → handler 一條線。
- **Graceful shutdown**：SIGTERM 後 router 停止取新訊息，但**正在跑的 handler 跑完、ack 寫進去**才結束（`eventbus.Draining` 包住每個 BC 的 subscriber
  與 redis 模式的 forwarder；上限 `eventbus.DefaultDrainTimeout` 8s，超過就 cancel 掉 handler 的 context 並放棄、訊息留給下一次）。
  不包的話 watermill-sql / redisstream 在 subscribe context 一 cancel 就把 message context 也 cancel、丟掉它的 ack：use case 的 transaction 中途失敗、重啟後再收一次。
  兩種 broker 都有測（`platform/eventbus/shutdown_test.go`），SIGTERM → exit 0 有 `apps/worker/shutdown_test.go`。k8s 的 `terminationGracePeriodSeconds` 要 > 18s（8s drain + router `CloseTimeout` 10s；Helm chart 的 `worker.terminationGracePeriodSeconds` 預設 30、低於 19 拒絕 render）。
- 換 broker：`message.Publisher` / `Subscriber` 是 port；handler 與 error contract 不變。已內建一個示範：`worker.broker: redis`
  （`redis.url` 必填，`task compose-up` 有起一個）時，worker 自己當 forwarder，把每個被消費 topic 的 outbox 表（Postgres consumer group
  固定叫 `forwarder`，所有 replica 共用，每則只轉一次）passthrough 進同名 Redis Stream，handler 再以 `<consumer_group>.<bc>` 的
  Redis consumer group 讀。**outbox 仍是寫入路徑**（use case 不直接發 Redis，否則 commit 失敗會留下幽靈事件），poison 也仍寫 Postgres 表。
  代價：forwarder ack 之後訊息只存在 Redis（要開 persistence，compose 已開 AOF）；SQL → Redis 是 at-least-once，重送的訊息帶同一個 UUID。
  既有環境第一次切到 redis 時，`forwarder` 這個 SQL group 與 `<group>.<bc>` 的 stream group 都還沒有 cursor，預設會把歷史 outbox 全部重播；
  不想要就設 `worker.start_from: latest`（stream group 在 `NewWorker` 裡先建好再啟動 forwarder，所以「latest」是從啟動那一刻算）。
  換 NATS / Kafka 同一個形狀：`eventbus` 多一組 publisher / subscriber，`NewWorker` 的 switch 多一個 case。
- 代價：watermill-sql 會把 `lib/pq` 一起連進來（只用它的 error type），實際連線仍走 pgx。
- **optimistic lock**：aggregate（例如 identity 的 Account）用 `version` 做樂觀鎖，輸的那邊拿 `ErrStale`。它 wrap `kernel.ErrConcurrentUpdate`，
  **刻意不是** `kernel.Error`：consumer 會 retry 而不是 drop，rpc 由 `server.ToConnectError` 回 `Aborted`（`x-error-code: concurrent_update`），
  `usecasex` 記成 `kind=aborted`、log 為 Warn。

## usecasex：use case 的統一形狀與 decorator

```go
type UseCase[I, O any] interface { Execute(c context.Context, in I) (O, error) }
```

adapter 只依賴 `UseCase[I, O]`，不依賴具體 struct。`provider.go` 的 `decorate()` 用 `usecasex.Chain` 把
metrics + logging 包在每個 usecase 外面，rpc 和 consumer 兩邊自動都有。寫入的 usecase 改用 `decorateTx()`，多包一層
transaction。Connect
interceptor 只管 transport 層的事（procedure、protocol、code）。

- `usecasex.Logging`：一行 log。成功是 `Debug`（rpc 路徑已有 interceptor 的 `rpc` access log，成功次數看 metrics），domain error 是 `Info`（預期結果），
  optimistic-lock 衝突是 `Warn`，其他 error 才是 `Error`
- `usecasex.Metrics`：OTel histogram `usecase.duration`（Prometheus：`usecase_duration_seconds{usecase, kind}`）；`kind` =
  `ok` / kernel error kind / `internal`，和 `server.ToConnectError` 同一套判準，attribute 值域有限。MeterProvider 由
  `platform/otel.ProviderSet` 注入（每個 `Initialize()` 一個，不用 global）
- `usecasex.Transactional`：把 usecase 整個放進 `Transactor.Do`。usecase 拿到的 `c` 帶著 tx，repository 與 outbox
  publisher 用 `platformpg.TxOrDB` 取用，所以全部寫入一起 commit / rollback。任何 `Do` 的錯誤（含 commit 失敗）都丟掉 output。
  放在最內層，commit 失敗會被 logging / metrics 記成這次 usecase 的失敗。只給寫入用：讀取包 transaction 只會多一次 BEGIN/COMMIT。
  它保證的是 atomicity，不是 isolation：Postgres 預設 READ COMMITTED，兩個同時的寫入都可能讀到同一份舊狀態。
  擋住重複寫入的是 aggregate 的 `version`（見上方 optimistic lock），不是 transaction

## Architecture rules：誰擋什麼

| 規則 | 類型 | 擋的工具 |
|---|---|---|
| domain 只 import kernel；application 不碰 infra / adapters / contract；infra 不碰 adapters；adapters 不碰 infra | Clean Arch 依賴方向 | **archtest**（`pkgs/platform/archtest/imports.go`，BC 內的 `arch_test.go` 跑，不逐 BC 列規則） |
| BC 只能從 adapters import 其他 BC 的 `contract/`；kernel / platform 不 import BC；apps 只碰 provider.go | 邊界 | **archtest**（`bc-isolation` 規則擋其他 BC 的 domain / application / infra / adapters；BC 的 domain / application / infra 讀其他 BC 的 `contract/` 目前沒有規則，靠 review；kernel / platform 與 apps 的規則跑在各自 module 的 `arch_test.go`） |
| domain struct 欄位 private；無 `Set*`；每個 struct 有 constructor（`*Params` 參數物件除外）；不呼叫 `time.Now()`；不 log | DDD aggregate | **archtest**（`pkgs/platform/archtest`，每個 BC 的 `arch_test.go` 跑） |
| `*Repository` interface 只能在 domain | DDD | archtest |
| usecase 只有 `Execute(c, in) (out, error)` 一個公開方法 | 單一職責、可被 decorator 包 | archtest |
| dto 無方法、不 import domain / proto；port 只有 interface | 邊界純度 | archtest |
| adapter 持有 `usecasex.UseCase[I,O]`，不持有 `*usecase.X` | decorator 生效 | archtest |
| generated code 和 source 一致；go.mod 無漂移 | 一致性 | CI `gen-freshness`、`work-sync` |
| domain 100% coverage | 測試 | `cover-gate.sh` |
| request 形狀（必填、範圍、格式）在 proto 宣告、進 use case 前就擋 | 邊界 | **protovalidate**（`platform/server/validate.go`） |

archtest 只看 AST，零依賴，毫秒級；規則表在 `archtest.DefaultRules()`，BC 可用 `RunRules` 自選。

## OpenAPI

proto-first：OpenAPI 是 proto 的產物，不是另一份契約。`task gen` 用 `protoc-gen-connect-openapi`
（懂 Connect 的路徑與 error shape）把每個 BC 的 spec 生到 `pkgs/<bc>/contract/v1/openapi.yaml`，
`protos/<bc>/v1/openapi.base.yaml` 提供 title / description 等手寫 metadata。
BC 用 `contract/v1/openapi.go` embed 它；`apps/rpc` 的 `NewDocs` 收集各 BC，`APP_HTTP_DOCS=true`
時才掛 `/api/docs`（Scalar UI，`/api/docs/<bc>`）與 `/api/openapi/<bc>.yaml`。production 不開。
**新增 RPC 要兩個檢查，不是一個**：生成檔存在（`task gen` / `check-gen-fresh`）不等於服務文件已發布（要在 `NewDocs` 掛上並由 wired rpc 的 `APP_HTTP_DOCS=true` 入口測到）。`check-gen-fresh` 的 pathspec 是 `*/contract/*/*openapi.yaml`。
spec 裡的 `servers` 是相對的 `/api`（任何 host 都對），但 serve 出去的 yaml 會把它改寫成 request 的絕對 origin（`X-Forwarded-Proto` / Host）：
Scalar 的 Send request 用 `new URL(server + path)`，相對 server 會噴「The URL seems to be invalid」；順便讓檔案直接匯入 Postman / Bruno 就能打。
兩份 `openapi.base.yaml`（admin、identity）都宣告 `securitySchemes`（`apiKey` = `X-API-Key`、`bearer` = JWT），所以 Scalar 右上 **Auth** 可以選一種填進去
（`auth.mode: required` 時填 `configs/local.yaml` 的 `auth.api_keys` 那把 key）；`auth.mode: off` 時填不填都一樣。前端不需要它 —— `web/packages/proto`
直接從 proto 生 connect-es client。

## Web console：`web/apps/console`

一個最小的操作介面，用來證明 `@repo/proto` 真的能被前端用：帳號管理（Accounts），以及 poison / prune 的操作頁。

```bash
task run            # rpc :8080
task web-dev        # http://localhost:5173；CONSOLE_API=http://host:port 可改 proxy 目標
task web-typecheck  # @repo/proto + @repo/console 的 tsc --noEmit（CI 的 web job 也跑這個 + web-build）
task web-e2e        # Playwright smoke：建好的 console（vite preview）打真的 ./bin/rpc；account / poison / prune 各頁走一遍。E2E_DATABASE_URL=postgres://… 先 migrate up 再接那個 DB，並多起一個 bin/worker（CI 的 console-e2e job 這樣跑：poison / prune 真的執行），沒設就 in-memory、admin 頁驗 admin.no_database；另一組 project auth 起 auth.mode=required（API key + HS256 JWT）的 rpc 驗 401 / 403 / 設 key 或 JWT 恢復、過期 / 錯 issuer 的 token、roles claim 過 admin gate、寫入也帶 credential。第一次要 cd web/apps/console && pnpm exec playwright install chromium（CI 的兩個 console job 走 .github/actions/playwright：chromium 以 @playwright/test 版本為 key 快取在 ~/.cache/ms-playwright，命中只裝 apt deps）
task web-e2e-dex    # oidc-dex project：Console 經真 Dex 登入（compose 的 dex 沒起就先起；需 dex.localtest.me 解析得出來，離線見 docs/operations.md「Dex dev IdP」）→ 未綁定 → CreateInvite + ClaimInvite → 進 Console，打只信任 Dex 的真 bin/rpc；CI 的 console-e2e 以 E2E_DEX=1 task web-e2e 一起跑
task web-e2e-image  # 只把需要 nginx 的 spec（project image：e2e/image-specs.ts = image.spec.ts + version / account-loop 兩支 smoke）改打真的 console image，其餘留在 vite preview（e2e/image.sh：build web/apps/console/Dockerfile、容器的 RPC_UPSTREAM 回指 host 上 Playwright 起的 bin/rpc；docker 或 podman），外加 e2e/image.spec.ts 驗 nginx 本身：deep link 回 index.html + no-cache、assets immutable、不存在的 asset 是 404、/api 經 proxy、/healthz、/config.json、CSP。啟動前 e2e/image-specs.check.ts 擋下沒分類的新 spec；task web-e2e-image-mutants 用壞掉的 nginx 設定證明這些 spec 會紅（不進 CI）。CI 的 console-image job
```

| 主題 | 做法 |
|---|---|
| 呼叫後端 | `@connectrpc/connect-web` + `@repo/proto` 的 service descriptor；vite dev server 把 `/api` proxy 到 Go server，瀏覽器維持 same-origin，所以 server **不需要 CORS** |
| 錯誤 | `describe()` 讀 `x-error-code` header，並解析 protovalidate 的 `Violations` detail 成 `items[0].sku: …`；`UNAUTHENTICATED` 另外提示去設 credential |
| 認證 | header 右側的 **Credential**：API key（`X-API-Key`）或 JWT（`Authorization: Bearer`），存 localStorage（`console.credential`），transport 的 interceptor 每次呼叫時讀，改了下一個 RPC 就生效；`VITE_CONSOLE_API_KEY` / `VITE_CONSOLE_TOKEN` 可在 build 時 seed。server `auth.mode: off` 時什麼都不用設 |
| 資料 | TanStack Query：server 只有 unary RPC。Poison 每 5s 輪詢；`/api/version` 每 5 分鐘重抓。domain error 不 retry |
| 路由 | TanStack Router（code-based，`/accounts`、`/accounts/$id`、`/poison`、`/prune`，另有 `/login`、`/auth/callback`；首頁導到 `/accounts`），不需要額外的 codegen plugin |
| Poison 頁 | `admin.v1.AdminService`：各 BC 的 poison 表，Show（metadata + payload base64）/ Requeue / Drop，每個 BC 有 Requeue all / Drop all；server auth 開著時 credential 要帶 `admin` role |
| UI | Tailwind v4（`@tailwindcss/vite`）+ shadcn/ui（`components.json`；component 在 `src/components/ui`，加新的用 `pnpm dlx shadcn@latest add <name>`） |

`@repo/proto` 的 `exports` 指向 `src/gen/*.ts`，由 bundler 直接吃 TS 原始檔，不需要 build step。

部署：`web/apps/console/Dockerfile` 把 `task web-build` 的 `dist/` 放進 `nginxinc/nginx-unprivileged`（:8080），`deploy/default.conf.template`
一個 server：`/` 走 SPA fallback（任何路由都回 `index.html`，`assets/` immutable cache）、`/api/` proxy 到 `RPC_UPSTREAM`（預設 `http://rpc:8080`），
瀏覽器維持 same-origin，rpc 一樣不需要 CORS；`/healthz` 給 probe。nginx 自己回的東西（index.html、assets）帶 `deploy/security-headers.inc.template` 渲染出的標頭：CSP（script 只准 self、style 允許 inline——Radix 用 style attribute、connect 只准 self——OIDC 開啟時加上 issuer 的精確 origin、`frame-ancestors 'none'`）、nosniff、`X-Frame-Options: DENY`、Referrer-Policy、Permissions-Policy；`/api/` 的 response 是 rpc 的，不加（`/api/docs` 的 Scalar 從 CDN 載）。image 只寫 `/tmp`（entrypoint 用 envsubst 把 template render 到 `/tmp/default.conf`、`/tmp/config.json`（SPA 的 runtime 設定：`CONSOLE_OIDC_ISSUER` / `CONSOLE_OIDC_CLIENT_ID` / `CONSOLE_API_KEY_LOGIN`，白名單 filter，見 [`docs/identity-login.md`](docs/identity-login.md) §8）、`/tmp/security-headers.inc`，
`conf.d` 只剩一行 include），所以 read-only rootfs + 任意 uid 都能跑——Helm chart 的 `console.enabled` 就是這樣部署它（見 [`docs/operations.md`](docs/operations.md)）。
release pipeline 把它和四個 Go image 一起推成 `ghcr.io/blackhorseya/x-monorepo/console:X.Y.Z`（見 [`docs/release.md`](docs/release.md)；`task helm-kind` 則從 working tree
build 成 `localhost/x-monorepo/console:kind`）。所以 console 的改動**會切版**：只有 `e2e/` 和 `playwright.config.ts` 不算。

## Conventions

- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`
- Interface 參數 `c context.Context`；實作內 `ctx := contextx.FromContext(c)`
- Domain error 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 code，`Code` 放
  `x-error-code` header；consumer 把 domain error 視為 drop（不重試），infra error 才 retry
- `wire.go` 是 AppConfig → component config 的唯一 mapping 點
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- goimports local-prefix `github.com/blackhorseya/x-monorepo`

## Persistence：Postgres、gorm、migrations

- `platform/postgres.NewDB` 提供 `*gorm.DB`（driver 底層是 pgx，`TranslateError` 開啟）。`APP_POSTGRES_URL` 空 → 回 nil，
  各 BC 的 `Provide*Repository` 看到 nil 就改用 `infra/memory`，所以 `task run`、E2E test 不需要 DB。
- **每個 BC 一個 Postgres schema**（目前只有 `identity.accounts`），版本表也在該 schema 裡（`identity.schema_migrations`）。
  多個 BC 共用一個 database 時，預設的單一 `public.schema_migrations` 會讓兩條 migration 序列互相覆寫版本號且不報錯；
  schema 隔離同時解決表名與版本表的衝突。
- SQL 放 `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql`，由 BC `embed`（go.work 下 embed 不能跨
  module）；`apps/migrate` 只收集各 BC 的 `Migration()` 並用 golang-migrate 執行（cobra：`up` / `down` / `version`，`--help` 有說明；跟 rpc / worker 一樣是同一個 image 的不同 binary）。**service 啟動不跑 migration**，
  部署流程是 `migrate up` → rollout。
- row model（`accountRow`）是持久化模型，不是 aggregate；timestamps 來自 domain 的 `port.Clock`，所以 gorm 的
  `autoCreateTime/autoUpdateTime` 關閉。
- driver error 在 `infra/postgres.translate` 轉成 domain error：`gorm.ErrRecordNotFound → account.ErrNotFound`、
  `gorm.ErrDuplicatedKey` → 各 repository 的 duplicate error，其他保持 wrapped 給 log。
- integration test（`infra/postgres`、`provider_test.go`、`apps/migrate`、`apps/worker`）用 `pkgs/platform/postgres/pgtest`：
  `TEST_DATABASE_URL` 沒設就以 testcontainers 起一個 `postgres:17-alpine`（每個 test binary 一個，`TestMain` 結束時 terminate），
  設了就直接用那個 DB（連不上是 FAIL，不是 skip）。例外：`pkgs/identity` 的 last-admin 到期矩陣（`admin_lock_expiry_test.go`）會 `CREATE DATABASE` 開一個私有 DB 來隔離 `TRUNCATE` / 鎖定整張 accounts 表，所以提供的 `TEST_DATABASE_URL` 角色需要 `CREATEDB`（testcontainers 預設角色已有；沒有時該測試以 42501 明確 FAIL，不 skip；補權限：`ALTER ROLE <role> CREATEDB;`）。只有「本機沒有 container runtime」會 skip，且 CI 裡一律 FAIL。
  Podman 使用者要在 `~/.testcontainers.properties` 放 `ryuk.disabled=true`：Ryuk 寫死用名為 `bridge` 的 network，Podman 沒有。
