import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useCredentialGeneration } from "../auth";
import { adminProbeQuery } from "../queries";
import { ErrorAlert } from "../routes/orders";

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
