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

全部 `server.RequireRole(auth.admin_role)`，**除了 `WhoAmI`**（回 caller 自己的 principal；anonymous 時 `anonymous: true`）：
`CreateAccount` / `GetAccount` / `ListAccounts`（id 升冪，opaque page token）/ `DisableAccount` / `EnableAccount` / `AssignRoles` /
`CreateApiKey`（回 secret 一次）/ `ListApiKeys`（含 REVOKED，新到舊）/ `RevokeApiKey`。

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

「DB 掛掉仍服務 30s 內驗過的 key」是刻意的（可用性 > 立即一致；不等於放行），要改成嚴格 fail-closed 就得每個 request 都查 DB，等於沒有 cache——這是產品決定，不是 bug。

v1 **不記** `last_used_at`（hot path 寫入）；要審計誰用了哪把 key看 access line 的 `sub`（account id）。

## 擴充路徑：帳號密碼 / 自己當 issuer

Account 已經是「誰」，password 不是它的欄位，而是第二種 credential aggregate：

1. `domain/password`：`PasswordCredential{accountID, argon2idHash, updatedAt}`，`Verify(plain)`，一個 account 一筆。
2. `Login(name, password)` use case：走同一條 `account.IsActive()` / roles 路徑，輸出同一個 principal 形狀。
3. `Login` RPC 發自簽 JWT（HS256，用現有 `auth.jwt.hs256_secret`，`sub` = account id）→ **interceptor 完全不用改**，它已經驗 JWT 的 sub / roles。
4. 要加的是 issuer 那一側：token 過期、refresh / logout、console login 頁、brute-force 鎖定——都在 identity BC 與 console 內，不碰 platform / 其他 BC。

## Ordering ownership（PR 3）

identity 存在的業務理由：訂單屬於 principal。授權判斷在 ordering 的 **application 層**（`application/ownership.Policy`，三個 use case 呼叫它；domain 不碰 principal），
admin role 名稱是注入的 `auth.admin_role`（`ProvideOwnership`），不是寫死的 `"admin"`。「誰是 customer」= `Principal.Subject`（identity key 是 account id、JWT 是 `sub`、static key 是 name）。

| RPC | 非 admin（有 principal） | admin（`auth.admin_role`） | anonymous（auth off，或 `auth.public` 的 procedure / optional 沒帶 credential） |
|---|---|---|---|
| `CreateOrder` | `customer_id` 空 = 自己；帶自己 = 自己；帶別人 → `PERMISSION_DENIED`（`order.permission_denied`），不寫入 | 帶了用帶的（代客下單），沒帶 = 自己 | 現行規則：`customer_id` 必填（`order.empty_customer`） |
| `ListOrders` | 沒帶 `customer_id` = 自己的；帶別人 → `PERMISSION_DENIED` | 現行：空 = 全部，或指定 customer | 現行 |
| `GetOrder` | 讀別人的訂單 → `NOT_FOUND`（`order.not_found`，跟不存在的 id 一樣，不洩漏存在與否） | 現行 | 現行 |
| `CancelOrder` / `CompleteOrder` | **這次沒改**（任何 caller 都能操作任何訂單；Follow-up） | 同 | 同 |

- 非 admin 的 `GetOrder` 走 `Repository.FindByIDForCustomer`（postgres：`WHERE id = ? AND customer_id = ?`，不符就在載入 items 之前結束）：「不存在」與「不是你的」走同一條查詢路徑，回應與成本都一樣，不能靠回應時間分辨。admin / anonymous 照舊 `FindByID`。
- page token 綁的是**解析後**的 customer，所以非 admin 不帶 filter 翻頁、token 跟自己的 Subject 一致。
- `auth.mode: off` 時沒有 principal，整張表只剩 anonymous 那欄；consumer path 永遠是 anonymous，不受影響。
- `auth.public` 列進去的 procedure 不跑 auth，也就是 anonymous：公開的 `GetOrder` 任何人都讀得到任何訂單。ownership 保護的是「帶了 credential 的人」。
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
