import { Link, Outlet, createRootRoute, createRoute, createRouter, lazyRouteComponent, redirect } from "@tanstack/react-router";
import { AdminGate, AdminProbeNotice, useIsAdmin } from "./components/admin";
import { getCredentialGeneration } from "./auth";
import { CredentialControl } from "./components/credential";
import { RouteError, RoutePending } from "./components/route-status";
import { ServerVersion } from "./components/version";
import { queryClient } from "./queryClient";
import { adminProbeQuery } from "./queries";
import type { PruneSearch } from "./routes/prune";

// Every page is its own chunk (lazyRouteComponent), fetched on first visit or on link intent
// (defaultPreload). The search type above is a type-only import, so it costs nothing at runtime.

// The pages whose RPCs the server answers to the admin role only.
const adminLinks = [
  { to: "/poison", label: "Poison" },
  { to: "/prune", label: "Prune" },
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
  const links = isAdmin ? adminLinks : [];
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
    throw redirect({ to: "/poison" });
  },
});

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

export const router = createRouter({
  routeTree: rootRoute.addChildren([indexRoute, poisonRoute, pruneRoute]),
  defaultPreload: "intent",
  defaultPendingComponent: RoutePending,
  defaultErrorComponent: RouteError,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
