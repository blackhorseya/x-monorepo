# CI 與 gates

<sub>← [README](../README.md)</sub>

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS；先跑 `scripts/gen-tools-test.sh`：`gen`／`gen-openapi`／`wire`／`check-gen-fresh(-all)` 在 `./bin` 少任一工具時必須在刪寫任何檔之前失敗並提示 `task tools`，不含 git、對整棵 fixture 做雜湊；fixture 的父路徑含空白，以及 `"`、`'`、`$`、反引號時，各入口的工具也必須找得到，#504、#507）、lint、work-sync、test（matrix by affected module）、coverage、build（`task build` 之後跑 `scripts/task-config-test.sh`：每個讀設定檔的真 `task` 入口，`CONFIG`／`CATALOG_CONFIG` 必須贏過父環境的 `APP_CONFIG`，路徑含空白也一樣，#414）、helm（`task helm-check`；pull request 上另跑 `deployments/helm/check-chart-bump.sh`：chart 的 templates/values 有變，`Chart.yaml` 的 version 必須比 base 高，#423）、helm-kind（`task helm-kind-core`：kind 真裝 + smoke + upgrade + `negative.sh` + `outage.sh`）、helm-kind-keda（`task helm-kind-keda`：KEDA；驗 ScaledObject 能 render、被 KEDA 接受且 `--wait` 成功，「依 consumer lag 實際擴縮」暫時沒有載具，等切片 3 的示範 BC），各自一個 job、一個 cluster，略過時各留一個 skipped check。helm-kind 的觸發條件：product Go、web、helm、image 檔。其餘 job：k6（`task k6-smoke`：rate limit / timeout 兩個 k6 腳本打真的 rpc + Postgres，各有自己的 rpc port、catalog-rpc 與 RUN_ID，隔離表在 `tests/k6/smoke.sh` 檔頭，#451）、k6-auth（`task k6-auth-matrix`：每種 auth 組合）、k6-wire（`task k6-wire`：壞掉的 HTTP response 不能把 key 帶進 k6 輸出；不需要 Postgres 和 rpc；各 cell 分 `WIRE_JOBS`（預設 4）條 lane 同時跑，每條 lane 自己的 port，輸出依 cell 緩衝後照順序印出，每格開始前清掉它的 log／summary／metrics，k6 以 `--tag wirenonce=…` 在每個 sample 蓋上本次呼叫的 nonce，判定要求這些檔案存在且帶本次 nonce；cell 沒有 PASS 行、輸出讀不到或缺 `.rc` 一律算 FAIL；用到的外部指令（grep、cat、sleep、rm 等）退出碼異常也算 FAIL，`tests/k6/parallel_test.sh` 逐一注入 exit 42，#451／#463）、release-snapshot、console-e2e / console-image（console-image 只跑真的需要 nginx image 的 spec：`image` project = `e2e/image-specs.ts` 的 `image.spec.ts` 加 `version`／`products` 兩支 smoke（真瀏覽器經 nginx 載入各路由家族，CSP 或資產路徑壞掉會紅），其餘 spec 留在 console-e2e 的 vite preview；`e2e/image-specs.check.ts` 用 `playwright test --list` 斷言每支 spec 恰好歸一類、image 集合等於釘死的那三支，漏標、重複標、多標、少標都會擋下，`task web-e2e-image` 先跑它與 `image-specs.selftest.ts`；`task web-e2e-image-mutants` 用壞掉的 nginx 設定證明留下的 spec 會紅，不進 CI，#449）、actions-resolve、scripts-test（`scripts/test.sh`：ci-changes、affected、releasable、cosign-sign-blob 對拋棄式 git repo 或假 cosign 的測試，加 k6-auth 分片、cache 寫入規則、release image 清單、MODULES 推導與 console OIDC env 的檢查——這些規則寫錯會靜默跳過 job 或漏掉東西，所以釘住）；`buf breaking` **只警告**。PR 上每個 job 只在它的輸入有變時跑（`scripts/ci-changes.sh`：只改 `*_test.go` → gen-freshness、lint、work-sync、coverage 與 test matrix，整合 job 都不跑（非 `_test.go` 的測試 helper 算產品碼）；產品 Go（含 `go.mod`／`go.sum`／`go.work`、`.golangci.yml`、`configs/`）→ 再加 build、console-e2e、helm-kind、k6-wire；proto → 另加 breaking、web、console-image；`web/` → gen-freshness、web、console-e2e、console-image、helm-kind；`deployments/helm/` → helm、helm-kind、helm-kind-keda；`.github/` → actions-resolve。較窄的觸發：console-image 只看 web、proto 與 image 檔（不看純 Go）；release-snapshot 只看 `.goreleaser.yaml`、`deployments/docker/Dockerfile*`、`.dockerignore`、`release.yml`、`go.mod`／`go.sum`／`go.work`（console image 由 release.yml 用 buildx 推，不在 snapshot 內）；helm-kind-keda 只看 helm、`apps/worker`、`pkgs/platform`、image 檔與 `go.mod`／`go.sum`／`go.work`；k6 只看 `tests/k6`、`pkgs/platform/server` 與 `pkgs/platform/config`（rate limit、timeout 的實作與設定）、`apps/rpc`、`apps/worker`、`configs/`、`go.mod`／`go.sum`／`go.work`，業務流程由 helm-kind 覆蓋；k6-auth 只看 `tests/k6`、`pkgs/platform/server` 與 `pkgs/platform/config`、`pkgs/identity`、`apps/rpc`、`configs/`；k6-wire 跟所有產品 Go 與 `tests/k6`（它防 key 洩漏，#209）。判斷不出來就跑：沒被這些規則明確排除的路徑維持原行為；ci.yml / `Taskfile.yml` / `scripts/` → 全部；`.goreleaser.yaml`、`go.work`、`tests/k6/auth-matrix.sh`、`tests/k6/catalog.sh`、`web/apps/console/deploy/18-console-env.envsh`（`scripts/test.sh` 讀取或 source 它們）另外觸發 scripts-test；docs / Grafana / compose → 幾乎沒有），**main 上一律全跑**（它是 release 的 gate） | 依改動 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改某個 BC → 該 BC + 依賴它的 apps）
- `scripts/ci-changes.sh`：由改動的路徑算出 ci.yml 每個 job 的 `run_<job>` 旗標（`affected` job 的 outputs，其餘 job 都 `if:` 它）；本機 `scripts/ci-changes.sh` 看這支 branch 會跑哪些，`CI_ALL=1`（全部 true）是 main 與 dispatch 的行為；對照表由 `scripts/test.sh`（`task scripts-test`）釘住代表性的規則（每條規則留一個只改單一路徑的案例），`ci.yml` 的每個旗標都宣告為 `affected` 的 output 且只被一個 job 的 `if:` 讀取。helm-kind、helm-kind-keda、k6-auth、k6-wire 是各自獨立的 job（不用動態 matrix），略過時留下 skipped 的 check。k6-auth 內部是**靜態** matrix（`shard: ["1/3","2/3","3/3"]`，`K6_AUTH_SHARD=<i>/<n>`：`K6_AUTH_LIST=1 tests/k6/auth-matrix.sh` 列出的 11 個 cell 依序號 `(p-1) % n == i-1` 分組），每個 leg 一台 runner、一個自己的 Postgres service，所以 DB、worker consumer group、outbox backlog、port、catalog-rpc 都不跨組共用，隔離靠分開的 runner 而不是清理（#448）；`scripts/test.sh` 釘住 shard 為 1/n…n/n、各組聯集等於全集且無重複，`auth-matrix.sh` 對不存在的 cell 名稱報錯（不再靜默什麼都沒跑）。本機 `task k6-auth-matrix` 與 `task k6-auth-matrix -- <cell…>` 不變維護規則：`scripts/test.sh` 的案例讀取 `scripts/`、`ci.yml`、`Taskfile.yml`、`.github/actions` 以外的檔案時（目前是 `.goreleaser.yaml`、`go.work`、`tests/k6/auth-matrix.sh`、`tests/k6/catalog.sh`、`web/apps/console/deploy/18-console-env.envsh`），要同步把該路徑列進 `ci-changes.sh` 的 `scripts_src`，否則只改那個檔案的 PR 不會跑 scripts-test
- `scripts/merge-ready.sh <pr>`（`task merge-ready -- <pr>`）：**merge 前必跑**。`pull_request` run 驗的是 `refs/pull/N/merge`——PR head 合進「run 開始當下的 main」；main 之後前進，這顆綠燈就不再是 main + 這個 PR 的結果（#187 改了一個 constructor 的簽章、#188 新測試還用舊簽章：各自綠、git 無衝突、合起來編譯失敗）。腳本取該 head 最新的 ci.yml run、找出它的 merge commit，比對 merge commit 的 base parent 與 main 現在的 HEAD、head parent 與 PR head；並確認 run 已完成、check 齊全且無 pending / fail / cancel。輸出 main / merge / base / head 四個 SHA 與 check 數。exit 0 可合、1 check 未完成或失敗、2 stale（不可合；先 rebase 或更新 branch 取得新 head 與新 merge ref，等新 CI 跑完再重跑 merge-ready。**不要用 `gh run rerun`**：rerun 沿用原本的 merge commit，等於重跑同一棵 stale tree，綠燈仍是舊 main 的結果。head 一變就要重審）、3 用法錯
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- Go cache（`.github/actions/setup`，#450）：兩種 cache，**只有 default branch 上的 `push` 與 `workflow_dispatch` 會寫**（`.github/actions/setup/plan.sh` 是唯一的判斷處，每個 cache step 只讀它的輸出，且每個會 save 的 step 都有一個 `if:` 剛好相反的 `actions/cache/restore` 雙胞胎；`scripts/test.sh` 只釘住「`pull_request`、`pull_request_target`、`workflow_run`、`schedule`、`issue_comment` 這些低信任觸發不可寫」；#461）。GitHub 對其他解析到 default branch 的 trigger（`workflow_run`、`pull_request_target`、`issue_comment`……）只給 read-only，pull request 寫的是 `refs/pull/N/merge`，只會把 main 要用的項目擠出去。以前每個 job／matrix 項目各存一份 mod＋build cache，一個 PR 一 miss 就存整組：2026-11-04 兩支 PR 讓 repo cache 到 14.8GB（上限 10GB），main 的 23 份被擠光，之後每個 job 冷編譯（870–926 次 compile、75–116 秒），冷的 module 下載也撞上 proxy 的 HTTP/2 `INTERNAL_ERROR`。現在：
  - **module cache**：全 repo 一份 `gomod-<os>-<go.sum hash>`，只放 `$GOMODCACHE/cache/download`（zip，約 0.26GB；module 在第一次用到時才解開，apps/rpc 約 2 秒，比多還原 400MB 的展開樹便宜）。module 版本不可變，同一份 go.sum 永遠有效，go.sum 變了才換一份，`restore-keys` 退回上一份。非 exact hit 時（main 上 go.sum 第一次變動、或 dependabot 類 PR 補差額）才跑 `go-mod-download.sh`：整個 workspace 的 `go mod download`，最多 3 次、2／4 秒退避，只重試下載，最後一次的退出碼原樣傳出，log 只寫固定訊息與 exit code，不轉印 proxy 的文字（不用 `gh run rerun` 蓋掉失敗）。
  - **build cache**（GOCACHE）：依「怎麼編譯」分 5 組，`gobuild-<os>-<group>-<ISO 週>`，每組只有一個指定的存檔 job，其餘同組唯讀：`race`＝test matrix 的各個 leg（存檔：`test-apps/rpc`，compile 集合最大）；`check`＝lint、gen-freshness、work-sync、coverage、scripts-test、affected（存檔：lint）；`bin`＝build、k6、k6-auth、k6-wire、console-e2e、console-image、helm（存檔：build）；`kind`＝helm-kind、helm-kind-keda（CGO=0 linux -trimpath；存檔：helm-kind）；`cross`＝release-snapshot（存檔：它自己；release.yml 是 `workflow_run`，只讀同一份）。對照表在 `plan.sh`，沒列的 scope 只有 module cache、沒有 build cache（不會悄悄長出新項目）。key 不含 go.sum（依賴變了仍吃得到大部分舊 object），`restore-keys` 只退回同組上一週。
  - **取捨（新鮮度）**：key 是週，當週第一次 main run 存完就是 exact hit，整週不再存，GOCACHE 逐日變舊。實測（run 41938380769 的 `exp-stale` 實驗，apps/rpc、`-race`、`go test -run '^$'` 數 `compile` 次數；先用 K 個 commit 之前的原始碼填 cache，再在 HEAD 量）：K＝0 → 0 次／3–4 秒；K＝25（約 1–2 天）→ 164 次／23 秒；K＝75（約 3.5 天）→ 270 次／29 秒；K＝150（約一週）→ 292 次／31 秒；沒有 cache → 926 次／86–111 秒。所以週末的 cache 仍省下約 68% 的編譯次數、約 65% 的時間，比起剛存好的多付約 28 秒。改成日 key 能把週末的 28 秒壓到約 20 秒內，但每個 main push 日要上傳整組（約 1.3GB），而且舊世代要靠 LRU 或主動刪除退場（沒有 `actions: write` 的刪除 job 時，最多會有 7 個世代並存，超過配額），所以維持週；`plan.sh` 的 `week` 是唯一的旋鈕。
  - **容量**（上限 10GB）：實測冷啟動的 main-like dispatch run（41938380769，逐個 post-step 加總）共存 8 份、2,129MB：module 405、race 306、check 183、bin 146、kind 137、cross 556、tools 102、Playwright 293（舊設計同樣的 run 是 28 份、8,460MB，run 41931769522）。再加 pnpm 約 74MB、golangci-lint 約 10MB。穩態一個世代約 2.2GB；換週當下 5 組 build cache 新舊並存（module、tools、Playwright 不換週）＝ 2×1.33＋0.41＋0.10＋0.29＋0.09 ≈ 3.5GB，離 10GB 還有 6GB 以上，可容納 其他小 cache（見下）。LRU 依最後存取日由舊到新淘汰、7 天沒被存取的也會被移除：上一週的 build cache 在新世代存好後沒人讀，最先退場；當週世代每個 main run 都被讀。保證條件是「2×build＋module＋非 Go＋PR 寫入 ＜ 10GB」，不靠 LRU 運氣；這個式子的左邊目前是 3.5GB＋PR 寫入。
  - **race 組只有一個存檔 leg**：test matrix 各 leg 的 compile 集合 95% 以上重疊（冷啟動 870–926 次，pkgs/kernel 135、tools/stripvalidate 174 除外），存檔者是 `test-apps/rpc`（926 次）。其他 leg 在「只有 rpc 的 cache」下還要編譯的 package（同一個實驗，K＝0，每個 leg 從 rpc 的 cache 快照開始）：catalog-rpc 7 次／4 秒、migrate 3／3、worker 7／7、pkgs/catalog 19／6、identity 24／9、kernel 2／1、platform 37／14、stripvalidate 2／2（冷啟動各約 66–112 秒）。差額是各 leg 自己的 package，不需要像 module cache 那樣由專用 step 補滿 union。
  - **matrix job**：一個 matrix 的各 leg 若沒有各自的 `cache-scope`，就共用 `github.job`，彼此搶同一個 key（#452 的 k6-auth 三個 leg：一個存成功、兩個 `Unable to reserve`，之後都沒命中自己的 key）。`plan.sh` 因此把每個 scope 對到一組、並且每組恰好一個存檔 scope；test matrix 的 scope 是 `test-<module>`（存檔者 `test-apps/rpc`），其餘 matrix job 的各 leg 共用同一個、非存檔的 scope，只讀。
  - **PR（含 fork、dependabot）**：只讀 default branch 的項目、永遠不存。go.sum 有變時 module cache 退回上一份、只補差額；build cache 不含 go.sum，照吃。fork 的唯讀 token 不會有寫入警告（沒有存檔步驟）。
  - **其他 cache 的清單與處置**：tools（`Linux-tools-…`）、Playwright 與 pnpm store（`.github/actions/pnpm-cache`，取代 `actions/setup-node` 的 `cache: pnpm`——它的 post step 在任何 trigger 都會存，PR 改到 lockfile 時會在 `refs/pull/N/merge` 寫約 78MB）都拆成 read-write／restore-only，由同一個 `plan.sh` 判斷；golangci-lint-action 的 cache 在 PR 上 `skip-save-cache`；`web` 設 `go-cache: "false"`（`task` 每次都跑 `go work edit` 求 `MODULES`，仍要 `go` 在 PATH 上，但它不編譯也不下載）。PR 上不再有任何會寫的 cache。
  - **合併後驗收**：main 的 push run 要有 `Cache saved with key: gomod-…`（lockfile 沒變時第二次起是 exact hit、不再存）與 5 個 `gobuild-…` 各一份、PR run 要零個 `Cache saved`、`gh cache list --ref refs/pull/<N>/merge` 沒有 `gomod-`／`gobuild-` 項目、`gh api repos/<repo>/actions/cache/usage` 總量低於上式。舊的 `go-Linux-*` 項目不再被取用，靠 LRU／7 天自然退場，沒有主動刪除。
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go bump 會讓其他 module 漂移，`dependabot-sync.yml` 在它的分支跑 `scripts/sync-fixpoint.sh`（重複 `task tidy`，直到某一輪不再改 go.work／go.mod／go.sum，最多 5 輪；#475 的 bump 第 1 輪給 12 個 go.mod 加了 go-internal、第 2 輪又從 11 個拿掉，只跑一輪會 commit 非不動點的樹）補 commit 再觸發 CI，否則 `work-sync` 會擋。不收斂或任一輪的指令失敗就讓 job 失敗，commit 步驟在它之後所以不會跑；一棵需要 k 個變動輪的樹要 k+1 輪才證明收斂。`task tidy`（每個 module `go mod tidy` 後 `go work sync`）修的是 `check-work-sync`（`go work sync` 後 go.work／go.mod 不得有 diff，不看 go.sum）判斷範圍的超集：不動點的最後一輪 `go work sync` 沒改任何東西，所以 `check-work-sync` 是綠的。`.github/dependabot.yml` 三組每週各一支 PR：gomod（所有 module 一組）、github-actions、npm（`web/` pnpm workspace 一組；lockfile 算 releasable，merge 會切 patch 版）

## best-effort.sh（#462）

**`best_effort`**（`scripts/best-effort.sh`，`. scripts/best-effort.sh` 後使用；kind 腳本是 `. "$(dirname "$0")/../../../scripts/best-effort.sh"`）：`best_effort <指令…>` 跑指令，失敗時往 stderr 印 `best-effort command failed (exit N): <指令>` 並回 0——失敗從靜默變成可見。**只給三類**：診斷（失敗報告時多印的 `kubectl get pods`、`diag.sh`）、清理（只限**回收**：wait 已被 kill 的 pid、刪 tmp 檔；後面另有地方判定結果）、wait-gone（`kubectl wait --for=delete`：對象已不在就是想要的狀態）。要讀輸出或看狀態的呼叫（poll、查找、取值）一律 `rc=0; cmd || rc=$?` 加明確分支（印出哪個呼叫失敗、怎麼處理），不准用 `best_effort`。**還原不是 best effort**：腳本承諾離開時恢復的狀態（helm values、replicas、secret、scale）一律用 `restore_step <指令…>`（失敗印 `restore failed (exit N): …`、記下第一個失敗碼、繼續跑後面的還原）加 EXIT trap 裡的 `restore_exit "$rc"`：進來時 rc 為 0 而還原失敗就以還原的失敗碼離開；進來時 rc 已非零就保留原 rc 並印 `restore also failed`。使用者：`outage.sh`、`negative.sh`。

## console-e2e 的平行度與 sharding（#447）

基準：run 41920885162，`console-e2e` 一個 job 369s（setup/build/dex ~75s、`task web-e2e` 254s 用 2 workers、ports 24s）。現在它是 3 個 shard 的 matrix（`console-e2e (1)`…`(3)`）：

- **觸發不變**：仍是 `run_console_e2e` 一個旗標、一個 job（matrix 不是新 job），`scripts/ci-changes.sh` 與 `scripts/test.sh` 的規則不動。沒有 branch protection / ruleset 綁舊的 check 名稱。
- **workers**：CI 設 `E2E_WORKERS=4`（ubuntu-latest 在 public repo 是 4 vCPU，實測 `nproc=4`）；本機沒設，`task web-e2e` 還是 Playwright 預設，行為不變。
- **shards**：`task web-e2e -- --shard=i/N`，N 是 matrix 大小（`strategy.job-total`，只有一個來源）。每個 shard 有自己的 runner、postgres service、dex、`bin/worker`，互不共用。
- **覆蓋**：shard 1 先跑 `task web-e2e-shards-check -- N`（`scripts/web-e2e-shards-check.sh`）：每個 shard 的 `--list` 合起來必須剛好等於未分片的 `--list`（每個 test 一次、不多不少，只列不啟 server）。每次 `--list` 都要 exit 0，且它自己的 `Total: K tests` 要等於印出的 test 行數（部分清單加非零退出、被截斷的清單都是紅燈，不是較小的套件）。`web-e2e-ports` 只在 shard 2 跑（它最輕）。
- **改 shard 數**：改 matrix 的 `shard: [1, 2, 3]`；ports 掛在 `matrix.shard == 2`，要確認該 shard 不會因此變成 critical path。

**量測方法與結論**（數字與 run id 在 PR #453 的描述；這裡只留結論）：

- 探索（單一 workflow run 內的平行 matrix，不是獨立重複，只用來選方案）：workers 2/3/4 是 run 41927472999，shard 2/3 路與 ports 獨立跑是 run 41929103982。結論：4 vCPU 上 workers=4 比預設 2 快約 80–100s 且 0 flaky、0 retry；3 shards 的 `task web-e2e` step 約 50–90s。
- 定案後的獨立量測是 3 個不同的 workflow run：41934848918（pull_request）、41937591178（workflow_dispatch）、41939558460（workflow_dispatch），同一個 head 9776da2c。三次的 9 個 shard job 全部成功（274 tests = 102+86+86，加 ports 5，0 flaky、0 retry）；整個 workflow 的最終狀態不是三次全綠：41934848918 與 41937591178 因 `gen-freshness`（base 問題，#454）失敗，41937591178 另被後續 dispatch 的 concurrency 取消；`task web-e2e` step 最長的 shard 是 87s／86s／80s（基準 254s）。各 shard 的 step 秒數、queue、setup、cache 命中在 PR #453 的描述。比較以 step 秒數為準，job 總秒數受 Actions cache 與 runner 排隊影響，只作參考。
- 多佔並行名額：1 個 job 變 3 個（`console-e2e (1..3)`，多 2 個）。名額飽和、排隊時，多拆 job 不一定讓 wall-clock 變短；排隊另外記，不算在 job 秒數裡。

選 workers=4 × 3 shards；ports 放 shard 2（探索 run 41929103982 裡它的 `task web-e2e` 最輕；加上 ports 後是否仍不超過其他兩片，看 PR #453 的獨立量測）。

## 本機 hook 證據：AgEnD hooks ≠ lefthook（#300）

`git commit` / `git push` 成功，不代表 `lefthook.yml` 的 `gate-fast` / `gate-local` 跑過：git 只執行 active `core.hooksPath` 底下**同名**的 hook。AgEnD 管理的 worktree 會把 `core.hooksPath` 指向 `~/.agend/hooks`，那裡只有 `prepare-commit-msg`、`reference-transaction` 等 AgEnD hooks，沒有 `pre-commit` / `pre-push`，也不呼叫 lefthook。`task tools:hooks` 裝的 lefthook 因此不會被觸發。

### 1. 先驗證 hook 有沒有真的掛上

```bash
git config --show-origin --get core.hooksPath   # 空 = 預設 .git/hooks；有值 = 看來源檔與路徑
ls -l "$(git rev-parse --git-path hooks)"       # 預設路徑
ls -l "$(git config --get core.hooksPath)"      # 有設 hooksPath 時看這個
```

要看到 `pre-commit` / `pre-push` 且內容呼叫 lefthook（`grep -l lefthook <hook>`），才能說 lefthook 掛上。找不到就視為**沒掛上**。AgEnD 的 hooks 與 Claude Code 的 `.claude/settings.json` hooks（`PostToolUse` / `Stop`）也各自獨立，不能互相當證據。

### 2. 沒掛上時：在 exact head 手動跑門檻

在要回報的 commit（`git rev-parse HEAD`）上跑 `task gate-local`（commit 前的檔案檢查用 `task gate-fast FILE=…`），回報要留命令、exit code 與輸出摘要。措辭固定：

| 情況 | 可以寫 | 不可以寫 |
|---|---|---|
| lefthook 掛上，hook 跑過（有 hook 輸出） | lefthook 通過 | — |
| 沒掛上，手動跑 gate 且 exit 0 | **手動 gate 通過，hook 未執行**（附 head SHA、命令、exit code） | lefthook 通過、hooks 通過 |
| 沒掛上，沒跑 gate | 本機 gate 未執行 | 任何「通過」 |

範例：

```text
head: 0b95a126…（git rev-parse HEAD）
core.hooksPath: /Users/sean/.agend/hooks（file:…/.git/config）；hooks 目錄無 pre-commit / pre-push
$ task gate-local → exit 0（check-gen-fresh 乾淨、buf lint 乾淨、affected modules go test -race ok）
結論：手動 gate 通過，hook 未執行
```

### 3. 每個 head 各自驗證

merge commit、rebase 後或任何新 head 都要在**該 head** 重驗，不沿用 parent 的結果。回報裡的 SHA 要與最後 push 的 head 一致。

### 4. 已由 exact-head CI 證明的不必重跑

CI 在 `refs/pull/N/merge` 上跑（見 `task merge-ready`），綠燈對得上目前 head 時，下列門檻不必本機重複：gen-freshness（含 TS）、`task lint`、work-sync、`go test -race`（test matrix）、coverage、build、helm、k6 / k6-auth / k6-wire / helm-kind / helm-kind-keda、release-snapshot、web。哪些 job 實際執行取決於改動路徑（`scripts/ci-changes.sh`）；被跳過的 job 不是證據。本機手動 gate 的價值在於「push 前就知道」，**不取代** CI；CI 也無法證明本機 hook 曾經執行。

### 5. 不要繞過 hook

不用 `-c core.hooksPath=/dev/null`、`--no-verify` 跳過 hook，除非 lead 明確授權；授權時要在回報裡寫明被跳過的 commit 與理由。也不要為了讓 lefthook 跑起來去改全域 `core.hooksPath` 或 `~/.agend/hooks`（影響其他 worktree 與 daemon 保護）；缺 hook 就走第 2 點手動跑。

### Claude-Session trailer

repo 沒有這個規範：`CLAUDE.md`、`docs/`、`.github/`、`scripts/` 都沒有 `Claude-Session`，近 60 筆 commit 也沒有；commit 規則只有 Conventional Commits。#297 提到的 trailer 來自當次派工 brief，不是 repo 要求。AgEnD 的 `prepare-commit-msg` 注入的是 `Agend-*` 與選用的 `Signed-off-by`，且 hook 失敗不擋 commit。
