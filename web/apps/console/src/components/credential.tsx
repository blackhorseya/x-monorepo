import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { KeyRound, LogOut } from "lucide-react";
import { useState } from "react";
import { flushSync } from "react-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { setCredential, subject, useCredential, type Credential } from "../auth";
import { getConfig } from "../oidc/config";
import { signOut } from "../oidc/session";

// Header control: who the console calls as. Set an API key or a JWT, or clear
// it to go anonymous. Every query is reset on the spot so an UNAUTHENTICATED /
// PERMISSION_DENIED page recovers (or appears) at once instead of on its next
// poll — the slowest one (poison) is 5s.
//
// Cancel + reset, not invalidate (#483). Most query keys (["orders", customer, status], ["quote", ...skus])
// do not name the credential, so what is cached under a key was read for whoever was calling when it was
// asked. invalidateQueries only marks it stale and refetches: a query with no data and a request in flight
// (the first, anonymous one) is not restarted, the new call is folded into the old one and its answer, a
// 401 for the previous credential, becomes the final state while the new credential's request is never sent;
// a query with data keeps showing the previous credential's rows until the new answer replaces them.
// cancelQueries drops what is in flight (its answer is discarded when it arrives, a retry waiting out its
// backoff never fires) and resetQueries sends every query back to "not loaded", so nothing read for the
// previous credential is shown or can land in the cache, and the queries on screen ask again at once.
// One call for every query, instead of adding the credential generation to each key by hand: a new query
// cannot forget it (the keys that already carry a generation, admin probe / payment / history, are simply
// reset too).
// The change is flushed to the screen first (flushSync): the admin pages sit behind AdminGate, which closes
// the moment the credential changes (its probe has no answer for the new one yet) and unmounts their
// queries. Resetting in the same tick, before that render, would refetch every still-mounted admin-only
// query with the NEW credential: a role-less key, or no key at all, asking for something only an admin may.
function changeCredential(qc: QueryClient, change: () => void): void {
  flushSync(change);
  void qc.cancelQueries();
  void qc.resetQueries();
}

export function CredentialControl() {
  // With OIDC on, credentials come from /login and there is nothing to paste here.
  return getConfig().oidc ? <SessionControl /> : <PasteCredentialControl />;
}

// Who the console calls as, and the way out. Signing out clears the local session; the way back in
// is /login (the router sends every page there once there is no credential).
function SessionControl() {
  const qc = useQueryClient();
  const current = useCredential();
  return (
    <div className="ml-auto flex flex-wrap items-center gap-2 text-sm">
      <span className="text-muted-foreground">{subject(current)}</span>
      {current && (
        <Button
          variant="ghost"
          size="sm"
          onClick={() => changeCredential(qc, () => void signOut())}
        >
          <LogOut /> Sign out
        </Button>
      )}
    </div>
  );
}

function PasteCredentialControl() {
  const qc = useQueryClient();
  const current = useCredential();
  const [open, setOpen] = useState(false);
  const [kind, setKind] = useState<Credential["kind"]>(current?.kind ?? "api_key");
  const [value, setValue] = useState("");

  const apply = (c: Credential | null) => changeCredential(qc, () => setCredential(c));
  const save = () => {
    apply({ kind, value });
    setValue("");
    setOpen(false);
  };

  return (
    <div className="ml-auto flex flex-wrap items-center gap-2 text-sm">
      <span className="text-muted-foreground">{subject(current)}</span>
      <Button variant="outline" size="sm" onClick={() => setOpen(!open)} aria-expanded={open}>
        <KeyRound /> Credential
      </Button>
      {current && (
        <Button variant="ghost" size="sm" onClick={() => apply(null)} title="Call anonymously">
          <LogOut /> Clear
        </Button>
      )}
      {open && (
        <form
          className="flex w-full flex-wrap items-end gap-2 rounded-md border p-3"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          <div className="grid gap-1">
            <Label htmlFor="credential-kind">Kind</Label>
            <select
              id="credential-kind"
              className="h-9 rounded-md border border-input bg-transparent px-2 text-sm"
              value={kind}
              onChange={(e) => setKind(e.target.value as Credential["kind"])}
            >
              <option value="api_key">API key (X-API-Key)</option>
              <option value="bearer">JWT (Authorization: Bearer)</option>
            </select>
          </div>
          <div className="grid min-w-64 flex-1 gap-1">
            <Label htmlFor="credential-value">Value</Label>
            <Input
              id="credential-value"
              type="password"
              autoComplete="off"
              placeholder={kind === "api_key" ? "the key from auth.api_keys" : "eyJ…"}
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          </div>
          <Button type="submit" size="sm" disabled={!value.trim()}>
            Use
          </Button>
        </form>
      )}
    </div>
  );
}
