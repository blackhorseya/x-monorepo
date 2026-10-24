import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, LogOut } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { setCredential, subject, useCredential, type Credential } from "../auth";

// Header control: who the console calls as. Set an API key or a JWT, or clear
// it to go anonymous. Every query is invalidated on the spot so an
// UNAUTHENTICATED / PERMISSION_DENIED page recovers (or appears) at once instead
// of on its next poll — the slowest one (poison) is 5s.
export function CredentialControl() {
  const qc = useQueryClient();
  const current = useCredential();
  const [open, setOpen] = useState(false);
  const [kind, setKind] = useState<Credential["kind"]>(current?.kind ?? "api_key");
  const [value, setValue] = useState("");

  const apply = (c: Credential | null) => {
    setCredential(c);
    void qc.invalidateQueries();
  };
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
