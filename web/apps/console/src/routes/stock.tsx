import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useResetOnCredentialChange } from "../auth";
import type { Stock } from "@repo/proto/inventory/v1/entity_pb";
import { useProductStatus, type ProductStatus } from "../catalog-status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useIsAdmin } from "../components/admin";
import { inventory } from "../client";
import { keys, productsQuery, stocksQuery } from "../queries";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";

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
  // The SKU is chosen, never typed: "" = nothing chosen yet.
  const [sku, setSku] = useState("");
  const [onHand, setOnHand] = useState(10);
  const [findInput, setFindInput] = useState("");
  const [findPrefix, setFindPrefix] = useState("");
  const list = useInfiniteQuery(stocksQuery(prefix, onlyReserved));
  const stocks = list.data?.pages.flatMap((p) => p.stocks) ?? [];
  // The picker's pages of products (admin's form only), narrowed by the Find prefix.
  const products = useInfiniteQuery({ ...productsQuery(findPrefix), enabled: isAdmin });
  const productList = products.data?.pages.flatMap((p) => p.products) ?? [];
  // Where each SKU on the list, and the chosen one, stands in the catalog. Asked per SKU, so a
  // product on a page of the catalog nobody has loaded is still a product.
  const statusOf = useProductStatus(sku === "" ? stocks.map((s) => s.sku) : [...stocks.map((s) => s.sku), sku]);
  const chosen: ProductStatus | undefined = sku === "" ? undefined : statusOf(sku);
  // Only a SKU the catalog vouched for is sent (a deactivated product still takes stock).
  const canSet = chosen === "active" || chosen === "inactive";

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
              Pick a product from the catalog (a deactivated one too); a SKU with no product has to be saved on Products
              first. On hand is a count, not a delta; it cannot go below what pending orders hold. Reserved changes as
              orders are created and cancelled.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <form
              className="flex items-end gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                setFindPrefix(findInput.trim());
              }}
            >
              <Field label="Find product" className="w-40">
                <Input value={findInput} placeholder="SKU-" onChange={(e) => setFindInput(e.target.value)} />
              </Field>
              <Button type="submit" variant="secondary" size="sm">
                Find
              </Button>
            </form>
            <div className="flex flex-wrap items-end gap-2">
              <Field label="SKU" className="w-72">
                <select
                  value={sku}
                  onChange={(e) => setSku(e.target.value)}
                  className="h-9 w-full min-w-0 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
                >
                  <option value="">Choose a product…</option>
                  {sku !== "" && !productList.some((p) => p.sku === sku) && <option value={sku}>{optionLabel(sku, undefined, chosen)}</option>}
                  {productList.map((p) => (
                    <option key={p.sku} value={p.sku}>
                      {optionLabel(p.sku, p.name, p.active ? "active" : "inactive")}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="On hand" className="w-28">
                <Input type="number" value={onHand} onChange={(e) => setOnHand(Number(e.target.value))} />
              </Field>
              <Button onClick={() => set.mutate()} disabled={set.isPending || !canSet}>
                Set
              </Button>
            </div>
            {products.hasNextPage && (
              <Button variant="outline" size="sm" className="justify-self-start" onClick={() => void products.fetchNextPage()} disabled={products.isFetchingNextPage}>
                Load more products
              </Button>
            )}
            {products.isSuccess && productList.length === 0 && (
              <p className="text-sm text-muted-foreground">
                {findPrefix === "" ? "The catalog has no products yet — save one on Products." : "No product starts with this prefix."}
              </p>
            )}
            {chosen !== undefined && <ChosenNote sku={sku} status={chosen} />}
            {products.isError && <ErrorAlert error={products.error} />}
            {set.isError && <ErrorAlert error={set.error} />}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Stock</CardTitle>
          <CardDescription>
            SKUs ascending, refreshed every 2s; narrow by prefix or to what pending orders hold. A SKU the catalog has no
            product for, or whose product was deactivated, is flagged.
          </CardDescription>
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
                  status={statusOf(s.sku)}
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

function optionLabel(sku: string, name: string | undefined, status: ProductStatus | undefined): string {
  const label = name === undefined ? sku : `${sku} — ${name}`;
  if (status === "inactive") return `${label} (deactivated)`;
  if (status === "missing") return `${label} (no product)`;
  return label;
}

// What the chosen SKU's catalog standing means for Set. Not an alert: nothing failed on the server.
function ChosenNote({ sku, status }: { sku: string; status: ProductStatus }) {
  const text =
    status === "missing"
      ? `${sku} has no product in the catalog; save the product before stocking it.`
      : status === "unknown"
        ? `Could not check ${sku} against the catalog. Set stays off until it can be checked.`
        : status === "inactive"
          ? "This product is deactivated; its stock can still be set."
          : undefined;
  if (text === undefined) return null;
  return (
    <p role="status" className={status === "inactive" ? "text-sm text-muted-foreground" : "text-sm text-destructive"}>
      {text}
    </p>
  );
}

// Flags on a list row. "checking" shows nothing; "unknown" is not "missing": the catalog could not be read.
function StatusBadge({ status }: { status: ProductStatus }) {
  if (status === "missing") return <Badge variant="destructive">No product</Badge>;
  if (status === "inactive") return <Badge variant="secondary">Deactivated</Badge>;
  if (status === "unknown") return <Badge variant="outline">Status unknown</Badge>;
  return null;
}

function StockRow({ stock, status, onEdit }: { stock: Stock; status: ProductStatus; onEdit?: () => void }) {
  const updated = stock.updateTime ? new Date(Number(stock.updateTime.seconds) * 1000) : undefined;
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">
        <span className="mr-2">{stock.sku}</span>
        <StatusBadge status={status} />
      </TableCell>
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
