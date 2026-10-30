import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { useEffect, useState } from "react";
import { AccountStatus, ApiKeyStatus, InviteStatus, type Account, type ApiKey, type Invite } from "@repo/proto/identity/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { AdminGate, useIsAdmin } from "../components/admin";
import { ConfirmButton } from "../components/confirm";
import { useCredentialGeneration, useResetOnCredentialChange } from "../auth";
import { identity } from "../client";
import { accountIdentitiesQuery, accountQuery, apiKeysQuery, keys } from "../queries";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";
import { AccountToggle, accountBadge, at, parseRoles } from "./accounts";

// /accounts/$id: one account — its roles, and its API keys. The key list shows a prefix and a
// status, never a secret: the server keeps only a hash. The secret exists once, in the dialog
// that CreateApiKey opens, and nowhere else (see NewKeyBody).
export function AccountPage() {
  const { id } = useParams({ from: "/accounts/$id" });
  return (
    <AdminGate>
      <div className="grid gap-6">
        <Link to="/accounts" className="justify-self-start text-sm text-muted-foreground underline">
          ← All accounts
        </Link>
        <AccountCard id={id} />
        <InviteCard accountId={id} />
        <BindingsCard accountId={id} />
        <SessionsCard accountId={id} />
        <KeysCard accountId={id} />
      </div>
    </AdminGate>
  );
}

function AccountCard({ id }: { id: string }) {
  const { data, isError, error } = useQuery(accountQuery(id));
  return (
    <Card>
      {isError && (
        <CardContent>
          <ErrorAlert error={error} />
        </CardContent>
      )}
      {data === null && (
        <CardContent>
          <p className="text-sm text-muted-foreground">No account with this id.</p>
        </CardContent>
      )}
      {data && <AccountDetail account={data} />}
    </Card>
  );
}

function AccountDetail({ account: a }: { account: Account }) {
  const qc = useQueryClient();
  const [roles, setRoles] = useState(a.roles.join(", "));
  // Follow the server (another tab, a refetch) unless the user is mid-edit on the same value.
  useEffect(() => setRoles(a.roles.join(", ")), [a.roles]);
  const assign = useMutation({
    mutationFn: () => identity.assignRoles({ id: a.id, roles: parseRoles(roles) }),
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: keys.account(a.id) }), qc.invalidateQueries({ queryKey: keys.accounts })]),
  });
  useResetOnCredentialChange(assign);

  return (
    <>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-3">
          {a.name}
          <Badge variant={accountBadge[a.status]}>{AccountStatus[a.status]}</Badge>
        </CardTitle>
        <CardDescription>
          <span className="font-mono text-xs">{a.id}</span> · created {at(a.createTime)}
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <p className="text-sm">
          Roles: {a.roles.length === 0 ? <span className="text-muted-foreground">none</span> : <strong>{a.roles.join(", ")}</strong>}
        </p>
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            assign.mutate();
          }}
        >
          <Field label="Roles" className="w-64">
            <Input value={roles} placeholder="comma separated; empty clears" onChange={(e) => setRoles(e.target.value)} />
          </Field>
          <Button type="submit" variant="outline" disabled={assign.isPending}>
            Save roles
          </Button>
        </form>
        <div className="flex items-start gap-2">
          <AccountToggle account={a} />
        </div>
        <p className="text-xs text-muted-foreground">Saving replaces the whole set. A change reaches existing keys within 30s.</p>
        {assign.isError && <ErrorAlert error={assign.error} />}
      </CardContent>
    </>
  );
}

const keyBadge: Record<ApiKeyStatus, "default" | "secondary" | "destructive" | "outline"> = {
  [ApiKeyStatus.UNSPECIFIED]: "outline",
  [ApiKeyStatus.ACTIVE]: "default",
  [ApiKeyStatus.REVOKED]: "destructive",
};

function KeysCard({ accountId }: { accountId: string }) {
  const qc = useQueryClient();
  const list = useQuery(apiKeysQuery(accountId));
  const [creating, setCreating] = useState(false);
  const revoke = useMutation({
    mutationFn: (id: string) => identity.revokeApiKey({ id }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.apiKeys(accountId) }),
  });
  useResetOnCredentialChange(revoke);
  const rows = list.data ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>API keys</CardTitle>
        <CardDescription>
          Newest first, revoked ones included. A revoked key stops working within 30s. The secret is shown once, when the key is
          created. Last used is best-effort and can trail the real
          last use by a couple of minutes.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <Button className="justify-self-start" onClick={() => setCreating(true)}>
          New API key
        </Button>
        <Dialog open={creating} onOpenChange={setCreating}>
          <DialogContent
            // The secret cannot be fetched again: only the Done button closes it, not a stray click.
            onInteractOutside={(e) => e.preventDefault()}
            onEscapeKeyDown={(e) => e.preventDefault()}
          >
            <NewKeyBody accountId={accountId} onDone={() => setCreating(false)} />
          </DialogContent>
        </Dialog>
        {list.isError && <ErrorAlert error={list.error} />}
        {revoke.isError && <ErrorAlert error={revoke.error} />}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Prefix</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead>Last used</TableHead>
              <TableHead>Created</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((k) => (
              <KeyRow key={k.id} apiKey={k} onRevoke={() => revoke.mutateAsync(k.id)} />
            ))}
            {list.isSuccess && rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={7} className="text-center text-muted-foreground">
                  No API keys yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}

function KeyRow({ apiKey: k, onRevoke }: { apiKey: ApiKey; onRevoke: () => Promise<unknown> }) {
  return (
    <TableRow>
      <TableCell>{k.name}</TableCell>
      <TableCell className="font-mono text-xs">{k.prefix}…</TableCell>
      <TableCell>
        <Badge variant={keyBadge[k.status]}>{ApiKeyStatus[k.status]}</Badge>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{k.expireTime ? at(k.expireTime) : "never"}</TableCell>
      <TableCell className="text-xs text-muted-foreground">{k.lastUseTime ? at(k.lastUseTime) : "—"}</TableCell>
      <TableCell className="text-xs text-muted-foreground">{at(k.createTime)}</TableCell>
      <TableCell className="text-right">
        {k.status === ApiKeyStatus.ACTIVE && (
          <ConfirmButton
            label="Revoke"
            title={`Revoke ${k.name}?`}
            description="Anything using this key is locked out within 30s. This cannot be undone."
            action="Revoke key"
            onConfirm={onRevoke}
          />
        )}
      </TableCell>
    </TableRow>
  );
}

// The body of the "New API key" dialog, in two steps: the form, then the secret.
//
// The secret lives in this component's state and nowhere else: not in the mutation's result
// (mutationFn hands it to setSecret and returns only the key, and gcTime 0 drops the mutation
// the moment it is unobserved), not in the URL, not in web storage, not in a log. Radix unmounts
// the dialog content on close, so Done discards it with the component.
function NewKeyBody({ accountId, onDone }: { accountId: string; onDone: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [expires, setExpires] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const [copied, setCopied] = useState<"yes" | "failed" | null>(null);

  const create = useMutation({
    gcTime: 0,
    mutationFn: async () => {
      const res = await identity.createApiKey({
        accountId,
        name: name.trim(),
        // End of the chosen day, local time; empty = never expires.
        expireTime: expires === "" ? undefined : timestampFromDate(new Date(`${expires}T23:59:59`)),
      });
      setSecret(res.secret);
      return res.apiKey;
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.apiKeys(accountId) }),
  });
  useResetOnCredentialChange(create);

  if (secret !== null) {
    return (
      <>
        <DialogHeader>
          <DialogTitle>Copy your API key</DialogTitle>
          <DialogDescription>
            This is the only time the secret is shown. The console does not keep it and the server stores only a hash, so a lost key
            has to be replaced.
          </DialogDescription>
        </DialogHeader>
        <code data-testid="api-key-secret" className="rounded-md border bg-muted px-3 py-2 text-sm break-all select-all">
          {secret}
        </code>
        <DialogFooter className="items-center">
          {copied === "yes" && <span className="text-xs text-muted-foreground">Copied</span>}
          {copied === "failed" && <span className="text-xs text-destructive">Copy failed — select the key and copy it by hand</span>}
          <Button
            variant="outline"
            onClick={() =>
              void navigator.clipboard.writeText(secret).then(
                () => setCopied("yes"),
                () => setCopied("failed"),
              )
            }
          >
            Copy
          </Button>
          <Button onClick={onDone}>Done</Button>
        </DialogFooter>
      </>
    );
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>New API key</DialogTitle>
        <DialogDescription>Name it after what will use it. The secret is shown once, after you create it.</DialogDescription>
      </DialogHeader>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Key name">
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Expires (optional)">
          <Input type="date" value={expires} onChange={(e) => setExpires(e.target.value)} />
        </Field>
        {create.isError && <ErrorAlert error={create.error} />}
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
          <Button type="submit" disabled={create.isPending || name.trim() === ""}>
            Create key
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}

// Invite: pre-bind an email to this account (IdentityService.CreateInvite). No mail is sent and
// there is no secret: the person signs in at the identity provider with that verified address and
// claims it. The invites made here are also listed, with their status, in the Sign-in bindings card below.
function InviteCard({ accountId }: { accountId: string }) {
  const qc = useQueryClient();
  const [email, setEmail] = useState("");
  const [made, setMade] = useState<Invite[]>([]);
  const create = useMutation({
    mutationFn: () => identity.createInvite({ accountId, email: email.trim() }),
    onSuccess: (res) => {
      if (res.invite) setMade((m) => [res.invite!, ...m]);
      setEmail("");
      return qc.invalidateQueries({ queryKey: keys.identitiesOf(accountId) });
    },
  });
  useResetOnCredentialChange(create);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Invite</CardTitle>
        <CardDescription>
          Binds an email address to this account. It is valid for 7 days and single use, and carries no secret. Nothing is sent: tell the person to
          sign in with that address, and the console will offer to claim it.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <Field label="Invite email" className="w-72">
            <Input inputMode="email" autoComplete="off" value={email} placeholder="person@example.com" onChange={(e) => setEmail(e.target.value)} />
          </Field>
          <Button type="submit" disabled={create.isPending || email.trim() === ""}>
            Create invite
          </Button>
        </form>
        {create.isError && <ErrorAlert error={create.error} />}
        {made.length > 0 && (
          <ul className="grid gap-2 text-sm">
            {made.map((i) => (
              <li key={i.id} data-testid="invite-made" className="rounded-md border px-3 py-2">
                <strong>{i.email}</strong> · expires {at(i.expireTime)} · single use
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

const inviteBadge: Record<InviteStatus, "default" | "secondary" | "destructive" | "outline"> = {
  [InviteStatus.UNSPECIFIED]: "outline",
  [InviteStatus.PENDING]: "default",
  [InviteStatus.CLAIMED]: "secondary",
  [InviteStatus.EXPIRED]: "destructive",
};

// Sign-in bindings (IdentityService.ListAccountIdentities, admin only; this page is behind AdminGate, so a
// non-admin never mounts it and never asks): the external identities (issuer, subject) bound to the
// account and its invites with their status. Only the fields the contract names are rendered: there
// is no token or secret in the reply, and none is read.
function BindingsCard({ accountId }: { accountId: string }) {
  // Only once the admin probe has answered true for the current credential (see accountIdentitiesQuery).
  const generation = useCredentialGeneration();
  const isAdmin = useIsAdmin();
  const list = useQuery(accountIdentitiesQuery(accountId, generation, isAdmin));
  const identities = list.data?.identities ?? [];
  const invites = list.data?.invites ?? [];
  return (
    <Card>
      <CardHeader>
        <CardTitle>Sign-in bindings</CardTitle>
        <CardDescription>
          Who can sign in as this account: the identity-provider sign-ins (issuer and subject) bound to it, and its invites. Newest first.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {list.isError && <ErrorAlert error={list.error} />}
        {list.isSuccess && (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Issuer</TableHead>
                  <TableHead>Subject</TableHead>
                  <TableHead>Bound</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {identities.map((i) => (
                  <TableRow key={`${i.issuer}\n${i.subject}`} data-testid="identity-row">
                    <TableCell className="font-mono text-xs">{i.issuer}</TableCell>
                    <TableCell className="font-mono text-xs">{i.subject}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{at(i.bindTime)}</TableCell>
                  </TableRow>
                ))}
                {identities.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={3} className="text-center text-muted-foreground">
                      No sign-ins bound yet
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Invite for</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Expires</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {invites.map((i) => (
                  <TableRow key={i.id} data-testid="invite-row">
                    <TableCell>{i.email}</TableCell>
                    <TableCell>
                      <Badge variant={inviteBadge[i.status]}>{InviteStatus[i.status]}</Badge>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{at(i.expireTime)}</TableCell>
                  </TableRow>
                ))}
                {invites.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={3} className="text-center text-muted-foreground">
                      No invites yet
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </>
        )}
      </CardContent>
    </Card>
  );
}

// Sessions: "sign out of all devices" (IdentityService.RevokeSessions). It refuses access tokens issued
// before now within 30 s; it cannot touch the identity provider's refresh tokens, so it is not the
// answer to a stolen account (disable the account and revoke at the IdP for that).
function SessionsCard({ accountId }: { accountId: string }) {
  const qc = useQueryClient();
  const { data } = useQuery(accountQuery(accountId));
  const revoke = useMutation({
    mutationFn: () => identity.revokeSessions({ accountId }),
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: keys.account(accountId) }), qc.invalidateQueries({ queryKey: keys.accounts })]),
  });
  useResetOnCredentialChange(revoke);
  if (!data) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>Sessions</CardTitle>
        <CardDescription>
          {data.sessionsRevokeTime ? `Revoked at ${at(data.sessionsRevokeTime)}` : "Never revoked"}
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div>
          <ConfirmButton
            label="Revoke sessions"
            title={`Revoke all sessions of ${data.name}?`}
            description="Access tokens issued before now stop working within 30s, on every device. This is sign out everywhere, not a response to a stolen account: the identity provider's refresh tokens are untouched, so they can get a new token. For that, disable the account and revoke its refresh tokens at the identity provider."
            action="Revoke all sessions"
            onConfirm={() => revoke.mutateAsync()}
          />
        </div>
        {revoke.isError && <ErrorAlert error={revoke.error} />}
      </CardContent>
    </Card>
  );
}
