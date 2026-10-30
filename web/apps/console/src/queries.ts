import { Code, ConnectError } from "@connectrpc/connect";
import { infiniteQueryOptions, queryOptions, type InfiniteData, type QueryClient } from "@tanstack/react-query";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import type { ListOrdersResponse } from "@repo/proto/ordering/v1/service_pb";
import { admin, catalog, identity, inventory, isNotFound, orders } from "./client";

// GET /api/version (platform/buildinfo): plain JSON, not a Connect RPC.
export type ServerVersion = { version: string; commit: string; date: string; go: string };

// The server exposes unary RPCs only, so the console polls. Reservations are
// written by apps/worker after it consumes OrderCreated, and orders can be
// cancelled by inventory (shortfall), so both change without a user action.
export const POLL_MS = 2000;
// A list query is fresh for a second: remounting a list (or two components asking for the
// same page) within a tick reuses the data instead of going back to the server.
const LIST_STALE_MS = 1000;

export const keys = {
  orders: ["orders"] as const,
  ordersPage: (customerId: string, status: OrderStatus) => ["orders", customerId, status] as const,
  order: (id: string) => ["order", id] as const,
  reservation: (orderId: string) => ["reservation", orderId] as const,
  quote: (skus: string[]) => ["quote", ...skus] as const,
  products: ["products"] as const,
  productsPage: (skuPrefix: string) => ["products", skuPrefix] as const,
  stocks: ["stocks"] as const,
  stocksPage: (skuPrefix: string, onlyReserved: boolean) => ["stocks", skuPrefix, onlyReserved] as const,
  reservations: (status: ReservationStatus) => ["reservations", status] as const,
  poison: ["poison"] as const,
  poisonDetail: (bc: string, offset: bigint) => ["poison", bc, String(offset)] as const,
  version: ["version"] as const,
  adminProbe: (generation: number) => ["admin-probe", generation] as const,
  whoAmI: (generation: number) => ["who-am-i", generation] as const,
  accounts: ["accounts"] as const,
  account: (id: string) => ["account", id] as const,
  apiKeys: (accountId: string) => ["api-keys", accountId] as const,
};

// The build never changes while a tab is open; refetch only on a reload or
// after a long idle so a rolling deploy eventually shows up.
export const versionQuery = queryOptions({
  queryKey: keys.version,
  queryFn: async (): Promise<ServerVersion> => {
    const res = await fetch("/api/version");
    if (!res.ok) throw new Error(`GET /api/version: ${res.status}`);
    return (await res.json()) as ServerVersion;
  },
  staleTime: 5 * 60_000,
  refetchInterval: 5 * 60_000,
});

// Orders newest first, 20 per page, by customer (empty = any) and status
// (UNSPECIFIED = any). Inventory cancels orders on its own, so the list refreshes, but not
// by refetchInterval: that would refetch every loaded page each tick (N pages = N requests).
// refreshFirstPage polls the newest page instead. The key starts with keys.orders so a
// mutation invalidates every filter.
export const ordersQuery = (customerId: string, status: OrderStatus) =>
  infiniteQueryOptions({
    queryKey: keys.ordersPage(customerId, status),
    queryFn: async ({ pageParam }) => orders.listOrders({ customerId, status, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    staleTime: LIST_STALE_MS,
  });

// One poll tick of the orders list: fetch only the first page. Same orders in the same order
// = nothing moved, so just swap in the fresh rows (status changes) and keep the other pages.
// Anything else (a new order, one that left the filter) shifts the page boundaries, so
// refetch every loaded page. Older pages catch up on a remount, a mutation or window focus.
export async function refreshFirstPage(qc: QueryClient, customerId: string, status: OrderStatus) {
  const key = keys.ordersPage(customerId, status);
  const cur = qc.getQueryData<InfiniteData<ListOrdersResponse, string>>(key);
  if (cur === undefined || qc.isFetching({ queryKey: key }) > 0) return;
  const stamp = qc.getQueryState(key)?.dataUpdatedAt;
  const fresh = await orders.listOrders({ customerId, status, pageSize: 20, pageToken: "" });
  // This request lives outside the query, so a mutation's refetch cannot cancel it. If the
  // cache moved on while it was in flight (a refetch running, or one that already landed),
  // the response is older than what is shown: drop it, the next tick tries again.
  if (qc.isFetching({ queryKey: key }) > 0 || qc.getQueryState(key)?.dataUpdatedAt !== stamp) return;
  const old = cur.pages[0]?.orders ?? [];
  const same = fresh.orders.length === old.length && fresh.orders.every((o, i) => o.id === old[i]?.id);
  if (!same) return qc.invalidateQueries({ queryKey: key });
  qc.setQueryData<InfiniteData<ListOrdersResponse, string>>(key, (d) =>
    d === undefined ? d : { ...d, pages: [fresh, ...d.pages.slice(1)] },
  );
}

// One order for /orders/$id; null = no such id. PENDING keeps polling because
// inventory cancels orders on its own; CANCELLED is terminal.
export const orderQuery = (id: string) =>
  queryOptions({
    queryKey: keys.order(id),
    queryFn: async () => {
      try {
        return (await orders.getOrder({ id })).order ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    refetchInterval: (q) => {
      const s = q.state.data?.status;
      return s === undefined || s === OrderStatus.PENDING ? POLL_MS : false;
    },
  });

// null = the worker has not reserved anything for this order (yet). One order's page polls it
// on its own state alone; the orders list does not use this at all: its rows share one
// BatchGetReservations per tick (orderReservations.ts).
export const reservationQuery = (orderId: string) =>
  queryOptions({
    queryKey: keys.reservation(orderId),
    queryFn: async () => {
      try {
        return (await inventory.getReservation({ orderId })).reservation ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    // Everything but RESERVED is terminal; RESERVED can still be released and
    // a missing reservation can still appear.
    refetchInterval: (q) => {
      const s = q.state.data?.status;
      return s === undefined || s === ReservationStatus.RESERVED ? POLL_MS : false;
    },
  });

// Is there a change to wait for on an orders-list row, given the order's status, the reservation's (undefined =
// none yet) and lateSince (when the order turned terminal with no reservation, ms epoch)?
// PENDING: the worker has not reserved yet. COMPLETED / CANCELLED with the stock still
// RESERVED: the commit / release event is on its way. COMPLETED / CANCELLED with no
// reservation yet is not final either: inventory takes OrderCancelled before OrderCreated and
// then records a CANCELLED tombstone however late the worker gets to it, and the order's
// status will not move again to prompt a re-fetch. Look for LATE_WAIT_MS from lateSince, then
// give up until the next mount / focus. A deadline, not a fetch count: fetches made while the
// order was PENDING, or by other mounts, must not use the window up. A CONFIRMED order holds
// its reservation until the customer acts, which moves the order's status and re-fetches.
const LATE_WAIT_MS = 30_000;
export function reservationPending(
  order: OrderStatus,
  reservation: ReservationStatus | undefined,
  lateSince: number | undefined,
  now = Date.now(),
): boolean {
  if (order === OrderStatus.PENDING) return true;
  if (order !== OrderStatus.COMPLETED && order !== OrderStatus.CANCELLED) return false;
  if (reservation === undefined) return lateSince === undefined || now - lateSince < LATE_WAIT_MS;
  return reservation === ReservationStatus.RESERVED;
}

// SKUs ascending, 20 per page, by prefix (empty = every SKU) and optionally
// only the ones with something reserved; the worker moves reserved / available
// as orders come and go, so the loaded pages are polled. The key starts with
// keys.stocks so Set stock invalidates every filter.
export const stocksQuery = (skuPrefix: string, onlyReserved: boolean) =>
  infiniteQueryOptions({
    queryKey: keys.stocksPage(skuPrefix, onlyReserved),
    queryFn: async ({ pageParam }) => inventory.listStocks({ skuPrefix, onlyReserved, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    refetchInterval: POLL_MS,
    staleTime: LIST_STALE_MS,
  });

// What the catalog would charge for these SKUs right now (CatalogService.QuotePrices, a read
// open to every caller). Only the order form's preview: CreateOrder asks the catalog again and the
// price it finds then is the one the order keeps.
export const quoteQuery = (skus: string[]) =>
  queryOptions({
    queryKey: keys.quote(skus),
    queryFn: () => catalog.quotePrices({ skus }),
    enabled: skus.length > 0,
    staleTime: LIST_STALE_MS,
    retry: false,
  });

// Products ascending by SKU, 20 per page, by prefix (empty = every SKU), inactive ones included.
// Nothing changes them behind the console's back, so no polling; the key starts with keys.products so
// saving or deactivating a product refreshes every filter.
export const productsQuery = (skuPrefix: string) =>
  infiniteQueryOptions({
    queryKey: keys.productsPage(skuPrefix),
    queryFn: async ({ pageParam }) => catalog.listProducts({ skuPrefix, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    staleTime: LIST_STALE_MS,
  });

// Reservations newest first, 20 per page, optionally one status (UNSPECIFIED =
// any); RESERVED rows can still be released by the worker, so loaded pages poll.
export const reservationsQuery = (status: ReservationStatus) =>
  infiniteQueryOptions({
    queryKey: keys.reservations(status),
    queryFn: async ({ pageParam }) => inventory.listReservations({ status, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    refetchInterval: POLL_MS,
    staleTime: LIST_STALE_MS,
  });

// Every BC's poison table; parking happens after retries, so a slower poll is fine.
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

// Can the current credential use the admin identity RPCs? Asked of the server itself (the
// cheapest admin call, one account) rather than guessed from WhoAmI's roles: the role the server
// wants is auth.admin_role, which the console cannot know, and with auth off there is no role at
// all. true = the call went through; PERMISSION_DENIED / UNAUTHENTICATED = false; anything else
// (network, 5xx) is an error, not a "no".
//
// Keyed by the credential generation (src/auth.ts), so a late answer for the previous credential
// lands under its own key and is never read for the new one; the new one asks again, unproven =
// not admin until it answers. "Not admin" is an answer (PERMISSION_DENIED / UNAUTHENTICATED) and
// is cached; a probe that could not be answered (5xx, network, timeout) throws, is retried with
// backoff, and is never cached as "not admin" (see AdminProbeNotice for what is left to the user).
export const adminProbeQuery = (generation: number) =>
  queryOptions({
    queryKey: keys.adminProbe(generation),
    queryFn: async () => {
      try {
        await identity.listAccounts({ pageSize: 1 });
        return true;
      } catch (err) {
        const code = ConnectError.from(err).code;
        if (code === Code.PermissionDenied || code === Code.Unauthenticated) return false;
        throw err;
      }
    },
    staleTime: 60_000,
    gcTime: 60_000,
    retry: 3,
    retryDelay: (attempt) => Math.min(500 * 2 ** attempt, 4_000),
  });

// Who the server thinks the current credential is. The order form reads `anonymous` from it: an
// anonymous caller (auth off / optional, nothing sent) has to name the customer, a known one is
// the customer unless the admin probe says otherwise. Keyed by the credential generation like the
// probe, so a late answer for the previous credential is never read for the new one.
export const whoAmIQuery = (generation: number) =>
  queryOptions({
    queryKey: keys.whoAmI(generation),
    queryFn: async () => identity.whoAmI({}),
    staleTime: 60_000,
    gcTime: 60_000,
    retry: false,
  });

// Accounts by id ascending, 20 per page. Created / disabled / re-roled from this console,
// so no polling; the key starts with keys.accounts so a mutation invalidates every page.
export const accountsQuery = infiniteQueryOptions({
  queryKey: keys.accounts,
  queryFn: async ({ pageParam }) => identity.listAccounts({ pageSize: 20, pageToken: pageParam }),
  initialPageParam: "",
  getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
  staleTime: LIST_STALE_MS,
});

// null = no such account.
export const accountQuery = (id: string) =>
  queryOptions({
    queryKey: keys.account(id),
    queryFn: async () => {
      try {
        return (await identity.getAccount({ id })).account ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    staleTime: LIST_STALE_MS,
  });

// Every key of one account, REVOKED included, newest first (one response, no paging).
export const apiKeysQuery = (accountId: string) =>
  queryOptions({
    queryKey: keys.apiKeys(accountId),
    queryFn: async () => (await identity.listApiKeys({ accountId })).apiKeys,
    staleTime: LIST_STALE_MS,
  });
