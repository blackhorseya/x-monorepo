# Identity：外部 OIDC 登入（Dex 作為開發 IdP）

<sub>← [README](../README.md) · [identity.md](identity.md)</sub>

> **狀態：決策已定案（Sean 2026-10-30，#269 的 Q1–Q9），未實作。** 這份是現行規格，實作拆成 #270–#274（§12）。
> x-monorepo 是通用 template：登入走**外部 OIDC（方案 B）**，本機開發與測試用 **Dex**；自建帳密 issuer（方案 A）降為可選模組，未排程（§13）。
> API key 繼續用於機器對機器與 break-glass。**OIDC 預設關閉，關閉時行為與現在完全相同。**
> 所有範例的 secret 都是佔位符（`<…>`），不是可用的值（紅線 R4）。

## 1. 目標與非目標

**目標**

- 人（員工與客戶）從**同一個 IdP 入口**登入 Console，拿到 IdP 簽發的 access token 呼叫 RPC。
- token 經 auth interceptor 解成同一個 `contextx.Principal`（`Subject` = account id、`Roles` = account roles、`Method` = `jwt`）；ordering ownership 等下游**不用改**。
- 密碼、MFA、帳號復原、登入節流全部由 IdP 負責；我們只做 verifier、帳號對應與撤銷。
- 預設關閉；關閉時錯誤碼、metrics label、Principal 與現在逐字相同。

**非目標**

- 不做 OAuth2 / OIDC provider（不發 token、沒有 authorize endpoint）。
- 不做註冊、忘記密碼、MFA（IdP 的事）。
- 不做授權（role 檢查仍是讀的人的事，interceptor 不判斷）。
- 不從 token claim 推導 account id 或 role（Q4）。
- 不改 API key 的設計。

## 2. 決策與章節對照

| Q | 決策 | 章節 |
|---|---|---|
| Q1 | 單一 IdP、員工與客戶同入口、role 區分；`customer_id` = account id；verifier 支援多 issuer | §4 |
| Q2 | `mapping: bound \| auto_provision`、`(iss, sub)` 綁定、role 只能 admin 授予、email 邀請 | §5 |
| Q3 | Console 自己跑 PKCE；access token 在記憶體、refresh token 在 sessionStorage；BFF 為升級路徑 | §8 |
| Q4 | 每個請求 `(iss, sub)` 查 account；resolver 契約；驗簽規則；未綁定只能 `ClaimInvite` | §3、§6 |
| Q5 | `worker bootstrap --admin-email`；Console 保留 API key break-glass | §9 |
| Q6 | 本機 issuer `http://dex.localtest.me:5556/dex`；非 https 的放行條件；無 `jwks_url` override | §10 |
| Q7 | JWKS cache、重抓、metrics、alert、演算法 | §7 |
| Q8 | `sessions_revoked_at`；admin 撤銷（定位是「登出所有裝置」，不是盜用處置）；Console 登出；盜用處置步驟 | §11 |
| Q9 | 6 支 PR 與依賴 | §12 |

## 3. 憑證優先序

| 憑證 | 來源 | 驗證 | 備註 |
|---|---|---|---|
| `X-API-Key`（static） | config `auth.api_keys` | sha256 查表，不打 DB | break-glass，DB 掛了也進得去 |
| `X-API-Key`（DB） | `identity.api_keys` | `CredentialResolver`（cache 30s） | `Subject` = account id、有 `CredentialID` |
| `Authorization: Bearer`（OIDC，**新**） | `auth.oidc` 設定的 issuer 清單 | 多 issuer verifier（§6、§7）+ account resolver（§6） | `Method` = `jwt` |
| `Authorization: Bearer`（既有外部 JWT） | `auth.jwt`（單一 HS256 secret 或單一 PEM public key） | 既有 `jwtVerifier`，不變 | 沒有 `kid` / JWKS |

1. 有 `X-API-Key`：config → resolver（現狀）。對不上就是 `auth.invalid_credential`，**不會**再 fallback 到 Bearer。
2. 沒有 `X-API-Key`、有 `Bearer`：**依 `iss` 分派到唯一一個 verifier**——`iss` 與某個 OIDC issuer **完全相等** → OIDC verifier；其他 → 既有 `auth.jwt`（沒設就拒絕）。`iss` 在驗簽前不可信，所以選完 verifier 仍完整驗簽；兩者**不互相 fallback**。
3. 都沒有：`auth.unauthenticated`（`required`）或 anonymous（`optional`）。

**啟動檢查（PR 2）**：OIDC 開啟且 `auth.jwt` 有設時，`auth.jwt.issuer` 必須非空，且不等於任何 OIDC issuer。理由：wildcard 的既有 verifier（issuer / audience 留空，任何 `iss` 的合法簽章都過）會與 OIDC 的信任邊界重疊。這是沿用先前審查結論的前提，不屬於 Q1–Q9。

## 4. 身分模型（Q1）

- **單一 IdP**：員工與客戶從同一個入口登入，差別只在 account 的 roles。沒有分開的「員工目錄」與「客戶目錄」。
- **`customer_id` 就是 account id**：ordering ownership 照舊用 principal 的 `Subject`。
- **多 issuer**：verifier 接受 issuer 清單，**每個 issuer 各有自己的設定**——`audience`（我們的 client id）、`mapping`（§5）、`allow_insecure_issuer`（§10）。同一個 `sub` 在不同 issuer 是不同身分，所以綁定鍵永遠是 `(iss, sub)`。
- 設定 key 名稱（`auth.oidc.issuers[]` 等）在 PR 2 隨 `docs/config.md` 定案，這份只固定語意。

## 5. 帳號對應與邀請（Q2）

**`mapping`**（每個 issuer 一個值，預設 `bound`）：

| 值 | 行為 |
|---|---|
| `bound`（預設，fail-closed） | token 的 `(iss, sub)` 必須已綁定某個 account；沒綁定 → 只能 `ClaimInvite`（§6） |
| `auto_provision` | 第一次見到 `(iss, sub)` 就建 account 並綁定；**該 account 沒有任何 role** |

**不變式**

- 綁定一律用 `(iss, sub)`，**不用 email**（email 可變、可被 IdP 重新指派）。
- **role 只能由 admin 手動授予**（`AssignRoles`）；不從 token claim 推導，`auto_provision` 建出的帳號一律沒有 role。
- `identity` BC 新增 external identity：`(iss, sub)` 唯一對到一個 account；一個 account 可以有多個 external identity。

**邀請（預先綁定）**

1. admin 用 **email** 發邀請（admin RPC，需 `auth.admin_role`）：期限 **7 天**、**只能用一次**。邀請只記 email，不含 secret。
2. 對方從 IdP 登入，拿著合法但未綁定的 token 呼叫 `ClaimInvite`。
3. server 要求 token 的 `email_verified=true` 且 email 與邀請相符；成功就把 `(iss, sub)` 綁到邀請指定的 account，邀請失效。邀請過期、已使用、email 不符、`email_verified` 不是 true 一律拒絕。

## 6. 每個請求的對應與驗簽規則（Q4）

**驗簽規則**（OIDC verifier，每個 issuer 各自套用）

- `iss` 與設定**完全相符**。
- `aud` **必須包含**我們的 client id。
- `exp` / `nbf` 容忍 **60 秒**時鐘誤差。
- `iat` 必填（沒有 `iat` 的 token 拒絕，見 §11）。
- 演算法規則見 §7。

**Resolver**（沿用 API key 的 `CredentialResolver` 模式）

- 每個請求用 `(iss, sub)` 查 account，取得 **account_id、roles、disabled 狀態**，組成 Principal。
- cache **30 秒**；**錯誤不 cache**；DB 錯誤一律**拒絕**（不放行、不把錯誤內容給 client）。
- **account_id 與 roles 不放進 token**，也**不做** `roles_from: claim`。停用 account 與 `AssignRoles` 最慢 30 秒生效。
- platform 不 import BC：platform 宣告 port，`identity` 的 `adapters/auth` 實作，`apps/rpc/wire.go` 用 `wire.Bind` 接（與 `CredentialResolver` 同模式）。

**未綁定的身分**：token 合法但 `(iss, sub)` 沒有 account（`bound` 模式）→ **只能呼叫 `ClaimInvite`，其他一律 `PERMISSION_DENIED`**（interceptor 依 procedure 白名單擋，不是每個 handler 各自判斷）。

**關閉時**：`auth.oidc` 沒開，所有 Bearer 走既有 verifier，`method` label 仍只有 `api_key / jwt / none`。

## 7. JWKS 與演算法（Q7）

| 項目 | 規則 |
|---|---|
| 位置 | 由 issuer 的 discovery 文件取得；**不提供 `jwks_url` override**（§10） |
| cache 時間 | 依 `Cache-Control`，夾在 **5 分鐘到 24 小時**；沒給用 **1 小時** |
| 未知 `kid` | **立即重抓**，但**每個 issuer 每 30 秒最多一次**；重抓後仍找不到 → 拒絕 |
| 重抓失敗 | 沿用舊 key，**最多 24 小時**；超過就拒絕 |
| 啟動時抓不到 | **不視為致命錯誤**（IdP 暫時不可用不該讓 rpc 起不來；之後第一次請求或背景重試補抓） |
| 演算法 | 只收 **RS256 / ES256 / EdDSA**，且 `alg` 必須與 JWK 一致；`none` 與 HS256 一律拒絕（OIDC issuer 不接受 HS256） |

**Metrics**（label 固定、無高基數）：`auth_jwks_fetch_total{issuer,result}`、`auth_jwks_age_seconds{issuer}`。
**Alert**：任一 issuer **超過 2 小時沒有成功重抓**。dashboard 與 alert 在 PR 6 加（`docs/observability.md`）。

## 8. Console（Q3）

- **SPA 自己跑 Authorization Code + PKCE**：`state`、`nonce`、`code_verifier` 由 SPA 產生與驗證；scope 含 `openid profile email offline_access`（Dex 需要 `offline_access` 才發 refresh token）。
- **token 存放**：
  - **access token 只放記憶體**（reload 就消失，靠 refresh token 靜默取得）。
  - **refresh token 採 rotation，存 `sessionStorage`**（同分頁 reload 可延續，關分頁就消失；每個分頁各自一份）。
  - 誠實的取捨：`sessionStorage` 可被同源 XSS 讀取。緩解是 refresh rotation、CSP `script-src 'self'`、access token 短命；不是密碼學上的不可能。
- **BFF 是升級路徑，不在這個範圍**：之後若要把 refresh token 移出瀏覽器，改由 BFF 持有並以 `HttpOnly` cookie 與 SPA 溝通；此時需要另訂 CSRF 規則。現在不做，只記錄。
- **API key 入口保留**為次要的 break-glass（貼 key，維持現狀存放方式），可用 `console.apiKeyLogin` 關閉。OIDC 與 API key 兩種 credential 互斥：設定其中一種就清掉另一種。
- 沿用既有 `generation` 機制：登入 / refresh 回應到達時若 generation 已變（期間登出或換 credential）就丟棄，不寫入 credential。
- 新增 accounts UI（admin）：邀請、綁定狀態、`AssignRoles`、撤銷 session（§11）。

## 9. 第一個 admin（Q5）

- `worker bootstrap --admin-email <email>`：在**空 store** 上建 admin account，**同時發出該 email 的邀請**（§5，7 天）。部署者從 IdP 登入並呼叫 `ClaimInvite` 後完成綁定。
- **不做 config 層級的 `bootstrap_admins`**。
- 執行方式不變：**操作者本機**經 `kubectl port-forward` 連 DB、`--expect-target host:port` 必填、不放進叢集 Job；runbook 要求 Postgres 連線用 `verify-ca` 或 `verify-full` TLS（`docs/operations.md`，PR 6）。
- 不帶 `--admin-email` 時行為不變（account + 第一把 API key）；帶 `--admin-email` 時是否同時產出 API key，在 PR 3 定案。
- 邀請過期（7 天內沒人登入）：用 break-glass 的 static admin API key 重新發邀請。
- Console 以 OIDC 為主；API key 登入入口見 §8。

## 10. 本機 issuer（Q6）

- 統一 issuer：**`http://dex.localtest.me:5556/dex`**（`localtest.me` 的公開 DNS 解析到 `127.0.0.1`）。
- 容器內的解析：compose 用 `extra_hosts`、kind 用 CoreDNS rewrite，把 `dex.localtest.me` 指到 Dex 服務，讓瀏覽器（host）與 rpc（容器 / Pod）看到**同一個 issuer 字串**（`iss` 必須完全相符）。
- **離線時** `localtest.me` 解不出來：退回 `/etc/hosts` 加一行 `127.0.0.1 dex.localtest.me`（runbook 寫明，PR 6）。
- **非 https 的 issuer 只在兩種情況放行**：host 是 loopback，或該 issuer 的 dev 設定明確開啟 `allow_insecure_issuer`。所以 compose / kind 的 dev 設定明確開這個旗標（「loopback」是字面 host 還是解析後位址，PR 2 定義）；正式環境永遠不開，非 https 的 issuer 在兩個條件都不成立時拒絕啟動。
- **不提供 `jwks_url` override**：JWKS 一律從 discovery 取，避免 issuer 與金鑰來源脫鉤。

## 11. 登出與撤銷（Q8）

- account 新增 **`sessions_revoked_at`**：token 的 `iat` **早於**這個時間就拒絕；**沒有 `iat` 的 token 也拒絕**。因為走 §6 的 resolver，**30 秒內生效**。
- admin 可以「**撤銷所有 session**」（針對某個 account），定位是**「登出所有裝置」**（見下方限制）：需要 `auth.admin_role`，**寫入 audit**（誰、對哪個 account、何時；不記 token）。RPC 名稱在 PR 3 定案。
- **Console「登出」只清除本機 token**（記憶體的 access token 與 sessionStorage 的 refresh token）；IdP 提供 `end_session_endpoint` 時才導去 IdP 登出（**Dex 不支援**，所以本機開發只清本機）。
- 登出本身不呼叫 server 撤銷；要讓已簽出的 token 提早失效，用 admin 撤銷。已簽出的 access token 在 IdP 設定的壽命內，除非 admin 撤銷，否則仍有效。

**限制（Sean 決定採此做法）**：`sessions_revoked_at` 只會拒絕 `iat` **早於**撤銷時間的 access token。拿著有效 IdP refresh token 的人仍能向 IdP 換到新 `iat` 的 token，我們這邊撤銷不了 refresh token。所以 admin 的「撤銷所有 session」只是「登出所有裝置」，**不能當作帳號遭盜用時的處置**。
**不做 `auth_time` 檢查**：各家 IdP 不一定把 `auth_time` 帶進 access token。日後如有需要，可做成 per-issuer 的 opt-in。

**懷疑帳號遭盜用的處置步驟**

1. **停用帳號**（`DisableAccount`）：§6 的 resolver 每個請求都檢查 disabled 狀態，**30 秒內生效**，連用 refresh token 換出的新 token 也會被擋。
2. **到 IdP 撤銷該使用者的 refresh token，或重設憑證**（密碼、MFA）；這一步只有 IdP 能做。
3. 確認安全後再**重新啟用**帳號。

## 12. 拆 PR（Q9）

每支一個分支、從 main 開、squash merge。

| # | issue | PR | 內容 |
|---|---|---|---|
| 1 | #269 | `docs(identity)` | 本文件；`identity.md` 擴充路徑與 README 指標 |
| 2 | #270 | `feat(platform)` | 多 issuer OIDC verifier + JWKS cache（§6 驗簽規則、§7）；§3 啟動檢查 |
| 3 | #271 | `feat(identity)` | external identity 綁定、邀請、`ClaimInvite`、`sessions_revoked_at`、resolver、`bootstrap --admin-email` |
| 4 | #272 | `feat(platform)` | interceptor 串接：verifier → resolver → Principal；未綁定只放行 `ClaimInvite` |
| 5 | #273 | `feat(console)` | PKCE 登入、邀請認領、accounts UI、撤銷 session UI |
| 6 | #274 | `chore(dev)` | Dex（compose / kind）、E2E、Helm、runbook、dashboard / alert |

**依賴**：1 →（2 ∥ 3）→ 4 → 5 → 6。**PR 4 完成後，PR 5 起（Console / E2E 改動）要等定價 #267 合併完才開工**，避免與它的 Console 改動衝突。

**RED 測試點**（每支 PR 先交 RED）

- PR 2：`iss` 不符、`aud` 不含 client id、過期超過 60 秒、`alg=none` / HS256、`alg` 與 JWK 不一致、未知 `kid` 30 秒內重抓不超過一次、重抓失敗沿用舊 key 且滿 24 小時後拒絕、非 https 且非 loopback 且無旗標 → 拒絕啟動。
- PR 3（真 Postgres）：同一 `(iss, sub)` 並發綁定只有一個成功；邀請過期 / 重用 / `email_verified=false` / email 不符皆拒絕；`auto_provision` 建出的帳號沒有 role；`iat` 早於 `sessions_revoked_at` 拒絕、缺 `iat` 拒絕；撤銷寫入 audit。
- PR 4：DB 錯誤拒絕且不 cache；停用 account 後 30 秒內拒絕；未綁定身分呼叫非 `ClaimInvite` → `PERMISSION_DENIED`；OIDC 關閉時既有行為逐字不變。
- PR 5：Playwright（真 Chromium + 真 rpc + Dex）；登入、reload 靜默 refresh、登出後 access token 不在任何 Web Storage、`console.apiKeyLogin` 關閉時入口消失。
- PR 6：kind 內 issuer 解析一致；JWKS stale alert 觸發；E2E 走完 bootstrap → 邀請 → 登入。

## 13. 可選模組：方案 A，自建帳密 issuer（未排程）

> **歷史設計，不是現行規格。** 沒有排程、沒有 issue。只有在需要「不依賴外部 IdP 的自給自足部署」時才重啟；屆時它只是 §4 多 issuer 清單裡**多一個 `iss`**（自家 keyset），§5–§7、§11 的規則照舊適用。完整的原始設計（含 `attempt_id` 取消機制的完整論證與 PR 5 的 RED 清單）在 git 歷史 `f754dc1` 的 `docs/identity-login.md`。

保留的設計要點：

- **密碼雜湊**：`argon2id`（m = 64 MiB、t = 3、p = 2，salt 16 bytes），PHC string，參數跟著 hash 走、登入成功時 `NeedsRehash` 補寫；併發 semaphore（預設 4）防記憶體 DoS；密碼長度 12–128 bytes。
- **簽章**：EdDSA；**不重用** `auth.jwt.hs256_secret`。私鑰存 DB，以 **KEK**（AES-256-GCM，AAD = `kid`，KEK 只走 env / Secret）加密；`ACTIVE → RETIRING → RETIRED` 輪替，JWKS 只列公鑰。代價：拿到 DB **加上** KEK 的人能偽造 token。
- **Token 與 session**：access 10 分鐘；refresh 不透明隨機值、DB 只存 sha256，**session family** 的 rotation + reuse 偵測 → 整個 family 撤銷；不做 server 端 replay grace（「回應遺失」= 重新登入，fail-closed）。
- **節流與不洩漏帳號**：帳號層漸進延遲（5 次後 1 分鐘、加倍到 15 分鐘上限，不永久鎖定）、dummy hash、所有失敗同一個錯誤碼。拒絕必須 **committed rejection**（業務拒絕是 output 結果值，不是 error，否則計數隨交易回滾）。
- **登入嘗試取消**：`StartLogin` 發 `attempt_id`、`Login` / `Logout` 對同一列取 row lock，使「Logout 返回後不存在可用的該 attempt session」成立；Console 側有 pending-cancel journal。這是 refresh token 放 `HttpOnly` cookie 才需要的複雜度。
- **新 RPC**：`StartLogin` / `Login` / `Refresh` / `Logout`（public）、`ChangePassword`、`SetPassword`、`ResetLoginThrottle`。

## 14. 原「未決問題」的新狀態

先前第 10 節的 14 個問題逐一結案；**已決**的內容只在上面的章節寫一次，這裡只指向它。

| # | 問題 | 狀態 |
|---|---|---|
| 1 | 方案 A 還是 B | **已決**：B（外部 OIDC + Dex），A 降為可選模組（開頭、§13） |
| 2 | 簽章金鑰保管 | 隨方案 A 擱置（§13）；B 不持有簽章私鑰 |
| 3 | access token 壽命 | 改由 OIDC 取代：壽命由 IdP 決定，我們只驗 `exp` / `iat`（§6） |
| 4 | 登出 / 停用的即時性 | **已決**（Q8）：`sessions_revoked_at`，30 秒內生效（§11） |
| 5 | 自動輪替 | 隨方案 A 擱置（§13）；IdP 金鑰輪替靠 §7 的 JWKS 重抓跟上 |
| 6 | 密碼政策 | 隨方案 A 擱置（§13）；B 由 IdP 負責 |
| 7 | 節流參數 | 隨方案 A 擱置（§13）；B 的登入節流由 IdP 負責，既有 `http.rate_limit` 不變 |
| 8 | `jwks_uri` 支援 | **已決**（Q7）：支援，由 discovery 取得、不提供 override（§7、§10） |
| 9 | 非 localhost 的 http | **已決**（Q6）：非 https 只在 loopback 或明確 `allow_insecure_issuer` 時放行（§10）；不再有 `Secure` cookie 問題 |
| 10 | 忘記密碼 / 註冊 / MFA | 改由 OIDC 取代：全部交給 IdP（§1 非目標） |
| 11 | roles 來源 | **已決**（Q2、Q4）：永遠以 account 為準，不從 claim 推導（§5、§6） |
| 12 | refresh 回應遺失 | 改由 OIDC 取代：refresh 規則由 IdP 決定，Console refresh 失敗就回登入頁（§8）；可推導 refresh token 的選項隨方案 A 擱置 |
| 13 | 外部 JWT 的 `iss` pin | 沿用既有設計（非 Q1–Q9）：§3 的啟動檢查，PR 2 驗收 |
| 14 | 登入限流粒度 | 隨方案 A 擱置（§13）；B 沒有我們自己的登入 endpoint |
