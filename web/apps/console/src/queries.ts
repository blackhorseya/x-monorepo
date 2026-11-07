import { Code, ConnectError } from "@connectrpc/connect";
import { CancelledError, infiniteQueryOptions, queryOptions, type InfiniteData, type QueryClient, type QueryKey } from "@tanstack/react-query";
import { ReservationStatus, type Reservation } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus, type Order } from "@repo/proto/ordering/v1/entity_pb";
import { getCredentialGeneration } from "./auth";
import { isTransient } from "./transient";
import { admin, catalog, identity, inventory, isNotFound, orders, payments } from "./client";

// GET /api/version (platform/buildinfo): plain JSON, not a Connect RPC.
export type ServerVersion = { version: string; commit: string; date: string; go: string };

// The server exposes unary RPCs only, so the console polls. Reservations are
// written by apps/worker after it consumes OrderCreated, and orders can be
// cancelled by inventory (shortfall), so both change without a user action.
export const POLL_MS = 2000;
// The slow poll, for what can still change but only on its own schedule (a CONFIRMED order whose
// reservation may expire, a reservation that stays RESERVED until the customer acts).
export const SLOW_POLL_MS = 15_000;
// A list query is fresh for a second: remounting a list (or two components asking for the
// same page) within a tick reuses the data instead of going back to the server.
const LIST_STALE_MS = 1000;

export const keys = {
  orders: ["orders"] as const,
  ordersPage: (customerId: string, status: OrderStatus) => ["orders", customerId, status] as const,
  // order(id) is the prefix every generation of one order's detail query starts with (invalidating it
  // refreshes them all); orderOf(id, generation) is the key the detail page reads.
  order: (id: string) => ["order", id] as const,
  orderOf: (id: string, generation: number) => ["order", id, generation] as const,
  // history(id) is the prefix every generation of one order's history starts with (a mutation invalidates it);
  // historyOf(id, generation) is the key the History card reads, so another credential is another entry.
  history: (id: string) => ["history", id] as const,
  historyOf: (id: string, generation: number) => ["history", id, generation] as const,
  reservation: (orderId: string) => ["reservation", orderId] as const,
  payment: (generation: number, orderId: string) => ["payment", generation, orderId] as const,
  quote: (skus: string[]) => ["quote", ...skus] as const,
  products: ["products"] as const,
  productsPage: (skuPrefix: string) => ["products", skuPrefix] as const,
  productStatus: (skus: string[]) => ["products", "status", ...skus] as const,
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
  identities: (accountId: string, generation: number) => ["account-identities", accountId, generation] as const,
  identitiesOf: (accountId: string) => ["account-identities", accountId] as const,
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
// refreshFirstPage (refreshOrders) polls the newest page instead. The key starts with keys.orders
// so a mutation invalidates every filter.
export const ordersQuery = (customerId: string, status: OrderStatus) =>
  infiniteQueryOptions({
    queryKey: keys.ordersPage(customerId, status),
    queryFn: async ({ pageParam }) => orders.listOrders({ customerId, status, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
    staleTime: LIST_STALE_MS,
  });

// One poll tick of a paged list: fetch only the first page. Same rows in the same order = nothing
// moved, so just swap in the fresh rows (a status or a count changed) and keep the other pages.
// Anything else (a new row, one that left the filter) shifts the page boundaries, so refetch every
// loaded page. Older pages catch up on a remount, a mutation or window focus. Shared by the lists
// that would otherwise poll with refetchInterval (N loaded pages = N requests per tick): `fetchFirst`
// asks for page one, `ids` names the rows of a page.
export async function refreshFirstPage<R extends { nextPageToken: string }>(
  qc: QueryClient,
  key: QueryKey,
  fetchFirst: () => Promise<R>,
  ids: (page: R) => readonly string[],
) {
  const cur = qc.getQueryData<InfiniteData<R, string>>(key);
  if (cur === undefined) {
    // Nothing to compare with: the first load has not succeeded. If it ended in a transient error
    // (react-query's own retries ran out), ask again each tick, the list comes back by itself; an
    // answer ("no") stays as it is, asked once. Only a list on screen (type "active"): this runs from
    // the page's own interval, and a query nobody watches (the page left, the credential changed and
    // the page closed) is not woken up by it. The credential is read when the request is sent, and a
    // credential change already invalidates every query (components/credential.tsx).
    const st = qc.getQueryState(key);
    if (st?.status === "error" && st.fetchStatus === "idle" && isTransient(st.error)) {
      return qc.refetchQueries({ queryKey: key, exact: true, type: "active" });
    }
    return;
  }
  if (qc.isFetching({ queryKey: key }) > 0) return;
  const stamp = qc.getQueryState(key)?.dataUpdatedAt;
  const fresh = await fetchFirst();
  // This request lives outside the query, so a mutation's refetch cannot cancel it. If the
  // cache moved on while it was in flight (a refetch running, or one that already landed),
  // the response is older than what is shown: drop it, the next tick tries again.
  if (qc.isFetching({ queryKey: key }) > 0 || qc.getQueryState(key)?.dataUpdatedAt !== stamp) return;
  const old = cur.pages[0] === undefined ? [] : ids(cur.pages[0]);
  const now = ids(fresh);
  const same = now.length === old.length && now.every((id, i) => id === old[i]);
  if (!same) return qc.invalidateQueries({ queryKey: key });
  qc.setQueryData<InfiniteData<R, string>>(key, (d) =>
    d === undefined ? d : { ...d, pages: [fresh, ...d.pages.slice(1)] },
  );
}

export const refreshOrders = (qc: QueryClient, customerId: string, status: OrderStatus) =>
  refreshFirstPage(
    qc,
    keys.ordersPage(customerId, status),
    () => orders.listOrders({ customerId, status, pageSize: 20, pageToken: "" }),
    (p) => p.orders.map((o) => o.id),
  );

export const refreshStocks = (qc: QueryClient, skuPrefix: string, onlyReserved: boolean) =>
  refreshFirstPage(
    qc,
    keys.stocksPage(skuPrefix, onlyReserved),
    () => inventory.listStocks({ skuPrefix, onlyReserved, pageSize: 20, pageToken: "" }),
    (p) => p.stocks.map((s) => s.sku),
  );

export const refreshReservations = (qc: QueryClient, status: ReservationStatus) =>
  refreshFirstPage(
    qc,
    keys.reservations(status),
    () => inventory.listReservations({ status, pageSize: 20, pageToken: "" }),
    (p) => p.reservations.map((r) => r.orderId),
  );

// One order for /orders/$id; null = no such id. Three different "nothing to show" answers:
// undefined = not loaded yet (keep asking), null = there is no such order (stop: asking again
// finds nothing new), an order = it depends on the status. PENDING polls fast, the worker is about
// to answer. CONFIRMED can still turn CANCELLED without anyone acting: inventory expires the
// reservation. That is a slow poll, not "invalidate the order when the reservation changes": the
// reservation can say EXPIRED before the saga has cancelled the order, and the refetch would read
// the stale CONFIRMED and stop looking. COMPLETED and CANCELLED are final.
export function orderInterval(order: Order | null | undefined): number | false {
  if (order === undefined) return POLL_MS;
  if (order === null) return false;
  switch (order.status) {
    case OrderStatus.CONFIRMED:
      // An order waiting for its payment is watched at the fast pace: the customer is on a checkout page
      // in another tab and comes back to a result. The Pay card is shown for exactly this condition (the
      // page, routes/order.tsx), so the two end together when the order moves on.
      return order.paymentRequired ? POLL_MS : SLOW_POLL_MS;
    case OrderStatus.COMPLETED:
    case OrderStatus.CANCELLED:
      return false;
    default:
      return POLL_MS; // PENDING (and a status this build does not know yet)
  }
}

//
// Keyed by the credential generation (src/auth.ts) like the admin probe: another credential is another
// entry, so an order read for the previous credential is never shown to the new one, and a reply that
// arrives late lands in the entry of the credential it was asked for. The page's Pay card is decided by this
// data (CONFIRMED and payment_required), so it must be the current credential's.
export const orderQuery = (id: string, generation: number) =>
  queryOptions({
    queryKey: keys.orderOf(id, generation),
    queryFn: async () => {
      try {
        return (await orders.getOrder({ id })).order ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    refetchInterval: (q) => orderInterval(q.state.data),
  });

// How often one order's History card asks for the history: with the order, at the pace the order itself is
// polled (orderInterval), because a transition is what adds a row. A final order's history does not change
// any more, so it is not polled; the card fetches once more when the status it follows changes (HistoryCard),
// which is what brings the last row in. `order` undefined/null: the order is not loaded (or is missing); the
// card is not asking for anything then.
export function historyInterval(order: Order | null | undefined): number | false {
  if (!order) return false;
  return orderInterval(order);
}

// The transitions of one order for the History card. Keyed by the credential generation (src/auth.ts) like the
// payment and the admin probe: the answer carries actor_subject only when the caller is an admin, and it must
// never outlive the credential it was asked with. Another credential reads another entry, and a reply that
// arrives late is written under the key it was asked for, not into the new credential's entry.
// NOT_FOUND is an answer (no such order for this caller) and is told apart from a failure by the card.
export const historyQuery = (id: string, generation: number, order: Order | null | undefined) =>
  queryOptions({
    queryKey: keys.historyOf(id, generation),
    // asOf: the status of the order when this request was made. The history is read after that, so it holds every
    // transition up to at least that status; the card compares it with the order's status now to know whether the
    // snapshot it holds can still be missing a row (HistoryCard). The closure is this render's: a request keeps
    // the status it started with.
    queryFn: async () => ({ entries: (await orders.getOrderHistory({ id })).entries, asOf: order?.status }),
    refetchInterval: historyInterval(order),
  });

// How often one order's page asks for its reservation, given the reservation (undefined = not
// loaded yet, null = none yet) and the order's status (undefined = not loaded). RESERVED can still
// be released, committed or expired: slow poll. Any other status is final. No reservation yet
// is waited for at the fast pace while the order can still get one, including CONFIRMED (the saga
// confirms right after the reservation exists, so the page may be a beat ahead of it), and not at
// all once the order is final: a reservation that appears after COMPLETED / CANCELLED is a
// tombstone this page has no use for.
export function reservationInterval(
  reservation: Reservation | null | undefined,
  order: OrderStatus | undefined,
): number | false {
  if (reservation === undefined) return POLL_MS;
  if (reservation !== null) return reservation.status === ReservationStatus.RESERVED ? SLOW_POLL_MS : false;
  return order === OrderStatus.COMPLETED || order === OrderStatus.CANCELLED ? false : POLL_MS;
}

// null = the worker has not reserved anything for this order (yet). One order's page polls it
// on its own state alone; the orders list does not use this at all: its rows share one
// BatchGetReservations per tick (orderReservations.ts). `order` is the status of the order the
// page shows, when the page knows it (/orders/$id); without it (the pinned card on /reservations)
// a missing reservation is waited for.
export const reservationQuery = (orderId: string, order?: OrderStatus) =>
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
    refetchInterval: (q) => reservationInterval(q.state.data, order),
  });

// The payment of one order for the Pay card, null = none yet (NOT_FOUND). Polled while the card is
// mounted, which is exactly while the order waits for its payment. Keyed by the credential generation
// (src/auth.ts, like the admin probe): a result obtained for one credential is not in the entry the next
// credential reads, nothing it asks for lands in the previous one's, and a reply that arrives late is
// written under the key it was asked for. Resetting the card's own state is not enough, the cache outlives it.
export const paymentQuery = (orderId: string, generation: number) =>
  queryOptions({
    queryKey: keys.payment(generation, orderId),
    queryFn: async () => {
      try {
        return (await payments.getPayment({ orderId })).payment ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    refetchInterval: POLL_MS,
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
// as orders come and go, so the first page is polled (refreshStocks; not refetchInterval, which
// would refetch every loaded page each tick). Older pages catch up on a remount, a mutation or window
// focus. The key starts with keys.stocks so Set stock invalidates every filter.
export const stocksQuery = (skuPrefix: string, onlyReserved: boolean) =>
  infiniteQueryOptions({
    queryKey: keys.stocksPage(skuPrefix, onlyReserved),
    queryFn: async ({ pageParam }) => inventory.listStocks({ skuPrefix, onlyReserved, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
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

// Where each of these SKUs stands in the catalog, asked per SKU (QuotePrices: up to 100, a read
// open to every caller) and not by scanning ListProducts, so a SKU's answer never depends on
// which page of products it falls on. A SKU in neither list is an active product. The key starts
// with keys.products, so saving or deactivating a product refreshes it; it also polls, since the
// catalog changes behind the stock list's back, but rarely: once a minute is enough.
export const PRODUCT_STATUS_MAX = 100;
export const productStatusQuery = (skus: string[]) =>
  queryOptions({
    queryKey: keys.productStatus(skus),
    queryFn: async () => {
      const res = await catalog.quotePrices({ skus });
      return { missing: new Set(res.missing), inactive: new Set(res.inactive) };
    },
    enabled: skus.length > 0,
    refetchInterval: 60_000,
    staleTime: LIST_STALE_MS,
  });

// Reservations newest first, 20 per page, optionally one status (UNSPECIFIED =
// any); RESERVED rows can still be released by the worker, so the first page is polled
// (refreshReservations), like the stock list: the older pages are refetched on a remount, a mutation
// or window focus, not every tick.
export const reservationsQuery = (status: ReservationStatus) =>
  infiniteQueryOptions({
    queryKey: keys.reservations(status),
    queryFn: async ({ pageParam }) => inventory.listReservations({ status, pageSize: 20, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.nextPageToken === "" ? undefined : last.nextPageToken),
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
// is cached; a probe that could not be answered (5xx, network, timeout) throws, is retried by the
// shared policy (src/transient.ts: transient failures only, bounded), and is never cached as "not admin"
// (see AdminProbeNotice for what is left to the user).
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

// Who can sign in as one account: its bound external identities and its invites (any status), one
// response, no paging. Admin only on the server; only the admin account pages ask for it. The reply
// carries no secret (an invite has none), and the page shows only the fields it names.
//
// Keyed by the credential generation like the admin probe, and refused to run for any other one:
// the credential control invalidates every query the moment it changes the credential, before the
// admin gate has re-asked who the new credential is, so an active admin-only query would refetch
// with the new (possibly role-less) credential. The guard makes that refetch a silent no-op, and
// `enabled` (the caller passes the admin probe's answer for this generation) keeps the new key
// from running until the new credential is known to be admin.
export const accountIdentitiesQuery = (accountId: string, generation: number, enabled: boolean) =>
  queryOptions({
    queryKey: keys.identities(accountId, generation),
    queryFn: async () => {
      if (generation !== getCredentialGeneration()) throw new CancelledError({ silent: true });
      const res = await identity.listAccountIdentities({ accountId });
      return { identities: res.identities, invites: res.invites };
    },
    enabled,
    staleTime: LIST_STALE_MS,
  });

// Every key of one account, REVOKED included, newest first (one response, no paging).
export const apiKeysQuery = (accountId: string) =>
  queryOptions({
    queryKey: keys.apiKeys(accountId),
    queryFn: async () => (await identity.listApiKeys({ accountId })).apiKeys,
    staleTime: LIST_STALE_MS,
  });
