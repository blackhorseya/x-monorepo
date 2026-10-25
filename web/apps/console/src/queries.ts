import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import { admin, inventory, isNotFound, orders } from "./client";

// GET /api/version (platform/buildinfo): plain JSON, not a Connect RPC.
export type ServerVersion = { version: string; commit: string; date: string; go: string };

// The server exposes unary RPCs only, so the console polls. Reservations are
// written by apps/worker after it consumes OrderCreated, and orders can be
// cancelled by inventory (shortfall), so both change without a user action.
const POLL_MS = 2000;

export const keys = {
  orders: ["orders"] as const,
  ordersPage: (customerId: string, status: OrderStatus) => ["orders", customerId, status] as const,
  order: (id: string) => ["order", id] as const,
  reservation: (orderId: string) => ["reservation", orderId] as const,
  stocks: ["stocks"] as const,
  stocksPage: (skuPrefix: string, onlyReserved: boolean) => ["stocks", skuPrefix, onlyReserved] as const,
  reservations: (status: ReservationStatus) => ["reservations", status] as const,
  poison: ["poison"] as const,
  poisonDetail: (bc: string, offset: bigint) => ["poison", bc, String(offset)] as const,
  version: ["version"] as const,
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
// (UNSPECIFIED = any). Loaded pages poll: inventory cancels orders on its own.
// The key starts with keys.orders so a mutation invalidates every filter.
export const ordersQuery = (customerId: string, status: OrderStatus) =>
  infiniteQueryOptions({
    queryKey: keys.ordersPage(customerId, status),
    queryFn: async ({ pageParam }) => orders.listOrders({ customerId, status, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    refetchInterval: POLL_MS,
  });

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

// null = the worker has not reserved anything for this order (yet).
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
