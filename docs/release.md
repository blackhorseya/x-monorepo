# Release

<sub>← [README](../README.md)</sub>

## Release（goreleaser + release-drafter）

**每次 CI 在 main 上綠就是一版**，沒有人手動打 tag（`.github/workflows/release.yml`，`workflow_run` 掛在 ci 後面；
`workflow_dispatch` 可對目前 main HEAD 重跑）。`workflow_run` 沒有 `paths-ignore`，所以 prepare 先自己 gate：拿該 commit 和
**上一個 tag** 做 diff（不是 parent commit——ci 對 main 是 cancel-in-progress，連續兩次 merge 只會觸發一次 release），
沒有任何檔案會進到 archive / image 就整條跳過（排除清單在 `scripts/releasable.sh`：docs、CI / hook / lint / `Taskfile.yml`、
`scripts/`、Helm chart、kind / compose 開發環境、tests 與 test-only 套件（含 console 的 `e2e/`）；`web/` 其餘都算，console image 是 artifact；
step summary 會列出原因；archive 裡的 README 會停在上一版，可接受）。
`task release-gate` 在本機跑同一個腳本，merge 前就知道會不會發版，會的話再跑 `scripts/next-version.sh` 從 commit title 算 drafter 會給的版號（`!` → major、`feat` → minor、其餘 patch；main 上每個 commit 都是 squash 進來的 PR title，所以和 drafter 一致）。
每個 PR 上另有一則 sticky comment 說同一件事（`.github/workflows/release-gate.yml` → `scripts/release-gate-pr.sh`：「merge 會發 vX.Y.Z（minor — feat: …）」或「不發版」、這個 PR 哪些檔會進 artifact、main 上從上一個 tag 起已經累積了什麼；版號用 PR title 當 squash commit 的 subject 算，retitle 會重算；純建議，不擋 PR，fork PR 不貼；release.yml 開的 chart PR 是 bot PR、`pull_request` 的 workflow 不會自己跑，所以它的 chart job 以 `workflow_dispatch -f pr=N` 補貼同一則；同一 PR 的 run 共用 concurrency group，只有 `pull_request` run 會取消進行中的 run，dispatch 排在後面，否則 approve 後啟動的 `pull_request` run 會被 dispatch 取消、PR 頁面顯示 `comment` 失敗）。`workflow_dispatch` 不過 gate：

| job | 做什麼 |
|---|---|
| prepare | gate 過了才跑 release-drafter：依 merge 進來的 PR title 分類（`when.conventional`，不用 label）產 draft release `vX.Y.Z`；版號：`!` → major、`feat` → minor、其餘 patch，`test:` 不進 notes |
| （labeler.yml） | 不在 release 鏈上：`pull_request_target` 跑 release-drafter 的 autolabeler，依 title 的 type 貼 `feature` / `fix` / `refactor` / `test` / `documentation` / `ci` / `breaking`，依路徑貼 `proto` / `web` / `helm` / `ordering` / `inventory` / `platform`。純顯示，版號不看它 |
| build | 在該 commit 本地打 tag（不 push，goreleaser 需要）→ `task web-build` → `goreleaser release`：archives 掛上那份 draft（`use_existing_draft` + `keep-existing`，notes 保留）、三個 multi-arch image 推 `ghcr.io/blackhorseya/x-monorepo/{rpc,worker,migrate}:X.Y.Z`（非 prerelease 另打 `latest`）→ `docker buildx build --push` 把 console 的 `dist/` 推成第四個 image `.../console:X.Y.Z`（同一套 tag 規則；goreleaser 沒有 binary 可以掛它）。四個 image 都附 SBOM（SPDX）+ SLSA provenance attestation，並由 cosign keyless 以 digest 簽名（job 有 `id-token: write`，certificate 的 identity 是這個 workflow）；`checksums.txt` 也簽（cosign v3 的 Sigstore bundle `checksums.txt.sigstore.json` 掛上 release，簽章 + certificate + Rekor entry 一個檔）；cosign 釘在 v3.1.4（`cosign-installer` 的 `cosign-release`，build / verify / release-snapshot 三處要相同）——v3.0.6 遇到 Rekor 回 409「equivalent entry already exists」整個簽章就失敗（v0.36.0 第一次嘗試，#195：client 重送了 Rekor 已寫入的 POST），v3.1.4 的 sigstore-go v1.3.0 會改取那筆既有 entry（Rekor v1；v2 的 409 上游仍未處理，見 #215 與下面的 Rekor v2 canary）；archive 與 `checksums.txt` 另由 `actions/attest-build-provenance` 寫一份 GitHub artifact attestation（SLSA provenance，存在 repo 的 Attestations，`gh attestation verify` 驗——cosign 蓋 image 與 checksums，這份補每個 archive 是哪一次 run 做的） |
| publish | `gh release edit --draft=false --target <sha>`：GitHub 建 tag、標 latest。image 已經在 registry，release 一出現就能拉 |

```bash
task release-check      # 驗 .goreleaser.yaml
task release-snapshot   # 本機 dry run：每個 app × linux/darwin × amd64/arm64 → bin/dist，不做 image、不簽名、不 publish
```

**驗證**（release notes 的 Verify 區塊也有同一組指令；package 是 public，不用 login）：

```bash
ID='https://github.com/blackhorseya/x-monorepo/.github/workflows/release.yml@refs/heads/main'
ISS=https://token.actions.githubusercontent.com
cosign verify --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" ghcr.io/blackhorseya/x-monorepo/rpc:X.Y.Z
cosign verify-blob --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" --bundle checksums.txt.sigstore.json checksums.txt
gh attestation verify x-monorepo_X.Y.Z_linux_amd64.tar.gz --repo blackhorseya/x-monorepo   # archive 的 SLSA provenance（任何 artifact 檔都行，含 checksums.txt）
docker buildx imagetools inspect ghcr.io/blackhorseya/x-monorepo/rpc:X.Y.Z --format '{{ json .SBOM }}'   # 或 .Provenance
```

`deployments/docker/Dockerfile` 只 COPY 預先 cross-compile 好的 binary 到 `distroless/static:nonroot`（有 CA certs、無 shell；
healthcheck 從外面打 `/api/readyz`）。兩個 binary 都吃 SIGTERM 做 graceful shutdown（rpc：readyz 先 503 → drain，見 README 的 server 一節；worker：in-flight handler 跑完才停，見 README 的 Events 一節），
部署的 grace period 要大於 `http.shutdown_delay + http.shutdown_timeout`（rpc）/ 18s（worker：8s drain + 10s router close）——Helm chart 把這兩個數字算死（下一節）。Binary 用 `-X` 把版本寫進 `pkgs/platform/buildinfo`，OTel resource 的 `service.version` 就是它；三個 binary 都吃 `--version`，rpc 的 `server listening` 與 worker 的 `worker started` log 帶 `version` / `commit`，rpc 另有 `GET /api/version`
（`go build` 直接跑的是 `dev`）。CI 的 `release-snapshot` job 在每個 PR 跑一次 snapshot，壞掉的 release 設定在 merge 前就會紅（也用同一個 cosign-installer 裝 cosign 跑 `scripts/cosign-smoke.sh`：拿 `.goreleaser.yaml` 的 `signs` args 配臨時 key 簽一次、驗一次，再用確定性的 RSA key 對同一個 artifact 簽第二次，重現 Rekor 的 equivalent-entry 409，要求仍簽得出、bundle 驗得過（簽章 + log inclusion）——release.yml 只在 main 跑，v0.26.0 前兩次就是 action tag 不存在、cosign v3 flag 變了這種 PR CI 看不到的錯；`actions-resolve` job 另用 `scripts/actions-resolve.sh` 確認 `.github` 裡每個 `uses:` 的 ref 真的存在），`release-snapshot` 還跑 `scripts/rekor-v2-canary.sh`（#215）：用同一個 cosign 對全新的 `TUF_ROOT` 做 `cosign initialize`，讀它預設的 signing config（`signing_config*.json`），`rekorTlogUrls` 只要有任何一筆 `majorApiVersion` 不是 1 就紅（看欄位不看 URL；v2 與未知版本都算），訊息指回 #215 與處理選項（對 409 加有上限的重簽 wrapper、cosign 釘在預設仍是 v1 的版本、或明確指定 v1 的 signing config）。TUF 或 config 讀不到（`cosign initialize` 重試 3 次）、或讀到的不是「一份 JSON、`rekorTlogUrls` 為非空的物件陣列」（空檔、空白檔、`{}`；`cosign initialize` 抓不到 signing config 只會 warning，jq 對零輸入也回 0）一律 fail-closed——跳過會在最看不見的時候變綠，且旁邊的 smoke 打不到 Rekor 時本來就紅；訊息會註明是網路或 sigstore 故障、不是 v2 的發現。`scripts/test.sh`（`scripts-test` job）用 majorApiVersion 2 的 fixture 與假 cosign 保留負例（v2 / v1+v2 / 未知版本 / 空 / 壞 JSON / TUF 不通），正例就是這一步對真實預設 config 變綠。`release.yml` 單獨改動（例如升 `cosign-release`）也會跑 `release-snapshot`（`scripts/ci-changes.sh` 的 release 區）。sigstore 切預設走 TUF root，repo 沒有 commit，所以另有 `.github/workflows/rekor-canary.yml` 每週一 03:17 UTC（與手動 dispatch）跑同一支，cosign 版本取自 `release.yml` 的 `cosign-release`（不多一處要對齊的釘版）；排程失敗時會開（或在既有的那張下留言）標題為 `rekor-canary: …` 的 issue，repo 的 watcher 收得到通知，最後編輯 cron 那行的人也會收到 GitHub 的失敗信；排程 workflow 在 repo 60 天沒活動後會被 GitHub 停用（發版算活動）。canary 紅了不要 rerun release：先處理 #215。release 的 build job 也共用它的 Go cache（`cache-scope: release-snapshot`），cross-compile 不用冷啟。
發布之後 release.yml 的 `verify` job 跑 `scripts/release-verify.sh vX.Y.Z`（`task release-verify -- vX.Y.Z` 本機同一支）：把 release notes 的 Verify 區塊真的跑一遍——下載 archives 對 checksums.txt、`cosign verify-blob --bundle`、`gh attestation verify`、四個 image 的 `cosign verify`——簽名或 attestation 壞了 release run 就紅，不用等人手動驗。
用 `GITHUB_TOKEN` publish 的 release 不會觸發其他 workflow 的 `push: tags` / `release` event（GitHub 防遞迴），要接部署就像
release.yml 那樣用 `gh workflow run` 明確 dispatch。第一次 push 到 ghcr 的 package 是 private，四個 package 都已在 GitHub → Packages 改成 public（新 app 的 image 第一次 push 後要再改一次：Package settings → Danger Zone → Change visibility）。
