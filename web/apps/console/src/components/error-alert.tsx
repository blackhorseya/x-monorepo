import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { describe, isUnauthenticated } from "../client";

export function ErrorAlert({ error }: { error: unknown }) {
  const unauthenticated = isUnauthenticated(error);
  return (
    <Alert variant="destructive">
      <AlertTitle>{unauthenticated ? "Not authenticated" : "Request failed"}</AlertTitle>
      <AlertDescription>
        {describe(error)}
        {unauthenticated && " — the server runs with auth on; set an API key or JWT with the Credential button above."}
      </AlertDescription>
    </Alert>
  );
}
