# Identity

<sub>← [README](../README.md)</sub>

`pkgs/identity` 是第三個 bounded context：**DB 管理的 account 與 API key**，給 rpc server 的 auth interceptor 解 `X-API-Key`。
它只做 verifier——JWT 仍由 `auth.jwt` 指的外部 issuer 發；它不發 event、不消費 event（沒有 outbox / inbox / poison 表）。

## 形狀

```
X-API-Key ──► platform/server auth interceptor
               ├─ 1. config auth.api_keys（static，break-glass，不需要 DB）
               └─ 2. server.CredentialResolver ──► identity/adapters/auth.Resolver（TTL cache 30s）
                                                    └─► ResolveAPIKey use case：sha256(secret) → api_keys.hash → key.Usable(now) → account.IsActive → principal
```

- **platform 不 import BC**（depguard `shared`）。platform 開一個 port `server.CredentialResolver`，identity 的 `adapters/auth` 實作它，
  `apps/rpc/wire.go` 用 `wire.Bind` 接起來。這是唯一一處 platform 反向呼叫 BC 的地方，而且只透過 platform 自己宣告的 interface。
- **查序**：config key 先、resolver 後。config key 永遠不打 DB，所以 DB 掛了 operator 還進得去；resolver 回 error（DB 掛）時 request 被拒成
  `auth.invalid_credential`，**不會**放行、也不會把 error 內容給 client（access log 有 `auth: credential resolver failed`）。
- **沒 DB 時**（`postgres.url` 空）repository 是 in-memory：`task run` / E2E 開 auth 一樣可以 `CreateApiKey` 然後用那把 key 打，重啟就沒了。

## Domain

| Aggregate | 欄位 | 行為 |
|---|---|---|
| `account.Account` | `id`（= principal subject）、`name`、`roles []string`（trim / dedupe / sort）、`status ACTIVE\|DISABLED`、`version` | `Disable` / `Enable`（不是終點，可來回）、`AssignRoles`（整組取代，空 = 清掉）。沒有 `Set*` |
| `apikey.APIKey` | `id`（= principal `CredentialID`）、`accountID`、`name`、`prefix`（secret 前 8 字元，列表辨識用）、`hash`（sha256）、`status ACTIVE\|REVOKED`、`expiresAt?`、`revokedAt?`、`version` | `New(…, secret, …)` 自己算 prefix + hash，plaintext 不存；`Usable(now)` 回 `ErrRevoked` / `ErrExpired`；`Revoke` 終點 |

secret 由 `infra/system.SecretGenerator` 產：`xk_` + 32 bytes base64url（43 字元，256 bits）。高 entropy 所以 sha256 就夠，不需要 argon2；
DB dump 不會漏 key，查詢是 index lookup 不是逐筆比對。`CreateApiKey` 的 response 是**唯一一次**看到 plaintext 的機會。

## Principal 語意（platform 的變更）

| credential | `Subject` | `Name` | `Roles` | `Method` | `CredentialID` |
|---|---|---|---|---|---|
| config `auth.api_keys` | entry 的 name | 空 | 冒號後的清單 | `api_key` | 空 |
| identity 的 key | **account id** | account name | account roles | `api_key` | **api key id** |
| JWT | `sub` | `name` claim | `roles` claim | `jwt` | 空 |

`CredentialID` 是新欄位：撤銷「這一把」key 用它找。access line / span 的 `sub` 對 identity key 是 account id，不是 key name。
metrics label 不變（`auth_decisions_total{result, method}`，method 仍只有 api_key / jwt / none）。

## RPC（`identity.v1.IdentityService`）

全部 `server.RequireRole(auth.admin_role)`，**除了 `WhoAmI` 與 `ClaimInvite`**（回 caller 自己的 principal；anonymous 時 `anonymous: true`）：
`CreateAccount` / `GetAccount` / `ListAccounts`（id 升冪，opaque page token）/ `DisableAccount` / `EnableAccount` / `AssignRoles` /
`CreateApiKey`（回 secret 一次）/ `ListApiKeys`（含 REVOKED，新到舊）/ `RevokeApiKey` /
`CreateInvite`（email 邀請，7 天、單次、不含 secret，寫 audit）/ `ListAccountIdentities`（見下）/ `RevokeSessions`（見下）；`ClaimInvite` 不要 admin role（見「外部身分綁定」）。

**`ListAccountIdentities(account_id)`**（#289，admin only，唯讀、不寫 audit）：列出一個 account 現在有誰能登入——`identities`（external identity 綁定：`issuer`、`subject`、`account_id`、`bind_time`）與 `invites`（`email`、`expire_time`、`claim_time`、`status`），都是新到舊（`created_at desc`；綁定同時間以 issuer、subject 排，invite 以 id desc）。
- **`status` 由 server 在回應當下判斷**（domain `Invite.Status(now)`，順序同 `Claim`）：已領 → `CLAIMED`（過了 `expire_time` 仍是 `CLAIMED`）；未領且 `now >= expire_time` → `EXPIRED`（到期那一刻就算過期）；其餘 `PENDING`。`Invite` 訊息新增 `status`，`CreateInvite` / `bootstrap` 的回應也帶（剛建的一律 `PENDING`）。
- **不回任何 secret**：invite 本來就沒有 token 或 hash，回應也沒有這類欄位；測試走 response descriptor 遞迴檢查沒有名字含 `token` / `hash` / `secret` 的欄位，並檢查實際序列化出來的 JSON 沒有這些字。
- **account 不存在** → `NOT_FOUND` + `account.not_found`（同 `ListApiKeys` / `CreateInvite`）；`account_id` 為空由 protovalidate 擋（`INVALID_ARGUMENT`）；非 admin → `PERMISSION_DENIED`（先於 account 是否存在，不洩漏有無）。
- **不分頁**：和 `ListApiKeys` 一樣是「單一 account 底下的子資源清單」，不是 `ListAccounts` 那種整體 collection（opaque page token）；一次回兩個清單也沒有單一 cursor 可用。綁定只在 claim / auto_provision 時新增；invite 則每次 `CreateInvite` 都多一筆，目前沒有上限也沒有清理，量大時要補分頁或清理（後續）。
- `invites` 沒有 `account_id` 索引（只有 `(email, created_at desc, id desc)`），這支查詢會掃 `identity.invites`；表目前很小，所以不為它加 migration，量大再補 `(account_id, created_at desc, id desc)`。
- 撤銷單一綁定／邀請**不在這支**（先經 Sean 決定）；Console `/accounts` 等 #273 合併後再接。

**`RevokeSessions` 只是「登出所有裝置」**：它設 `accounts.sessions_revoked_at = now`，`iat` 早於這個時間的 access token 在 resolver cache 過期後（≤ 30 秒）被拒；寫 audit（誰、哪個 account、何時，不記 token）。它**擋不住持有 IdP refresh token 的人**——對方能向 IdP 換到新 `iat` 的 token。**懷疑帳號被盜要用 `DisableAccount`**（對每個 token 都有效，包括 refresh 換出來的），再到 IdP 撤銷 refresh token 或重設憑證。proto 註解與 Console 文案都必須這樣寫（#273）。它不改 status / roles / key，所以沒有 last-admin guard。

**Last-admin guard**：管理面不能被自己鎖死。「有效 admin」= `status ACTIVE`、roles 含 `auth.admin_role`、且至少一把 key 現在可用（`REVOKED` 與已過 `expires_at` 的不算）的 account。`DisableAccount`、`AssignRoles`（拿掉 admin role）、`RevokeApiKey`（撤掉該 account 最後一把可用 key）若會讓**最後一個**有效 admin 失去資格，回 `FAILED_PRECONDITION` + `x-error-code: identity.last_admin`（訊息固定，不帶其他 account 的資料）；系統裡本來就沒有有效 admin 時什麼都不擋（沒東西可保護），不是有效 admin 的 account 動什麼都不受影響。
併發：這三個 use case 在同一個 transaction 內先 `AdminCensus.LockAdmins`（Postgres：`SELECT … FOR UPDATE` 鎖住 ACTIVE 且帶該 role 的 account 列，`ORDER BY id` 讓鎖序一致，再另一條 statement 算可用 key——不能合成一條 join，否則等鎖後 `api_keys` 還是舊 snapshot），**之後**才載入要改的 aggregate。「key 是否過期」用的 now 也是拿到鎖**之後**才讀（`LockAdmins` 回傳判斷當下的時間，use case 沿用它）：在鎖前排隊時過期的備援 key 不算。所以兩個 admin 同時互相停用、同一個 admin 的兩把 key 同時撤銷，只有一個會成功，另一個拿到 `identity.last_admin`。in-memory 的 `Transactor` 讓 writer 一次一個。沒有 migration（`api_keys_account_idx` 已涵蓋 `account_id`）。偏保守的方向：與 `EnableAccount`／新 admin 同時進行時，新加入的 admin 若不在這次鎖到的集合裡，可能多拿一個 `identity.last_admin`，重試即可。`ResolveAPIKey`／`TouchLastUsed` 不鎖。
**不算在內**：config `auth.api_keys` 的 static key 不是 DB account，guard 看不到它；它是 guard 之外的 break-glass——DB 裡最後一個 admin 被擋下時，static admin key 仍可操作，但 static key 不會讓 guard 放行（DB 的最後一個 admin 一樣不能停用）。要保留 break-glass 就在 config 留一把 `name=<key>:admin`。

**啟動行為的改變**：以前 `auth.mode: required` 但 `api_keys` 與 `jwt` 都空會開不起來；現在合法（resolver 一個人也能答），但 DB 空的話就是「auth 開著、沒有任何 credential 能過」的 fail-closed server——`server.New` 會印一行 Warn。Helm 少掛 secret 不再 CrashLoop，而是鎖在外面，看 log。

**Bootstrap**：`task bootstrap`（= `worker bootstrap`，use case `identity.bootstrap`）在**沒有任何 account** 的 store 上一個 transaction 建第一個 account（`--name root`、`--role` 預設 `auth.admin_role`）和它的第一把 key（`--key-name bootstrap`、`--expires-in 0` = 永不過期），secret 只印到 stdout 一次、不進 log。已有 account 就印 `already bootstrapped` 並 exit 0，但**只在操作者本機跑**（經 `kubectl port-forward` 連 DB，見 `docs/operations.md`；`--expect-target host:port` 必填，連線只准連它），不要放進叢集的 Job——secret 是 stdout，會進 Pod log；guard 是 read 不是 constraint（`accounts.name` 沒有 unique），兩個 run 在空 store 上搶跑會各建一個——從一個地方跑一次。拿印出來的 key 再 `CreateAccount` / `CreateApiKey` 建真正的帳號；config `auth.api_keys`（`root=<key>:admin`）不再是必經路，留著當 break-glass。沒有 seed migration。

## 生效延遲與 cache 契約

resolver 前面有 in-process cache，TTL `auth.DefaultCacheTTL` = 30s，hit 與 miss 都 cache（miss 也 cache 是為了擋猜 key 打 DB；真正的防線是 rate limit）。
每個 rpc replica 各自一份；上限 10k entries，超過整個丟掉重來。契約（`adapters/auth/resolver_test.go` 逐條有測）：

| 事件 | 生效 |
|---|---|
| revoke / disable / `AssignRoles` | 最多 **30s** 後（該 replica 的 entry 到期） |
| key 的 `expires_at` | **準時**：entry 的有效期取 `min(now+30s, expires_at)`，到期的下一個 request 重查、被拒 |
| DB 掛掉 | **bounded staleness**：30s 內驗過的 key 在那個 window 結束前照常通過；window 外、或沒驗過的 key，resolver 回 error → 拒絕（`auth.invalid_credential`）+ error log。DB 掛掉**不會讓任何沒驗過的 key 進來**，但也不會立刻踢掉剛驗過的 |
| resolver error | 不 cache，每個 request 都再試 |
| resolver 查太久 | `auth.resolver_timeout`（預設 2s）到期 = 拒絕，**到期後才回來的成功也拒絕**（interceptor 回來後查 context，不只看 resolver 的 error）；auth 在 `http.timeout` 之外，所以這個上限是獨立的。它是 context 的 deadline：resolver 不理 context 的話（例如 driver 不吃 ctx）request 還是會等它回來，只是回來後被拒 |

### Metrics

resolver 用注入的 `metric.MeterProvider`（不是 global）記兩個 instrument，exposition 名在括號：

| instrument | 型別 | 意思 |
|---|---|---|
| `identity.resolver.lookups{result}`（`identity_resolver_lookups_total`） | counter | 每次 `ResolveAPIKey` 的去向，見下表 |
| `identity.resolver.cache.entries`（`identity_resolver_cache_entries`） | observable gauge，無 label | 這個 replica 的 cache 現在有幾筆（≤ 10k） |

| `result` | 對應的路徑 |
|---|---|
| `hit` | cache 命中，不問 use case（cache 住的成功與 cache 住的拒絕都算） |
| `miss` | cache 沒有（沒進過、過了 TTL / `expires_at`、或 TTL=0）→ 接著問 use case，最後一定再多記下面三者之一 |
| `db_ok` | use case 回 principal（key 有效、account active），寫入 cache |
| `db_miss` | use case 回 domain error（`found=false`：不存在、revoked、expired、account disabled、太短），拒絕也寫入 cache |
| `error` | 其他錯誤（DB 掛、timeout）：回 err、不 cache，interceptor 拒成 `auth.invalid_credential` |

`hit + miss` = 全部 lookup；`miss = db_ok + db_miss + error`；hit ratio = `hit / (hit + miss)`。
**label 只有 `result`，值固定五個**：不放 key、prefix、hash、account id、key id——每把 key 一個 series 會讓 cardinality 隨 key 數與猜 key 的流量無限長；
要查「哪把 key」看 access line 的 `sub`。`resolver_metrics_test.go` 用 100 把不同的 key 打過，斷言 attribute 只有 `result`、series ≤ 5。
Dashboard「x-monorepo / service」的 Identity row 有四張圖（lookups/s by result、hit ratio、error rate、cache size）；alert `x-monorepo-identity-resolver-errors`
見 `docs/operations.md`。

「DB 掛掉仍服務 30s 內驗過的 key」是刻意的（可用性 > 立即一致；不等於放行），要改成嚴格 fail-closed 就得每個 request 都查 DB，等於沒有 cache——這是產品決定，不是 bug。

### last_used_at（節流、best-effort）

`api_keys.last_used_at`（nullable `timestamptz`，migration `0002`）記 key 最後一次通過驗證的時間，`ListApiKeys` 的 `last_use_time` 與 Console account 頁的「Last used」（沒用過顯示 `—`）讀它。它是**給人看的粗略指標，不是稽核紀錄**——要查「誰在什麼時候做了什麼」看 access line 的 `sub`（account id）與 `enduser.id`。

驗證成功要寫 DB，但 hot path 不能每個 request 都寫，所以有兩層節流（`auth.last_used_interval`，預設 `1m`，`0` = 關）：

1. **記憶體端（`Resolver`，每個 replica）**：同一把 key 在 interval 內只有第一個 request 會「認領」這次寫入，其餘（cache hit 也算）只付一次 map 查詢。這層擋掉 99.9% 的寫入。
2. **DB 端（`TouchLastUsed`）**：`UPDATE api_keys SET last_used_at = $at WHERE id = $id AND (last_used_at IS NULL OR last_used_at <= $at - interval)`。條件在 row 上，所以多 replica 同時認領同一把 key、或 replica 重啟丟了記憶體狀態，也只有一個寫得進去。

為什麼兩層都要：只做記憶體端，N 個 replica 每個 interval 會寫 N 次、重啟後又全部重寫；只做 DB 端，每個 request 都要多一趟 round trip。這個寫入**不碰 `version`**（不走 `Save`）：不然 hot path 的寫入會讓並行的 `RevokeApiKey` 吃 `ErrStale`；反過來 `Save` 也不會寫 `last_used_at`，舊 aggregate 存回去不會把它倒退。

失敗語意（best-effort）：

- 寫入在 request 路徑之外的 goroutine 跑（`context.WithoutCancel` + 2s timeout，同時最多 8 個，滿了就丟掉這次，之後的 request 會補），**不會讓驗證失敗、也不會拖慢 request**。
- 失敗只記一行 Warn（`identity: last_used_at write failed class=timeout|canceled|error`）：**只有錯誤類別**，沒有 error 文字（driver 的錯誤可能引用語句與參數）、沒有 key / hash / prefix（紅線 R4）。失敗會放掉認領，下一個 request 重試，不會把 key 停擺一整個 interval。
- **落後沒有嚴格上限**：interval 是寫入頻率的上限，不是「Last used」新鮮度的保證。兩層節流的相位會疊加——interval=60s，replica A 在 t=0 寫入成功；replica B 在 t=59s 認領，但 DB 條件拒絕（`Recorded: false`，A 剛寫過）；B 在 t=118s 仍被節流、t=119s 才可再次認領，所以 t=118s 時顯示落後 118s。所以健康時最多接近**兩個 interval**，寫入失敗或 DB 掛掉時更久（恢復後下一個 request 補上）。不把 DB 的 no-op 當失敗而放掉認領，是因為那樣每個 request 都會再打一次 DB，等於沒有記憶體端節流。resolver cache 的 30s 不影響它：cache hit 也會認領。
- 認領表有上限（`MaxCacheEntries`，兩代輪替：超過一個 interval 的認領本來就沒有作用，整代丟掉；**不整張清空**，否則 live key 會在 window 內全部回頭打 DB）。同一個 interval 內超過上限個不同的 key 不是 API key 的流量型態；超過的 key 這一代不寫，等下一代。
- 關閉（`0`）時不寫、不開 goroutine，欄位維持原值。
- Shutdown：`apps/rpc` 的 cleanup 會先讓還在跑的寫入最多再等 3s（`Resolver.WaitTimeout`，早於 DB 關閉），逾時就放棄，最後的 stamp 可能遺失（best-effort）。

## 外部身分綁定（#271，`docs/identity-login.md` §4–§6、§9、§11）

identity BC 這一半（#271）加上 auth interceptor 的串接（#272）。resolver 的輸入是一個值型別 `auth.ExternalIdentity{Issuer, Subject, Email, EmailVerified, IssuedAt}`，定義在 consumer 端，不依賴 `platform/oidc`。

- **綁定**：`identity.external_identities`，主鍵 `(issuer, subject)` → account；一個 account 可有多個。綁定鍵永遠是 `(iss, sub)`，不用 email。
- **邀請**：`identity.invites`（只記 email，無 secret）。`ClaimInvite` use case 在一個 transaction 內 `SELECT … FOR UPDATE` 鎖住該 email 最新的 invite，**拿到鎖之後**才讀 clock 判斷過期：兩個不同 sub 同時 claim 同一張，第二個等鎖、看到已用、拿 `invite.already_claimed`。檢查順序：已用 → 過期 → `email_verified` → email 相符；被拒不花掉 invite。重發邀請就是再 `CreateInvite`（取該 email 最新一張）。`ClaimInvite` RPC 在 contract 裡（**不是** admin-only；request 是空的，issuer / sub / email / `email_verified` 一律來自 server 驗過的 token，不信 client）。handler 透過 consumer 端的 `rpc.ExternalIdentitySource` 取得這組值：`identity.ProvideExternalIdentitySource` 回 `rpc.ContextExternalIdentity`，讀 auth interceptor 驗過 OIDC token 後放進 context 的 `contextx.ExternalIdentity`（沒帶 OIDC token 的呼叫沒有這個值，`ClaimInvite` 回 `UNAUTHENTICATED`，fail-closed）。`ClaimInvite` 是未綁定身分唯一放行的 procedure（`auth.ExternalCredentials.AllowsUnbound`，白名單在 identity 這一側，platform 不認識 identity 的 procedure）；已綁定的身分呼叫它是一般 principal 呼叫，由 use case 自己拒絕。
- **mapping**（`port.IssuerMapping`，預設 `bound`）：`bound` 未綁定 → resolver 回 `identity.unbound`；`auto_provision` 第一次見到就建 account（**沒有 role**，名稱 = email 或 sub）並綁定，同一身分並發時輸家 rollback、resolver 重讀。每個 issuer 的設定來自 `auth.oidc.issuers[].mapping`（`identity.ProvideIssuerMapping` 轉成 `auth.StaticMapping`），未知 issuer 一律 `bound`。
- **resolver**（`adapters/auth.ExternalResolver`，沿用 API key resolver 的模式）：`(iss, sub)` → account_id / roles / disabled / `sessions_revoked_at`，cache **30 秒**。fail-closed，拒絕都是 error：`identity.unbound`、`account.disabled`、`account.session_revoked`、`account.missing_iat`；其餘（DB 錯誤）原樣回傳。**只有找到的 account 進 cache**（錯誤與未綁定不進，所以 claim 成功後下個請求就通）。`iat` 的比對在每個請求跑、cache hit 也跑（cache key 是 `(iss, sub)`，同一人不同 token 的 `iat` 不同）。停用 / 改 role / 撤銷最慢 30 秒生效。Metrics：`identity_external_resolver_lookups_total{result}`（hit / miss / db_ok / unbound / provisioned / error）與 `identity_external_resolver_cache_entries`。
- **interceptor 串接**（#272，`auth.oidc.enabled`）：`Authorization: Bearer` 沒有 `X-API-Key` 時，先交 `platform/oidc` 的 verifier；它回 `ErrUnknownIssuer`（`iss` 不是任何 OIDC issuer）才交給既有的 `auth.jwt`，其他任何 OIDC 失敗都是終局拒絕，**不會**再試 `auth.jwt`。驗過的身分交 `server.ExternalIdentityResolver`（identity 側是 `adapters/auth.ExternalCredentials`，包 `ExternalResolver`），得到 principal：`Subject` = account id、`Name` 與 `Roles` = account 的、`Method` = `jwt`；token 裡的 `roles` / `name` 一律不看。resolver 的回答分三類：`server.ErrUnboundIdentity`（沒綁定）、`server.ErrCredentialRefused`（停用 / 撤銷 / 缺 `iat`，client 看到 `auth.invalid_credential`）、其餘（DB 錯誤等基礎設施失敗，同樣拒絕、不 cache，原因只進 error log）。這一查受 `auth.resolver_timeout` 約束，逾時後才回來的答案也拒絕。驗過的 OIDC 身分（不論綁定與否）都放進 context（`contextx.ExternalIdentity`）。未綁定身分打 `ClaimInvite` 時沒有 principal（`IsAnonymous`）、其他 procedure 回 `PERMISSION_DENIED` `auth.unbound_identity`。metrics 的 label 集合不變：未綁定放行 `ClaimInvite` 記 `result=ok, method=jwt`，未綁定被擋與其他拒絕記 `result=invalid, method=jwt`。
- **audit**：`identity.audit_events`（actor、action、target_account_id、occurred_at），與動作同一個 transaction；目前寫 `identity.revoke_sessions` 與 `identity.create_invite`。auth 關閉時 actor 是 `anonymous`。

**`worker bootstrap --admin-email <email>`**：在空 store 建 admin account（`--name`、`--role` 預設 `auth.admin_role`）並發出該 email 的 7 天邀請；**不建 API key，沒有任何 secret 可印**（設計文件 §9 留給這支 PR 定案的點：不產 key）。重跑是 no-op（`already bootstrapped`）。`--key-name` / `--expires-in` 與它互斥。部署者從 IdP 登入後呼叫 `ClaimInvite` 完成綁定；7 天沒人領就用 static admin key 重發。注意：last-admin guard 的「有效 admin」仍要求至少一把可用 API key（見上），所以只靠 OIDC 綁定的 admin 目前不被 guard 視為有效 admin——放寬成「或有綁定」屬後續。

## 擴充路徑：外部 OIDC 登入

現行規格見 [`docs/identity-login.md`](identity-login.md)：登入走**外部 OIDC**（本機開發用 Dex），本 BC 負責 `(iss, sub)` 到 account 的綁定、email 邀請、role 管理與 `sessions_revoked_at` 撤銷；多 issuer verifier 與 JWKS 在 platform。OIDC 預設關閉，關閉時本文件描述的行為不變。
自建帳密 issuer 降為可選模組（未排程），設計要點留在該文件的「方案 A」一節。先前這裡的草稿（`Login` 用 `auth.jwt.hs256_secret` 簽 HS256）已被取代：共用對稱 secret 讓每個 verifier 都能偽造 token，不重用。

## Inventory 寫入（`inventory.v1.InventoryService`）

`SetStock` 與 `ExpireReservations` 都是 `server.RequireRole(auth.admin_role)`：非 admin 的 principal → `PERMISSION_DENIED`（`auth.permission_denied`），沒有 credential（`optional` 沒帶 / `required` 被攔）→ `UNAUTHENTICATED`，`auth.mode: off` 不檢查。
`GetStock` / `ListStocks` / reservation 讀取不檢查 role。**行為變更**：以前任何 credential 都能 `SetStock`；整合（k6 setup、腳本）要改用 admin key。

## Ordering ownership（PR 3）

identity 存在的業務理由：訂單屬於 principal。授權判斷在 ordering 的 **application 層**（`application/ownership.Policy`，五個 use case 呼叫它；domain 不碰 principal），
admin role 名稱是注入的 `auth.admin_role`（`ProvideOwnership`），不是寫死的 `"admin"`。「誰是 customer」= `Principal.Subject`（identity key 是 account id、JWT 是 `sub`、static key 是 name）。

| RPC | 非 admin（有 principal） | admin（`auth.admin_role`） | anonymous（auth off，或 `auth.public` 的 procedure / optional 沒帶 credential） |
|---|---|---|---|
| `CreateOrder` | `customer_id` 空 = 自己；帶自己 = 自己；帶別人 → `PERMISSION_DENIED`（`order.permission_denied`），不寫入 | 帶了用帶的（代客下單），沒帶 = 自己 | 現行規則：`customer_id` 必填（`order.empty_customer`） |
| `ListOrders` | 沒帶 `customer_id` = 自己的；帶別人 → `PERMISSION_DENIED` | 現行：空 = 全部，或指定 customer | 現行 |
| `GetOrder` | 讀別人的訂單 → `NOT_FOUND`（`order.not_found`，跟不存在的 id 一樣，不洩漏存在與否） | 現行 | 現行 |
| `CancelOrder` / `CompleteOrder` | 操作別人的訂單 → `NOT_FOUND`（`order.not_found`，同 `GetOrder`；訂單狀態不變、不發 event）；自己的照常。`CompleteOrder` 的語意（任何 caller 都能完成）沒改 | 現行：任何訂單 | 現行（consumer 的 `OrderCancellationRequested` 沒有 principal，所以仍可取消任何訂單） |

- 非 admin 的 `GetOrder` / `CancelOrder` / `CompleteOrder` 走 `Repository.FindByIDForCustomer`（postgres：`WHERE id = ? AND customer_id = ?`，不符就在載入 items 之前結束）：「不存在」與「不是你的」走同一條查詢路徑：兩者都在選列時結束，不載入他人的 items，所以回應時間不隨訂單大小而異。這不是密碼學意義的 constant-time 保證。admin / anonymous 照舊 `FindByID`。
- page token 綁的是**解析後**的 customer，所以非 admin 不帶 filter 翻頁、token 跟自己的 Subject 一致。
- `auth.mode: off` 時沒有 principal，整張表只剩 anonymous 那欄；consumer path 永遠是 anonymous，不受影響。
- `auth.public` 列進去的 procedure 不跑 auth，也就是 anonymous：公開的 `GetOrder` 任何人都讀得到任何訂單。ownership 保護的是「帶了 credential 的人」。
- **冪等（`idempotency_key`）**：`CreateOrder` 的重送保護，範圍是**解析後的 customer**——語意見 README 的「CreateOrder 冪等」。
- **行為變更**：以前任何 credential 都能替任何 customer 下單。現在 role-less 的 static key / JWT 只能替自己下單，要代客下單的整合（例如後台系統）需要 `auth.admin_role`。
  proto 的 `CreateOrderRequest.customer_id` 拿掉 `min_len`（沒用 `optional` 關鍵字，Go 型別不變、`buf breaking` 無差異）；空值的語意由 use case 決定。
- Console 的下單表單：Customer 選填；admin（以 `ListAccounts` 探測，跟 Accounts 導覽同一個判斷）或 anonymous（`WhoAmI.anonymous`）才可填，其他人欄位鎖住、不送 `customer_id`。這只是便利，伺服器才是關卡。

## 檔案

```
pkgs/identity/
├── provider.go                 ProviderSet；decorate / decorateTx（沒有 inbox：沒有 consumer path）
├── domain/account, domain/apikey
├── application/{dto,usecase,port,mapper}   ResolveAPIKey 與 WhoAmI 也是 use case
├── infra/{memory,postgres,system}          identity.accounts / identity.api_keys（roles 是 jsonb array）
└── adapters/{auth,rpc}                     auth.Resolver 實作 server.CredentialResolver；rpc 是 Connect handler
```
