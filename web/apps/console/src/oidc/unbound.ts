import { useSyncExternalStore } from "react";
import { getCredentialGeneration, useCredentialGeneration } from "../auth";

// The server verified the token but has no account for its (issuer, subject): every RPC but
// ClaimInvite answers PERMISSION_DENIED with x-error-code auth.unbound_identity (docs/identity-login.md
// §6). One such answer is enough to know, so the console swaps its pages for the ClaimInvite flow.
// The flag belongs to the credential it was seen for: a sign-out or another sign-in (a new
// generation, src/auth.ts) makes it stale on the spot.
export const UNBOUND_CODE = "auth.unbound_identity";

let markedFor: number | null = null;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());

export function markUnbound(generation: number = getCredentialGeneration()): void {
  if (markedFor === generation) return;
  markedFor = generation;
  emit();
}

export function clearUnbound(): void {
  if (markedFor === null) return;
  markedFor = null;
  emit();
}

export function useUnbound(): boolean {
  const generation = useCredentialGeneration();
  const marked = useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => markedFor,
  );
  return marked === generation;
}
