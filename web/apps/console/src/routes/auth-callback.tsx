import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { completeLogin, useSessionState } from "../oidc/session";

// /auth/callback: the redirect URI the IdP sends the browser back to. It trades the single-use
// code for tokens (completeLogin: state check, PKCE verifier, nonce), then moves on to where the
// user was going; the code and state are gone from the URL by then.
export function AuthCallbackPage() {
  const navigate = useNavigate();
  const { error } = useSessionState();
  useEffect(() => {
    let live = true;
    completeLogin(window.location.search).then(
      (returnTo) => live && void navigate({ to: returnTo, replace: true }),
      () => undefined, // shown from the session state below
    );
    return () => {
      live = false;
    };
  }, [navigate]);
  if (!error) return <p role="status" className="text-sm text-muted-foreground">Signing you in…</p>;
  return (
    <Alert variant="destructive">
      <AlertTitle>Sign-in failed</AlertTitle>
      <AlertDescription>
        {error}{" "}
        <Link to="/login" className="underline">
          Back to sign in
        </Link>
      </AlertDescription>
    </Alert>
  );
}
