import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useResetOnCredentialChange } from "../auth";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { inventory } from "../client";
import { keys } from "../queries";
import { ErrorAlert, Field } from "./orders";

// Operator side of the reservation TTL (InventoryService.ExpireReservations, the
// RPC twin of `worker expire`): a dry run lists the RESERVED reservations held
// longer than the age; the real run gives their stock back and asks ordering to
// cancel the orders. The routine sweep is the Helm expire CronJob — this call
// runs under the server's RPC timeout and one batch at a time.
//
// hours lives in the URL (/expire?hours=24) so the alert runbook can land here
// prefilled. Dry run is deliberately not: a link never arms a real expiry.
export type ExpireSearch = { hours?: number };

export function ExpirePage() {
  const search = useSearch({ from: "/expire" });
  const navigate = useNavigate({ from: "/expire" });
  const hours = search.hours ?? 24;
  const setHours = (h: number) =>
    void navigate({ replace: true, search: () => ({ hours: h }) });
  const [limit, setLimit] = useState(100);
  const [dryRun, setDryRun] = useState(true);
  const qc = useQueryClient();

  const run = useMutation({
    mutationFn: () =>
      inventory.expireReservations({
        olderThan: { seconds: BigInt(Math.round(hours * 3600)), nanos: 0 },
        dryRun,
        limit,
      }),
    onSuccess: (res) => {
      if (res.dryRun) return;
      // Every reservation list (any status), the orders (ordering cancels them next) and the stock.
      void qc.invalidateQueries({ queryKey: ["reservations"] });
      void qc.invalidateQueries({ queryKey: ["reservation"] });
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.stocks });
    },
  });
  useResetOnCredentialChange(run);
  const verb = run.data?.dryRun === false ? "expired" : "would expire";

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Expire</CardTitle>
          <CardDescription>
            The reservation TTL: a RESERVED reservation held longer than the age
            gives its stock back and ordering is asked to cancel the order — the
            customer neither completed nor cancelled it. Oldest first, at most
            the limit per run; a run that comes back full means there is more.
            Start with a dry run. The routine sweep is the worker&apos;s expire
            CronJob; this is the operator&apos;s look, or a small run.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Older than (hours)" className="w-36">
              <Input
                type="number"
                min={1}
                value={hours}
                onChange={(e) => setHours(Number(e.target.value))}
              />
            </Field>
            <Field label="Limit" className="w-28">
              <Input
                type="number"
                min={1}
                max={1000}
                value={limit}
                onChange={(e) => setLimit(Number(e.target.value))}
              />
            </Field>
            <Label className="flex h-9 items-center gap-2">
              <input
                type="checkbox"
                checked={dryRun}
                onChange={(e) => setDryRun(e.target.checked)}
              />
              Dry run
            </Label>
            <Button
              variant={dryRun ? "default" : "destructive"}
              onClick={() => run.mutate()}
              disabled={
                run.isPending || !(hours > 0) || !(limit > 0 && limit <= 1000)
              }
            >
              {dryRun ? "Dry run" : "Expire"}
            </Button>
          </div>
          {run.isError && <ErrorAlert error={run.error} />}
        </CardContent>
      </Card>

      {run.isSuccess && (
        <Card>
          <CardHeader>
            <CardTitle>
              {verb} {run.data.expired.length} reservation(s)
            </CardTitle>
            <CardDescription>
              Held longer than {hours}h, oldest first.
              {run.data.expired.length >= limit &&
                " The run came back full: there may be more — run again."}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Order</TableHead>
                  <TableHead>Reserved at</TableHead>
                  <TableHead className="text-right">Held for</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {run.data.expired.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={3} className="text-muted-foreground">
                      Nothing due.
                    </TableCell>
                  </TableRow>
                )}
                {run.data.expired.map((e) => (
                  <TableRow key={e.orderId}>
                    <TableCell className="font-mono text-xs">
                      <Link
                        to="/orders/$id"
                        params={{ id: e.orderId }}
                        className="underline"
                      >
                        {e.orderId}
                      </Link>
                    </TableCell>
                    <TableCell className="text-xs">
                      {e.reservedAt
                        ? new Date(
                            Number(e.reservedAt.seconds) * 1000
                          ).toISOString()
                        : ""}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {age(Number(e.heldFor?.seconds ?? 0n))}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

// 42s / 15m / 7h / 7d, the CLI's spelling (same as the Prune page).
function age(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 48 * 3600) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}
