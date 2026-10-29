import { useMutation } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useResetOnCredentialChange } from "../auth";
import type { PruneBlocker, PruneTable } from "@repo/proto/admin/v1/service_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { admin } from "../client";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";

// Operator side of the outbox / inbox retention (admin.v1.AdminService.Prune,
// the RPC twin of `worker prune`): a dry run shows what would go and which
// consumer groups hold old rows back; forgetting a group that will never run
// again releases them. Big first backlogs belong to the CLI / CronJob — this
// call runs under the server's RPC timeout.
//
// bc / days / forget live in the URL (/prune?bc=ordering&forget=test-old.ordering)
// so the Poison page and alert runbooks can land here prefilled. Dry run and
// unconsumed are deliberately not: a link never arms a real delete.
export type PruneSearch = { bc?: string; days?: number; forget?: string };

export function PrunePage() {
  const search = useSearch({ from: "/prune" });
  const navigate = useNavigate({ from: "/prune" });
  const bc = search.bc ?? "";
  const days = search.days ?? 30;
  const forget = search.forget ?? "";
  const setSearch = (patch: Partial<PruneSearch>) =>
    void navigate({ replace: true, search: (prev) => ({ ...prev, ...patch }) });
  const [dryRun, setDryRun] = useState(true);
  const [unconsumed, setUnconsumed] = useState(false);
  // A group badge in the note adds that group to forget (deduplicated, URL included), so the
  // alert runbook is dry run → click → Dry run again → Prune, nothing retyped.
  const addForget = (group: string) => {
    const groups = forget.split(",").map((g) => g.trim()).filter((g) => g !== "");
    if (!groups.includes(group)) setSearch({ forget: [...groups, group].join(",") });
  };

  const run = useMutation({
    mutationFn: () =>
      admin.prune({
        boundedContext: bc.trim(),
        olderThan: { seconds: BigInt(Math.round(days * 86400)), nanos: 0 },
        dryRun,
        unconsumed,
        forgetGroups: forget
          .split(",")
          .map((g) => g.trim())
          .filter((g) => g !== ""),
      }),
  });
  useResetOnCredentialChange(run);
  const verb = run.variables === undefined || dryRun ? "would delete" : "deleted";

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Prune</CardTitle>
          <CardDescription>
            Deletes outbox rows every consumer group has acknowledged and consumed inbox rows older than the age; poison
            tables are never touched. Start with a dry run: the note column lists every consumer group with unread rows
            and the age of its oldest one — <em>held back by</em> when that age is past the retention, <em>behind</em> when
            a live group simply has not caught up yet — and a group that will never run again goes in <em>forget</em>
            (click its badge to add it).
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Bounded context (empty = all)" className="w-44">
              <Input value={bc} placeholder="ordering" onChange={(e) => setSearch({ bc: e.target.value || undefined })} />
            </Field>
            <Field label="Older than (days)" className="w-32">
              <Input type="number" min={1} value={days} onChange={(e) => setSearch({ days: Number(e.target.value) })} />
            </Field>
            <Field label="Forget groups (comma-separated)" className="w-72">
              <Input value={forget} placeholder="test-old.ordering" onChange={(e) => setSearch({ forget: e.target.value || undefined })} />
            </Field>
            <Toggle label="Dry run" checked={dryRun} onChange={setDryRun} />
            <Toggle label="Unconsumed tables too" checked={unconsumed} onChange={setUnconsumed} />
            <Button variant={dryRun ? "default" : "destructive"} onClick={() => run.mutate()} disabled={run.isPending || !(days > 0)}>
              {dryRun ? "Dry run" : "Prune"}
            </Button>
          </div>
          {run.isError && <ErrorAlert error={run.error} />}
        </CardContent>
      </Card>

      {run.isSuccess && (
        <Card>
          <CardHeader>
            <CardTitle>
              {verb} {String(run.data.total)} row(s)
            </CardTitle>
            <CardDescription>Per table; a table nothing has a cursor on is skipped unless unconsumed is on.</CardDescription>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Table</TableHead>
                  <TableHead className="text-right">Rows</TableHead>
                  <TableHead>Note</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {run.data.tables.map((t) => (
                  <TableRow key={t.table}>
                    <TableCell className="font-mono text-xs">{t.table}</TableCell>
                    <TableCell className="text-right tabular-nums">{String(t.rows)}</TableCell>
                    <TableCell className="text-xs">
                      <Note table={t} dryRun={dryRun} olderThanSeconds={days * 86400} onForget={addForget} />
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

function Toggle({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <Label className="flex h-9 items-center gap-2">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      {label}
    </Label>
  );
}

// Same wording as the CLI's NOTE column: forgotten groups, why skipped, who holds prunable
// rows back (oldest unread past the retention) and who is merely behind. Each group is a
// button that adds it to forget.
function Note({
  table,
  dryRun,
  olderThanSeconds,
  onForget,
}: {
  table: PruneTable;
  dryRun: boolean;
  olderThanSeconds: number;
  onForget: (group: string) => void;
}) {
  const held = table.blockers.filter((b) => oldestSeconds(b) >= olderThanSeconds);
  const behind = table.blockers.filter((b) => oldestSeconds(b) < olderThanSeconds);
  return (
    <span className="flex flex-wrap items-center gap-1">
      {table.forgotten.length > 0 && (
        <span>
          {dryRun ? "would forget" : "forgot"} {table.forgotten.join(", ")}
        </span>
      )}
      {table.skipped && <span className="text-muted-foreground">{table.skipped}</span>}
      {held.length > 0 && (
        <>
          <span>held back by</span>
          {held.map((b) => (
            <GroupBadge key={b.consumerGroup} blocker={b} variant="destructive" onForget={onForget} />
          ))}
        </>
      )}
      {behind.length > 0 && (
        <>
          <span className="text-muted-foreground">behind:</span>
          {behind.map((b) => (
            <GroupBadge key={b.consumerGroup} blocker={b} variant="outline" onForget={onForget} />
          ))}
        </>
      )}
    </span>
  );
}

function GroupBadge({
  blocker: b,
  variant,
  onForget,
}: {
  blocker: PruneBlocker;
  variant: "destructive" | "outline";
  onForget: (group: string) => void;
}) {
  return (
    <button
      type="button"
      className="cursor-pointer rounded-md focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      title="add to forget groups"
      aria-label={`forget ${b.consumerGroup}`}
      onClick={() => onForget(b.consumerGroup)}
    >
      <Badge variant={variant} className="font-mono">
        {b.consumerGroup} ({String(b.unread)} unread, oldest {age(oldestSeconds(b))})
      </Badge>
    </button>
  );
}

const oldestSeconds = (b: PruneBlocker) => Number(b.oldestUnread?.seconds ?? 0n);

// 42s / 15m / 7h / 7d, the CLI's spelling.
function age(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 48 * 3600) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}
