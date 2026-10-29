import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useCredentialGeneration } from "../auth";
import { adminProbeQuery } from "../queries";
import { ErrorAlert } from "./error-alert";

// Does the current credential pass the server's admin check? Asked of the server (see
// adminProbeQuery), so it follows auth.admin_role and auth.mode as configured. false until the
// answer for this very credential is in, so the nav entry never flashes in and a late answer for
// the previous credential cannot open it. A courtesy, not the gate: every identity RPC is still
// checked by the server.
function useAdminProbe() {
  return useQuery(adminProbeQuery(useCredentialGeneration()));
}

export function useIsAdmin(): boolean {
  return useAdminProbe().data === true;
}

// The probe could not be answered (it is already retried with backoff): say so, instead of
// hiding the admin UI as if the caller were known not to be an admin, and let the user ask again.
export function AdminProbeNotice() {
  const probe = useAdminProbe();
  if (!probe.isError) return null;
  return (
    <div role="status" className="mb-4 flex flex-wrap items-center gap-3 rounded-md border px-3 py-2 text-sm text-muted-foreground">
      <span>Could not confirm admin access; admin features stay hidden until it is.</span>
      <Button variant="outline" size="sm" onClick={() => void probe.refetch()} disabled={probe.isFetching}>
        Retry
      </Button>
    </div>
  );
}

// Page-level gate for the admin pages: children only render for an admin.
export function AdminGate({ children }: { children: ReactNode }) {
  const probe = useAdminProbe();
  if (probe.isError) return <ErrorAlert error={probe.error} />;
  if (probe.isPending) return null;
  if (!probe.data) {
    return (
      <Alert>
        <AlertTitle>Admin role required</AlertTitle>
        <AlertDescription>
          This page needs the server&apos;s admin role (<code>auth.admin_role</code>). Set a credential that has it with the
          Credential button above.
        </AlertDescription>
      </Alert>
    );
  }
  return children;
}
