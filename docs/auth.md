# Auth

<sub>← [README](../README.md)</sub>

這份文件給兩種讀者：要在衍生專案接上自己的 `server.CredentialResolver`／`server.ExternalIdentityResolver` 的人，以及設定 `auth.*` 的操作者。`auth.*` 各 key 的預設值與 env 名稱在 [config.md](config.md)，這裡不重複；`x-error-code` 在 [error-codes.md](error-codes.md)，metrics 與 log 在 [observability.md](observability.md)。

## 範圍

- 認證回答「這個呼叫以誰的身分執行」，結果是放進 context 的 `contextx.Principal`；「誰可以呼叫哪個方法」由方法在 proto 上的宣告決定，authorization interceptor 執行（見「授權宣告」）。資料層的規則（例如只能讀自己的那一筆）仍寫在 use case。
- `auth.mode: required` 時，`/api/healthz`、`/api/readyz`、`/api/version`、`/api/metrics` 仍不需要 credential；`/api/docs` 在 docs 開著時也是：它們是 Connect interceptor chain 之外的 gin 路由。
- 下面「憑證與優先序」「擴充點的契約」「失敗語意與逾時」描述的是請求經過 auth interceptor 時的行為，只在 `auth.mode` 為 `optional` 或 `required` 時成立；`off` 時不掛 interceptor（`NewAuthInterceptor` 回 nil），也不做 interceptor 內的啟動檢查（見「啟動檢查的順序」）。
- authorization interceptor 和 auth interceptor 一起掛：`auth.mode` 為 `off` 時兩個都不掛。啟動時對宣告的檢查則不看 `auth.mode`（見「啟動檢查的順序」）。

## Template 的接線

- 內建的憑證來源是 `auth.api_keys`（header `X-API-Key`）與 `auth.jwt`（header `Authorization: Bearer`）。
- `apps/rpc/wire.go` 用 `NoCredentialResolver` 與 `NoExternalIdentityResolver`；兩者回傳 nil interface，所以 template 的 rpc 沒有接任何 resolver。
- template 的 rpc 沒有接任何 resolver，所以 `auth.mode` 是 `optional` 或 `required` 時，`auth.api_keys` 與 `auth.jwt` 至少要設一個、且內容有效才起得來；`auth.oidc.enabled: true` 起不來（`auth.mode: off` 也是）。各項檢查在哪一層、誰先報，見「啟動檢查的順序」。
- 要接自己的實作：在 `apps/rpc/wire.go` 把 `NoCredentialResolver` 或 `NoExternalIdentityResolver` 換成回傳你的實作的 provider。

## 憑證與優先序

1. 請求帶了 `X-API-Key`：先比對 `auth.api_keys`；對不上且有接 `CredentialResolver` 時，再問 resolver；仍對不上是 `auth.invalid_credential`，不會改去看 `Authorization`。沒有接 resolver、也沒設 `auth.api_keys` 時，帶 `X-API-Key` 的請求回 `auth.invalid_credential`（訊息 `api keys are not accepted`）。
2. 沒有 `X-API-Key`、有 `Authorization`：scheme 不是 `Bearer`（不分大小寫）是 `auth.invalid_credential`。
3. `Bearer` token 的去向取決於 `auth.oidc.enabled`：
   - 關著：交給 `auth.jwt`；沒設 `auth.jwt` 回 `auth.invalid_credential`（訊息 `bearer tokens are not accepted`）。
   - 開著：先交給 OIDC verifier。token 的 `iss` 不是任何已設定的 issuer 時，才交給 `auth.jwt`（沒設 `auth.jwt` 就拒絕）；`iss` 屬於某個 issuer 而驗證失敗，以及解析不成 JWT，都是終局拒絕，不會再試 `auth.jwt`。
4. 沒有任何 credential：`required` 回 `auth.unauthenticated`，`optional` 以 anonymous 執行。
5. 帶了壞的 credential，在 `optional` 也是錯誤，不會變成 anonymous。
6. proto 上宣告為公開（`public: true`）的方法不檢查 credential，帶了壞的也照樣執行。

OIDC verifier 在驗簽前不信任 token 的 `iss`：它只用來挑 issuer 的設定，選完之後仍完整驗簽。

## Principal

- 欄位是 `Subject`、`Name`、`Roles`、`Method`、`CredentialID`；`Subject` 為空表示 anonymous（`IsAnonymous`）。
- `auth.api_keys` 的 key：`Subject` 是設定裡的 name，`Roles` 是冒號後以 `|` 分隔的清單，`Method` 是 `api_key`。
- `auth.jwt` 的 token：`Subject` 是 `sub`（必填），`Name` 是 `name` claim，`Roles` 是 `roles` claim，`Method` 是 `jwt`；`exp` 必填。
- resolver 回傳的 principal，`Method` 由 platform 蓋上：`CredentialResolver` 的是 `api_key`，`ExternalIdentityResolver` 的是 `jwt`。
- `CredentialID` 由 `CredentialResolver` 的實作填，platform 不設也不讀。

## 擴充點的契約

兩個介面都在 `pkgs/platform/server`，由 composition root 接上，platform 不 import 任何 BC。OIDC verifier（`pkgs/platform/oidc`）不認識帳號：它的輸出是驗過的外部身分 `(iss, sub, email, email_verified, iat)`，把它對到帳號是 `ExternalIdentityResolver` 的事。

### `server.CredentialResolver`

```go
ResolveAPIKey(c context.Context, key string) (p contextx.Principal, found bool, err error)
```

實作者必須做到：

- 沒有這把 key 時回 `found=false`；查詢本身失敗時回 `err`。
- 除 `Method` 外，principal 的欄位由你回傳；platform 不檢查 `Subject`，`Subject` 為空的 principal 是 anonymous（`IsAnonymous`）。
- 自己負責快取與雜湊。

platform 會做：

- 靜態 `auth.api_keys` 先比對，命中時不會問 resolver。
- 回 `found=true` 時，principal 用你回傳的值，但 `Method` 被設成 `api_key`。
- `found=false` 的請求回 `auth.invalid_credential`。
- 回 `err` 的請求被拒（`auth.invalid_credential`，訊息 `credential could not be verified`），計入 `invalid`；`err` 的內容寫進 server log，不給 client。
- `auth.resolver_timeout` 大於 0 時，傳給 resolver 的 context 帶 deadline。

### `server.ExternalIdentityResolver`

```go
ResolveExternal(c context.Context, id contextx.ExternalIdentity) (contextx.Principal, error)
AllowsUnbound(procedure string) bool
```

`contextx.ExternalIdentity` 是 `{Issuer, Subject, Email, EmailVerified, IssuedAt}`。

實作者必須做到：

- 對 `ResolveExternal` 回三類答案之一：`server.ErrUnboundIdentity`（身分沒有對到帳號）、`server.ErrCredentialRefused`（對到的帳號不得登入）、其他 error（查詢本身失敗）。
- 介面註解的契約是：principal 的 `Subject` 是帳號 id、`Roles` 是帳號的角色而不是 token 的；自己負責快取，且不快取失敗。platform 不檢查這些。
- `AllowsUnbound(procedure)` 只對該讓「還沒有帳號的人」呼叫的 procedure 回 true。

platform 會做：

- 回成功時，principal 除 `Method` 外用你回傳的值，`Method` 設成 `jwt`；platform 不檢查 `Subject`，同上。platform 不讀 token 的 `roles` 與 `name` claim：verifier 的輸出（`oidc.Identity`）沒有這兩個欄位。
- `ErrCredentialRefused` 的請求回 `auth.invalid_credential`，client 看到的與壞 token 相同。
- 其他 error 的請求被拒（`auth.invalid_credential`，訊息 `credential could not be verified`），原因寫進 server log，不給 client。
- `ErrUnboundIdentity`：`AllowsUnbound` 為 true 的 procedure 會通過 auth interceptor，此時沒有 principal（`IsAnonymous`），驗過的身分放在 context（`contextx.ExternalIdentityFrom`）；能不能執行還要看方法的授權宣告（見「授權宣告」）。其他非公開 procedure 回 `PERMISSION_DENIED`、`auth.unbound_identity`。`auth.mode: optional` 時也一樣，不會變成 anonymous。
- 已綁定的身分呼叫 `AllowsUnbound` 為 true 的 procedure，是一般的 principal 呼叫，interceptor 不特別處理。
- 驗過的外部身分（不論有沒有綁定）都放進 context；被拒絕的呼叫不帶它。
- verifier 與 resolver 要一起設或都不設：`auth.mode` 為 `optional` 或 `required` 時，只設一個會讓 `NewAuthInterceptor` 回錯誤 `auth: the oidc verifier and the external identity resolver are set together or not at all`。

## 失敗語意與逾時

- `auth.resolver_timeout`（預設 `2s`，`0` 表示不限）限制一次 resolver 呼叫，兩個 resolver 共用。auth 在 `http.timeout` 之外，所以 resolver 的查詢受它限制，不受 `http.timeout` 限制。
- `auth.resolver_timeout` 大於 0 時，超過它才回來的答案被拒。所以逾時後才回來的「沒綁定」不會讓 `AllowsUnbound` 的 procedure 通過。
- 不論 `auth.resolver_timeout` 為何，resolver 回來時呼叫的 context 已被取消，答案也被拒。被拒時不看答案是什麼：成功、沒綁定、帳號被拒、其他錯誤，結果都是 `auth.invalid_credential`。
- 三種拒絕的 client 訊息來自固定字串：resolver 查詢失敗（含逾時與 context 已取消）是 `credential could not be verified`；OIDC 路徑上 verifier 拒絕 token 或 `ErrCredentialRefused` 是 `token could not be verified`；`ErrUnboundIdentity` 且 procedure 不允許是 `identity is not bound to an account`（`PERMISSION_DENIED`）。其餘拒絕（例如 `auth.jwt` 驗證失敗）帶的是各自的訊息，不屬於這三類。

## OIDC verifier

設定欄位、https 規則、驗簽演算法、JWKS 的刷新與限速規則，在 [config.md](config.md) 的 `auth.oidc.issuers[]` 列。這裡補那一列沒有寫的：

- discovery 失敗後，同一個 issuer 在 30 秒內不再嘗試 discovery，期間該 issuer 的 token 被拒絕。
- JWKS 重抓失敗、key 超過 24 小時未成功刷新、`Close()` 的行為，在 `pkgs/platform/oidc/keyset.go` 的註解，測試在 `verifier_test.go` 與 `regression_test.go`。

JWKS 失敗與 discovery 失敗的 server log 分別是 `oidc: jwks refresh failed` 與 `oidc: discovery failed`（Warn，帶 `issuer` 與 `error`）；metrics 與 alert `x-monorepo-auth-jwks-stale` 見 [observability.md](observability.md)。

## 授權宣告

每個 RPC 方法在 proto 上用 `(common.v1.authz)`（`protos/common/v1/authz.proto`）宣告誰可以呼叫它：公開，或一組條件。

```proto
import "common/v1/authz.proto";

service OrderService {
  rpc Quote(QuoteRequest) returns (QuoteResponse) {
    option (common.v1.authz) = {public: true};
  }
  rpc Refund(RefundRequest) returns (RefundResponse) {
    option (common.v1.authz) = {
      rules: {
        any_of: [{kind: "user", permission: "orders.admin"}, {permission: "platform.admin"}]
      }
    };
  }
}
```

- `public` 與 `rules` 是同一個 `oneof`，一個宣告只能是其中一種。
- `any_of` 符合任一條就放行。一條條件的 `kind` 與 `permission` 至少寫一個，寫了的都要符合。
- 省略 `kind` 的條件表示「任何已驗證、持有該權限的呼叫者」，不依賴專案登記了哪些種類。

### 身分種類

- 種類不寫死在 platform：composition root 用 `server.WithPrincipalKinds(classify, kinds...)` 登記判斷函式與它可能回傳的種類；`PrincipalKind.RequiresPermission` 的種類，宣告時必須同時寫權限。
- `classify(c context.Context) string` 從 context 讀 principal（必要時加上 `contextx.ExternalIdentityFrom`），回傳一個種類名稱，`""` 表示沒有種類。
- 登記了種類、`auth.mode` 不是 `off` 時，非公開方法上每個有 principal 或外部身分的呼叫，authorization interceptor 呼叫它一次。它在 `http.timeout` 之外，context 沒有那個 deadline。
- 回傳未登記的種類時，這次呼叫視為不符合任何條件（只寫權限的條件也一樣），回 `PERMISSION_DENIED`，並記一筆 error log。
- template 的 `apps/rpc` 只登記一種：`KindUser`（`"user"`），有 principal 就是它，否則沒有種類（`apps/rpc/app.go` 的 `PrincipalKind`）。
- proto 宣告裡的種類是字串字面值：`task new-bc` 模板的 Get 寫 `kind: "user"`，模板的 kind 名稱要和 composition root 登記的一致；改了登記的名稱而沒改 proto，啟動檢查會擋下。

### 權限字串

- 權限字串格式是 `<bc>.<職能>`，符合 `^[a-z][a-z0-9]*\.[a-z][a-z0-9_]*$`。
- BC 在 `provider.go` 以 Go 常數定義，放進它的 `server.Mount.Permissions`；`apps/rpc` 的 `NewServerOptions` 用 `server.WithPermissions` 登記。持有條件是 `Principal.Roles` 有同名的角色。
- `platform.admin` 由 `server.New` 自己登記，對應 `auth.admin_role`（手組的 config 沒填時是 `admin`）。
- 比對 `Principal.Roles` 時要完全相等，沒有上下位關係。
- `Server.PermissionRoles()` 回傳登記的權限與各自對應的角色。

### 執行結果

| `auth.mode` | 公開方法 | 非公開，無 credential | 非公開，有 credential、不符合任何條件 | 非公開，符合某一條 |
|---|---|---|---|---|
| `off` | 放行 | 放行 | 放行 | 放行 |
| `optional` | 放行 | `UNAUTHENTICATED` / `auth.unauthenticated`（authorization interceptor） | `PERMISSION_DENIED` / `auth.permission_denied` | 放行 |
| `required` | 放行 | `UNAUTHENTICATED` / `auth.unauthenticated`（auth interceptor 先擋） | `PERMISSION_DENIED` / `auth.permission_denied` | 放行 |

- 沒有 principal（`IsAnonymous`）也沒有外部身分的呼叫，在非公開方法上是 `UNAUTHENTICATED`。
- OIDC 未綁定的身分（`AllowsUnbound` 放行、沒有 principal）：`classify` 回傳的種類符合某一條只寫 `kind` 的條件才放行；它不持有任何權限。不符合是 `PERMISSION_DENIED`。template 的 `classify` 不給它種類，所以在非公開方法上一律被拒。
- 不在 `AllowsUnbound` 的方法上，未綁定的身分在 auth interceptor 就被拒（`auth.unbound_identity`），公開方法除外：公開方法不檢查 credential。
- authorization interceptor 在 auth 之後、timeout 與 validate 之前：被拒的呼叫者看不到任何 request 規則。
- 放行時，非空的種類放進 context，use case 用 `contextx.PrincipalKindFrom` 讀，不必再判斷一次；公開方法、`auth.mode: off`、consumer path 都沒有種類。

## 啟動檢查的順序

啟動被拒時看到的是最先不通過的那一層的錯誤，後面的層不會執行。順序來自 `apps/rpc/main.go` 的 `run`：先 `config.Load`，再 `Initialize`；`Initialize`（wire 產生的 `apps/rpc/wire_gen.go`）裡 `NewOIDCAuth` 先於 `server.New`，`server.New`（`pkgs/platform/server/server.go`）依序跑第 3、4、5 層。

1. **`config.Load`**：載入設定時的檢查。讀完設定檔之後先看已移除的 `auth.public`（`refuseRemoved`）：設定檔有這個 key（值是空清單、`null`、空字串也算），或 env `APP_AUTH_PUBLIC` 存在（空值也算），就拒絕。之後才檢查下面兩組 `auth.*` 項目。`auth.api_keys` 的格式、`auth.jwt.hs256_secret` 的長度、`auth.jwt.public_key` 的 PEM 解析不在這一層，在第 3 層。
   - 與 `auth.mode` 無關（`pkgs/platform/config/config.go` 的 `validate`）：`auth.mode` 必須是 `off`／`optional`／`required`；`auth.admin_role` 不得為空；`auth.resolver_timeout` 不得為負；`auth.jwt.hs256_secret` 與 `auth.jwt.public_key` 互斥。
   - 只在 `auth.oidc.enabled` 時跑（同一個檔案的 `validateOIDC`，由 `validate` 呼叫）：`issuers` 至少一筆；每個 issuer 的 `issuer` 非空、通過 https 規則、`audience` 非空、`mapping` 是合法值、不重複；`auth.jwt` 有設時，`auth.jwt.issuer` 非空且不等於任何 OIDC issuer。這些與 `auth.mode` 無關，`off` 也會跑。
2. **`apps/rpc` 的 `NewOIDCAuth`**（`apps/rpc/app.go`）：`auth.oidc.enabled` 而沒有接 `ExternalIdentityResolver` 就拒絕，不看 `auth.mode`。template 沒接，所以這一層對 template 是必拒。`auth.oidc.enabled` 為 `false` 時這一層什麼都不檢查。
3. **`server.NewAuthInterceptor`**（`pkgs/platform/server/auth.go`）：
   - `auth.mode` 為 `off` 時直接回 nil、不掛 interceptor，本層後面的檢查都不做。
   - `optional`／`required` 時：verifier 與 `ExternalIdentityResolver` 要一起設或都不設；`auth.api_keys` 與 `auth.jwt` 的內容要能解析（key 格式與長度、`hs256_secret` 至少 32 bytes、`public_key` 是 PEM 的 RSA／ECDSA／Ed25519 公鑰，由 `parseAPIKeys` 與 `newJWTVerifier` 檢查）；`auth.api_keys`、`auth.jwt`、`CredentialResolver`、OIDC 四種憑證來源至少要有一個。
4. **登記檢查**（`pkgs/platform/server/authz.go` 的 `newAuthzRegistry`，不看 `auth.mode`）：`WithPrincipalKinds` 最多一次、判斷函式非 nil、至少一個種類、名稱非空且不重複；每個權限字串格式正確且不重複（`platform.admin` 已經登記）。
   - 通過以上之後，`auth.mode` 為 `optional` 或 `required` 且 credential resolver 是唯一憑證來源時，`server.New` 印一行 Warn（`auth relies solely on the credential resolver …`）。
5. **宣告檢查**（同一個檔案的 `check`，route 掛完之後，不看 `auth.mode`）：每個掛上的 procedure 的 `connect.Spec.Schema` 是 protobuf method descriptor；有 `(common.v1.authz)`；形狀是 `public: true`，或至少一條條件且每條至少有 `kind` 或 `permission`；用到的種類與權限都登記過；標記 `RequiresPermission` 的種類有寫權限。錯誤列出每一個不通過的 procedure 名稱。

從 template 的 `apps/rpc` 實際啟動（`bin/rpc`，環境變數覆蓋設定、`APP_POSTGRES_URL=''`、`APP_HTTP_ADDR=':0'`）跑過的格子如下。每一列是一組具體輸入，沒寫到的 `auth.*` 都是預設值；「通過」只代表這一組輸入起得來，不代表同一類設定都起得來，值的格式由上面第 3 層檢查；其他結果寫「在第幾層被拒」。

| 輸入 | 結果 |
|---|---|
| `APP_AUTH_PUBLIC` 設成一個 procedure 或空字串（含同時給 `auth.mode=bogus`） | 第 1 層被拒（訊息指出 `auth.public` 已移除） |
| `auth.mode=off` | 通過 |
| `auth.mode=optional` 或 `required` | 第 3 層被拒 |
| `auth.api_keys=adm=0123456789abcdefAAAA:admin`，`auth.mode` 各是 `off`、`optional`、`required` | 通過 |
| `auth.jwt.hs256_secret=0123456789abcdef0123456789abcdef01`（34 字元），`auth.mode` 各是 `off`、`optional`、`required` | 通過 |
| `auth.api_keys=bad`，`auth.mode=required` | 第 3 層被拒 |
| `auth.api_keys=bad`，`auth.mode=off` | 通過（第 3 層不跑） |
| `auth.jwt.hs256_secret=short`，`auth.mode=off` | 通過（第 3 層不跑） |
| `auth.jwt.hs256_secret=short`，`auth.mode=required` | 第 3 層被拒 |
| `auth.oidc.enabled=true`，只用環境變數（`issuers` 空），`auth.mode` 各是 `off`、`optional`、`required` | 第 1 層被拒 |
| `auth.oidc.enabled=true`，一筆 issuer `https://idp.example.invalid` 沒設 `audience`，`auth.mode` 是 `off` 或 `required` | 第 1 層被拒 |
| `auth.oidc.enabled=true`，一筆 issuer 設了 `audience: client-x`，另有 `mapping: bogus`、或同一個 issuer 出現兩次、或 issuer 是 `http://idp.example.invalid`、或 issuer 是空字串，`auth.mode=off` | 第 1 層被拒 |
| `auth.oidc.enabled=true`，一筆有效 issuer（`https://idp.example.invalid`、`audience: client-x`），`auth.mode` 各是 `off`、`optional`、`required`；另外各加上面那個有效的 `auth.api_keys` | 第 2 層被拒 |
| `auth.oidc.enabled=true`，同上的有效 issuer，加上面 34 字元的 `auth.jwt.hs256_secret` 而沒設 `auth.jwt.issuer`，`auth.mode` 各是 `off`、`optional`、`required` | 第 1 層被拒 |
| `auth.oidc.enabled=false`，`auth.oidc.issuers` 是字串 `not-a-list`，`auth.mode=off` | 第 1 層被拒（設定檔仍會被解碼，型別不對就失敗） |
| `auth.oidc.enabled=false`，`auth.oidc.issuers` 有一筆 `issuer: ""`，`auth.mode=off` | 通過 |

這張表以外的格子（接了 `CredentialResolver`、接了 `ExternalIdentityResolver` 與 verifier、`off` 時只設其中一個）template 的 `apps/rpc` 跑不到，因為它沒接任何 resolver；那些句子的依據是上面列的程式行，以及直接呼叫 `NewAuthInterceptor` 的測試。第 4、5 層拒絕的情況 template 的 `apps/rpc` 也跑不到（它的登記與 `admin.v1` 的宣告都合法），依據是從 `server.New` 跑的 `pkgs/platform/server/authz_test.go`、`authz_startup_test.go` 與 `apps/rpc/authz_assembly_test.go`。

## 設計決定

這一節沒有程式出處，是設計時的取捨，與上面的行為敘述分開。

- 不要用 `auth.jwt.hs256_secret` 去簽另一個 verifier 會接受的 token：共用同一把對稱 secret 的 verifier，彼此都能偽造對方的 token。
- 驗簽與 claims 檢查用 `golang-jwt/jwt/v5`，JWKS 用 `MicahParks/keyfunc` 與 `jwkset`，不自己實作；platform 只保留一層薄 adapter。
