# Payment

<sub>← [README](../README.md)</sub>

> **狀態：設計稿，切片 1、2 已實作（#367）。** 切片 1（Ordering 契約）：`PAID`／`MarkPaid`／`OrderPaid`、`OrderConfirmed.total`、訂單 `payment_required` 快照與 `ordering.require_payment`（預設 `false`）、`CompleteOrder` 依快照擋（`order.payment_required`）、Console 顯示。切片 2（Payment 核心）：`pkgs/payment`（付款單 aggregate 與 §3 全表、`StartPayment`／`GetPayment`、Postgres／memory repository、`payment.payments` 表、`PaymentProvider` port 與 mock provider）、`apps/payment-rpc serve`。**尚未實作**：`consume`、webhook route、`expire`、退款流程（切片 3）、`apps/rpc` 轉發與 Console 付款頁（切片 4）、image／chart（切片 5）、切換（切片 6）；所以目前沒有任何路徑會走到 `PAID`。
> 決策來源：#367 七個決定，加上 Sean 對 PR #369 暴露的未決項的後續裁示（[#367 comment](https://github.com/blackhorseya/x-monorepo/issues/367#issuecomment-6565500716)，decision `d-20261101232833097592-18`）。兩者有衝突時以後者為準；§13 列出哪一條取代了哪一條。
> 「保證」只列已指定到切片測試的條目；測試名稱是預定名稱，實作時以實際為準。寫不出測試的就不寫成保證。
> 改動任何已定決定都要回 Sean。標 **〔實作細節〕** 的是本文為了讓已定規則成立而補上的內部做法，實作時可調整，但不得改變它所服務的規則。

`pkgs/payment` 是第五個 bounded context：補上訂單從 `CONFIRMED` 到完成之間的**收款**。
流程是 建單 → 預留 → `CONFIRMED` → 付款 → `PAID` → `COMPLETED`；`CONFIRMED` 的語意（庫存已保留）不變。
付款細節只存在 Payment，訂單**不加** `payment_status` 欄位（訂單只多一個建立時快照的 `payment_required`，見 §2）。卡片資料完全不進系統（hosted checkout）。

## 1. 形狀

```
顧客 ─► apps/rpc ──(service key + customer_id)──► apps/payment-rpc serve ──► provider（第一版 mock）
                                                      ▲   │ 自己的 payment schema
provider ──webhook（簽章）─────────────────────────────┘   │ outbox
                                                          ▼
ordering ──OrderConfirmed / OrderCancelled / OrderCompleted──► payment-rpc consume ──PaymentCaptured / PaymentExpired / PaymentRefunded──► ordering
payment-rpc expire（CronJob）──► provider 查狀態 ──► PaymentExpired / PaymentCaptured
```

- `apps/payment-rpc` 是新 binary，三個子命令：`serve`（RPC 與 webhook）、`consume`（訂閱 `OrderConfirmed`、`OrderCancelled`、`OrderCompleted`，處理 webhook 事件）、`expire`（CronJob）。自己的 `payment` schema，provider 密鑰只有它持有。
- `apps/worker` **不載入** Payment。`apps/rpc` 只依賴 payment 的 `contract/` 與 `adapters/client`，不載入領域程式（同 catalog 的 #320 作法；用 archtest 固定）。
- `payment-rpc` 掛掉時，只有付款操作回 503，下單與預留不受影響。
- **剩下的耦合**：Postgres broker 下，Payment 要讀 ordering 的 outbox 表（與 ordering 讀 inventory 的 outbox 同一種耦合）。
- **對外入口**：ingress 只對外開 webhook 路徑；`serve` 的 Connect RPC 只給 `apps/rpc` 內部呼叫（切片 5 的 chart 與 helm-check 負例固定，見 §9）。
- `customer_id` 由 `apps/rpc` 驗完身分後以 service key 傳入 `payment-rpc`（request 的明確欄位，不轉傳使用者 credential）。持有該 service key 的人可冒充任何 customer，所以 service key 只能是 rpc 那一把（單一 key，`--require-durable` 檢查），而且 RPC 路徑不對外（上一條）。

### 與 `CLAUDE.md` 規則的關係

| 規則（`CLAUDE.md`） | 本設計 | 性質 |
|---|---|---|
| 「新增 Bounded Context」清單：`apps/worker` 的 `NewWorker` 收該 BC 的 `Subscriptions` | Payment 的 `Subscriptions` 掛在 `payment-rpc consume` 自己的 router，**不進 `apps/worker`** | **例外**，只限 payment；不改規則。原因：provider 密鑰只有 payment 持有，worker 載入 payment 會讓 worker 也持有 provider 憑證 |
| 同清單：`poison.go` 的 `schemas` 加該 BC | **照做**，但只加表名：`apps/worker/poison.go` 的 `schemas`、`apps/rpc/app.go` 的 `NewPoisonService` 清單各加 `payment`。清單目前引用 `ordering.Schema`／`inventory.Schema`（BC 套件的常數）；payment 不能 import `pkgs/payment`（會把領域程式帶進 worker／rpc），所以用不載入領域程式的來源（platform 的字串常數，或 contract 套件） | 規則照走，差別只在表名的來源；見 §9 切片 3 |
| `apps/rpc` 不載入 catalog（#320） | 同樣不載入 payment，經 `contract/` 的 Connect client 問 `payment-rpc` | 沿用既有規則 |
| 「decorator 只在 `provider.go` 加」「usecase 不自己開 transaction」 | `StartPayment` 要呼叫 provider，不在 DB transaction 內：走 `decorateNoTx()`，比照 `CreateOrder`（持有 `port.Transactor`，只包住寫入） | **第二個例外**，與 #320 同類（寫入前先打外部服務）。範圍：`StartPayment`、`expire` 的 provider 查詢、退款呼叫 |
| platform `server.New` 的 `routes` 只收 Connect `Route` | webhook 是普通 HTTP route，要在 platform 開一個掛 gin route 的 option（切片 3） | **platform 變更**，不是規則例外；auth interceptor 只包 Connect，gin route 不經它 |
| 「Auth 只做 authentication」 | 不變。擁有權比對（`customer_id`）在 Payment 的 application 層，不在 interceptor | 沿用 |

## 2. 訂單端的變更（切片 1）

- 新狀態 `PAID`：`CONFIRMED → PAID → COMPLETED`。`PAID` = 收到款；`COMPLETED` = 交易結束、庫存售出，語意不變。
- **第一版 `PAID` 由付款成功自動走到 `COMPLETED`**：ordering 處理 `PaymentCaptured` 時，在同一個 transaction 內先 `MarkPaid` 再 `Complete`，發 `OrderPaid` 與 `OrderCompleted`；inventory 沿用 `OrderCompleted` 去 commit 預留 〔實作細節〕。所以第一版外部觀測不到停留在 `PAID` 的訂單。
- `PAID` 之後不能走 `CancelOrder`：沿用現有的 `order.cannot_cancel`（`pkgs/ordering/domain/order/errors.go`），做法是 `CanTransitionTo` 不放行 `PAID → CANCELLED`。訂單一旦 `CANCELLED` 就不復活。
- **`payment_required` 快照**（Sean 後續裁示）：訂單進 `CONFIRMED` 的那一刻，把當下的 `ordering.require_payment` 存成訂單上的 `payment_required`，之後不再改。**ordering 對這張單怎麼處理付款事件、`CompleteOrder` 要不要擋，一律只看這個快照，不看目前的開關，也不看有沒有付款單。**所以開關從開切回關也安全（§8）。
  - 切片 1 的 migration 讓既有訂單的 `payment_required` 為 `false`（它們在 payment 存在之前就 `CONFIRMED` 了）。
  - 訂單 entity 與 Console 顯示這個欄位（讓 `StartPayment` 的檢查與營運查得到）。
- `OrderConfirmed` 新增 `total`（`common.v1.Money`）。**舊事件沒有 `total` 時視為明確錯誤，不能當成 0 元收款**：payment 的 consumer 回 domain error（drop，不 retry）並告警，不建付款單。
- **已知限制**：inventory 目前在 `OrderCompleted` 才 commit 預留。第一版 `PAID` 立即走到 `COMPLETED`，沒有問題。之後 Fulfillment 讓 `PAID` 停留時，要改成 `OrderPaid` 時 commit，否則已付款訂單的預留可能過期（24 小時 TTL 的 `expire` 會把它還掉）。
- Console 顯示 `PAID` 與 `payment_required`（切片 1）。

## 3. 付款單（`payment.Payment`）

| 欄位 | 說明 |
|---|---|
| `id` | 付款單 ID（UUID v7，#365）；**退款呼叫的冪等 key** |
| `order_id` | 一張訂單最多一張付款單（`unique(order_id)`） |
| `customer_id` | 取自 `OrderConfirmed`，擁有權比對用 |
| `amount`（`kernel.Money`）| 取自 `OrderConfirmed.total` |
| `status` | 見下 |
| `expires_at` | 建單當下 + 窗口，預設 30 分鐘，由 Payment 管理 |
| `provider` / `provider_ref` / `checkout_url` / `checkout_expires_at` | `StartPayment` 才有值 |
| `order_outcome` 〔實作細節〕 | `NONE`／`CANCELLED`／`COMPLETED_UNPAID`：Payment 已知訂單結局。`CANCELLED` = 收過 `OrderCancelled`；`COMPLETED_UNPAID` = 在付款單未收款時收過 `OrderCompleted`（訂單不靠這張付款單完成）。晚到的收款靠它判斷要不要退款 |
| `terminal_at` 〔實作細節〕 | 進入 `EXPIRED`／`CANCELLED` 的時間；`expire` 補查窗口（`payment.reconcile_window`）從它起算 |
| `version` | optimistic lock，同其他 BC |

### 狀態機

| 狀態 | 意義 | 終態？ |
|---|---|---|
| `PENDING` | 已建、計時中；未收款 | 否 |
| `CAPTURED` | provider 確認收款，金額與幣別相符 | 否（之後可能 `REFUNDING`） |
| `EXPIRED` | 窗口到期且 provider 查無收款 | 否（晚到的收款仍可進 `CAPTURED`） |
| `CANCELLED` | 訂單取消或已不靠這張付款單完成，Payment 取消 provider 端付款（Sean 已接受） | 否（晚到的收款仍可進 `CAPTURED`） |
| `REFUNDING` | 要全額退款；provider 退款呼叫進行中或待重試 | 否 |
| `REFUNDED` | 退款完成 | 是 |
| `NEEDS_REVIEW` | 收到款但金額／幣別與付款單不符；不進 `CAPTURED`；**第一版只告警，到 provider 後台處理**（Sean 已接受）。系統（含 `expire`）不再推進它，訂單由預留過期取消 | 是 |

沒有 `FAILED`：hosted checkout 讓顧客在窗口內自行重試，provider 的單次失敗事件不改付款單狀態，窗口到期才 `EXPIRED`。

### 轉移與觸發來源

| # | 轉移 | 觸發來源 | 條件／動作 | 發布事件 |
|---|---|---|---|---|
| T1 | ∅ → `PENDING` | `consume`：`OrderConfirmed` | 驗 `total` 存在；`unique(order_id)` 重送忽略；不打 provider。若 `order_outcomes` 已有該訂單的紀錄（事件亂序），改走 T14 | — |
| T2 | `PENDING` →（不變，補 `provider_ref`、`checkout_url`）| `serve`：`StartPayment` | 顧客本人；`now < expires_at`；向 provider 開 hosted checkout。**重複呼叫回傳尚未過期的同一個 URL**；`checkout_expires_at` 已過則重開 | — |
| T3 | `PENDING` → `CAPTURED` | `consume`：webhook `payment.captured` | 金額、幣別與付款單相符 | `PaymentCaptured` |
| T4 | `PENDING` → `NEEDS_REVIEW` | 同 T3，或 `expire` 查 provider（T6'） | 已收款但金額或幣別不符 | — （告警） |
| T5 | `PENDING` → `EXPIRED` | `expire` | `now ≥ expires_at`，且**先向 provider 查一次狀態**確認沒收款 | `PaymentExpired` |
| T6 | `PENDING` → `CAPTURED` | `expire` | 同 T5 但 provider 查到已收款（webhook 沒送到），金額相符 | `PaymentCaptured` |
| T6' | `PENDING` → `NEEDS_REVIEW` | `expire` | provider 查到已收款但金額／幣別不符 | — （告警） |
| T7 | `PENDING` → `CANCELLED` | `consume`：`OrderCancelled` | 取消 provider 端 checkout（best-effort，失敗不阻擋）；`order_outcome = CANCELLED` | — |
| T7' | `PENDING` → `CANCELLED` | `consume`：`OrderCompleted` | 訂單不靠這張付款單完成（`payment_required = false` 的訂單被 `CompleteOrder`）；取消 provider 端 checkout；`order_outcome = COMPLETED_UNPAID`（Sean 後續裁示 #3） | — |
| T8 | `EXPIRED` / `CANCELLED` → `CAPTURED` | webhook（逾時或取消後 provider 才通知成功），**或 `expire` 補查**（webhook 缺席；T7 取消 checkout 可能失敗） | 錢已動：必須記錄；金額相符才進；之後是否接 T10 由 T10 的條件決定 | `PaymentCaptured` |
| T9 | `EXPIRED` / `CANCELLED` → `NEEDS_REVIEW` | webhook 或 `expire` 補查 | 同 T8 但金額／幣別不符 | — （告警） |
| T10 | `CAPTURED` → `REFUNDING` | `consume`：`OrderCancelled`；或 T3／T6／T8 進入 `CAPTURED` 時 `order_outcome ≠ NONE` | 兩個順序（先收款後取消／先取消後收款）收斂到同一個條件：`CAPTURED ∧ order_outcome ≠ NONE`。正常付款路徑的 `OrderCompleted`（收款後 `PAID→COMPLETED`）到達時付款單已是 `CAPTURED`，不改 `order_outcome`，所以不會誤退 | `PaymentRefundRequested`（內部） |
| T11 | `REFUNDING` → `REFUNDED` | `consume`：處理 `PaymentRefundRequested` | 呼叫 provider 退款（冪等 key = 付款單 `id`）成功。失敗有界重試，超過次數進 poison 並告警，狀態維持 `REFUNDING` | `PaymentRefunded`（目前沒有 consumer） |
| T12 | `EXPIRED` / `NEEDS_REVIEW` → 狀態不變 | `consume`：`OrderCancelled`／`OrderCompleted` | 只記 `order_outcome`（`OrderCompleted` 只在此時付款單未收款才記，值為 `COMPLETED_UNPAID`）。`EXPIRED` 之後若 T8 收款，會因 `order_outcome ≠ NONE` 接 T10 | — |
| T13 | `CANCELLED` → 狀態不變 | `consume`：`OrderCancelled`／`OrderCompleted` | `order_outcome` 已在進 `CANCELLED` 時設定，不覆寫 | — |
| T14 | ∅ → `CANCELLED` | `consume`：`OrderConfirmed`，且 `order_outcomes` 已有紀錄 | 事件亂序（`OrderCancelled`／`OrderCompleted` 先到）：直接建成 `CANCELLED`，`order_outcome` 取自紀錄，不計時 | — |
| T15 | ∅ → ∅ | `consume`：`OrderCancelled`／`OrderCompleted`，付款單尚未存在 | 寫 `payment.order_outcomes(order_id, outcome)`（〔實作細節〕墓碑表）；沒有它，亂序會留下一張永遠 `PENDING` 且可被付款的單 | — |

### 狀態 × 事件全表

每個狀態對每個事件都有結果。「忽略」＝ 記錄後不動付款單、不報錯；「拒絕」＝ 回明確錯誤（`serve`）或 domain error drop（`consume`）；「—」＝ 該狀態不可能收到（但不是靠「其餘拒絕」兜底：實作要有對應測試，見 §9）。

`expire` 對 `EXPIRED`／`CANCELLED` 的補查：這兩個狀態可能已有錢在 provider（T7 取消 checkout 是 best-effort 會失敗；webhook 可能缺席），所以 `expire` 對有 `provider_ref` 的這類付款單也向 provider 查狀態。**補查有界**：只在付款單進入 `EXPIRED`／`CANCELLED` 之後的固定窗口內查（`payment.reconcile_window`，預設 24 小時，必須大於 provider checkout 的有效期，啟動時檢查），窗口後不再查。需要記錄進入終態的時間（`terminal_at`，〔實作細節〕）。沒有 `provider_ref`（從未 `StartPayment`）的付款單不可能收款，不查。窗口後才收款的情況見「已知限制」。

事件欄：**A** `OrderConfirmed`（有 `total`）；**A'** `OrderConfirmed`（無 `total`）；**B** `OrderCancelled`；**C** `OrderCompleted`；**D** webhook `payment.captured` 且相符；**D'** 同上但金額／幣別不符；**E** webhook 其他類型或未知類型；**F** `StartPayment`；**G** `expire` 到期（`PENDING`）或補查（`EXPIRED`／`CANCELLED`）且 provider 未收款；**G'** `expire` 查到 provider 已收款相符；**G''** `expire` 查到已收款不符；**H** 退款處理（`PaymentRefundRequested`）。`expire` 未到期一律無動作，不列。

| 狀態＼事件 | A | A' | B | C | D | D' | E | F | G | G' | G'' | H |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| ∅（無付款單） | T1（有紀錄則 T14） | 拒絕 + 告警，不建 | T15 | T15 | 落地為孤兒 webhook + 告警，無付款單可動 | 同 D | 落地、忽略 | `NOT_FOUND` | — | — | — | — |
| `PENDING` | 忽略（`unique(order_id)`） | 拒絕 + 告警 | T7 | T7' | T3 | T4 | 落地、忽略 | T2（`now ≥ expires_at` 則拒絕 `payment.expired`） | T5 | T6 | T6' | — |
| `CAPTURED` | 忽略 | 拒絕 + 告警 | T10 | 忽略（正常付款路徑，見 T10） | 忽略（重複收款通知） | 忽略 + 告警 | 落地、忽略 | 拒絕 `payment.not_pending` | 無動作 | 無動作 | 無動作 | — |
| `EXPIRED` | 忽略 | 拒絕 + 告警 | T12 | T12 | T8 | T9 | 落地、忽略 | 拒絕 `payment.not_pending` | 補查未收款：無動作（窗口外：不再查） | T8（窗口內） | T9（窗口內） | — |
| `CANCELLED` | 忽略 | 拒絕 + 告警 | T13 | T13 | T8（隨後 T10） | T9 | 落地、忽略 | 拒絕 `payment.not_pending` | 補查未收款：無動作（窗口外：不再查） | T8（窗口內） | T9（窗口內） | — |
| `REFUNDING` | 忽略 | 拒絕 + 告警 | 忽略（`order_outcome` 已設） | 忽略 | 忽略 | 忽略 + 告警 | 落地、忽略 | 拒絕 `payment.not_pending` | 無動作 | 無動作 | 無動作 | T11（失敗：有界重試→poison，狀態不變） |
| `REFUNDED` | 忽略 | 拒絕 + 告警 | 忽略 | 忽略 | 忽略 | 忽略 + 告警 | 落地、忽略 | 拒絕 `payment.not_pending` | 無動作 | 無動作 | 無動作 | 無動作（冪等 key，不重退） |
| `NEEDS_REVIEW` | 忽略 | 拒絕 + 告警 | T12 | T12 | 忽略 + 告警 | 忽略 + 告警 | 落地、忽略 | 拒絕 `payment.not_pending` | 無動作（`expire` 不動這個狀態） | 無動作 | 無動作 | — |

### 失敗來源 × 狀態交叉

全表是「事件 × 狀態」；下表補「失敗來源 × 狀態」，確認每種失敗在每個狀態都有出口，錢不會停在沒人看的地方。

| 失敗來源＼狀態 | `PENDING` | `EXPIRED` | `CANCELLED` | `CAPTURED` | `REFUNDING` | `REFUNDED` | `NEEDS_REVIEW` |
|---|---|---|---|---|---|---|---|
| **webhook 缺席**（錢已在 provider，沒有通知） | 到期後 `expire` 補查：T6／T6'（到期前不查，付款單維持 `PENDING`，顧客用 `GetPayment` 看不到結果直到補查） | 窗口內補查 T8／T9；**窗口後不再查（已知限制）** | 同 `EXPIRED` | 不適用（已收款） | 不適用：退款結果看同步回應，不靠 webhook | 不適用 | 不適用：系統不再動它 |
| **webhook 重送／亂序** | `unique(provider, provider_event_id)` 去重；舊事件忽略（全表 D／D'／E 欄） | 同左 | 同左 | 同左 | 同左 | 同左 | 同左 |
| **`expire` 查 provider 失敗**（錯誤、逾時） | **不轉移**：不得因為查不到就 `EXPIRED`（T5 要求「確認沒收款」）；下個 tick 重試；連續失敗超過上限告警。持續失敗時付款單維持 `PENDING`，訂單由預留 TTL（§2 已知限制所述的既有 sweep）取消 → `OrderCancelled` → T7，之後以 T7 進入的 `CANCELLED` 為起點補查 | 不轉移；窗口內下個 tick 重試；窗口結束時仍失敗則告警「補查未完成」（已知限制） | 同 `EXPIRED` | 不查 | 不查 | 不查 | 不查 |
| **取消 checkout 失敗**（T7／T7'，best-effort） | 照走 `CANCELLED`，不阻擋 | 到期時不呼叫取消（provider 端 session 自然失效） | 失敗的後果由補查接住：T8／T9 | 不適用 | 不適用 | 不適用 | 不適用 |
| **開 checkout 失敗**（`StartPayment` 的 `CreateCheckout`） | 付款單不變，回 `payment.provider_unavailable`，可重試 | 在呼叫 provider 之前就拒絕 `payment.not_pending` | 同 `EXPIRED` | 同 `EXPIRED` | 同 `EXPIRED` | 同 `EXPIRED` | 同 `EXPIRED` |
| **退款失敗**（T11） | 不適用 | 不適用 | 不適用 | 不適用（T10 前不退） | 依 T11（有界重試、poison、告警），狀態維持 `REFUNDING` | 不適用；重送被冪等 key 擋下 | 不適用 |

### 已知限制（不寫成保證）

- **補查窗口之後才收款、而且 webhook 缺席**：付款單停在 `EXPIRED`／`CANCELLED`，錢在 provider，系統不會發現。webhook 若送到仍走 T8（與窗口無關）。這種情況到 provider 後台對帳處理。窗口長度與下限見上方「補查有界」。
- `payment-rpc` 的補查依賴 provider 的查詢 API 可用；查詢持續失敗時只有告警，沒有自動出口。

### 錯誤碼（Connect code 的唯一權威表）

新增的碼進 `docs/error-codes.md`；碼名在實作切片定案時以實際為準，語意與 Connect code 以本表為準，其他章節只引用碼名：

| code | Connect code | 語意 |
|---|---|---|
| `payment.disabled` | `FAILED_PRECONDITION`（Sean 已定） | 開關關閉或訂單快照 `payment_required = false` |
| `payment.expired` | `FAILED_PRECONDITION` | `PENDING` 已過 `expires_at`、`expire` 尚未處理 |
| `payment.not_pending` | `FAILED_PRECONDITION` | 付款單已不是 `PENDING` |
| `payment.provider_unavailable` | `UNAVAILABLE` | provider 暫時故障（可重試的服務失敗，不是業務前提錯誤）；固定訊息，provider 原始錯誤不外洩 |
| `payment.permission_denied` | `PERMISSION_DENIED` | admin 不可代付：`StartPayment` 的 `admin=true` 且 `customer_id` 不是付款單的 customer（§6 擁有權）。第一版不開這扇門 |
| `order.payment_required`（ordering） | `FAILED_PRECONDITION` | 快照 `payment_required = true` 的 `CONFIRMED` 訂單被 `CompleteOrder`（§8） |

## 4. 事件清單

Topic 名稱 = proto 全名（同現有慣例）。下表「欄位」只列要點，完整定義在切片 1／2 的 proto。

| 事件 | 發布者 | 訂閱者 | 欄位 | 說明 |
|---|---|---|---|---|
| `ordering.v1.OrderConfirmed`（改） | ordering | **payment**（新） | `order_id`、`customer_id`、`confirmed_at`、**`total`（新）** | T1 |
| `ordering.v1.OrderPaid`（新） | ordering | 目前沒有 | `order_id`、`customer_id`、`paid_at` | 為 Fulfillment 預留（見 §2 已知限制） |
| `ordering.v1.OrderCancelled`（現有） | ordering | inventory、**payment**（新） | `order_id`、`customer_id`、`cancelled_at` | T7、T10、T12、T13、T15 |
| `ordering.v1.OrderCompleted`（現有） | ordering | inventory、**payment**（新，Sean 後續裁示 #3） | 不變 | T7'、T12、T13、T15；正常付款路徑到達時為忽略 |
| `payment.v1.PaymentCaptured` | payment | ordering | `payment_id`、`order_id`、`customer_id`、`amount`、`captured_at` | ordering 依訂單快照與狀態決定 `PAID`／忽略／拒絕（§8） |
| `payment.v1.PaymentExpired` | payment | ordering | `payment_id`、`order_id`、`customer_id`、`expired_at` | ordering 依快照決定取消訂單或忽略；取消後 inventory 走現有 `OrderCancelled` 還庫存 |
| `payment.v1.PaymentRefunded` | payment | 無 | `payment_id`、`order_id`、`amount`、`refunded_at` | 保留給之後 |
| `payment.v1.PaymentRefundRequested`（內部）〔實作細節〕 | payment | payment（`consume`） | `payment_id` | 讓退款呼叫在 DB transaction 之外、走 eventbus 的 retry／poison；不對外承諾 |

`ordering.v1.OrderCancellationRequested` 的註解寫著「例如 payment timeout」，但本設計**不用它**：逾時一律走 `PaymentExpired`，由 ordering 的狀態機判定（切片 3 順手改註解）。

### 亂序與重送

| 情境 | 處理 |
|---|---|
| 同一事件重送 | consumer inbox（`payment.inbox` / `ordering.inbox`，message id）+ `unique(order_id)` + webhook 的 `unique(provider, provider_event_id)` |
| `OrderCancelled`／`OrderCompleted` 比 `OrderConfirmed` 先到（不同 topic 無順序保證） | T15 先寫 `order_outcomes`，之後 `OrderConfirmed` 走 T14 |
| webhook 收款比 `OrderCancelled` 先到 | T3 → T10 |
| webhook 收款比 `OrderCancelled` 晚到 | T7 → T8 → T10 |
| 逾時 → ordering 取消訂單 → 晚收款 | T5 → T12 → T8 → T10 |
| `PaymentCaptured` 與 `PaymentExpired` 同時待處理 | 由 ordering 狀態機判定（§8 ordering 表）：先處理者贏，輸的被拒（domain error，drop）；Payment 側是否退款由 T10 的條件決定 |
| 舊事件晚到、版本落後 | 付款單 optimistic lock；`ErrStale` 讓 consumer retry，domain error drop |
| webhook 簽章錯 | 回 4xx、不落地、不重試；log 只寫固定分類 |

## 5. 競態（`payment_required = true` 的訂單）

每個情境的結果由 T-row 與 §8 ordering 表決定，這裡只列情境與引用。`payment_required = false` 的訂單沒有競態可言（§8）。

| 情境 | 引用 | 預定測試（切片） |
|---|---|---|
| 收款先到，訂單 `CONFIRMED` | §8 ordering 表（`true`／`CONFIRMED`，`PaymentCaptured`）；T3 | S3；S6 kind |
| 訂單已取消，收款才到 | §8（`true`／`CANCELLED`，`PaymentCaptured`）；T10 | S3；S6 kind |
| 顧客在付款途中取消訂單 | T7、T10 | S3；S6 kind |
| 逾時後 provider 才通知成功 | T5、T8；§8（`PaymentCaptured`／`PaymentExpired` 兩列）；T10 條件 | S3；S6 kind |
| 逾時 → 訂單取消 → 晚收款 | T5 → T12 → T8 → T10 | S3 consumer 測試（完整順序）；S6 kind |

## 6. 發起方式與 RPC

- **建單**：`consume` 收到 `OrderConfirmed` 建 `PENDING` 付款單並開始計時（T1），**此時不打 provider**。這對每張 `CONFIRMED` 訂單都發生，不看快照——ordering 才是看快照的一方（§8）。
- **`StartPayment(order_id)`**：顧客呼叫才向 provider 開 hosted checkout，回傳導向 URL；重複呼叫回傳尚未過期的同一個 URL（T2）。provider 呼叫不在 DB transaction 內（`decorateNoTx`）。
  - **開關關閉時回 `payment.disabled`**（Sean 後續裁示 #7；Connect code 見 §3 錯誤碼表）。檢查在 `apps/rpc`（它持有 `ordering.require_payment` 設定），在轉發給 `payment-rpc` 之前。
  - 〔實作細節〕同一個 code 也用在「訂單的 `payment_required = false`」：開關後來才打開、但這張單建立時是 `false`，ordering 會忽略它的付款事件，讓顧客付款只會收錢不 `PAID`，所以一樣擋掉。rpc 載入 ordering，可直接讀訂單快照。
- **`GetPayment`**：client 輪詢進度。
- 擁有權：付款單的 `customer_id` 來自事件；非本人（非 admin）一律 `NOT_FOUND`，同 `GetOrder` 的形狀。admin 可讀，不可 `StartPayment` 代付（第一版不開）。
  - **「本人」「admin」由誰說了算**（切片 2 定案）：`payment-rpc` 的呼叫者永遠只有持有 service key `rpc` 的 `apps/rpc`，所以 request 帶兩個欄位：`customer_id`（rpc 驗完身分後填的使用者）與 `admin`（該使用者是否持有 admin role）。`payment-rpc` 相信它們，唯一的前提是請求通過了 auth interceptor（沒有有效的 service key 進不來）；這也是 service key 只能有一把的另一個理由（§1）。`customer_id` 空白是 `INVALID_ARGUMENT`，**不是**「任何人」。
  - `GetPayment`：`customer_id` 等於付款單的 customer，或 `admin=true`，才讀得到；否則 `NOT_FOUND`（和「這張訂單沒有付款單」同一個答案）。
  - `StartPayment`：`customer_id` 等於付款單的 customer 才可以；不是的話，`admin=true` 回 `payment.permission_denied`（代付，admin 付自己的單不受影響），`admin=false` 回 `NOT_FOUND`。

## 7. Provider、webhook 與認證邊界

### Provider

第一版只有 mock，走 application 的 `PaymentProvider` port（`CreateCheckout`、`GetPayment`、`CancelCheckout`、`Refund`、`VerifyWebhook`）。mock 要能模擬：hosted checkout、webhook 簽章、延遲、重送與亂序（測試與 kind 用）。

### Webhook

1. 普通 HTTP route，不走 Connect，也不走 API key；憑證是**簽章**，用**原始 body** 驗簽（不先 parse 再重組）。
2. 驗簽後存進 `payment.webhook_events`，`(provider, provider_event_id)` 唯一；成功就回 200，之後由 `consume` 處理。重送也回 200（已落地）。
3. 付款單狀態只往前走，晚到的舊事件忽略（§3 狀態 × 事件全表）。
4. 金額和幣別必須和付款單相符；不符時不進 `CAPTURED`，進 `NEEDS_REVIEW` 並告警（T4、T6'、T9）。
5. `expire` 在標記 `EXPIRED` 前先向 provider 查一次狀態，涵蓋 webhook 沒送到的情況（T5／T6／T6'）。
6. Log 只寫固定分類與 code，不寫 provider 的原始錯誤或 payload（#347）；簽章、body 與 provider 密鑰不進 log、error、metric label、span。

### `payment-rpc` 認證入口逐一列出

認證比照 `catalog-rpc`：不接 identity、沒有 `CredentialResolver`、只認靜態 `auth.api_keys`；契約不支援的 credential 來源 **fail closed 拒啟動**（#323）。

| 入口 | 路徑 | 憑證 | 為什麼 |
|---|---|---|---|
| Connect RPC（`PaymentService`） | `/api/<svc>/<Method>` 與原生路徑 | `X-API-Key`，且只有 service key `rpc`；`auth.mode` 必須 `required` | 呼叫者只有 `apps/rpc`；ingress 不對外開（§1） |
| Webhook | 一條 gin route（路徑待切片 3 定）；ingress 只對外開這條 | **簽章**（provider 簽，用原始 body 驗） | provider 打不出我們的 API key，也不該有；API key 是我們發給自己服務的 credential，給 provider 等於把 service key 交出去。**gin route 不在 Connect chain 內，auth interceptor 不會保護它**，簽章是唯一一道閘 |
| `/api/healthz`、`/api/readyz`、`/api/version` | gin | 無（永遠開著） | probe；`readyz` 只看 payment 自己的 DB，不依賴 provider（同 rpc 的 `readyz` 不依賴 catalog）；provider 全部失敗時 `readyz` 仍 200，由 `TestPayment_Probes` 保證（切片 2） |
| `/api/metrics` | gin | 無（gin route，不經 auth interceptor，見 `pkgs/platform/server/auth.go` 檔頭註解） | 同其他 app；不經 ingress 對外 |
| `/api/docs` | gin | 無；預設關 | 同其他 app |
| 啟動拒絕（fail closed） | — | `auth.jwt` 有設、`auth.oidc.enabled`、`auth.mode` 開著但 `auth.api_keys` 空；`--require-durable` 另要求 `auth.mode: required`、`auth.api_keys` 恰好一把且名為 `rpc`、`postgres.url` 不空 | #323：傳 nil 不等於停用 |
| 沒有的入口 | — | 沒有 streaming RPC（auth 是 unary-only）；沒有 Bearer；沒有 identity key | 加了要另外包 |

`consume`、`expire` 是背景程序，沒有對外 listener；`serve` 與它們共用同一份 provider 密鑰設定，但只有 `serve` 的 webhook route 讀「驗簽金鑰」。

## 8. `payment_required` 快照 × 目前開關 × 付款單狀態

規則（§2）：ordering 只看訂單的 `payment_required` 快照。**目前的 `ordering.require_payment` 只決定兩件事**：(1) 之後進 `CONFIRMED` 的新訂單，快照存成什麼；(2) `StartPayment` 能不能用（§6）。它**不**影響任何已 `CONFIRMED` 訂單的 `CompleteOrder`、`CancelOrder` 與付款事件處理。

開關預設 `false`（切片 6 之前）；切片 6 之後**新部署**預設 `true`，範例設定與 chart 同步更新（已存在部署的 values 不自動改）。

### ordering 端：快照 × 訂單狀態 × 動作

| 快照 | 訂單狀態 | `CompleteOrder` | `CancelOrder` | `PaymentCaptured` | `PaymentExpired` |
|---|---|---|---|---|---|
| `false` | `CONFIRMED` | **可直接完成**（現狀） | 可取消（現狀） | 忽略（drop；`StartPayment` 擋下的前提下不該出現，出現就是缺陷，計數告警） | 忽略（不取消） |
| `false` | `COMPLETED` | `order.cannot_complete`（現狀） | `order.cannot_cancel`（現狀） | 忽略 | 忽略 |
| `false` | `CANCELLED` | `order.cannot_complete`（現狀） | `order.cannot_cancel`（現狀） | 忽略 | 忽略 |
| `true` | `CONFIRMED` | **拒絕** `order.payment_required`（`CONFIRMED` 只能經付款前進） | 可取消 | `PAID`→`COMPLETED` | 取消訂單 |
| `true` | `PAID` | 第一版不可達（與 `COMPLETED` 同 transaction） | `order.cannot_cancel` | 重複通知：no-op | 拒絕（drop）：訂單非 `CONFIRMED` |
| `true` | `COMPLETED` | `order.cannot_complete` | `order.cannot_cancel` | 重複通知：no-op | 拒絕（drop） |
| `true` | `CANCELLED` | `order.cannot_complete` | `order.cannot_cancel` | 拒絕 `PAID`（drop；Payment 側見 T10） | no-op（已取消） |
| 無（`PENDING`，尚未 `CONFIRMED`） | `PENDING` | `order.cannot_complete`（現狀） | 可取消（現狀） | 不可達（付款單在 `OrderConfirmed` 之後才建）；收到則拒絕（drop） | 同左 |

**開關狀態對上表每一格都沒有影響**——這是刻意的，也是「開關切回關閉是安全的」的依據。下表把「開關 × 快照」逐格列出，只引用上表與 §3，不重述結果：

| 目前開關 | 快照 | 新進 `CONFIRMED` 的訂單快照 | ordering 行為 | `StartPayment` | 預定測試 |
|---|---|---|---|---|---|
| `false`（**預設**） | `false` | `false` | 上表 `false` 列 | `payment.disabled` | S1：**不設定任何開關**（吃預設值）建單 → 快照為 `false` → `CompleteOrder` 成功；既有 usecase 測試在預設設定下全綠不改 |
| `false` | `true`（開關**從開切回關**） | `false` | 上表 `true` 列 | `payment.disabled`。顧客無法再發起付款後的結局由 T5 與 §3 失敗來源表（`expire` 查 provider 失敗列）決定；這是「切回關閉」的已知後果 | S6：kind 先開再關，斷言 ordering 行為同上表 `true` 列，並涵蓋 T5 與查詢失敗兩條結局 |
| `true` | `false`（舊單，或開關打開前建立的單） | `true` | 上表 `false` 列 | `payment.disabled` | S6：切換前建的 `CONFIRMED` 單，開關打開後仍可 `CompleteOrder`，且 `StartPayment` 回 `payment.disabled` |
| `true` | `true` | `true` | 上表 `true` 列 | 可用 | S3、S6 |

Payment 端對 `payment_required = false` 的訂單：付款單照建（§6），`OrderCompleted` 到時把 `PENDING` 轉 `CANCELLED`（T7'）；`OrderCancelled` 到時照 T7。**Payment 不看快照**，只有 ordering 看。

## 9. 切片與保證對照

| # | 切片 | 內容 | 改變現有下單行為？ |
|---|---|---|---|
| 0 | 設計文件 | 本文 | 否 |
| 1 | Ordering 契約（**已實作**） | `OrderConfirmed.total`、`PAID`、`OrderPaid`、訂單 `payment_required` 快照（migration：既有訂單為 `false`）、`require_payment` 設定（預設 `false`）、`CompleteOrder` 依快照擋、Console 顯示 | 否（預設 `false`，快照皆為 `false`） |
| 2 | Payment 核心 | `pkgs/payment`、schema、mock provider、`payment-rpc serve`（RPC；`StartPayment` 的 `payment.disabled` 在 rpc 側，切片 4） | 否（沒部署） |
| 3 | Saga | `consume`（訂閱 `OrderConfirmed`／`OrderCancelled`／`OrderCompleted`）、webhook 落地（含 platform 的 gin route option）、`expire`、自動退款（T10–T11）、ordering 訂閱 `PaymentCaptured`／`PaymentExpired`（依快照）、**`poison`／`prune` 的 `payment` 納入**（見下） | 否（預設快照 `false` 時 ordering 忽略付款事件） |
| 4 | 入口 | rpc 轉發 `StartPayment`／`GetPayment`（含 `payment.disabled`）、Console 付款頁 | 否 |
| 5 | 部署 | goreleaser 第六個 image、chart（ingress 只開 webhook）、Grafana、alert | 否 |
| 6 | 切換 | 開啟開關（新部署預設 `true`），kind 跑完整流程與第 5 節全部競態 | **是** |

每片一個 PR，切片 6 之前都不改變現有下單行為。

**切片 3 的 `poison`／`prune`（Sean 後續裁示 #4）**：要改的清單有兩處，都只加表名、不載入領域程式——`apps/worker/poison.go` 的 `schemas`（`worker poison` 與 `worker prune` 共用）與 `apps/rpc/app.go` 的 `NewPoisonService` 清單（admin RPC 的 poison／prune）。`payment` 的表名來源不能是 `pkgs/payment` 套件（會 import 領域程式）；用 platform 的字串常數或 contract 套件。payment 的 outbox／inbox／poison 表形狀與其他 BC 相同，`worker prune` 才能照既有路徑清理。`payment.webhook_events` 的 retention 不在 `PruneSchema` 既有範圍（它只處理 outbox／inbox），切片 3 要決定是否擴充同一入口或另設，並在該切片的 PR 寫明；本文不預設。

**切片 5 的順序**：image 先發版 → Sean 手動把新 GHCR package（`payment-rpc`）設成 public → chart 才能引用。bot chart bump 的常設授權 `d-20261031033617246691-4` 的匿名 image 檢查要從 5 個改成 6 個，**隨切片 5 同步改書面**（不能只改實際做法）。同時更新 `docs/release.md` 的 image 清單與「第一次 push 後改 public」那段。

### 保證 → 測試

每列：情境 → 引用哪些規則 → 斷言什麼。條件、時間、後果、Connect code 都由被引用的 T-row／表決定，這裡不重述。

| 保證 | 切片 | 引用 | 斷言 |
|---|---|---|---|
| 舊 `OrderConfirmed`（無 `total`）不當 0 元 | S2/S3 | 全表 A' 欄 | 沒有 `payment.payments` 列；domain error drop |
| 付款單狀態 × 事件的每一格 | S2/S3 | §3 全表 | domain 表驅動逐格斷言（含「—」格：送進去要有確定結果）；`domain/*` 100% 覆蓋由 CI `cover-gate` 守 |
| 失敗來源 × 狀態的每一格（不適用格除外） | S3 | §3 失敗來源表、T5、T7、T8、T9、T11 | 對 provider 的查詢、取消 checkout、退款、開 checkout 各做一次「一律失敗」fault injection，斷言狀態、告警／poison／錯誤碼符合該表；positive control：fake 確實被呼叫（計數 > 0），恢復後收斂 |
| `StartPayment` 重複呼叫 | S2 | T2 | URL 相同／過期後不同；`CreateCheckout` 呼叫次數 |
| `StartPayment` 的 provider 呼叫不在 DB transaction 內 | S2 | §1 例外表 | provider fake 在呼叫當下斷言沒有 open tx（觀察同一個 pool／handle） |
| 擁有權與 admin 權限 | S2/S4 | §6 擁有權 | 正負例：本人讀／付成功；他人讀／付皆 `NOT_FOUND`；admin 讀成功、代付被拒；rpc 層與 usecase 層各一 |
| 開關關閉或快照 `false` 時 `StartPayment` | S4 | §6、§3 錯誤碼表 | 預設設定（不設開關）下回 `payment.disabled`；開關打開 + 快照 `false` 也回 |
| webhook 簽章、原始 body | S3 | §7 Webhook 第 1 點 | 改一個 byte、重排 JSON key 各一例 → 4xx、不落地 |
| webhook 重送 | S3 | §7 Webhook 第 2 點 | `(provider, provider_event_id)` 唯一 + 重送測試（Postgres） |
| 金額／幣別不符 | S3 | T4、T6'、T9、全表 `NEEDS_REVIEW` 列 | 金額、幣別各一例；`expire` 對 `NEEDS_REVIEW` 單跑一輪狀態不變；不發 `PaymentCaptured` |
| `expire` 對 `PENDING` 的三種 provider 回應 | S3 | T5、T6、T6' | mock provider 三種回應，webhook 缺席 |
| `EXPIRED`／`CANCELLED` 的補查 | S3、S6 | T8、T9、T10、§3「補查有界」 | 三個時序：取消先贏、`PaymentCaptured` 先贏、金額不符；各斷言訂單最終狀態與是否出現 `PaymentRefundRequested`；「取消 checkout 失敗」與「逾時」兩種進入方式各一；窗口內外（fake clock）與啟動檢查；positive control：mock provider 被查到；S6 kind 同情境 |
| 查詢持續失敗的完整序列 | S3、S6 | §3 失敗來源表（`expire` 查 provider 失敗列）、T7 | fake clock + mock provider 查詢一律失敗：`PENDING` 不變 → 預留 TTL sweep → 訂單取消 → T7 → 補查；恢復後依 T8／T9 收斂；S6 以 `expire` CronJob 實跑 |
| 事件亂序與墓碑 | S3 | §4 亂序表、T10、T12、T14、T15 | 各順序的 consumer 測試，Postgres |
| 正常付款路徑的 `OrderCompleted` | S3 | 全表 `CAPTURED` 列 C 欄、T10 | `order_outcome` 仍為 `NONE`、沒有 `PaymentRefundRequested` |
| 快照 `false` 的訂單被 `CompleteOrder` | S3 | T7' | consumer 測試 |
| 退款冪等與失敗 | S3 | T11 | mock provider 以冪等 key 去重，重送 `PaymentRefundRequested`；`Refund` 一律失敗的 fault injection（上限、poison 列），positive control：恢復後成功 |
| `poison`／`prune` 納入 `payment` | S3 | §9「切片 3 的 `poison`／`prune`」、§1 例外表 | 清單測試（`schemas` 含 `payment`）+ depguard／archtest 負例（`apps/worker`、`apps/rpc` 不得 import payment 領域套件） |
| ordering 端每一格 | S1/S3 | §8 ordering 表 | usecase 表驅動；**預設設定**下的測試不顯式設開關，驗預設值本身 |
| 開關對已 `CONFIRMED` 訂單沒有影響 | S3/S6 | §8 | 同一張快照 `true` 的單，開關 `true`／`false` 下 `CompleteOrder`、`CancelOrder`、兩個付款事件結果相同；S6 kind 先開再關 |
| 快照的設定與 migration | S1 | §2 | 新進 `CONFIRMED` 的快照等於當下開關；既有訂單 migration 後為 `false`（Postgres） |
| `PAID` 後不可取消、`CANCELLED` 不復活 | S1/S3 | §2 | domain 與 usecase 測試 |
| 敏感資料不外洩（#347） | S2/S3 | §7 Webhook 第 6 點 | 以合成敏感標記放進 provider 錯誤、webhook body 與簽章，真入口觸發失敗；caller 錯誤、log、metrics label、span 都不含標記；positive control：標記有進到 fake／請求 |
| `apps/rpc` 不載入 payment 領域程式 | S4 | §1 | archtest／depguard 負例（比照 `apps/rpc/catalog_split_test.go`） |
| `payment-rpc` 掛掉時下單與預留照常 | S4/S6 | §1 | kind 故障演練：停 payment-rpc → `CreateOrder` 成功且到 `CONFIRMED`；付款操作失敗；恢復後同一 pod 繼續 |
| fail closed | S2/S5 | §7 認證入口表 | 比照 `apps/catalog-rpc/durable_test.go`：「單獨配置」與「與支援來源並存」兩種負例 |
| webhook route 不受 API key 影響 | S3 | §7 認證入口表 | `auth.mode: required` 下無 `X-API-Key` 但簽章正確 → 200；簽章錯 → 4xx |
| ingress 只對外開 webhook | S5 | §1 | `task helm-check` 負例：渲染出的 ingress 規則只含 webhook path |
| 競態在真環境成立 | S6 | §5 | kind 全流程（mock provider 的延遲／重送／亂序） |

## 10. 儲存（`payment` schema）

`payment.payments`、`payment.webhook_events`（`unique(provider, provider_event_id)`、原始 body 與簽章結果、處理狀態）、`payment.order_outcomes`（〔實作細節〕墓碑表，見 T15）、`payment.events`／`payment.inbox`／`payment.poison`（同其他 BC 的 outbox／inbox／poison 形狀）、`payment.schema_migrations`。`apps/migrate` 收集 payment 的 `Migration()`。`postgres.url` 空 → in-memory（`task run`），遵守同一份 `Save` 合約。

## 11. 不做

admin 退款、部分退款、`COMPLETED` 之後的退款（先到 provider 後台處理）、真 provider（第一版只有 mock）、Fulfillment（所以 `PAID` 不停留）、`NEEDS_REVIEW` 的系統內出口（第一版只告警）、`FAILED` 狀態。

## 12. 已決事項對照（原 U1–U6）

PR #369 審閱時曾列出的未決事項，已由 Sean 裁示，改寫成上文的規則：

| 原未決 | 裁示 | 落在 |
|---|---|---|
| U1 `CompleteOrder` 完成後仍可付款 | Payment 也訂閱 `OrderCompleted`，把 `PENDING` 轉 `CANCELLED` | §3 T7'、§4、§8 |
| U2 `customer_id` 以 request 欄位傳 | 接受，由 rpc 以 service key 傳入；ingress 只對外開 webhook | §1、§7 |
| U3 `poison`／`prune` 入口 | 清單加入 `payment` schema，只操作表名、不載入領域程式 | §1、§9 切片 3 |
| U4 ordering 怎麼知道「有付款單」 | 不用「有沒有付款單」；改用訂單 `payment_required` 快照 | §2、§8 |
| U5 `NEEDS_REVIEW`／`CANCELLED` 狀態與人工出口 | 接受兩個狀態；`NEEDS_REVIEW` 第一版只告警，到 provider 後台處理，`expire` 不動 | §3 |
| U6 開關 `false` 時 `StartPayment` | 回 `payment.disabled` | §6、§3 錯誤碼表 |

## 13. 決定來源 → 權威章節

| 決定 | 本文章節 | 備註 |
|---|---|---|
| #367 §1 先預留再付款 | §2 | Sean 原決定 |
| #367 §2 `PAID` 狀態 | §2 | 原決定；同 transaction 發 `OrderPaid`／`OrderCompleted` 為〔實作細節〕 |
| #367 §3 付款窗口與競態 | §3、§4、§5 | 原決定；`order_outcome`、墓碑表為〔實作細節〕 |
| #367 §4 發起方式 | §3 T1–T2、§6 | 原決定 |
| #367 §5 Provider／webhook／部署／認證 | §1、§7 | 原決定；gin route option 為 platform 變更（§1） |
| #367 §6 退款範圍 | §3 T10–T11、§11 | 原決定；`PaymentRefundRequested` 為〔實作細節〕 |
| #367 §7 切片與上線 | §8、§9 | 原決定；其中「**切換當下的舊訂單依有沒有付款單判斷**」已被**取代**（見下） |
| 後續裁示 #1、#2 快照 | §2、§8 | **取代** #367 §7「依有沒有付款單判斷，不看建單時間」。舊訂單改由 migration 設 `payment_required = false`，語意仍是「舊單照舊可完成」，但判斷依據是快照 |
| 後續裁示 #3 訂閱 `OrderCompleted` | §3 T7'、§4 | **擴充** #367 §5 的 `consume` 訂閱清單（原為 `OrderConfirmed`、`OrderCancelled`） |
| 後續裁示 #4 `poison`／`prune` | §1、§9 | 補 #367 §5「`apps/worker` 不載入 Payment」的營運面 |
| 後續裁示 #5、#6 狀態與 `NEEDS_REVIEW` | §3 | 補 #367 §5.4、§6 的狀態集合 |
| 後續裁示 #7 `payment.disabled` | §6 | 補 #367 §7 開關 `false` 時的入口行為 |
| 後續裁示 #8 `customer_id`、ingress | §1、§7 | 補 #367 §5 認證 |
