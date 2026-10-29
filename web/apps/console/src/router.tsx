import { Link, Outlet, createRootRoute, createRoute, createRouter, lazyRouteComponent, redirect } from "@tanstack/react-router";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import { AdminGate, AdminProbeNotice, useIsAdmin } from "./components/admin";
import { getCredentialGeneration } from "./auth";
import { CredentialControl } from "./components/credential";
import { RouteError, RoutePending } from "./components/route-status";
import { ServerVersion } from "./components/version";
import { queryClient } from "./queryClient";
import { adminProbeQuery } from "./queries";
import type { ExpireSearch } from "./routes/expire";
import type { OrdersSearch } from "./routes/orders";
import type { PruneSearch } from "./routes/prune";
import type { ReservationsSearch } from "./routes/reservations";
import type { StockSearch } from "./routes/stock";

// Every page is its own chunk (lazyRouteComponent), fetched on first visit or on link intent
// (defaultPreload). The search types above are type-only imports, so they cost nothing at runtime.

const navLinks = [
  { to: "/orders", label: "Orders" },
  { to: "/reservations", label: "Reservations" },
  { to: "/stock", label: "Stock" },
] as const;

// The pages whose RPCs the server answers to the admin role only.
const adminLinks = [
  { to: "/poison", label: "Poison" },
  { to: "/prune", label: "Prune" },
  { to: "/expire", label: "Expire" },
  { to: "/accounts", label: "Accounts" },
] as const;

// An admin page behind AdminGate: for anyone else the page never mounts, so it asks the server
// nothing and shows the gate instead. Its chunk is not fetched either: the router calls a route
// component's preload before anything renders (direct load, link intent, navigation), so preload
// only reaches the page's own while the admin probe has already answered true for this
// credential. Until then the chunk is fetched when the page first renders, behind the gate.
type LazyPage = ReturnType<typeof lazyRouteComponent>;
const gated = (Page: LazyPage): LazyPage => {
  const Gated = () => (
    <AdminGate>
      <Page />
    </AdminGate>
  );
  Gated.preload = async () => {
    const known = queryClient.getQueryData(adminProbeQuery(getCredentialGeneration()).queryKey);
    if (known === true) await Page.preload?.();
  };
  return Gated;
};

// Layout: the nav shows the admin pages only to a caller the server's admin check lets through
// (with auth.mode=off that is everyone, as on the server). That is a courtesy, not a gate: the
// server checks the role on every admin RPC.
function Root() {
  const isAdmin = useIsAdmin();
  const links = isAdmin ? [...navLinks, ...adminLinks] : navLinks;
  return (
    <div className="mx-auto max-w-5xl p-4 md:p-8">
      <header className="mb-6 flex flex-wrap items-baseline gap-6">
        <h1 className="text-xl font-semibold">x-monorepo console</h1>
        <nav className="flex gap-4 text-sm text-muted-foreground">
          {links.map((l) => (
            <Link key={l.to} to={l.to} activeProps={{ className: "text-foreground font-medium" }}>
              {l.label}
            </Link>
          ))}
        </nav>
        <CredentialControl />
      </header>
      <AdminProbeNotice />
      <Outlet />
      <footer className="mt-10 flex justify-end text-xs text-muted-foreground">
        <ServerVersion />
      </footer>
    </div>
  );
}

const rootRoute = createRootRoute({ component: Root });

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/orders" });
  },
});

const ordersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/orders",
  component: lazyRouteComponent(() => import("./routes/orders"), "OrdersPage"),
  validateSearch: (raw: Record<string, unknown>): OrdersSearch => ({
    status: typeof raw.status === "string" && raw.status in OrderStatus ? (raw.status as OrdersSearch["status"]) : undefined,
    customer: typeof raw.customer === "string" && raw.customer !== "" ? raw.customer : undefined,
  }),
});
// A sibling of /orders, not a child: OrdersPage renders no Outlet, and the nav's
// activeProps on /orders still light up here (prefix match).
const orderRoute = createRoute({ getParentRoute: () => rootRoute, path: "/orders/$id", component: lazyRouteComponent(() => import("./routes/order"), "OrderPage") });
const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/reservations",
  component: lazyRouteComponent(() => import("./routes/reservations"), "ReservationsPage"),
  validateSearch: (raw: Record<string, unknown>): ReservationsSearch => ({
    status: typeof raw.status === "string" && raw.status in ReservationStatus ? (raw.status as ReservationsSearch["status"]) : undefined,
    order: typeof raw.order === "string" && raw.order !== "" ? raw.order : undefined,
  }),
});
const stockRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/stock",
  component: lazyRouteComponent(() => import("./routes/stock"), "StockPage"),
  validateSearch: (raw: Record<string, unknown>): StockSearch => ({
    prefix: typeof raw.prefix === "string" && raw.prefix !== "" ? raw.prefix : undefined,
    reserved: raw.reserved === true ? true : undefined,
  }),
});
const accountsRoute = createRoute({ getParentRoute: () => rootRoute, path: "/accounts", component: lazyRouteComponent(() => import("./routes/accounts"), "AccountsPage") });
// A sibling of /accounts, like /orders/$id: the nav's prefix match lights Accounts up here too.
const accountRoute = createRoute({ getParentRoute: () => rootRoute, path: "/accounts/$id", component: lazyRouteComponent(() => import("./routes/account"), "AccountPage") });
const poisonRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poison", component: gated(lazyRouteComponent(() => import("./routes/poison"), "PoisonPage")) });
const pruneRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/prune",
  component: gated(lazyRouteComponent(() => import("./routes/prune"), "PrunePage")),
  validateSearch: (raw: Record<string, unknown>): PruneSearch => ({
    bc: typeof raw.bc === "string" && raw.bc !== "" ? raw.bc : undefined,
    days: typeof raw.days === "number" && Number.isFinite(raw.days) && raw.days > 0 ? raw.days : undefined,
    forget: typeof raw.forget === "string" && raw.forget !== "" ? raw.forget : undefined,
  }),
});

const expireRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/expire",
  component: gated(lazyRouteComponent(() => import("./routes/expire"), "ExpirePage")),
  validateSearch: (raw: Record<string, unknown>): ExpireSearch => ({
    hours: typeof raw.hours === "number" && Number.isFinite(raw.hours) && raw.hours > 0 ? raw.hours : undefined,
  }),
});

export const router = createRouter({
  routeTree: rootRoute.addChildren([indexRoute, ordersRoute, orderRoute, reservationsRoute, stockRoute, accountsRoute, accountRoute, poisonRoute, pruneRoute, expireRoute]),
  defaultPreload: "intent",
  defaultPendingComponent: RoutePending,
  defaultErrorComponent: RouteError,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
