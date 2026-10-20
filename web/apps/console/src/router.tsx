import { Link, Outlet, createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { CredentialControl } from "./components/credential";
import { OrdersPage } from "./routes/orders";
import { PoisonPage } from "./routes/poison";
import { StockPage } from "./routes/stock";

const rootRoute = createRootRoute({
  component: () => (
    <div className="mx-auto max-w-5xl p-4 md:p-8">
      <header className="mb-6 flex flex-wrap items-baseline gap-6">
        <h1 className="text-xl font-semibold">x-monorepo console</h1>
        <nav className="flex gap-4 text-sm text-muted-foreground">
          {[
            { to: "/orders", label: "Orders" },
            { to: "/stock", label: "Stock" },
            { to: "/poison", label: "Poison" },
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

const ordersRoute = createRoute({ getParentRoute: () => rootRoute, path: "/orders", component: OrdersPage });
const stockRoute = createRoute({ getParentRoute: () => rootRoute, path: "/stock", component: StockPage });
const poisonRoute = createRoute({ getParentRoute: () => rootRoute, path: "/poison", component: PoisonPage });

export const router = createRouter({
  routeTree: rootRoute.addChildren([indexRoute, ordersRoute, stockRoute, poisonRoute]),
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
