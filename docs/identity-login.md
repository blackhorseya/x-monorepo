# Identity：帳號密碼登入與自家 JWT issuer（設計）

<sub>← [README](../README.md) · [identity.md](identity.md)</sub>

> **狀態：提案，未實作。** 這份只定設計與拆 PR 計畫；`docs/identity.md` 的「擴充路徑」只有四行草稿，這裡把它展開並與替代方案比較。
> 所有範例的 secret 都是佔位符（`<…>`），不是可用的值（紅線 R4）。

## 1. 目標與非目標

**目標**

- 人（Console 使用者）可以用 account name + password 登入，拿到短命 access token 呼叫 RPC。
- token 是**自家簽發**的 JWT，經由既有的 auth interceptor 解成同一個 `contextx.Principal`（`Subject` = account id、`Roles` = account roles、`Method` = `jwt`）；ordering ownership 等下游**不用改**。
- 可以撤銷（登出、停用 account）、可以輪替簽章金鑰、登入有節流、錯誤不洩漏帳號是否存在。
- 預設關閉；`enabled=false` 時行為與現在逐字相同。開啟後，既有 API key 使用者不受影響；外部 JWT 使用者在**滿足 §2 的啟用前提**（外部 verifier 已 pin `iss`）時不受影響。

**非目標**

- 不做 OAuth2 / OIDC provider（不發 id_token、沒有 authorize endpoint、沒有第三方 client 註冊）。
- 不做註冊、忘記密碼、email 驗證、MFA、social login（沒有 email / 通知通道；列在未決問題）。
- 不做授權（role 檢查仍是讀的人的事，interceptor 不判斷）。
- 不改 API key 的設計；機器對機器的呼叫繼續用 API key。

## 2. 現況與憑證優先序

| 憑證 | 來源 | 驗證 | 備註 |
|---|---|---|---|
| `X-API-Key`（static） | config `auth.api_keys` | sha256 查表，不打 DB | break-glass，DB 掛了也進得去 |
| `X-API-Key`（DB） | `identity.api_keys` | `CredentialResolver`（cache 30s） | `Subject` = account id、有 `CredentialID` |
| `Authorization: Bearer`（外部） | `auth.jwt`（單一 HS256 secret **或**單一 PEM public key） | `jwtVerifier`，演算法家族釘死在 key 型別，`exp` 必填 | 只有 verifier，沒有 `kid` / JWKS |
| `Authorization: Bearer`（自家，**新**） | `identity.signing_keys` | 本設計 | `iss` = 自家 issuer |

**優先序（開啟自家 issuer 後也不變）**

1. 有 `X-API-Key`：config → resolver（維持現狀）。對不上就是 `auth.invalid_credential`，**不會**再 fallback 到 Bearer。
2. 沒有 `X-API-Key`、有 `Authorization: Bearer`：**依 `iss` 分派到唯一一個 verifier**——
   - `iss` 與 `auth.issuer.name` **完全相等** → 自家 keyset（依 `kid`，只收 EdDSA）；
   - 其他（含空、未知）→ 既有的外部 verifier，由它自己的規則決定（`auth.jwt.issuer` 有 pin 就只收那個 `iss`；沒有外部 verifier 就拒絕）。
   - 分派只為了選 verifier：`iss` 在驗簽前是不可信的，所以 **選完之後仍完整驗簽 + `iss` / `aud` / `exp`**；兩個 verifier **不互相 fallback**（不用驗簽失敗後的重試來掩蓋邊界；JWT profile 互斥，見 RFC 8725 §3.12）。
   - **現況的反例**：目前外部 `auth.jwt.issuer` / `audience` 可以留空（wildcard，任何 `iss` 的合法簽章 token 都過；reviewer 已用 current main 實測）。wildcard 外部 verifier 與自家 `iss` 的信任邊界會重疊——帶著自家 `iss` 的既有外部 token 會被改派到自家 verifier 而被拒。所以**不能**宣稱「開啟後對所有外部 JWT 設定零影響」，改為下面的啟用前提。
3. 都沒有：`auth.unauthenticated`（`required`）或 anonymous（`optional`）。

**啟用前提（啟動時檢查，不滿足就拒絕啟動）**：若有設外部 verifier（`hs256_secret` 或 `public_key`），則 `auth.jwt.issuer` **必須非空**且 ≠ `auth.issuer.name`；自家 `aud` ≠ 外部 `aud`。沒有外部 verifier 則無此限制。這讓兩個 profile 以 `iss` 互斥、信任邊界不重疊。
遷移條件：外部 `auth.jwt.issuer` 目前留空的部署，要先把它 pin 成該 IdP 實際簽發的 `iss`（這本身是收緊，在啟用 issuer 之前獨立上線並觀察 `auth_decisions_total{result="invalid"}`），再啟用自家 issuer。audience 不強制，issuer 必設。

## 3. 方案比較

### 方案 A：自建 issuer（identity BC 內帳密 + 簽 JWT）

- **安全面**：密碼雜湊、節流、session、金鑰保管全部由我們負責，攻擊面變大（credential stuffing、timing、refresh token 外洩）。好處：資料不出自家，沒有第三方依賴；DB dump 本身不夠偽造 token（見 §5.2 的 KEK 設計）。
- **維運面**：多一組要 rotate 的簽章金鑰與 KEK、一組 login 節流 alert、一個要備份的 password 表。不依賴外部服務可用性。
- **工作量**：大（約 7 支 PR，見 §9）；platform 要從「單一 verifier」改成「依 `iss` / `kid` 的 keyset」。
- **使用者面**：沒有 SSO、沒有 MFA（日後要補得自己做）。

### 方案 B：接外部 OIDC provider，只做 verifier

- **安全面**：密碼、MFA、鎖定、帳號復原都交給 provider（Keycloak、Auth0、Cloud IdP 等），我們只驗 JWT。攻擊面最小。Console 要走 Authorization Code + PKCE（redirect、`state`、`nonce`、token 交換），安全正確性轉移到 OIDC client 實作。
- **維運面**：要自己運行或購買 provider；provider 掛了就不能登入（既有 token 與 API key 不受影響）。需要 JWKS 支援：`jwtVerifier` 要能抓 `jwks_uri`、依 `kid` 選 key、cache + 輪替，**這部分和方案 A 的 platform 改動重疊**。
- **工作量**：中（verifier 加 JWKS、Console 加 OIDC client、account ↔ provider `sub` 對應）；但**多一個部署單元**，本 repo 的 Helm chart 目前沒有任何 IdP。
- **account 對應**：provider 的 `sub` 要對到 `identity.accounts`（否則 ownership 的 customer id 和 admin role 不知道從哪來）；要決定 roles 來自 token claim 還是 account 表（見未決問題）。

### 建議

**建議 A，但分階段，且把 JWKS / 多 issuer verifier 當成共用的第一步（PR 2）**——這一步無論最後走 A 或 B 都需要，做完以後方案 B 只剩「把 provider 的 `jwks_uri` 加進 keyset」。理由：

1. 專案目前是自給自足的 monorepo（Postgres + Redis，沒有 IdP），為了一組帳密引入一個 IdP 的維運成本比自建高；
2. 需求是「帳號密碼」，沒有 SSO / MFA / 社群登入的需求，OIDC provider 的主要價值用不上；
3. 風險以設計約束住（argon2id、固定時間的錯誤路徑、refresh rotation、節流、KEK 分離），每一項都有 RED 測試點；
4. 預設關閉 + 可逆：`auth.issuer.enabled=false` 時整個功能不存在。

**若 Sean 的真實需求其實是 SSO / MFA / 多個內部系統共用登入，改選 B**，PR 2 照做，PR 3–5 以「OIDC verifier + account 對應」取代。

## 4. 設計（方案 A）

```
Console ──Login(name, password)──►  IdentityService.Login   (public procedure)
   ▲                                   │ 1. throttle(sha256(normalized name), peer)
   │                                   │ 2. find account by name；不存在 → 對 dummy hash 驗一次
   │                                   │ 3. password.Verify（argon2id，併發上限）
   │                                   │ 4. account.IsActive
   │                                   │ 5. 建 session（refresh token 只存 sha256）
   │   access JWT（EdDSA，10 min）◄────┘ 6. 用目前 active signing key 簽 access token
   │   refresh token（HttpOnly cookie）
   │
   ├─ 一般 RPC：Authorization: Bearer <access>  ──► auth interceptor：iss=自家 → keyset(kid) → Principal
   ├─ Refresh（cookie）：rotate refresh token，換新 access
   └─ Logout：撤銷 session，清 cookie
```

### 4.1 Domain 新增（identity BC）

| Aggregate / VO | 欄位 | 行為 |
|---|---|---|
| `password.Credential` | `accountID`、`hash`（PHC string）、`updatedAt`、`version` | `New(accountID, plain, policy, now)`（驗長度、算 hash）、`Verify(plain)`、`NeedsRehash(policy)`、`Replace(plain, …)`；plaintext 不存、不 getter |
| `session.Session` | `id`、`accountID`、`familyID`、`refreshHash`、`createdAt`、`idleExpiresAt`、`absoluteExpiresAt`、`revokedAt?`、`rotatedAt?`、`version` | `Rotate(newSecret, now)`、`Revoke(now)`、`Usable(now)`；`Rotate` 已 rotated 的 session = reuse 偵測訊號（`ErrReused`） |
| `signingkey.Key` | `kid`、`alg`、`publicKey`、`encryptedPrivate`、`notBefore`、`retireAt`、`status ACTIVE\|RETIRING\|RETIRED` | `Sign` 只在 ACTIVE；`VerifiableAt(now)` |

archtest 的形狀規則照舊（private 欄位、constructor、無 `Set*`、不呼叫 `time.Now()`）；`*/domain/*` 100% 覆蓋 gate 要過。

### 4.2 密碼雜湊（argon2id）

- 演算法：`argon2id`，參數 **m = 64 MiB、t = 3、p = 2**，salt 16 bytes（`crypto/rand`），輸出 32 bytes；以 PHC string（`$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`）存，**參數跟著 hash 走**，日後調整參數不需要 migration——登入成功時 `NeedsRehash(policy)` 為真就在同一個流程補寫新 hash。
- 參數是起點（高於 OWASP 最低建議），上線前在目標 pod 規格實測（目標：單次 verify ≈ 100–300 ms）。**做法是量測後調整 config，不是猜。**
- **併發上限**：argon2 每次吃 64 MiB；沒有上限的話 N 個同時登入 = N × 64 MiB，是 memory DoS。用 weighted semaphore（`auth.issuer.argon2.max_concurrent`，預設 4），拿不到 slot 等到 context deadline → `RESOURCE_EXHAUSTED`。chart 的 rpc memory limit 要依此估算（文件化，不自動算）。
- **密碼政策**：長度 ≥ 12、≤ 128 bytes（上限擋 hash DoS）；不強制字元組合；不做週期性換密碼。不引入外部 breached-password 查詢（不送密碼的任何衍生值出去）。pepper 不做（多一把要保管的 secret，收益有限），列在未決問題。
- hash 與 plaintext **不進 log、不進 error、不進 metrics label、不進 span**（R4）。

### 4.3 簽章演算法與金鑰來源

- **簽章**：`EdDSA`（Ed25519）。不用 HS256：共用 secret 讓每個 verifier 都能偽造 token，也和 `auth.jwt.hs256_secret` 重用會造成「外部 secret 洩漏 = 自家 token 可偽造」。**不重用 `auth.jwt.hs256_secret`**（推翻 `identity.md` 擴充路徑草稿的第 3 點）。verifier 釘死 `EdDSA`，拒絕 `none` 與其他演算法（防 algorithm confusion）。
- **約束**：image 非 root、唯讀 rootfs，只寫 `/tmp`；不用 `APP_CONFIG` 檔、不把私鑰放 Secret 檔。所以**私鑰不放檔案系統**：

| 方案 | 私鑰在哪 | 輪替 | 評估 |
|---|---|---|---|
| (a) 私鑰直接放 Secret / env | k8s Secret | 改 Secret + 重啟 rollout | 最簡單，但輪替要 rollout、多個 `kid` 同時有效要自己拼；私鑰直接在 Secret 裡 |
| **(b) DB 存「被 KEK 加密的私鑰」，KEK 放 Secret（建議）** | `identity.signing_keys.encrypted_private`（AES-256-GCM，AAD = `kid`） | 在 DB 內新增 / 退役，**不需重啟**，所有 replica 讀同一份 | DB dump 單獨不夠偽造；Secret 只剩一把對稱 KEK（`APP_AUTH_ISSUER_KEK`，32 bytes base64）；KEK 輪替 = 重包所有私鑰（`worker identity rewrap-keys`） |
| (c) 外部 KMS / Vault Transit | KMS | KMS 管 | 最強、不需要 KEK 在 process 內；但本 repo 沒有 KMS 依賴，工作量與維運成本最高 |

  **建議 (b)**：符合現有「secret 走 `APP_*` env」慣例（`existingSecret` 已支援），又不需要重啟輪替。(c) 作為日後升級路徑：KEK 介面抽成 port（`Sealer`），換實作不動 domain。
  誠實的代價：拿到 DB **加上** KEK 的人能偽造 token；防線是 KEK 與 DB 在不同的存取控制面（Secret vs Postgres 憑證），不是密碼學上的不可能。
- **kid / JWKS**：每把 key 有 `kid`（隨機 128 bit，base64url）。公鑰由 `GET /api/.well-known/jwks.json`（gin route，和 healthz 同層，永遠開、只含 ACTIVE + RETIRING 的**公鑰**）提供，給外部 verifier（日後的其他服務）用；內部 platform verifier 走 port（見 §4.4），不繞 HTTP。
- **輪替（rotation）**：`ACTIVE`（簽新 token）→ 新增一把新 key 成為 ACTIVE、舊的轉 `RETIRING`（**只驗不簽**，保留到「它簽過的最後一個 access token 過期」= `access_ttl` + leeway 之後）→ `RETIRED`（從 JWKS 與 keyset 移除，私鑰可清）。操作是 operator 子命令 `worker identity rotate-key`（與 `bootstrap` 同型：本機 port-forward、`--expect-target` 必填、不進叢集 Job）。排程自動輪替列為未決問題。
- keyset 在記憶體，TTL cache（預設 30s，和 resolver 同量級）；找不到 `kid` → 重新載入一次（限速，避免用亂 `kid` 打 DB）再拒絕。DB 掛：已載入的 keyset 繼續驗（bounded staleness，同 resolver 契約）；沒有可簽的 ACTIVE key → `Login` 回 `UNAVAILABLE`，**不降級成別的演算法**。

### 4.4 platform 的變更（PR 2，行為相容）

- 現在 `jwtVerifier` 是單一 key。改成 `server.TokenVerifier` port：`Verify(c, token) (Principal, error)`，auth interceptor 依 `iss` 選 verifier（§2）。外部 verifier 包現有實作；自家 verifier 由 identity 的 `adapters/auth` 提供，`apps/rpc/wire.go` 用 `wire.Bind` 接（與 `CredentialResolver` 同模式，platform 不 import BC）。
- 外部 verifier 順便支援 `jwks_uri`（方案 B 的前置，可獨立 PR / 以 flag 保留，見未決問題）。
- **`enabled=false` 行為不變**：沒設自家 issuer 時，所有 Bearer 都走外部 verifier（含 wildcard 設定），錯誤碼、metrics label（`method` 仍只有 `api_key / jwt / none`）完全相同。`enabled=true` 的相容範圍以 §2 的啟用前提為界。

### 4.5 Token 壽命、refresh 與登出

| Token | 形式 | 壽命 | 存放（Console） |
|---|---|---|---|
| access | JWT（`iss`、`aud`、`sub`、`name`、`roles`、`iat`、`exp`、`jti`、`sid`、`kid`） | **10 分鐘**（`auth.issuer.access_ttl`，上限 1h） | 只在記憶體 |
| refresh | 不透明 256-bit 隨機（`xr_` + base64url），DB 只存 sha256 | idle 8h、absolute 7d | `HttpOnly; Secure; SameSite=Strict; Path=/api/identity.v1.IdentityService/` cookie |

- **Rotation + reuse 偵測**：每次 `Refresh` 換一個新 refresh token，舊的標 rotated。已 rotated 的 token 再被出示 = 被偷（或客戶端 race）→ **整個 family 撤銷**，回 `auth.invalid_credential`，記一行 Warn（只有 family id 前綴與類別，無 token）。
- **不提供 server 端的 grace / 冪等 replay**。refresh token 在 DB 只存 sha256，換出去的新 token 是獨立隨機值，Pod A commit 後回應遺失、Pod B 只有舊 token 與 hash，**無從重現同一顆新 token**；所以不承諾「重送回同一結果」。後果與處理：
  - 合法的並行 refresh（多分頁）由 **client 端 Web Locks 序列化**（§5），不靠 server 寬容。
  - 「commit 後回應遺失」（網路中斷、pod 重啟）→ 客戶端重送舊 token → 被視為 reuse → family 撤銷 → **使用者重新登入**。這是有意的 fail-closed 取捨，文件化、有測試（PR 5）。
  - 替代機制是**可推導的 refresh token**（`HKDF(KEK) → HMAC(sid ‖ generation)`，server 只存 generation，重送舊 token 可重新推導下一顆），代價是 KEK 外洩可偽造 refresh token；列在未決問題 12，預設不做。
- **登出**：`Logout` 撤銷該 session（family），清 cookie；已簽出的 access token **最多再有效 `access_ttl`**。這是明確的取捨（見未決問題 4：是否加 `sid` 即時檢查）。
- **停用 account**：`Refresh` 每次檢查 `account.IsActive`，停用後下一次 refresh 失敗；access token 最多再有效 `access_ttl`，比 API key 的 30s cache 長，文件化。`DisableAccount` 同時撤銷該 account 全部 session（同一 transaction）。
- roles 在**簽發當下**從 account 取，不放進 refresh token；`AssignRoles` 後最慢 `access_ttl` 生效。
- 時間一律注入的 clock，domain 不呼叫 `time.Now()`。

### 4.6 登入節流與帳號鎖定（對齊既有機制）

既有的機制：`http.rate_limit`（interceptor，token bucket，在 auth 之外，global 或 per-peer）、resolver 對 miss 也 cache（擋猜 key 打 DB）、`last_used_at` 的條件式 `UPDATE`（多 replica 只有一個寫得進去）。登入沿用同樣的兩個思路：

1. **洪水層**：沿用 `http.rate_limit`；另加 procedure 層級的較緊限制（`auth.issuer.login_rate_limit`，per-peer）。
2. **帳號層（漸進式延遲，不是永久鎖定）**：`identity.login_throttle(key_hash, failures, locked_until, updated_at)`。
   - `key_hash = sha256(normalize(name))`，**不是 account id、也沒有 FK**：不存在的 name 也會累積 failure，節流行為對存在 / 不存在的帳號**完全一致**，不洩漏存在性。
   - 連續失敗 5 次後鎖 1 分鐘，之後每次失敗加倍，上限 15 分鐘；成功登入清零。鎖定中的請求**不驗密碼**（不花 argon2）。
   - 條件式更新（`UPDATE … WHERE locked_until <= $now` / upsert），多 replica 競爭只有一方生效，不需要 advisory lock。
   - 用漸進延遲而非永久鎖定：永久鎖定讓攻擊者可以對任何人的帳號做 DoS。管理員可 `AssignRoles` 之外另有 `ResetLoginThrottle`（admin RPC）解鎖。
3. 鎖定回應：`RESOURCE_EXHAUSTED` + `x-error-code: login.throttled` + `Retry-After`；對存在 / 不存在的帳號格式與延遲一致。
4. metrics：`identity.login.attempts{result}`，`result` ∈ `ok / invalid / throttled / error`（固定四值；**不放 name / account id / peer**，同 resolver 的 cardinality 紀律）；alert：`throttled` 比率異常（credential stuffing 訊號）。

**失敗結果必須在拒絕時仍持久化（committed rejection）**：`usecasex.Transactional`（`pkgs/platform/usecasex/transactional.go`）會把 use case 回的 error 傳給交易、導致回滾——如果 `Login` 在同一交易寫 failure counter 後 `return err`，計數會跟著回滾，節流形同虛設；`Refresh` 的 reuse → family 撤銷同理。所以：

- `Login` / `Refresh` use case 的**業務拒絕不是 error，是 output 的結果值**（`Outcome: Rejected{Reason}`，與 CLAUDE.md「業務上的失敗結果要做成狀態，不是 error」一致），交易正常 commit（計數 / 撤銷落地）；只有 infra 失敗才回 error（並回滾）。
- 由 **adapter（rpc handler）** 把 `Outcome: Rejected` 映成 `UNAUTHENTICATED` / `RESOURCE_EXHAUSTED`（`x-error-code`、`Retry-After`）。對外仍是錯誤，對內已 commit。
- RED：拒絕回應之後，**重新從 repo 讀**，斷言 `failures` 已增加、reuse 後 family 已全部 revoked。

### 4.7 不洩漏帳號是否存在

- `Login` 的所有失敗（name 不存在、密碼錯、account DISABLED、無 password credential）回**同一個** `UNAUTHENTICATED` + `x-error-code: login.invalid_credentials` + 同一段訊息。
- **時間**：name 不存在或無 credential 時，對一個固定的 dummy PHC hash 跑一次相同參數的 verify，再回錯誤；DISABLED 的帳號仍完整驗過密碼才回錯（不能比錯密碼快）。測試用注入的 hasher 計數（斷言呼叫次數相同），不靠 wall-clock。這不是密碼學意義的 constant-time 保證（和 `docs/identity.md` 對 ownership 的說法一致）。
- 管理 RPC（`CreateAccount`）對重名回 `ALREADY_EXISTS` 是 admin 才看得到的，不算洩漏；註冊流程不存在。
- log 只記類別（`login failed class=invalid|throttled|error`），**不記 name、不記 password、不記 hash**。

### 4.8 新 RPC

`identity.v1.IdentityService` 新增（`Login` / `Refresh` / `Logout` 要在 public procedure 內，不能要求 credential）：

| RPC | 授權 | 說明 |
|---|---|---|
| `StartLogin` | public | 無參數 → attempt ticket（見 §5.1）；無 DB 存取 |
| `Login` | public | `name`、`password`、`ticket` → access token + `expires_in`；`Set-Cookie` refresh |
| `Refresh` | public（憑 cookie） | rotate；回新 access token |
| `Logout` | public（憑 cookie 與 / 或 attempt `tickets`） | 撤銷 family、把對應 attempt 列設為 CANCELLED（§5.1）；冪等 |
| `ChangePassword` | 登入者本人 | 需要 current password；成功撤銷其他 session |
| `SetPassword` | `auth.admin_role` | 設定 / 重設某 account 的密碼；撤銷該 account 全部 session |
| `ResetLoginThrottle` | `auth.admin_role` | 解鎖 |

- 開啟 issuer 時，這三個 public procedure 由 wire **自動併入** `auth.public`（不要求 operator 手動維護，避免漏列 = 登入壞掉，或多列 = 洩漏）；`auth.issuer.enabled=false` 時它們回 `UNIMPLEMENTED`。
- `auth.mode: off` 時不可啟用 issuer（啟動檢查）：沒有 auth 的 server 簽 token 沒有意義。
- cookie 由 Connect handler 在 response header 設定（與「response meta 走 header」一致）；nginx 的 `/api/` proxy 預設會傳 `Set-Cookie`，不需改。

### 4.9 Config 新增（`docs/config.md` 在 PR 7 補）

| key | env | 預設 | 說明 |
|---|---|---|---|
| `auth.issuer.enabled` | `APP_AUTH_ISSUER_ENABLED` | `false` | 開關 |
| `auth.issuer.name` / `.audience` | … | 空（enabled 時必填） | `iss` / `aud`；必須與 `auth.jwt.*` 不同 |
| `auth.issuer.kek` | `APP_AUTH_ISSUER_KEK` | 空（enabled 時必填） | 32 bytes base64；**只走 env / Secret**，config 檔載入時拒絕這個 key |
| `auth.issuer.access_ttl` | … | `10m` | ≤ 1h |
| `auth.issuer.login_timeout` | … | `10s` | `Login` 單次最長處理時間，必須 > 0 且 ≤ 1m；不依賴 `http.timeout`（見 §5.1） |
| `auth.issuer.attempt_window` | … | `60s` | attempt ticket 的有效期（`not_after`），必須 > 0 且 ≤ 5m；取消契約的不可重設 admission 界線（見 §5.1） |
| `auth.issuer.session_idle_ttl` / `.session_max_ttl` | … | `8h` / `168h` | refresh |
| `auth.issuer.argon2.*` | … | m=65536KiB、t=3、p=2、max_concurrent=4 | 見 §4.2 |
| `auth.issuer.login_rate_limit` | … | 待量測 | per-peer |

## 5. 與 Console 的整合

現況（`web/apps/console/src/auth.ts`）：credential 是 `{kind: api_key | bearer, value}`，存 `localStorage`；`generation` 計數器讓「who am I」的 cache 不會把舊 credential 的答案當新的；`useResetOnCredentialChange` 重設 mutation；interceptor 在 call time 讀 credential。CSP 是 `script-src 'self'`（沒有 inline script）。

**新增**

- **登入頁**（`/login`）：name + password（`autocomplete="username"` / `current-password`，讓密碼管理員運作）；失敗只顯示 `login.invalid_credentials` 的固定文案；`throttled` 顯示 `Retry-After` 倒數。
- **token 存放：access token 只放記憶體，refresh token 放 `HttpOnly` cookie；兩者都不進 `localStorage` / `sessionStorage`**。理由：CSP 雖然擋 inline script，但任何一個 XSS（或被入侵的 npm 依賴）都能讀 `localStorage` 並帶走長效 credential；`HttpOnly` cookie 讀不到，記憶體中的 access token 只有 10 分鐘壽命、重新整理頁面就消失。代價：reload 後要靠 cookie 靜默 `Refresh`（一次 round trip）；CSRF 風險由 `SameSite=Strict` + cookie `Path` 限縮到 identity service + Connect 要求 `Content-Type: application/json` / `Connect-Protocol-Version` header（跨站表單送不出）+ 只接受同源 `Origin` 處理。`Secure` 在 `http://localhost` 的瀏覽器行為視為安全來源，本機開發不受影響；非 localhost 的 http 部署不支援登入（文件化）。
- **既有的貼 API key / JWT 控制保留**（break-glass 與機器 token），它設的仍是 `localStorage`（維持現狀，不擴大改動）；新增 `Credential.kind = "session"`，其值**不持久化**。兩者互斥：設定其中一種就清掉另一種。
- **舊回應不得覆寫新的憑證結果**（沿用並延伸 `generation`）：
  1. `login()` / `refresh()` 開始時記下 `generation`；回應到達時若 `generation` 已變（使用者在期間按了 Clear、換了 credential、或登出）→ **丟棄 body**，不 `setCredential`，也不顯示錯誤。
  2. **但丟棄 body 不能阻止瀏覽器處理 `Set-Cookie`**：慢的 `Login` 回應在 Logout 之後到達時，瀏覽器仍會收下 HttpOnly refresh cookie，reload 的靜默 `Refresh` 就把 session 救回來（reviewer 已用真 Chromium 重現這個順序）。所以**取消必須發生在 server 端**，見 §5.1；JS 的 generation 只管 UI 狀態。
  3. `refresh` 是 **single-flight**，且跨分頁用 **Web Locks**（`navigator.locks.request("console.refresh", …)`）序列化，**持鎖到 fetch 完成**。access token 只在各分頁記憶體、不跨分頁共享，所以不能假設別的分頁已更新自己的 token；拿到鎖後直接用目前共享的 cookie 送出（以最新 cookie 再 rotate 不構成 reuse），不靠 server 寬容（見 §4.5）。
  4. 401 的自動重試**每個 generation 最多一次**：收到 `UNAUTHENTICATED` → refresh → 重放原請求；refresh 失敗 → 轉為 anonymous 並導向 `/login`，不迴圈。
  5. 登出 / Clear：先 `bump generation`，再送 `Logout`（帶上所有 in-flight 的 attempt ticket，§5.1）；`Logout` 的回應（成功或失敗）都不改變 credential 狀態。
  6. 多分頁：登出 / 登入用 `BroadcastChannel` 通知其他分頁（只傳事件，不傳 token），其他分頁各自靜默 refresh 或回登入頁。

### 5.1 登入嘗試的 server 端取消（`attempt_id`）

- Console 每次登入先呼叫 `StartLogin`（public、無狀態、不碰 DB、受 rate limit）取得 **attempt ticket**：server 產生 128-bit 隨機 `attempt_id`，簽 `MAC = HMAC(HKDF(KEK, "attempt"), attempt_id ‖ not_after)`，`not_after = now + auth.issuer.attempt_window`（預設 60s，≤ 5m）；ticket = `attempt_id.not_after.MAC`。`Login` / `Logout` 都帶 ticket。**ticket 的 `not_after` 是不可重設的 admission 界線**：同一個 attempt 無法靠重送、延遲或重新進 queue 取得新的期限。
- `Logout` 接受 `tickets[]`（public procedure，不需 cookie）。
- **原子性契約**：以 `attempt_id` 為鍵有**一列**狀態 `identity.login_attempts(attempt_id PK, state PENDING|COMMITTED|CANCELLED, family_id?, not_after)`，`Login` 與 `Logout` 都**先對這一列取 row lock**，靠同一把鎖跨 pod 互斥（不是「各自一個普通 transaction 再各查一次」——那樣有 check→commit 的中間窗口）：
  - `Login`（單一 transaction，READ COMMITTED 即可）：先驗 ticket 的 MAC；**在 transaction 內以 DB 時鐘**（`transaction_timestamp()`，與 prune 同一個時鐘來源，不用 pod 時鐘）檢查 `now ≤ not_after`，不成立 → 拒絕（不建列、不建 session、不發 `Set-Cookie`）；成立才 `INSERT … (attempt_id, PENDING, not_after) ON CONFLICT DO NOTHING` → `SELECT … FOR UPDATE` 重讀該列。狀態是 `CANCELLED` → 拒絕；`COMMITTED`（重用）→ 拒絕；否則建 session、列改 `COMMITTED` 並記 `family_id`，同一 transaction commit。
  - `Logout`（單一 transaction）：驗 ticket MAC；`INSERT … (attempt_id, CANCELLED, not_after) ON CONFLICT (attempt_id) DO UPDATE SET state='CANCELLED'`（取同一把 row lock），再依列上的 `family_id`（不論 state 現在是什麼）撤銷整個 family，同一 transaction commit。ticket 已過 `not_after` 的 Logout 仍可執行（冪等，只會多寫一列）。
  - 互斥的推論：兩個 transaction 對同一列的寫入必須排隊。Login 先拿到鎖 → Logout 等 Login commit 後看到 `COMMITTED` + `family_id` 並撤銷；Logout 先拿到鎖 → Login 重讀時必看到 `CANCELLED`。**所以 `Logout` 成功返回之後，不存在可用的該 attempt 的 session，也不可能再被建立**（不論 Login 是在 check 前、check 後還是 commit 前被打斷）。
  - 記憶體 repo（無 DB）以 per-`attempt_id` mutex 提供同一語意，並跑同一組測試。
- **保留與 admission 界線（正確性不靠猜 TTL）**：
  - `login_timeout`（`Login` 單次最長處理時間，預設 10s，必須 > 0 且 ≤ 1m，與 `http.timeout` 無關——現況 `http.timeout=0` 合法）只限制**一次處理**；真正限制「這個 attempt 還能不能被受理」的是 ticket 的 `not_after`（上面的 DB 時鐘檢查）。
  - 列的保留期 = `not_after + login_timeout + 60s margin`（以 DB 時鐘比較），之後 prune 才可刪。因為 admission 檢查與 `INSERT` 在同一個 transaction、同一個 DB 時鐘，任何通過檢查的 Login 必在 `not_after` 之前開始，且在 `login_timeout` 內結束，所以**列被刪除之後，原 attempt 的任何 Login 都會在 ticket 檢查就被拒絕**，不可能「prune 後重新 INSERT 出全新 PENDING」。
  - 反例（reviewer 提出）：Login(A) 卡在 queue、Logout(A) 先建 CANCELLED、列被 prune、Login(A) 才進來 → 現在在 admission 就因 `now > not_after` 被拒。單純延長 TTL 或在每次 admission 重設 timeout 都不是這個契約。
  - `enabled=true` 時 `attempt_window` 必須 > 0 且 ≤ 5m、`login_timeout` 必須 > 0 且 ≤ 1m，否則拒絕啟動。
- `attempt_id` 是不可猜的 128-bit 值，等同取消能力：**按 credential 保密，不進 log / span / metrics**；它只能取消自己的嘗試，不是 account ownership 的證明，也沒有任何用 account id 撤銷他人 session 的路徑。同一個 `attempt_id` 只能用一次（在 ticket 有效期內由列的 state 保證；期限後由 `not_after` 保證）。
- 取消只作用於該瀏覽器的 attempt / family，**不是全裝置登出**；跨裝置不共享 cookie 與 attempt。收到「其他分頁登出」通知的分頁必須**取消自己的 pending attempts（送 `Logout`）並 bump generation**，不能只導回登入頁。
- `Refresh` 的遲到回應（輪替後的新 refresh token）屬於**同一個 family**；Logout 撤銷 family（以出示的 cookie 或 ticket 找到它），新舊 token 一併失效，不需要另外的機制。
- `AbortController` 中止 request 只是 best-effort 的省流量手段，**不作為 session 已取消的證明**；測試與規格都以 server 端狀態為準。
- E2E（`web/apps/console/e2e/auth/`）在既有 `auth` project 增加登入情境；需要 `auth.issuer.enabled` 的 rpc 設定。

## 6. 遷移與向下相容

- 全部是**加法**：新增 migration（接在 `0002` 後：`password_credentials`、`sessions`、`signing_keys`、`login_throttle`），不改既有表；`down` 只刪新表。
- `auth.issuer.enabled=false`（預設）：沒有新 RPC 生效、沒有新 route 行為、interceptor 的 Bearer 全走既有 verifier——**既有 API key / 外部 JWT / static key 使用者零影響**。
- `enabled=true`：API key 路徑不變；外部 JWT 在 §2 的啟用前提下不變（`iss` 已 pin 且與自家不同 → 互斥）。外部 issuer 未 pin 的部署必須先 pin 才能啟用（遷移步驟，見 §2）。
- PR 2 的 characterization 涵蓋：`enabled=false`；外部 issuer/audience 皆未設（wildcard）、只設其中之一、兩者皆設；外部 `iss` 與自家 `iss` 衝突 → 啟動拒絕；同樣的輸入在 `enabled=false` 下錯誤碼與 metrics 與現在相同。
- 沒有 password credential 的既有 account 不能登入，但 API key 照常。account 不需要 backfill。
- 啟動行為：`enabled=true` 但缺 `kek` / `name` / `audience`，或 `name` 與 `auth.jwt.issuer` 相同，或 `auth.mode=off` → **拒絕啟動**（fail-fast，不要 fail-open）。
- **Bootstrap**：`worker bootstrap` 目前只建 account + API key。要讓第一個管理者能用密碼登入，另開子命令 `worker identity set-password --expect-target host:port --account <id>`，**密碼只從 stdin / TTY 讀，不收 argv、不印出**；其餘規則同 bootstrap（本機 port-forward、不放叢集 Job）。第一把 admin API key 仍是 break-glass。
- 發版：新的 migration 與 chart 設定要寫進 `docs/operations.md`（升級順序：先 `migrate up`，再 rollout；因為預設關閉，順序錯了也不會壞）。

## 7. 風險與緩解

| 風險 | 緩解 |
|---|---|
| credential stuffing | per-peer 與帳號層節流、漸進延遲、`throttled` 比率 alert |
| 記憶體 DoS（argon2） | 併發 semaphore、密碼長度上限、鎖定中不驗密碼 |
| 帳號列舉 | 單一錯誤碼 / 訊息、dummy hash、節流 key 不依賴帳號存在 |
| refresh token 被偷 | 只存 hash、rotation + reuse 偵測 → 撤銷 family、`HttpOnly` cookie |
| 簽章金鑰外洩 | KEK 與 DB 分離、私鑰不落檔、kid 輪替與 RETIRING 窗口、可 rewrap |
| algorithm confusion / 跨 issuer | verifier 釘死 `EdDSA`、依 `iss` 分派且不互相 fallback、外部 `iss` 必須 pin 且 ≠ 自家 `iss`、`aud` 必須不同 |
| XSS 偷 token | 不進 Web Storage；access 只在記憶體且短命 |
| CSRF（cookie） | `SameSite=Strict`、`Path` 限縮、非簡單請求、`Origin` 檢查 |
| DB 掛掉 | 已載入 keyset 繼續驗（bounded staleness）；`Login` / `Refresh` 失敗；static API key 仍可用 |

## 8. 可觀測性與維運

- metrics（固定 label、無高基數）：`identity.login.attempts{result}`、`identity.refresh.attempts{result}`（`ok / invalid / reused / error`）、`identity.signing_key.active_age_seconds`（輪替提醒 alert）、`identity.keyset.lookups{result}`。
- `auth_decisions_total` 不變（仍只有 `api_key / jwt / none`）；access line 的 `sub` = account id，與現在一致。
- runbook（`docs/operations.md`）：輪替 key、KEK rewrap、解鎖、撤銷某 account 全部 session、`reused` 事件的處理。

## 9. 拆 PR 建議（每支一個分支、從 main 開、squash merge）

| # | PR | 內容 | RED 測試點 |
|---|---|---|---|
| 1 | `docs(identity): …`（本文件） | 設計、方案比較、未決問題；`identity.md` 擴充路徑改指向這份 | 免 RED（純文件） |
| 2 | `refactor(platform): select JWT verifier by iss` | `TokenVerifier` port、依 `iss` 分派、`kid` keyset 抽象；`enabled=false` 行為不變（選配 `jwks_uri`）；啟用前提的啟動檢查 | characterization：既有 JWT 測試全綠不改，且補外部 issuer/audience 未設 / 只設其一 / 皆設（含 wildcard 接受自訂 `iss`）；**assertion RED**：兩個 verifier 時，`iss=A` 的 token 用 B 的 key 簽 → 拒絕；外部 `iss` 未 pin 或等於自家 `iss` → 啟動拒絕；`alg=none` / `HS256`（對 EdDSA keyset）→ 拒絕 |
| 3 | `feat(identity): password credential domain` | `password.Credential`、argon2id、PHC、`NeedsRehash`、migration + repo（memory / postgres） | 新 API 型 RED（compile）+ GREEN 的 assertion：錯密碼 `Verify` 為假；參數低於 policy 下限 → `ErrWeakParams`；兩次 hash 同密碼結果不同（salt）；plaintext 不出現在 `%v` / error |
| 4 | `feat(identity): signing keys, KEK sealing and JWKS` | `signingkey` domain、`Sealer`（AES-GCM，AAD=kid）、keyset provider、`/.well-known/jwks.json`、`worker identity rotate-key` | **assertion**：錯 KEK 解不開；JWKS 不含任何私鑰欄位；`RETIRING` 只驗不簽、`RETIRED` 不在 keyset；輪替後舊 token 在窗口內仍可驗、窗口後被拒；未知 `kid` 重載限速 |
| 5 | `feat(identity): login, refresh and logout` | session domain、`StartLogin` / `Login` / `Refresh` / `Logout` / `ChangePassword` / `SetPassword`、節流表、auth.public 自動併入、metrics、config 與啟動檢查 | **assertion**：不存在 name 與錯密碼的回應（code / message）逐字相同，且 hasher 呼叫次數相同；DISABLED 帳號同樣完整驗密碼；第 N 次失敗後 `throttled`，且鎖定中不呼叫 hasher；對不存在 name 也會 throttled；reuse 舊 refresh → family 全撤銷（拒絕回應之後從 repo 重讀確認已落地，驗 committed rejection）；失敗登入的 counter 在拒絕後仍已遞增；「Pod A commit rotation 後丟回應、Pod B 用舊 token 重試」→ family 撤銷、需重新登入（明確行為）；`Logout{ticket}` 對已 commit 的 session 撤銷 family，對尚未 commit 的 Login 把列設為 CANCELLED 使其不建 session、不發 cookie；**用 barrier 固定「Login 已 `INSERT … ON CONFLICT` 且已 `SELECT … FOR UPDATE`（或尚未取鎖）、尚未 commit」時插入 Logout**，經真 handler＋repo（Postgres 與 memory 各跑一次）驗證：Logout 返回後重讀 session 為 revoked 或不存在、`Refresh` 失敗；另測 Login 先 / Logout 先 / 兩者交錯的所有順序；**取消 → 列到期 prune → 原 attempt 延遲 admission／重送**（用可控的 DB 時鐘或注入 clock）必須拒絕、不能建 session；也驗「跨過 prune 邊界但單次處理仍未超過 `login_timeout`」的情境；ticket MAC 被改、`not_after` 被改、他人 ticket 都拒絕；撤銷依列上的 `family_id`，不依賴 state=COMMITTED；`DisableAccount` 撤銷全部 session；`enabled=false` → `UNIMPLEMENTED`；缺 KEK 拒絕啟動 |
| 6 | `feat(console): login page and session credential` | `/login`、`session` credential、`StartLogin` ticket 與取消、Web Locks 序列化 refresh、generation 防護、BroadcastChannel | Playwright（`auth` project，**真 Chromium + 真 rpc**）：登入後呼叫成功；reload 靜默 refresh；**慢的 login 回應在 Logout/Clear 之後到達 → 不設定 credential，且之後 reload / `Refresh` 仍不能恢復該 session**（斷言 cookie jar 與 server session 兩側，不能只斷言 JS 狀態）；401 重試每個 generation 最多一次；登出後 access token 不留在任何 Web Storage |
| 7 | `docs/chore(helm)`: config / runbook / chart | `docs/config.md`、`docs/operations.md`、Helm `secrets.authIssuerKek`（支援 `existingSecret`）、`helm-check` profile、k6 `login`（throttle 429 + `Retry-After`）、dashboard row / alert | `helm-check` 新 profile 失敗在「缺 KEK 的 profile 不該 render 成功」；k6 threshold |

順序：1 → 2 →（3 ∥ 4）→ 5 → 6 → 7。2 與 3 互相獨立，可平行；5 依賴 2、3、4；6 依賴 5。每支都過 `task gate-local`、domain 100% 覆蓋。

## 10. 未決問題（請 Sean 決定）

1. **方案**：A（自建）還是 B（外部 OIDC）？本文建議 A；若實際需求包含 SSO / MFA / 多個內部系統共用登入，改 B。
2. **簽章金鑰保管**：(b) DB 加密私鑰 + KEK in Secret（建議），還是 (a) 直接 Secret，或 (c) 預留 KMS？KEK 輪替頻率與誰來執行？
3. **access token 壽命**：10 分鐘是否可接受？更短（5m）撤銷更快但 refresh 更頻繁。
4. **登出 / 停用的即時性**：接受「最多 `access_ttl` 後生效」，還是要在 interceptor 對自家 token 做 `sid` 檢查（30s cache，登出 / 停用 ≤ 30s 生效，但自家 JWT 變成有狀態、DB 掛掉時的行為要另訂）？
5. **自動輪替**：key 輪替先手動（`worker identity rotate-key`）還是做成 CronJob？
6. **密碼政策**：長度下限 12 是否合適？要不要 pepper？要不要在初期就支援 admin 強制「首次登入改密碼」？
7. **節流參數**：5 次失敗 / 1 分鐘起、加倍到 15 分鐘上限是否合適？是否需要 per-peer 的獨立上限？
8. **`jwks_uri` 支援**：PR 2 一併做（為方案 B 鋪路）還是延後到真的選 B 才做？
9. **非 localhost 的 http 部署**：登入要求 HTTPS（`Secure` cookie）——是否接受本機以外不支援 http？
10. **忘記密碼 / 註冊 / MFA**：確認都不在這個範圍（沒有 email 通道）？之後要做的話走哪個通道？
11. **roles 來源**（方案 B 才需要）：roles 取自 provider token claim，還是永遠以 `identity.accounts` 為準？
12. **refresh 回應遺失的處理**：接受「重新登入」（預設，fail-closed），還是改用可推導的 refresh token 容忍重送（KEK 外洩可偽造 refresh token）？
13. **外部 JWT 的 `iss` pin**：啟用 issuer 前，現有外部 `auth.jwt.issuer` 未設的部署（若有）要先 pin；這是收緊，要不要單獨一支 PR 先上？
