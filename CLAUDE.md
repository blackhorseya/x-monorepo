# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Protobuf-first Go monorepo：Clean Architecture + DDD（bounded context 優先）、`go.work` 多 module、
Connect 對外、Wire 組裝、Turborepo 前端（`web/`）。細節以 `README.md`（架構與程式碼規則）和 `docs/`（CI / release / 維運 / config / error codes / workflow）為準，這份只放「開工就要知道」的事。

## 常用指令

所有檢查的單一事實來源是 `Taskfile.yml`；hooks、lefthook、CI 都只是呼叫它。但「呼叫它」不等於「跑過」：AgEnD worktree 的 `core.hooksPath` 指向 `~/.agend/hooks`，lefthook 通常沒掛上。回報要分開寫，沒掛上就在 exact head 手動跑 `task gate-local`，寫「手動 gate 通過，hook 未執行」，不寫成 lefthook 通過；驗證與措辭見 `docs/ci.md`「本機 hook 證據」。

```bash
task tools                 # 固定版本工具鏈（buf / wire / golangci-lint / protoc-gen-*）→ ./bin，第一次必跑
task tidy                  # 每個 module go mod tidy + go work sync
task new-bc -- <name> [--events]   # 新增 BC：產生 pkgs/<name>、protos/<name>、migration 並在共用檔各加一筆，再 gen → tidy → wire（見 .claude/skills/new-bc）；task new-bc-check 在工作樹複本驗產生物（CI gen-freshness 也跑）
task gen wire              # protos → pkgs/<bc>/contract（pb + Connect + openapi.yaml）；DI → apps/*/wire_gen.go
task lint                  # buf lint + go vet + golangci-lint（分層 / BC 邊界是 archtest，跑在 `task test`）
task test                  # go test -race，每個 module
task gate-fast FILE=a.go   # gofmt + vet + golangci-lint --fast-only + archtest，只跑該檔所屬 module（秒級）
task gate-local            # gen freshness + buf lint + affected modules 的 go test -race（~1 min）
task gate-ci               # 與 CI 等價的全量檢查
task check-gen-fresh-all   # gen freshness 含 TS（= CI gen-freshness；需要網路）；check-gen-fresh 只含 Go / OpenAPI / Wire
task breaking              # proto 對 main 做 buf breaking
task compose-up            # 本機 Postgres :5432 + Redis :6379 + grafana/otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up         # 跑每個 BC 的 migrations；service 啟動不會自己跑
task merge-ready -- <pr>   # merge 前必跑：PR 最新綠燈是不是對「現在的 main + 這個 head」跑的（列 main / merge / base / head SHA 與 check 數）；stale（main 已前進）exit 2，不可合；先 rebase 或更新 branch 取得新 head 與 merge ref，等 CI 跑完再重跑 merge-ready；不要用 `gh run rerun`（沿用舊 merge commit，等於重跑同一棵 stale tree）；head 一變就要重審
task release-snapshot      # goreleaser dry run → bin/dist（真的 release 由 main 上的 CI 綠燈自動觸發，不手動打 tag；沒改到會進 artifact 的檔案就被 gate 跳過，task release-gate 先看：列 releasable 檔 + 預測版號；四個 Go image 加 console image 與 checksums.txt 都由 cosign keyless 簽名、archive 另有 GitHub artifact attestation，發布後 verify job 跑 scripts/release-verify.sh 全驗一遍；snapshot 以 --skip=sign 跳過；CI 的 release-snapshot 另跑 scripts/rekor-v2-canary.sh——cosign 預設 signing config 出現非 v1 的 Rekor 就紅（#204 的 409 處理只適用 v1，見 #215），.github/workflows/rekor-canary.yml 每週再跑一次，紅了不要 rerun release）
task poison -- list        # 看各 BC 的 poison 表；requeue <bc> <offset>…|all 送回原 topic、drop 刪掉（worker poison 子命令）
task prune -- --older-than 720h --dry-run   # 刪每個 group 都 ack 過的 outbox 列 + 消費過的 inbox 列（worker prune；先 dry-run；NOTE 列出每個落後的 group 與最老未讀年齡：held back by = 過了 retention、behind = 還沒，--forget-group 刪掉不會再跑的）
scripts/dex-token.sh [email] [--id-token]    # 本機 Dex（compose 的 `dex` 服務；kind 的 dex.yaml）：authorization code + PKCE 登入拿一顆真 JWT；issuer / 帳號 / 離線 /etc/hosts 見 docs/operations.md「Dex dev IdP」
# 所有讀設定檔的 task（run／worker／poison／bootstrap／prune／migrate）都在命令裡 `export APP_CONFIG=<CONFIG>`，環境裡已有的 `APP_CONFIG` 不會蓋過；Task 的 `env:` 蓋不過 OS 環境（#358），不要再用 `env:` 設 `APP_CONFIG`。`scripts/task-config-test.sh`（CI 的 build job）從真 `task` 入口驗證這一點，路徑含空白也要成立
task bootstrap                              # identity 第一個 account + API key（空 store 才建；worker bootstrap；secret 只印一次）
task helm-check            # deployments/helm/x-monorepo：每個 ci/*.yaml profile 跑 helm lint + kubeconform + check.py（derived 數字：grace period、hook 順序、prune args、console 的 RPC_UPSTREAM、ServiceMonitor / NetworkPolicy 的選擇器）
task k6-wire               # tests/k6/wire.sh：k6 腳本打一個回壞掉 HTTP response（status code / version / header 行帶著請求的 X-API-Key、body 被截斷、連線直接關）的假 server；每個 run 必須失敗，且 key 不在 k6 輸出、summary、metrics tag 任何地方（不需要 Postgres；CI 的 k6-wire job）
task k6-smoke              # tests/k6：ratelimit（429 + Retry-After）/ timeout（1ns → 全部 DEADLINE_EXCEEDED）各自起一個 bin/rpc 打 `identity.v1.IdentityService/ListAccounts`；task k6 -- <script> 打現成 server
task helm-kind             # 真裝，本機全套：同一個 cluster 上跑 `helm-kind-core`（CI 的 `helm-kind` job 跑的就是它）；helm-kind-up 只跑一次；helm-kind-down 刪 cluster
task helm-kind-core        # kind cluster + deployments/helm/kind（拋棄式 Postgres / Redis）+ tree build 的 image（含 console：web-build → web/apps/console/Dockerfile），install → smoke → upgrade → smoke；CI 的 `helm-kind` job
task helm-kind-down        # 刪 cluster。kind task（helm-kind-up／helm-kind／helm-kind-core／helm-kind-down）的目標在命令列指定：`CLUSTER=<名字>`（預設 x-monorepo）、`NS=<namespace>`（預設 x-monorepo）；只設在 OS 環境不算（這兩個是檔案層級的常數，環境蓋不過它），名字只收小寫字母、數字、`.`、`-`。**kubectl／helm／kind 一律不吃你的 current-context 或 `KUBECONFIG`**（這台機器的 `~/.kube/config` 預設可能指向正式環境）。保證來自隔離的環境，不是來自某個 gate 讀對了字：所有 kubectl／helm／kind 指令與 `kind/*.sh` 都經 `deployments/helm/kind/run.sh "<bin/kind/kubeconfig-CLUSTER>" <指令…>`（Taskfile 的 `{{.KIND_RUN}}`）。runner 做的事：`env -i` 清掉 caller 的環境、只放行具名的 env（沒有任何 KUBECONFIG 等價物；容器引擎的 `DOCKER_*`／`CONTAINER_*`／`XDG_*`、proxy、`KIND_EXPERIMENTAL_*`、幾個腳本旋鈕）；HOME 換成空的臨時目錄（沒有 `~/.kube`，結束就刪）；`KUBECONFIG`／`KIND_KUBECONFIG` 設成該 cluster 專屬的檔（`helm-kind-cluster` 用 `kind get kubeconfig --name <CLUSTER>` 寫出，只含那一個 cluster）；PATH 是 `deployments/helm/kind/shim` 加上各工具所在目錄；helm 的 config／cache／data 放在 `bin/kind/helm-<CLUSTER>/`（`helm repo add` 才跨指令留得住）；`BASH_ENV=lock.sh` 讓 `KUBECONFIG KIND_KUBECONFIG PATH HOME` 在 runner 底下每一層 bash 都是 readonly，腳本寫 `unset KUBECONFIG`、`read -r KUBECONFIG PATH <<<…`、`export HOME=…` 都被 shell 擋下（"readonly variable"），值不變。第二層：Taskfile 的 kubectl／helm／`kind create|delete|export` 仍各自帶 `--kubeconfig "{{.KIND_KUBECONFIG}}"`，shim 對腳本裡走 PATH 的 kubectl／helm 補同一個 flag。**直接手動跑 `kind/*.sh` 會被拒絕**（exit 2，指向 task 入口）：每支腳本開頭檢查 runner 留在臨時 HOME 的 `.kind-runner` 標記，且 `KIND_RUNNER_HOME` 等於 `$HOME`（#444）。要用另一個 cluster：`task helm-kind CLUSTER=probe NS=probe`。

**驗證**：只有 `run.sh` 的入口檢查（#435 之後把三層驗證縮成這一個）。每個經 `{{.KIND_RUN}}` 的 kubectl／helm／`kind/*.sh` 在動手前，若 `bin/kind/kubeconfig-<名字>` 已存在，就要求它與 `kind get kubeconfig --name <名字>` 現在回的內容一致（cluster 的 server 位址與 context 名稱）；不一致、讀不出來、或 kind 沒有那個 cluster，就印出原因並 exit 1（fail closed）。context 名稱可以任意取，所以比的是 server 位址，不是名稱。檔案還不存在時放行（kubectl 自己會失敗）；`kind` 子命令不檢查（它們以名稱對容器引擎動作，create／delete 只增減自己在該檔的條目）。不再有靜態掃描 Taskfile 的 gate、`task --dry` 來源矩陣或兩個真 kind cluster 的 CI job；驗證是一次性的：從 `run.sh` 入口對「server 指向別處的 alias kubeconfig」與「與 kind 回報一致的 kubeconfig」各跑一次（結果貼在引入這個檢查的 PR），日後改 `run.sh` 的人照同樣方式重跑。

**沒有被涵蓋的**（threat model）：(a) 腳本寫死 caller 真實的絕對路徑（例如 `/Users/<你>/.kube/config`）；(b) 用 `~user`、`getent passwd` 取得別的使用者 HOME 再去讀；(c) runner 底下的非 bash 子程序（`sh -c`、python、`env X=… cmd`）可以改自己的變數——`lock.sh` 只管 bash，靠 shim 補 flag；(d) `export -n` 能把變數從子程序環境拿掉（結果是子程序沒有 KUBECONFIG、HOME 又沒有 `.kube`，會失敗而不是連到別處）。這些要靠審查，不是 gate 能證明的。
task run                   # build 後以 configs/local.yaml 跑 ./bin/rpc（Connect + gRPC + gRPC-Web 同 port；RPC 在 /api/<svc>/<Method> 與原生路徑都通，/api/docs 開）
```

跑單一測試：`go.work` 根目錄不是 module，`./...` 在那裡比不到任何東西，必須 `cd` 進 module：

```bash
cd pkgs/identity && go test -race -run 'TestAccountLifecycle' ./application/usecase/
cd pkgs/identity && go test -count=1 -run '^TestArchitecture$' .      # 只跑 archtest
go test -race ./infra/postgres/                                       # integration test：testcontainers 自起 Postgres（需 Docker/Podman）
TEST_DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable \
  go test -race ./infra/postgres/                                     # 或指定現成 DB；設了連不上是 FAIL 不是 skip
```

自起的 container（pgtest／redistest）會被回收，不會留在機器上（#481）：正常結束由 `Terminate`；`SIGINT`／`SIGTERM` 由 `pgtest.Main`／`redistest.Main` 裝的 `ctrreap.OnSignal` 立刻回收並以 128+signal 結束（會對自己送 SIGTERM 測 graceful shutdown 的 package——`apps/worker`——不能用 `Main`，TestMain 是 `m.Run` + `pgtest.Terminate()`，沒有 layer 1，靠下一次啟動回收；新增這類測試時照辦）；
`go test -timeout`、panic、`SIGKILL` 留下的，由**下一次**任何用到 pgtest／redistest 的 test 啟動時清掉（`ctrreap.ReapBeforeStart`，每次 start 都掃：只動帶本 repo label、且擁有者 host+pid+啟動時間證明已不存在的 container）。
Podman 上 Ryuk 仍是停用的（`~/.testcontainers.properties`），這裡不改它；CI（Docker）的 Ryuk 照舊。細節與已知限制見 `pkgs/platform/ctrreap` 的 package doc。

前端：`web/` 是 Turborepo（pnpm）。`packages/proto` 是 connect-es client（`task gen-ts` 需要網路，產出到那裡；先過 `tools/stripvalidate` 拿掉 `buf.validate` options 與 `validate.proto`，只留 `Violations`）；
`apps/console` 是 Vite + TanStack Router / Query + Tailwind v4 + shadcn/ui，`task web-dev`（proxy 到 :8080）/ `task web-typecheck` / `task web-build` /
`task web-e2e`（Playwright：`web/apps/console/e2e`，起 `bin/rpc` + vite preview 真打；`E2E_DATABASE_URL` 有設就先 `migrate up` 走 Postgres 並由 `e2e/worker.setup.ts` 多起一個 `bin/worker`（CI），沒設是 in-memory，spec 用 `e2e/mode.ts` 的 `withDB` 分支；project `auth`（`e2e/auth/`）另起一個 `auth.mode=required` 的 rpc + preview 驗 credential 流程（API key 與 `jwt.ts` 現場簽的 HS256 JWT；`helpers.ts` / `jwt.ts` 不是 spec，project 的 testMatch 只收 `*.spec.ts`）；locator 用 `getByLabel` / `getByRole`，第一次先 `pnpm exec playwright install chromium`）；**e2e 的 port 都由 `E2E_RPC_PORT` / `E2E_WEB_PORT` 一處派生**（`e2e/ports.ts`，playwright.config 與 `e2e/assert-clean.ts` 共用；預設 18091／4173）：fixtures 不寫死位址——`oidc-backend` 的 rpc 設定是 `e2e/oidc-backend/rpc-server.ts` 從模板 `rpc.yaml` 依 IdP port 現場產生的暫存檔（SIGTERM 時刪掉，所以那個 webServer 設了 `gracefulShutdown`），logout／「console 自己的 origin」的 assertion 從 project 的 `baseURL` 算。換 port 跑一定要 `CI=1`（否則 `reuseExistingServer` 會接到別人的 server）；`task web-e2e-ports` 是非預設 port 的 smoke（只跑標 `@ports` 的案例，不含 RevokeSessions 的快取等待），跑完檢查所有派生 port 在 `127.0.0.1` 與 `::1` 都已關（只有 ECONNREFUSED 算關，逾時或其他錯誤算失敗；`localhost` 在 IPv6 優先的機器上是 `::1`，Vite preview 綁它）、暫存設定已刪（`e2e/assert-clean.ts` 做這個檢查，它本身沒有 selftest；CI 在 `task web-e2e` 之後跑它）；`task web-e2e-image` 把 image project（`e2e/image-specs.ts`：`image.spec.ts` 加 `version`／`account-loop` 兩支 smoke，其餘 spec 只在 vite preview 跑，`e2e/image-specs.check.ts` 擋下沒分類的新 spec）改打真的 console image（`e2e/image.sh`，`E2E_CONSOLE_URL` 取代 vite preview；`image.spec.ts` 只在這模式跑，驗 nginx 的 fallback / cache header / proxy）。
console 每個頁面（`src/routes/*`）是 `router.tsx` 裡 `lazyRouteComponent` 拆出的獨立 chunk，layout 與 `client.ts` / `queries.ts` 留在首屏；頁面之間共用的東西（`Field`、`ErrorAlert`、`formatMoney` 等）放 `src/components/`，不要讓 layout 或別的頁面 import `routes/<page>` 的 runtime 值（會把那頁綁回首屏，型別用 `import type` 無妨）；載入中 / chunk 載入失敗由 `defaultPendingComponent` / `defaultErrorComponent`（`components/route-status.tsx`，含 Retry）顯示。
**Console 切換 credential（#483）**：`components/credential.tsx` 的 `changeCredential` 先 `cancelQueries` 再 `resetQueries`，不是 `invalidateQueries`。多數 query key（例如列表的 query key）沒帶 credential，快取裡的東西是「當時誰在呼叫」讀到的；invalidate 對「還沒有資料、第一個（匿名）請求還在飛」的 query 不會重送（新請求併進舊的，匿名的 401 成了最終狀態，新 key 的請求根本沒送出），有資料的則會留著舊身分的資料直到新答案取代。cancel 之後舊請求的答案會被丟棄、不會寫進快取，reset 把所有 query 清回「未載入」，畫面上的 query 立刻用新身分重抓。新增 query 不用自己處理 credential（已帶 generation 的 admin probe 一起被 reset，無害）。e2e：`e2e/credential-switch.spec.ts` 用 `e2e/fail-requests.ts` 的 `scriptByKey` 依 `X-API-Key` 腳本化回應、`holdRequests` 扣住某個回應等切換後才放行、`watchRequests` 證明被扣住的是目標請求（三者的契約測試在 `e2e/auth/fail-requests.spec.ts`）（匿名請求還在飛、舊身分的答案晚到、已快取舊身分資料後切換）。重試×切換（舊身分的重試正在飛／正在等 backoff 時切換）也在同一支 spec：cancel 會丟掉在飛的重試、取消還沒觸發的 backoff，新身分立刻重抓。

**Console 的 query 失敗與重試（#483）**：`src/transient.ts` 是唯一一個判斷「失敗的讀取要不要再問一次」的地方（`queryClient.ts` 設成所有 query 的預設，query 自己不寫 `retry`）。只有暫時性錯誤重試：Connect `Unavailable`（502/503/504/429 也映成這個）、`DeadlineExceeded`、`ResourceExhausted`，以及網路錯誤（connect-web 把 fetch 的 `TypeError` 包成 `Unknown`，`cause` 是那個 `TypeError`；沒有這個 cause 的 `Unknown` 不重試）；其餘（PermissionDenied、Unauthenticated、NotFound、InvalidArgument、FailedPrecondition…）是答案，立刻顯示、只問一次。只看 Connect code，不比對訊息文字。上限：最多重試 3 次（共 4 個請求），等 500ms、1s、2s（倍增、封頂 2s），錯誤顯示前最多多等 3.5s。mutation 一律不自動重試（`mutations.retry: 0`；會改狀態，使用者再按一次）。單元測試 `task web-test`（CI 的 web job 跑）：`src/transient.test.ts` 每個 Connect code 一格；`src/queries.retry.test.ts` 是整合矩陣——用真的 `QueryClient` 預設（`queryClient.getDefaultOptions()`）、`queries.ts` 匯出的每個 query 與它真實的 queryFn，網路由 `src/testing/client.stub.ts` 假的（`src/testing/loader.mjs` 在 node 裡把 `./client` 換掉並處理無副檔名 import 與 enum），時鐘用 mock，每個 query 兩格：一次 Unavailable 後等 500ms 重試並成功、InvalidArgument／PermissionDenied 只問一次；新增 `*Query` 匯出沒進 `CASES` 會紅；`src/retry-overrides.test.ts` 掃 `src`（排除 `*.test.ts`），`queryClient.ts` 以外出現 `retry`／`retryDelay`／`new QueryClient`／`setDefaultOptions` 等都會紅，除非列在 `EXCEPTIONS`（附理由與對應測試）；e2e `e2e/transient-recovery.spec.ts` 用共用的 `e2e/fail-requests.ts`（`failRequests`／`holdRequests`，可依 `bodyIncludes` 只打中某個請求）注入失敗與扣住回應。
**OIDC 登入（#273，`docs/identity-login.md` §8）**：`src/oidc/`（`config.ts` 讀同源 `/config.json`、`pkce.ts`、`discovery.ts`、`session.ts` = 登入 / callback / refresh rotation / 登出、`unbound.ts`）+ `routes/login.tsx`、`routes/auth-callback.tsx`（redirect URI `/auth/callback`）。設定是 **runtime**：nginx image 的 `deploy/18-console-env.envsh` 驗證 `CONSOLE_OIDC_ISSUER` / `CONSOLE_OIDC_CLIENT_ID` / `CONSOLE_API_KEY_LOGIN`，envsubst（精確白名單）渲染 `/config.json` 與 CSP（`connect-src 'self'` + issuer 的精確 origin；兩個都沒設 = OIDC 關閉；只設一個、含控制字元或格式不合 = container 啟動失敗）；Helm 先走 `console.extraEnv`。access token 只在記憶體（`auth.ts` 的 `setSessionCredential` / `rotateSessionToken`，不碰 localStorage）、refresh token 在 sessionStorage 一個 key；PKCE verifier / state / nonce 在 callback 讀取當下就刪。refresh 失敗：IdP 拒絕（400/401/403）或 `expires_in` 不是 10s–24h 的整數 = 登出，沒回應 / 5xx / 429 = 保留 token + 全域 backoff（人按 Retry 可突破）+ Retry；access token 到期當下就撤下（refresh 在飛行中也一樣，晚到的成功回應仍接受）= 回 /login 顯示 Retry；期限取 `expires_in` 與 JWT `exp` 較早者（都沒有 = 60s，< 10s 拒絕）；對 IdP 的請求 10s timeout。discovery 的 token endpoint 必須與 issuer 同 origin（console 自己比對，CSP 擋不住 'self'）。API key 入口是 `/login` 下方的 break-glass，`apiKeyLogin=false` 時不在 DOM。`/accounts/$id` 的 Sign-in bindings 卡讀 `ListAccountIdentities`（admin only，只顯示 contract 欄位）。後端回 `auth.unbound_identity` 時 `components/claim-invite.tsx` 取代頁面。Playwright：`e2e/auth/oidc*.spec.ts` 用 `e2e/auth/oidc.ts` 以 `page.route()` 當 IdP（簽 HS256 token 給真的 auth project rpc），project `oidc-backend`（`e2e/oidc-backend/`：`idp-server.ts` 是 loopback ES256 IdP、`rpc.yaml` 開 `auth.oidc` 的真 rpc；驗 server 接受 token、真的 `auth.unbound_identity` → ClaimInvite 綁定、`invite.email_unverified`、`RevokeSessions` 後舊 token 被拒（等 30s resolver cache）；`oidc-dex`（`E2E_DEX=1` 才存在，`task web-e2e-dex`；`e2e/oidc-dex/`：真 Dex 登入表單 → 未綁定 → `CreateInvite` + `ClaimInvite` → 進 Console，每個測試自己起一個只信任 compose 的 Dex 的 rpc（`rpc.ts` fixture，retry 也重來；Dex 帳號固定、binding 跟 rpc 同壽）、console 在 port 4203——該 origin 與 `web.allowedOrigins` 都得登記在 `deployments/docker/dex/config.yaml`）；`e2e/image.spec.ts` 驗 image 的 `/config.json` 與 CSP（`image.sh` 另起 OIDC 設定的容器，並驗證各種錯誤 env 讓容器拒絕啟動）。
shadcn component 用 `pnpm dlx shadcn@latest add <name>` 加，產出的 `cn` import 要確認是 `@/lib/utils`。

**新 clone / 新 worktree 第一件事**：`task tools`。`./bin` 是 gitignored，而 `task gen`（也被 `gate-local` 間接呼叫）會**先刪掉**
所有 `*.pb.go` / `*.connect.go` 再呼叫 `bin/protoc-gen-*`。所以 `gen`、`gen-openapi`、`wire`、`check-gen-fresh`、`check-gen-fresh-all`
的第一個動作是 `tools-check`（internal task）：它要用的工具有任何一個不存在或不可執行，就以 exit 1 結束並提示 `task tools`，
在那之前不刪、不寫任何檔（不會自動安裝：gate 不靠網路）。`scripts/gen-tools-test.sh`（CI gen-freshness job 的一個 step）從真入口驗這件事。
萬一仍遇到生成檔被刪光的 tree（例如工具跑到一半失敗），就 `git checkout -- pkgs` 還原，**不要**用 `git commit -a` 把刪除一起 commit 進去。
生成物（pb.go、connect.go、`wire_gen.go`、`contract/v1/openapi.yaml`）全部有 commit，gen-freshness 靠 `git diff` 比對它們。
**repo／worktree 路徑的限制**：只有 codegen（`gen`、`gen-openapi`、`wire`、`check-gen-fresh(-all)`）、`lint`、`breaking` 這幾個入口經 `scripts/gen-tools-test.sh` 驗證過，路徑含空白或 `"`、`'`、`$`、反引號時仍找得到工具（#505、#508）。
其他 task（`build`、kind／helm、`run` 類等）在路徑含空白或 shell 特殊字元時都沒有保證，例如含空白或 `"` 時 `task build` 會失敗（#509）。建議 repo／worktree 路徑只用 `[A-Za-z0-9._/-]`。

## Hook harness（會擋你的東西）

`.claude/settings.json` 掛了兩個 hook，exit 2 時 stderr 會回到 agent：

- **PostToolUse（Edit / Write）**：對 `.go / .proto / go.mod / go.work / buf*.yaml` 跑 `task gate-fast`。
  用 Bash（sed、heredoc）改檔**不會**觸發，改完請手動 `task gate-fast FILE=<path>`。
- **Stop**：跑 `task gate-local`，失敗就不讓你宣告完成。gen-freshness 是和 HEAD 比 diff，
  改了 proto 或 wire provider 後要 `task gen wire` 並把產物一起 commit。

CI（`.github/workflows/ci.yml`）另外有 `work-sync`（go.mod 不得漂移，加依賴後 `task tidy`；Taskfile 的 `MODULES` 由 go.work 的 `use` 導出，新增 module 只加 `use`；`scripts/modules.jq` 是路徑唯一的規則——只收 `[A-Za-z0-9._/-]`、空的 `use` 清單也不行，每個 task 都會失敗；`scripts/check-modules.sh` 在 CI 跑同一份）、
`cover-gate`（所有 `*/domain/*` package 必須 100% 覆蓋）。PR 上每個 job 只在輸入有變時跑（`scripts/ci-changes.sh` 決定；main 一律全跑），所以一支 PR 沒跑 helm-kind / k6 / k6-wire 是正常的，不是壞掉；改 `scripts/ci-changes.sh` 的對照表要一起改 `scripts/test.sh`（`task scripts-test`）。

## 架構：誰依賴誰

```
pkgs/kernel      shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
pkgs/platform    技術共用：contextx、config、server（gin + Connect）、usecasex、otel、postgres、archtest；
                 admin（admin.v1.AdminService：poison 操作 + Prune RPC，與 worker 的 CLI 同一組 eventbus 函式；contract 在 protos/admin，不屬於任何 BC）
pkgs/<bc>        一個 bounded context = 完整六角形（目前只有 identity，沒有 event；有 event 的版本〔consumer／events adapter／outbox／inbox／poison〕在目前主線沒有範例，完整版在 https://github.com/blackhorseya/x-monorepo-reference）：
                 identity：DB 管理的 account / API key，沒有 event；`adapters/auth` 實作 platform 的 `server.CredentialResolver`，auth interceptor 對 `X-API-Key` 先查 config `auth.api_keys` 再問它（TTL cache 30s）；`auth.oidc.enabled` 時 `Authorization: Bearer` 先交 `platform/oidc` verifier（`iss` 不認識才走 `auth.jwt`，其他失敗終局），驗過的 `(iss, sub)` 經 `adapters/auth.ExternalCredentials` 解成 account 的 principal（`Subject` = account id、roles 取自 account、不看 token claim；停用 / `iat` < `sessions_revoked_at` / DB 錯誤都拒絕且不 cache），未綁定的身分只能呼叫 `ClaimInvite`（其餘 `PERMISSION_DENIED` `auth.unbound_identity`），有 `X-API-Key` 時不看 Bearer——`docs/identity.md`「外部身分綁定」；key 通過驗證會節流、best-effort 地寫 `last_used_at`（`auth.last_used_interval`，預設 1m，寫入失敗不影響驗證）；`DisableAccount` / `AssignRoles` / `RevokeApiKey` 有 last-admin guard：會讓最後一個有效 admin（ACTIVE、有 `auth.admin_role`、至少一把未撤銷未過期 key）失格就回 `identity.last_admin`（`FAILED_PRECONDITION`），同一 transaction 內先 `port.AdminCensus.LockAdmins`（Postgres `FOR UPDATE` 鎖 admin 列、再另一條 statement 算 key；memory 靠 Transactor 串行）再載入 aggregate，「現在」也在拿到鎖之後才讀（`LockAdmins` 回傳，等鎖期間過期的 key 不算），config 的 static key 不算——`docs/identity.md`）
  provider.go      wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorate() / decorateTx() 包起來
  contract/v1/     buf 生成的 wire format（pb + Connect + openapi.yaml），只有 adapters / apps 可 import
  domain/<agg>/    aggregate、VO、Repository interface、domain errors（手寫，不是 proto）
  application/     dto（手寫 application 邊界）、usecase（orchestration）、port、mapper
  infra/           driven：memory / postgres（gorm row model + embed migrations）/ system（clock、id）
  adapters/        driving：rpc（Connect handler，pb ↔ dto）、consumer（broker handler，有 event 的 BC 才有）
apps/rpc         composition root：main + wire.go，無業務、無 mapping；沒有任何下游呼叫，不需要 upstream 就能啟動；`rpc` 只有 `--version`／`-v` 一個參數
apps/worker      事件 consumer：每個 BC 的 Subscriptions 掛同一個 Watermill router，各 BC 自己的 consumer group / poison；
                 worker.broker=redis 時自己當 forwarder 把 outbox 表轉進 Redis Streams 再讀（outbox 仍是寫入路徑）；
                 `worker poison list|show|requeue|drop` 操作各 BC 的 poison 表（requeue = 重新 insert 回原 topic 的 outbox 表）；
                 `worker prune --older-than <d>` 是 outbox / inbox 的 retention（poison 表不清）；`worker bootstrap` 是 identity 的第一個 account + key（adapters/cli，獨立的 wire injector；identity 沒 event 所以不在 router 裡）；
                 有 DB 時 `eventbus.Lag` 另報每張 outbox 表 / inbox 的大小與最老一列年齡（`eventbus_outbox_*` / `eventbus_inbox_*`），alert 用它抓 prune 沒跑與 stale group
apps/migrate     收集各 BC 的 Migration()，唯一能 import infra 的 app
deployments/helm Helm chart：rpc / worker Deployment、migrate pre-install/pre-upgrade hook Job、prune CronJob、console（nginx，/api proxy 到 rpc Service，`console.enabled` 預設 off）、
                 選配 rpc ServiceMonitor 與 ingress-only NetworkPolicy（rpc 永遠放行 console；`ingressFrom` 空 = 任何來源）；terminationGracePeriodSeconds
                 從 rpc.shutdown / worker 的 drain 算出來（改 shutdown 行為要一起改 values 註解與 check.py）；CI 的 helm-kind job 在 kind 真裝 + smoke
```

**archtest（`pkgs/platform/archtest/imports.go`）強制的 import 方向**，每條規則跑在「被判斷的檔案所在的 module」的 `arch_test.go`（BC 的 `archtest.Run`、`pkgs/platform` 的測試［含 `pkgs/kernel`］、每個 app 的 `archtest.RunImports`）。BC 是 `pkgs/*` 去掉 kernel、platform，不用逐 BC 列規則；新增 app 要自己加 `arch_test.go`，否則沒有 import 規則檢查它。「任何 BC 的 domain / application / infra 不 import 其他 BC 的 contract」目前沒有規則（`bc-isolation` 只擋其他 BC 的 domain / application / infra / adapters），靠 review：

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
  它們從 context 拿到 tx。
- **Domain error** 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 Connect code，
  `Code` 放 `x-error-code` header。consumer 把 domain error 視為 drop（不重試），infra error 才 retry——所以「業務上的失敗結果」
  要做成 aggregate 狀態 + event，不能做成 error；反過來 optimistic-lock 的 `account.ErrStale` 等刻意不是 `kernel.Error`：
  它們 wrap `kernel.ErrConcurrentUpdate`，consumer 會 retry，rpc 回 `Aborted`（`x-error-code: concurrent_update`），metrics 記 `kind=aborted`。
- **Optimistic lock**：aggregate 帶 `version`，`Save` 比對後 +1。存完後記憶體中的 aggregate 版本**不會**前進，要再存一次請先 reload。
- **Validation**：request 形狀由 protovalidate interceptor 擋（`x-error-code: request.validation_failed` + `Violations` detail），
  consumer path 沒有它，所以 domain 的檢查不能拿掉。
- **Auth 只做 authentication**：`auth.mode` 預設 `off`（`task run`、E2E、console 都沒帶 credential）；開了之後 `server.NewAuthInterceptor`
  把 `X-API-Key`（config `auth.api_keys` 先、再問 `server.CredentialResolver` = identity 的 DB key；resolver 失敗是拒絕不是放行）/ `Authorization: Bearer <jwt>` 解析成 `contextx.Principal` 放進 context（identity key 的 `Subject` 是 account id、`CredentialID` 是 key id），usecase 用 `contextx.PrincipalFrom(c)` 讀，
  consumer path 永遠是 anonymous。`rpc` access line 的 `auth` / `sub` 與 span 的 `enduser.id` 由 auth interceptor 透過 logging interceptor 留在 context 的 slot 回報（`recordAuth`），不要把 principal 塞進 metrics label——它記的 `auth_decisions_total{result, method}` 只有 ok / public / anonymous / missing / invalid × api_key / jwt / none（Grafana 的 Auth row 與 `rpc-auth` alert 讀它；OIDC 的 `method` 仍是 `jwt`，未綁定放行 `ClaimInvite` 記 `ok`，未綁定被擋記 `invalid`，不加新值）。要不要做授權（role 檢查）是讀的人的事，不要在 interceptor 裡加（role 檢查在 usecase 或 `server.RequireRole`，見 `docs/identity.md`）。
  Interceptor chain 由外而內：`tracing, meta, logging, [rate limit], [auth], [timeout], validate, refuseExpired`——rate limit 在 auth 之外（洪水先擋再驗簽章）、
  timeout 在 auth 之內（deadline 算 validate + usecase）；`http.timeout` 到期的失敗一律 `DEADLINE_EXCEEDED` + `request.timeout`，超過 `http.rate_limit` 是 `RESOURCE_EXHAUSTED` + `request.rate_limited` + `Retry-After`。
  `refuseExpired` 在最內層：context 在進 handler 前已逾時或取消就不執行 use case（v1 的 generated unary wrapper 做這件事，v2 的 generated handler 不做）。
  interceptor 是 connect-go v2 的 `connect.ServerInterceptor`，套用到每一種 RPC 型別，錯誤的 metadata 用 `server.WithMeta` 掛在 error 上（`pkgs/platform/server/errmeta.go`）。
  目前 protos 沒有 streaming RPC；哪天加了，tracing / meta / logging 每個呼叫各一次（一個 span、一行 access line），rate limit 每個呼叫扣一個 token，
  auth 只在呼叫開始時看一次 request header、整條 stream 共用，timeout 的 deadline 蓋住整條 stream（長連線的 stream 會在 `http.timeout` 被切），
  validate 對 handler 每次 `Receive` 到的訊息各驗一次，`refuseExpired` 只在呼叫開始時查一次。
- **Graceful shutdown 是有測試保護的行為**：rpc 收到 SIGTERM 先把 `/api/readyz` 翻 503 再服務 `http.shutdown_delay`（預設 0），然後關 listener、
  in-flight request 有 `http.shutdown_timeout`；worker 的 `eventbus.Register` 把 subscriber 包進 `eventbus.Draining`，正在跑的 handler 跑完 + ack 才結束
  （上限 8s）。每次 handler attempt 另有 `worker.handler_timeout`（預設 10s，0 = 不限）：到期的 attempt 照常 retry，用完進 poison。改 `Server.Serve` / `Draining` 請一起跑 `platform/server` 與 `platform/eventbus` 的 shutdown tests。
- **Response meta 走 header**（`X-Request-Id`、`X-Trace-Id`、`traceparent`），不包進 proto message。
- **Persistence**：每個 BC 一個 Postgres schema（`identity.accounts`、`identity.schema_migrations`）；SQL 放
  `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql` 由 BC embed（go.work 下 embed 不能跨 module）。
  `postgres.url` 空 → `*gorm.DB` 為 nil → 各 BC 的 `Provide*Repository` 改用 in-memory，所以 `task run` 與 E2E test 不需要 DB。
- **`wire.go` 是 AppConfig → component config 的唯一 mapping 點**。

## Config

`platform/config.Load()`：defaults → YAML（`$APP_CONFIG`，否則 `./configs/config.yaml`）→ `APP_*` env，由低到高。
`configs/local.yaml` gitignored，`task run` / `task migrate` 第一次會從 `config.example.yaml` 複製。
注意：viper 忽略空 env，`APP_POSTGRES_URL=` 清不掉 key，要「不接 DB」請改 local.yaml。完整 key 表見 `docs/config.md`。
`auth.*`（mode / api_keys / jwt / public）見 `docs/config.md`。

## 新增東西的步驟

**新增 RPC**：改 `protos/<bc>/v1/service.proto`（request 欄位規則用 `buf.validate` 寫在 proto，interceptor 會擋；domain 仍保留不變量）→ `task gen`（`task breaking` 檢查相容；允許 wire-incompatible 的變更，CI 沒有 breaking job，`task breaking` 只在本機提示）→ domain 加行為 →
`application/dto` + `usecase` → `provider.go` 加 `ProvideXxx` 並用 `decorate()`（讀）或 `decorateTx()`（寫）包 → `adapters/rpc` 實作
（`var _ XxxServiceHandler` 會提醒）→ `task wire`。

**新增 Bounded Context**：`task new-bc -- <name> [--events]`（`tools/newbc`，樣板在 `tools/newbc/templates/`，skill 在 `.claude/skills/new-bc/SKILL.md`）。
它產生 `protos/<name>/v1/*`、`pkgs/<name>/**`（一個 aggregate、`provider.go` 匯出 `RPC`、`arch_test.go`、`0001` migration）並在 `go.work`、`buf.gen.yaml`、`apps/{rpc,migrate}/go.mod`、`apps/rpc/{wire.go,app.go}`、`apps/migrate/main.go` 各加一筆（插在 `new-bc:*` 錨點註解上方，不要刪或複製錨點）；
`--events` 另產生 event、outbox、同 BC 的 consumer、inbox、`0002_events`／`0003_inbox` migration，並加 `apps/worker/{go.mod,wire.go,consumers.go,poison.go}`。名稱過 `scripts/bcs.jq`（`^[a-z][a-z0-9]*$`）、最長 30 字元、不是 Go keyword／`kernel`／`platform`，任何一條不過或 BC 已存在都在寫任何檔之前拒絕。
接著自動跑 `task gen` → `scripts/sync-fixpoint.sh`（tidy 到沒有 diff）→ `task wire`；之後手動：`task gen-ts`、把樣板改成真正的領域、commit tidy 的結果。`task new-bc-check` 在工作樹複本（`git ls-files -co --exclude-standard`，含未 `git add` 的新檔）產生兩種樣本並 lint／build／test；CI 的 gen-freshness 也跑它，**但沒有 DB，不跑 postgres repository 的測試**。
`--events` 的 BC 需要 Postgres：`apps/worker` 在 `postgres.url` 為空時拒絕啟動（`apps/worker/main.go:63-65`）。
有跨 BC 的 consumer、List、stats projection 等樣板沒有的東西，照 `pkgs/identity` 與 `docs/workflow.md` 加。

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
    例（在固定 RED SHA 的 named worktree 裡跑，不要切換自己綁定 worktree 的 HEAD）：`cd <RED SHA 的 named worktree> && go test -run '^TestX$' ./pkgs/identity/... 2>&1 | scripts/red-stage.sh -m 'want 1 call'`
