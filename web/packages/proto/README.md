# @repo/proto

TypeScript types and Connect clients generated from `/protos` (protobuf-es v2).

```bash
# from repo root
task gen-ts            # → web/packages/proto/src/gen/**
```

Usage (see `web/apps/console/src/client.ts` for the real thing):

```ts
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { OrderService } from "@repo/proto/ordering/v1/service_pb";

const client = createClient(OrderService, createConnectTransport({ baseUrl: window.location.origin }));
const { order } = await client.createOrder({ customerId: "c-1", items: [] });
```

`src/gen/` is committed so the frontend builds without buf installed. `exports` maps
`@repo/proto/<path>` to `src/gen/<path>.ts`; consumers' bundlers compile it, there is no build step.
