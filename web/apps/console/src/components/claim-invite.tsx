import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ConnectError } from "@connectrpc/connect";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { describe, identity } from "../client";
import { clearUnbound } from "../oidc/unbound";

// What each refusal of ClaimInvite means to the person holding the token (the codes are the
// contract of IdentityService.ClaimInvite). The server's own message follows the code.
const hints: Record<string, string> = {
  "invite.not_found": "There is no invite for the email on your sign-in. Ask an administrator to invite that address.",
  "invite.expired": "The invite has expired (invites last 7 days). Ask an administrator for a new one.",
  "invite.already_claimed": "That invite was already used. Ask an administrator for a new one.",
  "invite.email_unverified": "Your identity provider has not verified your email address. Verify it there, then try again.",
  "invite.email_mismatch": "The email on your sign-in is not the one that was invited. Sign in with the invited address.",
  "identity.already_bound": "Your sign-in is already linked to an account. Sign out and in again; if this persists, ask an administrator.",
};

// Shown instead of the pages for a signed-in identity the server has no account for. ClaimInvite
// takes no arguments: the server reads issuer, subject and email from the token it verified, so
// there is nothing here to type and nothing the client could claim falsely.
export function ClaimInvite() {
  const qc = useQueryClient();
  const claim = useMutation({
    mutationFn: () => identity.claimInvite({}),
    onSuccess: () => {
      clearUnbound();
      return qc.invalidateQueries();
    },
  });
  const code = claim.error ? (ConnectError.from(claim.error).metadata.get("x-error-code") ?? "") : "";
  return (
    <Card className="mx-auto max-w-lg">
      <CardHeader>
        <CardTitle>
          <h2>Link your account</h2>
        </CardTitle>
        <CardDescription>
          You are signed in, but your sign-in is not linked to an account yet. If an administrator invited your email address,
          claim the invite to link it.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {claim.isError && (
          <Alert variant="destructive">
            <AlertTitle>Could not claim the invite</AlertTitle>
            <AlertDescription>
              {describe(claim.error)}
              {hints[code] && <p className="mt-1">{hints[code]}</p>}
            </AlertDescription>
          </Alert>
        )}
        <Button className="justify-self-start" disabled={claim.isPending} onClick={() => claim.mutate()}>
          Claim invite
        </Button>
      </CardContent>
    </Card>
  );
}
