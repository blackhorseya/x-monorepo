import { Code, ConnectError } from "@connectrpc/connect";

// Stands in for src/client.ts in node tests (see loader.mjs). Every RPC method of every service client asks
// `server.answer` what to do, so each query's real queryFn runs and only the network is scripted.
export type Answer = { ok: unknown } | { fail: ConnectError };
type Handler = (service: string, method: string) => Answer;

// A success: the fields a queryFn unwraps (`.apiKeys`, `.messages`), empty. A queryFn that returns undefined is a
// react-query error, so an empty `{}` would turn "the retry succeeded" into a failure of its own.
export const OK = { apiKeys: [], messages: [] };

export const server = {
  calls: [] as string[],
  // The request each call carried, in the same order as `calls`: a test asserts WHICH RPC and WITH WHAT.
  requests: [] as unknown[],
  answer: ((): Answer => ({ ok: OK })) as Handler,
  reset() {
    this.calls = [];
    this.requests = [];
    this.answer = () => ({ ok: OK });
  },
};

function service(name: string) {
  return new Proxy(
    {},
    {
      get: (_t, method: string) => async (req?: unknown) => {
        server.calls.push(`${name}.${method}`);
        server.requests.push(req);
        const a = server.answer(name, method);
        if ("fail" in a) throw a.fail;
        return a.ok;
      },
    },
  );
}

type Rpc = Record<string, (req?: unknown) => Promise<any>>;
export const admin = service("admin") as Rpc;
export const identity = service("identity") as Rpc;

export const isNotFound = (err: unknown): boolean => ConnectError.from(err).code === Code.NotFound;
export const isUnauthenticated = (err: unknown): boolean => ConnectError.from(err).code === Code.Unauthenticated;
export const describe = (err: unknown): string => String(ConnectError.from(err).message);
