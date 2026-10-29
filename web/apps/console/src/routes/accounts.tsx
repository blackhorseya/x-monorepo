import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { AccountStatus, type Account } from "@repo/proto/identity/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { AdminGate } from "../components/admin";
import { ConfirmButton } from "../components/confirm";
import { useResetOnCredentialChange } from "../auth";
import { identity } from "../client";
import { accountsQuery, keys } from "../queries";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";

// "viewer, ops" → ["viewer", "ops"]; the server trims / dedupes / sorts, this only splits.
export const parseRoles = (raw: string): string[] =>
  raw
    .split(",")
    .map((r) => r.trim())
    .filter((r) => r !== "");

export const at = (t?: { seconds: bigint }) => (t ? new Date(Number(t.seconds) * 1000).toLocaleString() : "");

export const accountBadge: Record<AccountStatus, "default" | "secondary" | "destructive" | "outline"> = {
  [AccountStatus.UNSPECIFIED]: "outline",
  [AccountStatus.ACTIVE]: "default",
  [AccountStatus.DISABLED]: "secondary",
};

// /accounts: the DB-managed accounts (identity BC). Every call is admin-only on the server;
// AdminGate keeps the page itself from rendering for anyone else.
export function AccountsPage() {
  return (
    <AdminGate>
      <div className="grid gap-6">
        <CreateAccountCard />
        <AccountsCard />
      </div>
    </AdminGate>
  );
}

function CreateAccountCard() {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [roles, setRoles] = useState("");
  const create = useMutation({
    mutationFn: () => identity.createAccount({ name: name.trim(), roles: parseRoles(roles) }),
    onSuccess: () => {
      setName("");
      setRoles("");
      return qc.invalidateQueries({ queryKey: keys.accounts });
    },
  });
  useResetOnCredentialChange(create);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Create account</CardTitle>
        <CardDescription>
          An account is who a key belongs to; its roles are what the key may do (<code>admin</code> opens these pages and the
          admin RPCs). Keys are created on the account&apos;s page.
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
          <Field label="Name" className="w-48">
            <Input value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Roles" className="w-64">
            <Input value={roles} placeholder="comma separated, e.g. admin" onChange={(e) => setRoles(e.target.value)} />
          </Field>
          <Button type="submit" disabled={create.isPending || name.trim() === ""}>
            Create account
          </Button>
        </form>
        {create.isError && <ErrorAlert error={create.error} />}
      </CardContent>
    </Card>
  );
}

function AccountsCard() {
  const list = useInfiniteQuery(accountsQuery);
  const rows = list.data?.pages.flatMap((p) => p.accounts) ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>Accounts</CardTitle>
        <CardDescription>By id, 20 per page. A disabled account&apos;s keys stop working within 30s.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {list.isError && <ErrorAlert error={list.error} />}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Roles</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((a) => (
              <AccountRow key={a.id} account={a} />
            ))}
            {list.isSuccess && rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-muted-foreground">
                  No accounts yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        {list.hasNextPage && (
          <Button
            variant="outline"
            size="sm"
            className="justify-self-start"
            onClick={() => void list.fetchNextPage()}
            disabled={list.isFetchingNextPage}
          >
            Load more
          </Button>
        )}
      </CardContent>
    </Card>
  );
}

// Disable / Enable for one account, shared by the list row and the detail page. Disable asks
// first (it locks every key of the account out); Enable is the way back and does not.
export function AccountToggle({ account: a }: { account: Account }) {
  const qc = useQueryClient();
  const toggle = useMutation({
    mutationFn: async () => {
      if (a.status === AccountStatus.ACTIVE) await identity.disableAccount({ id: a.id });
      else await identity.enableAccount({ id: a.id });
    },
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: keys.accounts }), qc.invalidateQueries({ queryKey: keys.account(a.id) })]),
  });
  useResetOnCredentialChange(toggle);
  return (
    <span className="flex flex-col items-end gap-1">
      {a.status === AccountStatus.ACTIVE ? (
        <ConfirmButton
          label="Disable"
          title={`Disable ${a.name}?`}
          description="Every API key of this account stops working within 30s. You can enable the account again."
          action="Disable account"
          onConfirm={() => toggle.mutateAsync()}
        />
      ) : (
        <Button variant="outline" size="sm" onClick={() => toggle.mutate()} disabled={toggle.isPending}>
          Enable
        </Button>
      )}
      {toggle.isError && <ErrorAlert error={toggle.error} />}
    </span>
  );
}

function AccountRow({ account: a }: { account: Account }) {
  return (
    <TableRow>
      <TableCell>
        <Link to="/accounts/$id" params={{ id: a.id }} className="underline">
          {a.name}
        </Link>
      </TableCell>
      <TableCell className="text-xs">{a.roles.length === 0 ? <span className="text-muted-foreground">none</span> : a.roles.join(", ")}</TableCell>
      <TableCell>
        <Badge variant={accountBadge[a.status]}>{AccountStatus[a.status]}</Badge>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{at(a.createTime)}</TableCell>
      <TableCell className="text-right">
        <AccountToggle account={a} />
      </TableCell>
    </TableRow>
  );
}
