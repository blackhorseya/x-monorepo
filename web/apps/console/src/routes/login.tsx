import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Field } from "../components/field";
import { getConfig } from "../oidc/config";
import { refreshNow, safeReturnTo, signInWithApiKey, startLogin, useSessionState } from "../oidc/session";

export type LoginSearch = { returnTo?: string };

// /login: OIDC is the way in; an API key (break-glass) is offered below it unless the deployment
// turned it off (console.apiKeyLogin=false), in which case it is not rendered at all.
export function LoginPage() {
  const { oidc, apiKeyLogin } = getConfig();
  const { returnTo } = useSearch({ from: "/login" });
  const { error, unreachable } = useSessionState();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  return (
    <div className="mx-auto grid max-w-md gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Sign in</CardTitle>
          {oidc && <CardDescription>You are sent to your organisation&apos;s identity provider and come back signed in.</CardDescription>}
        </CardHeader>
        <CardContent className="grid gap-4">
          {error && (
            <Alert variant="destructive">
              <AlertTitle>Sign-in failed</AlertTitle>
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {unreachable && (
            <div role="status" className="grid gap-2 rounded-md border px-3 py-2 text-sm">
              <span>
                The identity provider could not be reached. Your session is kept and the console keeps trying; you can try now.
              </span>
              <Button
                variant="outline"
                size="sm"
                className="justify-self-start"
                disabled={busy}
                onClick={() => {
                  setBusy(true);
                  void refreshNow()
                    .then((r) => {
                      if (r === "ok") void navigate({ to: safeReturnTo(returnTo), replace: true });
                    })
                    .finally(() => setBusy(false));
                }}
              >
                Retry
              </Button>
            </div>
          )}
          {oidc && (
            <Button
              disabled={busy}
              onClick={() => {
                setBusy(true);
                void startLogin(safeReturnTo(returnTo)).finally(() => setBusy(false));
              }}
            >
              Sign in
            </Button>
          )}
        </CardContent>
      </Card>
      {apiKeyLogin && <ApiKeyCard returnTo={safeReturnTo(returnTo)} />}
    </div>
  );
}

function ApiKeyCard({ returnTo }: { returnTo: string }) {
  const navigate = useNavigate();
  const [value, setValue] = useState("");
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Use an API key</CardTitle>
        <CardDescription>Break-glass access for machine accounts and when the identity provider is down.</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            signInWithApiKey(value);
            setValue("");
            void navigate({ to: returnTo, replace: true });
          }}
        >
          <Field label="API key" className="min-w-48 flex-1">
            <Input type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} />
          </Field>
          <Button type="submit" variant="outline" disabled={!value.trim()}>
            Use API key
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}
