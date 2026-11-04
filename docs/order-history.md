# Order history（訂單事件歷程）

每張訂單一份**只追加、不修改**的狀態轉換紀錄：誰、何時、從哪個狀態到哪個狀態、為什麼。表是 `ordering.order_history`
（migration `0010`，cause 的 CHECK 由 `0011` 放寬）。寫入路徑是切片 1；讀取的 `ordering.v1.OrderService.GetOrderHistory` 是切片 2（見〈讀取〉）；Console 的 History 卡是切片 3（見〈Console〉）。

## 為什麼不直接讀 outbox

`ordering.events_v1_*` 是傳遞管道：`worker prune` 會刪掉每個 group 都 ack 過的列、payload 是 proto bytes、沒有 actor。
history 是紀錄，所以另開一張表，**`worker prune` 不碰它**（有測試從真入口 `worker prune` 驗證）。

## 寫入路徑（結構性）

`ordering.ProvideEventPublisher` 把 outbox publisher 包進 `adapters/events.Recorder`，所有 use case 都經過它 publish。
Recorder 對每個 event 先 publish、再 `HistoryStore.Append`，兩者在 use case 的同一個 transaction 裡（`decorateTx`，
或 `CreateOrder` 自己開的 `Transactor`），所以一起 commit、一起 rollback。新增的 use case 只要 publish 就會被記，不用記得呼叫 record。

不發 event 的路徑不寫 history：冪等重放、`ErrIdempotencyConflict`（在 Publish 之前就 return）、被 domain 拒絕或 no-op 的 payment event。

## 每一列的欄位

| 欄位 | 來源 |
|---|---|
| `order_id`, `seq` | `seq` 是每張訂單 1..N，由 store 寫入時指派 |
| `event` | CREATED／CONFIRMED／PAID／COMPLETED／CANCELLED，由 `to_status` 決定（PENDING = CREATED） |
| `from_status`, `to_status` | domain event 的 `From()`／`To()`；CREATED 的 `from_status` 是 NULL |
| `occurred_at` | `e.OccurredAt()`（domain clock），不是 DB 的 `now()` |
| `actor_kind`, `actor_subject` | 見下 |
| `cause` | domain event 的 `Cause()`，固定列舉 |

### seq

`INSERT … SELECT COALESCE(MAX(seq),0)+1 … WHERE order_id=$1`，**在該訂單 `Save` 之後**、同一個 transaction 讀。
`Save` 的 UPDATE 會拿 `orders` 的 row lock 直到 commit，同一張單的兩個 writer 因此被序列化，後到的在 version 比對時就 stale、
重新載入後才會再寫。`seq` 不等於 `version`：`ApplyPaymentCaptured` 在一次 `Save` 裡產生 PAID 和 COMPLETED 兩列。
primary key `(order_id, seq)` 是最後防線：沒先 `Save` 的 writer 撞 key 會得到 infra error（不吞、不轉成 domain error、不重試）。

### actor

由 `Recorder` 依 transition 的 cause 與 `contextx.Principal` 決定：

- cause 是 `RPC_REQUEST`（rpc 的 Create／Cancel／Complete）：有 principal 時依 `auth.admin_role` 分 `ADMIN`／`CUSTOMER`，
  `actor_subject` 是 `Principal.Subject`（account id）；沒有 principal（`auth.mode=off`）是 `ANONYMOUS`。
- 其他 cause（consumer 路徑）：一律 `SYSTEM`，`actor_subject` 空。

`actor_subject` 只給 admin 看：遮蔽在 `GetOrderHistory` 的 use case 做（見〈讀取〉）。

### cause（固定列舉）

`RPC_REQUEST`、`PAYMENT_CAPTURED`、`PAYMENT_EXPIRED`、`STOCK_RESERVED`、`CANCELLATION_REQUESTED`、`STOCK_SHORTAGE`、`RESERVATION_EXPIRED`、`UNSPECIFIED`。
cause 跟著 domain event 走：domain 自己知道的在 domain 決定（Confirm = `STOCK_RESERVED`，`ApplyPayment*` 各自的 cause，
New／Complete = `RPC_REQUEST`）；Cancel 的來源 domain 不知道，由 adapter 在 `dto.CancelOrderInput.Source` 明說
（rpc = `RPC_REQUEST`、consumer = `CANCELLATION_REQUESTED`），沒說（零值）就被 `order.ErrInvalidCause` 拒絕，不會悄悄變成別的值。

`OrderCancellationRequested.reason` 是 inventory 自己組的字串（含 SKU 與時長），不可信也不穩定：**不寫進 history，也不寫進 log**。

### OrderCancellationRequested.cause

切片 2 在該 event 加了 typed 的 `cause`（`CancellationCause`，additive，`reason` 保留）。inventory 的 publisher 依來源填：庫存不足 → `STOCK_SHORTAGE`、預約逾時 → `RESERVATION_EXPIRED`。ordering 的 consumer 把它對應成 history 的同名 cause；**沒有 `cause` 的舊事件**（欄位出現前發布、仍在 outbox 或 poison 表裡的）和**不認得的值**（更新的 publisher 送來的）都對應成 `CANCELLATION_REQUESTED`，不報錯、不猜。（兩種都有從 consumer 進去的測試，一個用不含第 4 號欄位的真 bytes。）

升級順序：先 migrate（`0011` 放寬 CHECK）再升 worker；詳見 `docs/operations.md`（`0011` 的鎖、順序與 rollback）。

## 讀取：GetOrderHistory

`ordering.v1.OrderService.GetOrderHistory(id)` 回傳該訂單依 `seq` 排序的全部轉換，**不分頁**。上界是 4 列（PENDING→CONFIRMED→PAID→COMPLETED 的 CREATED 加三次轉換）：`domain/order` 的測試從 `CanTransitionTo` 算出最長路徑，再和 `service.proto` 註解裡寫的數字比對，狀態機若允許更長的路徑，測試就轉紅。

| 情況 | 回應 |
|---|---|
| 訂單不存在 | `NOT_FOUND`（`order.not_found`） |
| 非 admin 的 principal 讀別人的訂單 | `NOT_FOUND`，和不存在同一個答案。擁有者條件在選訂單時就放進查詢（`FindByIDForCustomer`，同 `GetOrder`），history 表根本不會被讀 |
| 訂單存在但沒有 history（migration 之前建立的） | 空清單，不是錯誤 |
| `auth.mode=required` 且沒帶 credential | `UNAUTHENTICATED`（`auth.unauthenticated`），在 interceptor 就擋下 |
| 儲存的 `cause` 這個版本不認得 | 該列的 `cause` 是 `UNSPECIFIED`，其他欄位照常；不會讓整個 RPC 失敗 |

`actor_subject` **只回給 admin**（principal 持有 `auth.admin_role`）。其他人拿到的這一欄一律是空字串，`actor_kind` 照給：擁有者讀自己的單、`auth.mode=optional` 下沒帶 credential 的呼叫、以及 `auth.mode=off`（預設）下的所有呼叫（此時沒有 interceptor，連帶了 admin key 的請求也是 anonymous）。遮蔽在 use case 做，DTO 根本不帶非 admin 看得到的 subject，adapter 不需要知道誰是 admin。

`apps/rpc` 的 `TestOrderHistory_AuthMatrix` 在真 Postgres 上逐格測：`auth.mode`（off＝`config.Load()` 的預設、optional、required）×（admin、擁有者、別的 customer、帶 `admin` role 但不是 `auth.admin_role` 的 caller、anonymous）×（自己的單、別人的單、沒有 history 的單、不存在的單）；資料由真的 `CreateOrder`／`CancelOrder` 以真 key 寫入，所以 table 裡的 `actor_subject` 不是空的（測試先查表確認），每格「看不到」都和 admin 看同一批列的結果對照。

## 既有訂單不 backfill

migration 之前建立的訂單沒有 history，或只有 migration 之後的轉換（缺前段）。`HistoryStore.List` 對沒有任何列的訂單回空清單、不是錯誤。

## migration

只有 `CREATE TABLE`，**不加指向 `ordering.orders` 的 FK**：加 FK 要在 `orders` 上拿鎖，而整份 migration 在同一個 transaction 裡，
鎖會持有到 commit（#318）。要加另開一題，先量測鎖的影響。

## Console：History 卡

`/orders/$id` 在 Reservation 卡下方有一張 History 卡（`web/apps/console/src/components/history-card.tsx`），依 `seq` 列出時間軸：事件（CREATED／CONFIRMED／PAID／COMPLETED／CANCELLED）、`from → to`、時間、誰（actor_kind，有 subject 時一併顯示）、為什麼（cause 的固定標籤）。

- **cause 與 actor 的標籤是固定的句子**，不顯示伺服器給的文字；`UNSPECIFIED` 與這個版本不認得的值（較新的伺服器）都顯示 `Unknown cause`／`Unknown`，不是空白也不是數字。
- **誰看得到 subject 由伺服器決定**（只回給 admin，見〈讀取〉）；卡片只在回應帶有 subject 時才顯示它。卡片負責的是：答案不能比取得它的 credential 活得久。query 以 credential generation 為 key（`queries.ts` 的 `historyQuery`），頁面再以 generation 當卡片的 key，所以切換 credential 或登出之後，前一個身分的列、錯誤與 subject 不在新身分讀的那一格裡；舊身分的慢回覆寫進它自己的 key。
- **空清單**（migration 之前建立的訂單）顯示明確的空狀態，不當成錯誤；`NOT_FOUND`（訂單不存在或不是你的）與其他失敗分開顯示。訂單本身讀不到時（`NOT_FOUND`、未登入），頁面只顯示訂單的錯誤，沒有 History 卡。
- **輪詢跟著訂單**：間隔沿用訂單自己的輪詢（PENDING 與要付款的 CONFIRMED 快、其餘慢），訂單狀態一變就再抓一次（終態訂單的最後一列這樣進來），終態之後不再輪詢；Cancel／Complete 成功後也會讓它失效重抓。沒有另外的輪詢節奏。
- 測試：`e2e/history.spec.ts`（真實流程、空狀態、錯誤分流、未知 cause、輪詢）與 `e2e/auth/history.spec.ts`（真 server：admin 看得到 subject、擁有者看不到、別人 `NOT_FOUND`、切換與登出後 subject 消失、舊 admin 的慢回覆與「新身分的答案還沒到時」都不會顯示 admin 的列；拿掉 generation 這兩條會轉紅）。

