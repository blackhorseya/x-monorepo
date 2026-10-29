import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useResetOnCredentialChange } from "../auth";
import type { Product } from "@repo/proto/catalog/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { catalog } from "../client";
import { keys, productsQuery } from "../queries";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";
import { formatMoney } from "../components/order-status";

// The catalog's side of pricing (CatalogService): what each SKU sells for. Behind AdminGate in
// the router, because saving and deactivating are the admin's (the server checks the role too).
// prefix is a search param (/products?prefix=SKU-1), so a filtered view is a shareable link.
export type ProductsSearch = { prefix?: string };

export function ProductsPage() {
  const qc = useQueryClient();
  const search = useSearch({ from: "/products" });
  const navigate = useNavigate({ from: "/products" });
  const prefix = search.prefix ?? "";
  const [prefixInput, setPrefixInput] = useState(prefix);
  const setPrefix = (p: string | undefined) => void navigate({ search: () => ({ prefix: p }) });
  const [sku, setSku] = useState("");
  const [name, setName] = useState("");
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState("USD");
  const list = useInfiniteQuery(productsQuery(prefix));
  const products = list.data?.pages.flatMap((p) => p.products) ?? [];

  const refresh = () => void qc.invalidateQueries({ queryKey: keys.products });
  const save = useMutation({
    mutationFn: () => catalog.upsertProduct({ sku, name, price: { amountMinor: BigInt(amount), currency } }),
    onSuccess: refresh,
  });
  const deactivate = useMutation({
    mutationFn: (target: string) => catalog.deactivateProduct({ sku: target }),
    onSuccess: refresh,
  });
  useResetOnCredentialChange(save, deactivate);

  // The amount is whole minor units; anything else is not sent (the server would refuse it anyway).
  const amountOk = /^\d+$/.test(amount);

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle>Save product</CardTitle>
          <CardDescription>
            Creates the SKU, or replaces the name and price of an existing one (and puts a deactivated product back on
            sale). The price is in minor units (cents) and carries its own currency.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <form
            className="flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (amountOk) save.mutate();
            }}
          >
            <Field label="SKU" className="w-40">
              <Input value={sku} placeholder="SKU-1" onChange={(e) => setSku(e.target.value)} />
            </Field>
            <Field label="Name" className="w-56">
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
            <Field label="Price (minor units)" className="w-40">
              <Input inputMode="numeric" value={amount} placeholder="1999" onChange={(e) => setAmount(e.target.value)} />
            </Field>
            <Field label="Currency" className="w-24">
              <Input value={currency} maxLength={3} onChange={(e) => setCurrency(e.target.value.toUpperCase())} />
            </Field>
            <Button type="submit" disabled={save.isPending || !amountOk}>
              Save product
            </Button>
          </form>
          {save.isError && <ErrorAlert error={save.error} />}
          {deactivate.isError && <ErrorAlert error={deactivate.error} />}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Products</CardTitle>
          <CardDescription>SKUs ascending, inactive ones included; narrow by prefix.</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              setPrefix(prefixInput.trim() || undefined);
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
                  setPrefix(undefined);
                }}
              >
                Clear
              </Button>
            )}
          </form>
          {list.isError && <ErrorAlert error={list.error} />}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>SKU</TableHead>
                <TableHead>Name</TableHead>
                <TableHead className="text-right">Price</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {products.map((p) => (
                <ProductRow
                  key={p.sku}
                  product={p}
                  busy={deactivate.isPending}
                  onEdit={() => {
                    setSku(p.sku);
                    setName(p.name);
                    setAmount(String(p.price?.amountMinor ?? ""));
                    setCurrency(p.price?.currency ?? "USD");
                  }}
                  onDeactivate={() => deactivate.mutate(p.sku)}
                />
              ))}
              {list.isSuccess && products.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    {prefix !== "" ? "No product matches the filter." : "No product yet — save one above."}
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

function ProductRow({ product, busy, onEdit, onDeactivate }: { product: Product; busy: boolean; onEdit: () => void; onDeactivate: () => void }) {
  const updated = product.updateTime ? new Date(Number(product.updateTime.seconds) * 1000) : undefined;
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">{product.sku}</TableCell>
      <TableCell>{product.name}</TableCell>
      <TableCell className="text-right tabular-nums">{formatMoney(product.price?.amountMinor, product.price?.currency)}</TableCell>
      <TableCell>
        <Badge variant={product.active ? "default" : "secondary"}>{product.active ? "active" : "inactive"}</Badge>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{updated?.toLocaleString()}</TableCell>
      <TableCell className="space-x-2 text-right">
        <Button variant="outline" size="sm" onClick={onEdit}>
          Edit
        </Button>
        {product.active && (
          <Button variant="outline" size="sm" onClick={onDeactivate} disabled={busy}>
            Deactivate
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
