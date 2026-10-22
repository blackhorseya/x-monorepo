import { Link, Outlet, createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import { CredentialControl } from "./components/credential";
import { OrdersPage, type OrdersSearch } from "./routes/orders";
import { PoisonPage } from "./routes/poison";
import { PrunePage } from "./routes/prune";
import { ReservationsPage, type ReservationsSearch } from "./routes/reservations";
import { StockPage } from "./routes/stock";

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
          ].map((l) => (
            <Link key={l.to} to={l.to} activeProps={{ className: "text-foreground font-medium" }}>
              {l.label}
            </Link>
          ))}
        </nav>
        <CredentialControl />
      </header>
      <Outlet />
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
const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/reservations",
  component: ReservationsPage,
  validateSearch: (raw: Record<string, unknown>): ReservationsSearch => ({
    status: typeof raw.status === "string" && raw.status in ReservationStatus ? (raw.status as ReservationsSearch["status"]) : undefined,
    order: typeof raw.order === "string" && raw.order !== "" ? raw.order : undefined,
  }),
});
const stockRoute = createRoute({ getParentRoute: () => rootRoute, path: "/stock", component: StockPage });
const poisonRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poison", component: PoisonPage });
const pruneRoute = createRoute({ getParentRoute: () => rootRoute, path: "/prune", component: PrunePage });

export const router = createRouter({
  routeTree: rootRoute.addChildren([indexRoute, ordersRoute, reservationsRoute, stockRoute, poisonRoute, pruneRoute]),
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
