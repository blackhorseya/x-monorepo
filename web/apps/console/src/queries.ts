import { Code, ConnectError } from "@connectrpc/connect";
import { queryOptions } from "@tanstack/react-query";
import { admin } from "./client";

// GET /api/version (platform/buildinfo): plain JSON, not a Connect RPC.
export type ServerVersion = { version: string; commit: string; date: string; go: string };

export const keys = {
  poison: ["poison"] as const,
  poisonDetail: (bc: string, offset: bigint) => ["poison", bc, String(offset)] as const,
  version: ["version"] as const,
  adminProbe: (generation: number) => ["admin-probe", generation] as const,
};

// x-error-code of the rpc answering "the role check passed, but there is no database" (pkgs/platform/admin).
const NO_DATABASE_CODE = "admin.no_database";

// Plain fetch, not an RPC, so the HTTP status has to be turned into what the shared retry policy
// (src/transient.ts) understands: 429/502/503/504 mean the server (or the proxy in front of it) is not
// answering, the same thing Connect maps to Unavailable for the RPCs, so they throw that code and are retried;
// any other status is an answer, a plain Error, asked once.
const TRANSIENT_HTTP = new Set([429, 502, 503, 504]);

// The build never changes while a tab is open; refetch only on a reload or
// after a long idle so a rolling deploy eventually shows up.
export const versionQuery = queryOptions({
  queryKey: keys.version,
  queryFn: async (): Promise<ServerVersion> => {
    const res = await fetch("/api/version");
    if (!res.ok) {
      const msg = `GET /api/version: ${res.status}`;
      throw TRANSIENT_HTTP.has(res.status) ? new ConnectError(msg, Code.Unavailable) : new Error(msg);
    }
    return (await res.json()) as ServerVersion;
  },
  staleTime: 5 * 60_000,
  refetchInterval: 5 * 60_000,
});

// The poison tables of the bounded contexts that consume events; parking happens after retries, so a slower poll is fine.
export const poisonQuery = queryOptions({
  queryKey: keys.poison,
  queryFn: async () => (await admin.listPoison({})).messages,
  refetchInterval: 5000,
});

export const poisonDetailQuery = (bc: string, offset: bigint) =>
  queryOptions({
    queryKey: keys.poisonDetail(bc, offset),
    queryFn: async () => (await admin.getPoison({ boundedContext: bc, offset })).message ?? null,
  });

// Can the current credential use the admin RPCs? Asked of the server itself (the cheapest
// read-only admin call, ListPoison with limit 1) rather than guessed from a role claim: the role the
// server wants is auth.admin_role, which the console cannot know, and with auth off there is no role at
// all. true = the call went through; PERMISSION_DENIED / UNAUTHENTICATED = false; anything else
// (network, 5xx) is an error, not a "no".
//
// One more "yes": FAILED_PRECONDITION with x-error-code admin.no_database. The rpc checks the role first
// and the database second (pkgs/platform/admin/poison_service.go guard()), so an rpc with no database
// (in-memory: local dev, the e2e `auth` and `image` projects) only sends that to a caller that already
// passed the role check. Only that one code: any other FAILED_PRECONDITION says nothing about the role
// and is an error like the rest.
//
// Keyed by the credential generation (src/auth.ts), so a late answer for the previous credential
// lands under its own key and is never read for the new one; the new one asks again, unproven =
// not admin until it answers. "Not admin" is an answer (PERMISSION_DENIED / UNAUTHENTICATED) and
// is cached; a probe that could not be answered (5xx, network, timeout) throws, is retried by the
// shared policy (src/transient.ts: transient failures only, bounded), and is never cached as "not admin"
// (see AdminProbeNotice for what is left to the user).
export const adminProbeQuery = (generation: number) =>
  queryOptions({
    queryKey: keys.adminProbe(generation),
    queryFn: async () => {
      try {
        await admin.listPoison({ limit: 1 });
        return true;
      } catch (err) {
        const e = ConnectError.from(err);
        if (e.code === Code.PermissionDenied || e.code === Code.Unauthenticated) return false;
        if (e.code === Code.FailedPrecondition && e.metadata.get("x-error-code") === NO_DATABASE_CODE) return true;
        throw err;
      }
    },
    staleTime: 60_000,
    gcTime: 60_000,
  });
