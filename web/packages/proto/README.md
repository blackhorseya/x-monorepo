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

## buf.validate is stripped from the TS output

`task gen-ts` does not feed the protos straight to the plugin: `buf build -o - | go run ./tools/stripvalidate | buf generate -`.
`tools/stripvalidate` rewrites the image so that

- every options message (file, message, field, oneof, enum, service, method…) loses its extension and unknown fields,
  which is where `(buf.validate.field)` and friends live;
- no file depends on `buf/validate/validate.proto` any more, and that file becomes `buf/validate/violations.proto`
  with only `Violations`, `Violation`, `FieldPath` and `FieldPathElement` (package still `buf.validate`, so type URLs are unchanged).

Why: `validate.proto`'s descriptor was a ~70 kB string every generated file listed as a dependency, so neither tree-shaking
nor dynamic `import()` could drop it, and the console only needs `ViolationsSchema` to render field errors
(`web/apps/console/src/client.ts`).

Trade-offs:

- The browser has no protovalidate rules to evaluate (`@bufbuild/protovalidate` on the client would see none). Validation
  stays on the server (`NewValidateInterceptor`) and in the domain; Go codegen is untouched.
- Unknown option fields are stripped from all options, not only `buf.validate.*`. A new custom option read in the browser
  would need a pass-through in `tools/stripvalidate/strip.go`.
- Comments of the four kept messages are not carried into `violations_pb.ts` (the stripped file has no source info).

Wire format is unchanged: `tools/stripvalidate`'s test encodes `Violations` with the original schema and decodes it with
the stripped one, and the reverse.

