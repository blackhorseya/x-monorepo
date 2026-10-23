import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import type { PoisonMessage } from "@repo/proto/admin/v1/service_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { admin, describe } from "../client";
import { keys, poisonDetailQuery, poisonQuery } from "../queries";
import { ErrorAlert } from "./orders";

// Operator view of every BC's poison table (admin.v1.AdminService): the
// messages a worker handler gave up on. Requeue puts one back on its topic
// (same message id); Drop deletes it. Needs the auth.admin_role role when the
// server runs with auth on.
export function PoisonPage() {
  const list = useQuery(poisonQuery);
  const [open, setOpen] = useState<PoisonMessage | null>(null);
  const bcs = [...new Set(list.data?.map((m) => m.boundedContext) ?? [])];

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Poison queue</CardTitle>
          <CardDescription>
            Messages parked after every retry, oldest first, refreshed every 5s. Fix the handler, then requeue; drop
            what should never run.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {list.isError && <ErrorAlert error={list.error} />}
          {bcs.length > 0 && (
            <div className="flex flex-wrap gap-2">
              {bcs.map((bc) => (
                <BulkActions key={bc} bc={bc} />
              ))}
            </div>
          )}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Offset</TableHead>
                <TableHead>BC</TableHead>
                <TableHead>Parked at</TableHead>
                <TableHead>Topic</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.data?.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    Nothing parked.
                  </TableCell>
                </TableRow>
              )}
              {list.data?.map((m) => (
                <PoisonRow key={`${m.boundedContext}/${m.offset}`} message={m} onShow={() => setOpen(m)} />
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
      {open && <DetailCard message={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

function useActions(bc: string, offsets: bigint[] | "all") {
  const qc = useQueryClient();
  const body = offsets === "all" ? { boundedContext: bc, all: true } : { boundedContext: bc, offsets };
  const done = () => qc.invalidateQueries({ queryKey: keys.poison });
  const requeue = useMutation({ mutationFn: () => admin.requeuePoison(body), onSuccess: done });
  const drop = useMutation({ mutationFn: () => admin.dropPoison(body), onSuccess: done });
  return { requeue, drop };
}

function BulkActions({ bc }: { bc: string }) {
  const { requeue, drop } = useActions(bc, "all");
  return (
    <div className="flex items-center gap-2 rounded-md border px-3 py-1.5 text-sm">
      <span className="font-medium">{bc}</span>
      <Button variant="outline" size="sm" onClick={() => requeue.mutate()} disabled={requeue.isPending}>
        Requeue all
      </Button>
      <Button
        variant="destructive"
        size="sm"
        onClick={() => window.confirm(`Drop every parked message of ${bc}?`) && drop.mutate()}
        disabled={drop.isPending}
      >
        Drop all
      </Button>
      {(requeue.isError || drop.isError) && (
        <span className="text-xs text-destructive">{describe(requeue.error ?? drop.error)}</span>
      )}
    </div>
  );
}

function PoisonRow({ message: m, onShow }: { message: PoisonMessage; onShow: () => void }) {
  const { requeue, drop } = useActions(m.boundedContext, [m.offset]);
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">{String(m.offset)}</TableCell>
      <TableCell>
        <Link to="/prune" search={{ bc: m.boundedContext }} title={`Prune ${m.boundedContext} (dry run first)`}>
          <Badge variant="secondary">{m.boundedContext}</Badge>
        </Link>
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs">{m.parkedAt ? timestampDate(m.parkedAt).toLocaleString() : ""}</TableCell>
      <TableCell className="font-mono text-xs">{m.topic}</TableCell>
      <TableCell className="max-w-md truncate text-xs" title={m.reason}>
        {m.reason}
      </TableCell>
      <TableCell className="whitespace-nowrap text-right">
        <Button variant="ghost" size="sm" onClick={onShow}>
          Show
        </Button>
        <Button variant="outline" size="sm" onClick={() => requeue.mutate()} disabled={requeue.isPending}>
          Requeue
        </Button>
        <Button variant="destructive" size="sm" onClick={() => drop.mutate()} disabled={drop.isPending}>
          Drop
        </Button>
        {(requeue.isError || drop.isError) && (
          <p className="text-xs text-destructive">{describe(requeue.error ?? drop.error)}</p>
        )}
      </TableCell>
    </TableRow>
  );
}

function DetailCard({ message: m, onClose }: { message: PoisonMessage; onClose: () => void }) {
  const detail = useQuery(poisonDetailQuery(m.boundedContext, m.offset));
  const full = detail.data ?? m;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between">
          <span>
            {m.boundedContext} #{String(m.offset)}
          </span>
          <Button variant="ghost" size="sm" onClick={onClose}>
            Close
          </Button>
        </CardTitle>
        <CardDescription className="font-mono text-xs">{m.uuid}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {detail.isError && <ErrorAlert error={detail.error} />}
        <div>
          <span className="text-muted-foreground">handler </span>
          <span className="font-mono text-xs">{full.handler}</span>
        </div>
        <div>
          <span className="text-muted-foreground">reason </span>
          <span className="whitespace-pre-wrap font-mono text-xs">{full.reason}</span>
        </div>
        <div>
          <div className="text-muted-foreground">metadata</div>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-3 font-mono text-xs">
            {Object.entries(full.metadata)
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([k, v]) => [
                <dt key={`${k}-k`} className="text-muted-foreground">
                  {k}
                </dt>,
                <dd key={`${k}-v`} className="break-all">
                  {v}
                </dd>,
              ])}
          </dl>
        </div>
        <div>
          <div className="text-muted-foreground">
            payload · {full.payload.length} bytes, base64 (the topic&apos;s proto message; decode with <code>buf convert</code>)
          </div>
          <pre className="max-h-48 overflow-auto rounded-md bg-muted p-2 text-xs break-all whitespace-pre-wrap">
            {toBase64(full.payload)}
          </pre>
        </div>
      </CardContent>
    </Card>
  );
}

function toBase64(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}
