import { useEffect, useRef, useSyncExternalStore } from "react";

// The credential the console sends with every RPC, when the server runs with
// auth.mode optional / required (platform/server auth interceptor). Either a
// static API key (X-API-Key) or a bearer JWT (Authorization). It lives in
// localStorage so a reload keeps it, and is seeded from VITE_CONSOLE_API_KEY /
// VITE_CONSOLE_TOKEN at build time for dev setups. Nothing here decides
// anything: the server answers UNAUTHENTICATED (x-error-code
// auth.unauthenticated / auth.invalid_credential) and the pages show it.
export type Credential = { kind: "api_key" | "bearer"; value: string };

const STORAGE_KEY = "console.credential";

function seed(): Credential | null {
  const key = import.meta.env.VITE_CONSOLE_API_KEY;
  if (key) return { kind: "api_key", value: key };
  const token = import.meta.env.VITE_CONSOLE_TOKEN;
  if (token) return { kind: "bearer", value: token };
  return null;
}

function load(): Credential | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === null) return seed();
    const parsed = JSON.parse(raw) as Partial<Credential>;
    if ((parsed.kind === "api_key" || parsed.kind === "bearer") && typeof parsed.value === "string" && parsed.value) {
      return { kind: parsed.kind, value: parsed.value };
    }
  } catch {
    // unreadable storage: behave as anonymous
  }
  return null;
}

let current: Credential | null = load();
// Bumped on every setCredential. Anything that answers "who am I" for the current credential keys
// its cache by this, so an answer for the previous credential can never be read as the new one's.
// A counter, not the credential: the secret stays out of query keys (devtools, error reports).
let generation = 0;
const listeners = new Set<() => void>();

export function getCredential(): Credential | null {
  return current;
}

export function setCredential(c: Credential | null): void {
  current = c && c.value.trim() ? { kind: c.kind, value: c.value.trim() } : null;
  generation++;
  try {
    if (current) localStorage.setItem(STORAGE_KEY, JSON.stringify(current));
    else localStorage.setItem(STORAGE_KEY, "null"); // explicit "none" beats the env seed
  } catch {
    // storage unavailable: in-memory only
  }
  listeners.forEach((l) => l());
}

export function useCredentialGeneration(): number {
  return useSyncExternalStore(subscribe, () => generation);
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function useCredential(): Credential | null {
  return useSyncExternalStore(subscribe, getCredential);
}

// Mutations are not queries: the credential control invalidates every query,
// so a list that failed UNAUTHENTICATED recovers on the spot, but a form's own
// failed mutation would keep showing its error until the next attempt. Pages
// hang their mutations on this so a credential change (set / clear) resets
// them too — the error was about the old credential.
export function useResetOnCredentialChange(...mutations: { reset: () => void }[]): void {
  const credential = useCredential();
  const latest = useRef(mutations);
  latest.current = mutations;
  useEffect(() => {
    for (const m of latest.current) m.reset(); // idle mutations: no-op
  }, [credential]);
}

// Headers for the current credential: what the Connect interceptor attaches.
export function credentialHeaders(c: Credential | null = current): Record<string, string> {
  if (!c) return {};
  return c.kind === "api_key" ? { "X-API-Key": c.value } : { Authorization: `Bearer ${c.value}` };
}

// subject is a best-effort label for the header: the API key's name is not
// known client-side, a JWT's sub is in its payload.
export function subject(c: Credential | null): string {
  if (!c) return "anonymous";
  if (c.kind === "api_key") return "api key";
  try {
    const payload = JSON.parse(atob(c.value.split(".")[1].replace(/-/g, "+").replace(/_/g, "/"))) as { sub?: string };
    return payload.sub ? `jwt · ${payload.sub}` : "jwt";
  } catch {
    return "jwt";
  }
}
