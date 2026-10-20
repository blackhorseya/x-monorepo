import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import type { Stock } from "@repo/proto/inventory/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { inventory } from "../client";
import { keys, stocksQuery } from "../queries";
import { ErrorAlert, Field } from "./orders";

export function StockPage() {
  const qc = useQueryClient();
  const [sku, setSku] = useState("SKU-1");
  const [onHand, setOnHand] = useState(10);
  const list = useInfiniteQuery(stocksQuery);
  const stocks = list.data?.pages.flatMap((p) => p.stocks) ?? [];

  const set = useMutation({
    mutationFn: () => inventory.setStock({ sku, onHand }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.stocks }),
  });

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Set stock</CardTitle>
          <CardDescription>
            On hand is a count, not a delta, and creates the SKU when unknown; it cannot go below what pending orders
            hold. Reserved changes as orders are created and cancelled.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="flex flex-wrap items-end gap-2">
            <Field label="SKU" className="w-40">
              <Input value={sku} onChange={(e) => setSku(e.target.value)} />
            </Field>
            <Field label="On hand" className="w-28">
              <Input type="number" value={onHand} onChange={(e) => setOnHand(Number(e.target.value))} />
            </Field>
            <Button onClick={() => set.mutate()} disabled={set.isPending}>
              Set
            </Button>
          </div>
          {set.isError && <ErrorAlert error={set.error} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Stock</CardTitle>
          <CardDescription>Every SKU, ascending, refreshed every 2s.</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {list.isError && <ErrorAlert error={list.error} />}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>SKU</TableHead>
                <TableHead className="text-right">On hand</TableHead>
                <TableHead className="text-right">Reserved</TableHead>
                <TableHead className="text-right">Available</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {stocks.map((s) => (
                <StockRow
                  key={s.sku}
                  stock={s}
                  onEdit={() => {
                    setSku(s.sku);
                    setOnHand(s.onHand);
                  }}
                />
              ))}
              {list.isSuccess && stocks.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    No stock yet — set one above.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
          {list.hasNextPage && (
            <Button variant="outline" size="sm" className="justify-self-start" onClick={() => void list.fetchNextPage()} disabled={list.isFetchingNextPage}>
              Load more
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function StockRow({ stock, onEdit }: { stock: Stock; onEdit: () => void }) {
  const updated = stock.updateTime ? new Date(Number(stock.updateTime.seconds) * 1000) : undefined;
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">{stock.sku}</TableCell>
      <TableCell className="text-right tabular-nums">{stock.onHand}</TableCell>
      <TableCell className="text-right tabular-nums">{stock.reserved}</TableCell>
      <TableCell className="text-right">
        <Badge variant={stock.available > 0 ? "default" : "destructive"} className="tabular-nums">
          {stock.available}
        </Badge>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{updated?.toLocaleString()}</TableCell>
      <TableCell className="text-right">
        <Button variant="outline" size="sm" onClick={onEdit}>
          Edit
        </Button>
      </TableCell>
    </TableRow>
  );
}
