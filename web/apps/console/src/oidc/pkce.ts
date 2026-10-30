// PKCE (RFC 7636) and the other one-time values of an authorization request. Pure: no DOM,
// only Web Crypto, so they run (and are tested) in node as well.
const base64url = (bytes: Uint8Array): string => {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

export function randomToken(bytes = 32): string {
  return base64url(crypto.getRandomValues(new Uint8Array(bytes)));
}

// 32 random bytes → 43 characters, the RFC's minimum length with the most entropy per character.
export const newVerifier = (): string => randomToken(32);

export async function challengeFor(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64url(new Uint8Array(digest));
}
