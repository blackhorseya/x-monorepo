import { Link, Outlet, createRootRoute, createRoute, createRouter, lazyRouteComponent, redirect } from "@tanstack/react-router";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import { useIsAdmin } from "./components/admin";
import { CredentialControl } from "./components/credential";
import { RouteError, RoutePending } from "./components/route-status";
import { ServerVersion } from "./components/version";
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
  { to: "/poison", label: "Poison" },
  { to: "/prune", label: "Prune" },
  { to: "/expire", label: "Expire" },
] as const;

// Layout: the nav shows Accounts only to a caller whose WhoAmI carries the admin role. That is
// a courtesy, not a gate: the server checks the role on every identity RPC.
function Root() {
  const isAdmin = useIsAdmin();
  const links = isAdmin ? [...navLinks, { to: "/accounts", label: "Accounts" } as const] : navLinks;
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
const poisonRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poison", component: lazyRouteComponent(() => import("./routes/poison"), "PoisonPage") });
const pruneRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/prune",
  component: lazyRouteComponent(() => import("./routes/prune"), "PrunePage"),
  validateSearch: (raw: Record<string, unknown>): PruneSearch => ({
    bc: typeof raw.bc === "string" && raw.bc !== "" ? raw.bc : undefined,
    days: typeof raw.days === "number" && Number.isFinite(raw.days) && raw.days > 0 ? raw.days : undefined,
    forget: typeof raw.forget === "string" && raw.forget !== "" ? raw.forget : undefined,
  }),
});

const expireRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/expire",
  component: lazyRouteComponent(() => import("./routes/expire"), "ExpirePage"),
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
