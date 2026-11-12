# Observability

<sub>← [README](../README.md)</sub>

## Observability：OTel → LGTM

`platform/otel.New(cfg.OTel, cfg.Log)` 回傳 `*otel.Providers`（TracerProvider、MeterProvider、Propagator、root `*slog.Logger`、
`/api/metrics` handler），不碰 global。程式碼只用 OTel API instrument，Prometheus 只是輸出格式：

- **metrics 永遠開**：MeterProvider 掛一個 Prometheus reader，`server` 把它放在 `GET /api/metrics`（沒 collector 也能 scrape）。
  `usecasex.Metrics` 記 `usecase.duration`（exposition：`usecase_duration_seconds{usecase, kind}`），
  `otelconnect` 產 `rpc.server.call.duration`，`contrib/instrumentation/runtime` 產 Go runtime metrics，
  auth interceptor 記 `auth.decisions`（`auth_decisions_total{result, method}`：`ok` / `public` / `anonymous` / `missing` / `invalid` × `api_key` / `jwt` / `none`——
  RPC status 只會說 `unauthenticated`，分不出「沒帶」和「帶了被拒」；label 只有這兩個有限集合，沒有 subject；OIDC 的 `method` 仍是 `jwt`：未綁定但 `AllowsUnbound` 放行的 procedure 記 `ok`，被擋或其他拒絕記 `invalid`）；
  `auth.oidc` 開著時另有 `auth_jwks_fetch_total{issuer, result}`（`ok` / `error`）與 `auth_jwks_age_seconds{issuer}`（距上次成功抓 JWKS 的秒數，沒成功過就從啟動算起），alert `x-monorepo-auth-jwks-stale` 在超過 2 小時時觸發；issuer 來自設定，沒有高基數
- **worker 的 backlog**（`platform/eventbus`，`apps/worker` 註冊）：`eventbus.consumer.duration{topic, result}` 每次 handler
  attempt 一個 sample（retry 也算，被 park 的訊息就是它最後一次 `failed`）；`eventbus.Lag` 掛 observable gauge，每次 collect
  才去查：`eventbus_consumer_lag{topic, consumer_group, broker}` = 該 group 還沒 ack 的訊息數——Postgres 用和 watermill-sql
  subscriber 一樣的謂詞（cursor 之後、且 `transaction_id < snapshot xmin`，所以只算「現在真的讀得到」的），Redis 是 XINFO 的
  `lag + pending`；`eventbus_consumer_oldest_age_seconds` 是最老那筆等了多久（在 SQL / 用 Redis server 的時鐘算，不吃本機時區與時鐘偏移）；
  `eventbus_poison_messages{topic}` 是 `<bc>.poison` 表的 row 數（沒人消費，所以也是累計）。redis 模式多一組
  `consumer_group="forwarder"` 的 Postgres cursor（outbox → stream 那一跳）。有 DB 時同一個 callback 再報 retention 狀態：
  `eventbus_outbox_rows{table}` / `eventbus_outbox_oldest_age_seconds{table}` 是每張 outbox 表的大小與最老一列的年齡，
  `eventbus_outbox_held{table}` / `eventbus_outbox_oldest_held_age_seconds{table}` 是 offsets 表裡**任一** group 還沒 ack 的列
  （prune 不管多老都留著；held 的年齡一直長但 consumer lag 很低 = 有個沒人跑的 stale group），`eventbus_inbox_rows{schema}` /
  `eventbus_inbox_oldest_age_seconds{schema}` 是 `<bc>.inbox`。這些是 `count(*)` 全表掃，表很大時把 OTLP 的 collect 間隔拉長。
  每個 replica 都回報同一組數字：**用 `max`，不要 `sum`**。
  worker 沒有 HTTP server，這些只走 OTLP（60s 一次）
- **rpc 不延續外部帶進來的 `traceparent`**。信不信任遠端 trace 由各 composition root 在程式裡宣告（`apps/rpc` 的 `TraceOptions` 是 `server.DistrustRemoteTrace()`，用 wire 提供），不是 config、env 或 Helm value，operator 沒有入口能在 rpc 上打開；`server.New` 不帶選項時也是不信任（`pkgs/platform/server/trace_test.go`，protocol path 與 `/api` 兩個 mount、成功與業務錯誤都測）。`server.TrustRemoteTrace()` 仍在 `pkgs/platform/server`，但目前沒有任何服務使用它。
  - **兩種 SDK 狀態都不延續**。要查這張單的 trace，用回應的 `X-Trace-Id`，**不要**用自己送進來的 trace id。
    - rpc 開 SDK（`otel.endpoint` 有設）：server span 是新的 root，呼叫端的 span 只留成一個 link，`X-Trace-Id`（成功與錯誤都有）是 rpc 自己的。
    - rpc 關 SDK（`otel.endpoint` 空）：tracer 是 no-op，而 no-op tracer 會原樣傳遞 parent 的 trace id，所以 `otel.New` 在這時給**空的 propagator**，不讀也不寫 trace header（`pkgs/platform/otel/propagation_test.go`；不是設定項，沒有 tracer 就沒有 trace）。rpc 不回 `X-Trace-Id`／`traceparent`，外部 ID 不會出現在回應。
  - SDK 開關兩格由 `apps/rpc/trace_test.go` 的 `TestTrace_RPCDistrustsAnExternalTraceparent` 釘住（帶外部 `traceparent` 打 `AdminService.ListPoison`）：開 SDK → `X-Trace-Id` 有效且 ≠ 外部 ID；關 SDK → 回應沒有 `X-Trace-Id` 也沒有 `traceparent`。
  - rpc 沒有下游呼叫，所以沒有「轉送 `traceparent`」的問題；以後新增下游呼叫時，要把「轉送」這一格（與測試）補回來。
- **有 `otel.endpoint` 才推 OTLP**：traces 走 batcher、metrics 多一個 periodic reader、logs 走 batch processor；`otelconnect`
  interceptor 掛最外層，每個 RPC 一個 span。endpoint 空 = tracer no-op、logger 只寫 stdout、不 dial
- **span 的來源**：RPC 由 `otelconnect`；`/api/docs`、`/api/openapi/*` 等普通 route 由 `otelgin`（filter 掉 RPC prefix、
  `/api/healthz`、`/api/readyz`、`/api/metrics`，不會重複）；每條 SQL 由 `platform/postgres` 自己的小 gorm plugin 掛在 request span 底下（只有 SQL 形狀，不帶變數；不用
  `gorm.io/plugin/opentelemetry`，它會把 ClickHouse / MySQL driver 拖進 binary）
- **logs**：root logger 永遠寫 stdout（`log.format` / `log.level`），有 endpoint 時再 fan-out 一份給 `otelslog` bridge → Loki，
  同一個 level gate。`contextx.Contextx` 的 `Info/Warn/…` 會把 context 帶給 handler，所以 bridge 自動附 `trace_id` / `span_id`
  （stdout 那份靠 interceptor 的 `trace_id` attr）。Loki 裡 attr 是 structured metadata：查 `{service_name="rpc"} | trace_id="…"`，
  不要用 `|=` line filter（比不到）
- **resource**：明確設 `service.name`（= Prometheus `job`）與 `service.instance.id`（= `instance`）——OTLP→Prometheus
  只有這兩個 attribute 會變成 series label，少了 instance.id 多 replica 會寫同一條 series、counter 亂跳

本機：`task compose-up` 連 `grafana/otel-lgtm` 一起起（Grafana `http://localhost:3000`，匿名 admin；OTLP gRPC :4317），
`configs/local.yaml`（rpc）的 `otel.endpoint` 已指向它。
