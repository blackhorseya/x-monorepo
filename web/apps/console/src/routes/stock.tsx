import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useResetOnCredentialChange } from "../auth";
import type { Stock } from "@repo/proto/inventory/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useIsAdmin } from "../components/admin";
import { inventory } from "../client";
import { keys, stocksQuery } from "../queries";
import { ErrorAlert, Field } from "./orders";

// prefix / reserved are search params (/stock?prefix=SKU-1&reserved=true), so
// a filtered view is a shareable link, like /orders and /reservations.
export type StockSearch = { prefix?: string; reserved?: boolean };

export function StockPage() {
  const qc = useQueryClient();
  // Setting stock is the admin's; anyone else gets the list only. A courtesy: the server decides.
  const isAdmin = useIsAdmin();
  const search = useSearch({ from: "/stock" });
  const navigate = useNavigate({ from: "/stock" });
  const prefix = search.prefix ?? "";
  const onlyReserved = search.reserved ?? false;
  const [prefixInput, setPrefixInput] = useState(prefix);
  const setSearch = (patch: Partial<StockSearch>) => void navigate({ search: (prev) => ({ ...prev, ...patch }) });
  const [sku, setSku] = useState("SKU-1");
  const [onHand, setOnHand] = useState(10);
  const list = useInfiniteQuery(stocksQuery(prefix, onlyReserved));
  const stocks = list.data?.pages.flatMap((p) => p.stocks) ?? [];

  const set = useMutation({
    mutationFn: () => inventory.setStock({ sku, onHand }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.stocks }),
  });
  useResetOnCredentialChange(set);

  return (
    <div className="grid gap-6">
      {isAdmin && (
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
      )}

      <Card>
        <CardHeader>
          <CardTitle>Stock</CardTitle>
          <CardDescription>SKUs ascending, refreshed every 2s; narrow by prefix or to what pending orders hold.</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="flex flex-wrap items-end gap-4">
            <form
              className="flex items-end gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                setSearch({ prefix: prefixInput.trim() || undefined });
              }}
            >
              <Field label="SKU prefix" className="w-40">
                <Input value={prefixInput} placeholder="SKU-" onChange={(e) => setPrefixInput(e.target.value)} />
              </Field>
              <Button type="submit" variant="secondary" size="sm">
                Filter
              </Button>
              {prefix !== "" && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setPrefixInput("");
                    setSearch({ prefix: undefined });
                  }}
                >
                  Clear
                </Button>
              )}
            </form>
            <Button size="sm" variant={onlyReserved ? "default" : "outline"} onClick={() => setSearch({ reserved: onlyReserved ? undefined : true })}>
              Reserved only
            </Button>
          </div>
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
                  onEdit={
                    isAdmin
                      ? () => {
                          setSku(s.sku);
                          setOnHand(s.onHand);
                        }
                      : undefined
                  }
                />
              ))}
              {list.isSuccess && stocks.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    {prefix !== "" || onlyReserved ? "No SKU matches the filter." : isAdmin ? "No stock yet — set one above." : "No stock yet."}
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

function StockRow({ stock, onEdit }: { stock: Stock; onEdit?: () => void }) {
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
        {onEdit && (
          <Button variant="outline" size="sm" onClick={onEdit}>
            Edit
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
