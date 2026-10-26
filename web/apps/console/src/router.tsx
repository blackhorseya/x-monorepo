import { Link, Outlet, createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import { CredentialControl } from "./components/credential";
import { ServerVersion } from "./components/version";
import { ExpirePage, type ExpireSearch } from "./routes/expire";
import { OrderPage } from "./routes/order";
import { OrdersPage, type OrdersSearch } from "./routes/orders";
import { PoisonPage } from "./routes/poison";
import { PrunePage, type PruneSearch } from "./routes/prune";
import { ReservationsPage, type ReservationsSearch } from "./routes/reservations";
import { StockPage, type StockSearch } from "./routes/stock";

const rootRoute = createRootRoute({
  component: () => (
    <div className="mx-auto max-w-5xl p-4 md:p-8">
      <header className="mb-6 flex flex-wrap items-baseline gap-6">
        <h1 className="text-xl font-semibold">x-monorepo console</h1>
        <nav className="flex gap-4 text-sm text-muted-foreground">
          {[
            { to: "/orders", label: "Orders" },
            { to: "/reservations", label: "Reservations" },
            { to: "/stock", label: "Stock" },
            { to: "/poison", label: "Poison" },
            { to: "/prune", label: "Prune" },
            { to: "/expire", label: "Expire" },
          ].map((l) => (
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
  ),
});

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
  component: OrdersPage,
  validateSearch: (raw: Record<string, unknown>): OrdersSearch => ({
    status: typeof raw.status === "string" && raw.status in OrderStatus ? (raw.status as OrdersSearch["status"]) : undefined,
    customer: typeof raw.customer === "string" && raw.customer !== "" ? raw.customer : undefined,
  }),
});
// A sibling of /orders, not a child: OrdersPage renders no Outlet, and the nav's
// activeProps on /orders still light up here (prefix match).
const orderRoute = createRoute({ getParentRoute: () => rootRoute, path: "/orders/$id", component: OrderPage });
const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/reservations",
  component: ReservationsPage,
  validateSearch: (raw: Record<string, unknown>): ReservationsSearch => ({
    status: typeof raw.status === "string" && raw.status in ReservationStatus ? (raw.status as ReservationsSearch["status"]) : undefined,
    order: typeof raw.order === "string" && raw.order !== "" ? raw.order : undefined,
  }),
});
const stockRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/stock",
  component: StockPage,
  validateSearch: (raw: Record<string, unknown>): StockSearch => ({
    prefix: typeof raw.prefix === "string" && raw.prefix !== "" ? raw.prefix : undefined,
    reserved: raw.reserved === true ? true : undefined,
  }),
});
const poisonRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poison", component: PoisonPage });
const pruneRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/prune",
  component: PrunePage,
  validateSearch: (raw: Record<string, unknown>): PruneSearch => ({
    bc: typeof raw.bc === "string" && raw.bc !== "" ? raw.bc : undefined,
    days: typeof raw.days === "number" && Number.isFinite(raw.days) && raw.days > 0 ? raw.days : undefined,
    forget: typeof raw.forget === "string" && raw.forget !== "" ? raw.forget : undefined,
  }),
});

const expireRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/expire",
  component: ExpirePage,
  validateSearch: (raw: Record<string, unknown>): ExpireSearch => ({
    hours: typeof raw.hours === "number" && Number.isFinite(raw.hours) && raw.hours > 0 ? raw.hours : undefined,
  }),
});

export const router = createRouter({
  routeTree: rootRoute.addChildren([indexRoute, ordersRoute, orderRoute, reservationsRoute, stockRoute, poisonRoute, pruneRoute, expireRoute]),
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
