# Catalog

<sub>← [README](../README.md)</sub>

`pkgs/catalog` 是第四個 bounded context：**商品價格的 server 端來源**。`Product` = SKU + 名稱 + 價格（`kernel.Money`，每個 product 各自的幣別）+ 是否在售。
和 identity 一樣沒有 event（沒有 consumer / events adapter / outbox / inbox / poison），所以 `apps/worker` 與 `NewPoisonService` 都不用碰它。

> 目前 **ordering 還沒接它**：`CreateOrder` 的單價仍由呼叫端提供（`LineItem.unit_price`）。這個 BC 先獨立上線；ordering 改用 server 定價是後續的變更。

## 模型

| 欄位 | 規則 |
|---|---|
| `sku` | 1–64 bytes（`product.invalid_sku`）；主鍵 |
| `name` | 1–200 字元，不能全是空白（`product.invalid_name`）；「空白」= `strings.TrimSpace` 去掉的字元（tab / LF / VT / FF / CR / 空格 / U+0085 / U+00A0 / Unicode Z 類），schema 的 CHECK 用明列的字元集表達同一個定義（不用 `[[:space:]]`，它跟 locale 走）；DB 寫得進去的 name，Get / List 一定重建得起來 |
| `price` | `kernel.Money`：minor units（cents）、不能為負、幣別必須是**已指派的 ISO 4217 代碼**（`product.unsupported_currency`；格式不對是 `product.invalid_price`）。`Money` 的零值、`Multiply(-1)`、`ZeroMoney(任意字串)` 都能繞過 `NewMoney`，所以 aggregate 對每個進來的價格（`New` / `Reprice` / `Reconstitute`）重新檢查。**幣別屬於 product**，改價可以同時換幣別。白名單來源 SIX List One（2026-09-17，178 個代碼，`domain/product/currency_gen.py` 產生 `currency_codes.go` 與 migration 的 seed）；**不動 `kernel.NewMoney`**（ordering 也用它） |
| `active` | 下架（`Deactivate`）的 product 還在，但不能被報價 |
| `version` / `update_time` | optimistic lock（`product.ErrStale` wrap `kernel.ErrConcurrentUpdate`，rpc 回 `ABORTED` + `x-error-code: concurrent_update`）；時間由 `port.Clock` 給，domain 不呼叫 `time.Now()` |

Domain 方法反映行為：`Rename`、`Reprice`、`Deactivate`、`Activate`。`Deactivate` / `Activate` 在已是該狀態時什麼都不改（包含 `update_time`）。

## CatalogService（`protos/catalog/v1`）

| RPC | 說明 |
|---|---|
| `UpsertProduct` | 沒有就建立；有就**取代**名稱與價格，已下架的會**重新上架**（目錄沒有別的上架路徑）。auth 開著時要 `auth.admin_role` |
| `GetProduct` | 讀一個 SKU，不論在不在售；不存在 → `NOT_FOUND` + `product.not_found`。讀取不檢查 role |
| `ListProducts` | SKU 升冪、keyset 分頁（預設 20、上限 100）、`sku_prefix` 篩選（字面比對，`%` `_` 會跳脫）；page token 綁著篩選條件，換條件續頁是 `INVALID_ARGUMENT`（同 `ListStocks`）。**下架的也會列**，看 `active` |
| `DeactivateProduct` | 下架。已下架再下架成功、不改任何東西；不存在 → `NOT_FOUND`。auth 開著時要 `auth.admin_role` |

非 admin → `PERMISSION_DENIED` + `auth.permission_denied`，沒帶 credential → `UNAUTHENTICATED`，`auth.mode: off` 全開（和 `InventoryService.SetStock` 同一個 gate：`server.RequireRole`）。
請求的形狀（金額不得為負、幣別 `^[A-Z]{3}$`、價格必填、sku / name 長度）由 protovalidate 擋（`request.validation_failed`），domain 仍保留同樣的不變量。

## 批次報價（給 ordering 用的 port 來源）

`usecase.QuotePrices`（`provider.go` 的 `ProvideQuotePrices`，型別 `catalog.QuotePricesUC`）一次查多個 SKU（一次 query：`sku IN (…)`），
回答每個被問到的 SKU **恰好一次**，而且分得出三種結果：

| 欄位 | 內容 |
|---|---|
| `Prices` | 在售的 SKU → 價格（金額 + 幣別） |
| `Missing` | 沒有任何 product 的 SKU（多半是打錯） |
| `Inactive` | product 存在但已下架 |

`Missing` / `Inactive` 保持輸入順序、重複的 SKU 只算一次；空 SKU 字串 → `product.invalid_sku`。ordering 要用時，在它的 application/port 定義自己的報價 interface，
由 apps 或 ordering 的 adapter 接到這個 use case（BC 之間不直接 import，見 README 的 Dependency rules）。

## 儲存

- Postgres：schema `catalog`，表 `catalog.products`（migration `0001_catalog`，已接進 `apps/migrate`）。欄位 `price_amount bigint`、`price_currency char(3)`，
  `catalog.currencies`（ISO 4217 參照表，與 Go 白名單同一份，測試比對一致）+ `products.price_currency` 的 FK；CHECK 把其餘不變量也寫進 schema（金額 ≥ 0、sku / name 長度與 name 不得全空白），手寫的壞資料進不來。
  `sku` 欄位是 `COLLATE "C"`：keyset 分頁、`ORDER BY sku`、`LIKE 'prefix%'` 都是 bytewise，和 memory repo / Go 一致，不隨 cluster 的 locale 變（在 en_US.UTF-8 的 cluster 上實測）；C collation 的主鍵本身就服務 prefix 搜尋，所以沒有額外的 `text_pattern_ops` index。
  下一次 ISO 4217 發布 = 新 migration 對 `catalog.currencies` 補 / 刪差異，並重跑 `currency_gen.py` 更新 Go 白名單。
- `postgres.url` 空 → in-memory repository（`task run` 與 E2E 不需要 DB），遵守同一份 `Save` 合約（`ErrStale`）。
- `Save`：version 0 是 insert（同 SKU 第二次 insert 輸 → `ErrStale`），其他是 `UPDATE … WHERE version = ?`；`UpsertProduct` 在同一個 transaction 內 find → save。

## Console

`/products`（admin 頁，在 `AdminGate` 之後，nav 只對 admin 顯示）：儲存商品（SKU / Name / Price（minor units）/ Currency）、列表（`?prefix=` 篩選、Load more）、Edit（把該列帶回表單）、Deactivate。
E2E：`web/apps/console/e2e/products.spec.ts`（儲存 → 改價換幣別 → 下架 → 重新上架、server 拒絕壞價格、prefix 篩選與可分享連結），`e2e/auth/admin-gating.spec.ts` 驗非 admin 看不到、也不會對 `CatalogService` 發任何請求。
