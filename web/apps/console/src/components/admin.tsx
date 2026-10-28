import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { whoamiQuery } from "../queries";
import { ErrorAlert } from "../routes/orders";

// The role the server's admin RPCs want: auth.admin_role, default "admin". A hint for the UI
// only — the server stays the gate, and answers auth.permission_denied for anything else.
const ADMIN_ROLE = "admin";

// Does the current credential carry the admin role? false while WhoAmI is loading or failed, so
// the nav entry never flashes in. An anonymous caller has no roles: with auth off the server
// would let it through, but the console cannot tell that from auth.mode=optional.
export function useIsAdmin(): boolean {
  const { data } = useQuery(whoamiQuery);
  return data !== undefined && !data.anonymous && data.roles.includes(ADMIN_ROLE);
}

// Page-level gate for the admin pages: children only render for an admin.
export function AdminGate({ children }: { children: ReactNode }) {
  const who = useQuery(whoamiQuery);
  const isAdmin = useIsAdmin();
  if (who.isError) return <ErrorAlert error={who.error} />;
  if (who.isPending) return null;
  if (!isAdmin) {
    return (
      <Alert>
        <AlertTitle>Admin role required</AlertTitle>
        <AlertDescription>
          This page needs the <code>{ADMIN_ROLE}</code> role. Set a credential that has it with the Credential button above.
        </AlertDescription>
      </Alert>
    );
  }
  return children;
}
