import { createHmac } from "node:crypto";
import { JWT_AUDIENCE, JWT_ISSUER, JWT_SECRET } from "../../playwright.config";

// Mints the HS256 tokens the auth project's server verifies (platform/server
// jwtVerifier: exp required, iss / aud pinned, sub required, roles claim is
// the role list). Overrides let a test produce the rejected variants.
export function mintJWT(
  sub: string,
  overrides: Record<string, unknown> = {},
  secret = JWT_SECRET
): string {
  const now = Math.floor(Date.now() / 1000);
  const payload = {
    sub,
    iss: JWT_ISSUER,
    aud: JWT_AUDIENCE,
    iat: now,
    exp: now + 300,
    ...overrides,
  };
  const head = b64url(JSON.stringify({ alg: "HS256", typ: "JWT" }));
  const body = b64url(JSON.stringify(payload));
  const sig = createHmac("sha256", secret)
    .update(`${head}.${body}`)
    .digest("base64url");
  return `${head}.${body}.${sig}`;
}

const b64url = (s: string) => Buffer.from(s).toString("base64url");
