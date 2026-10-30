// OIDC discovery (<issuer>/.well-known/openid-configuration): where to authorize, where to get
// tokens, and, when the IdP has one, where to end its session (Dex has none).
export type Endpoints = {
  authorization: string;
  token: string;
  endSession: string | null;
};

export async function discover(issuer: string, fetcher: typeof fetch = fetch): Promise<Endpoints> {
  const res = await fetcher(`${issuer.replace(/\/+$/, "")}/.well-known/openid-configuration`, {
    cache: "no-store",
    credentials: "omit",
  });
  if (!res.ok) throw new Error(`identity provider discovery failed (HTTP ${res.status})`);
  const doc = (await res.json()) as Record<string, unknown>;
  // The document must be for the issuer we were configured with (OIDC Discovery 4.3).
  if (doc.issuer !== issuer) throw new Error("identity provider discovery document is for another issuer");
  const authorization = httpsOrSame(doc.authorization_endpoint, issuer);
  const token = httpsOrSame(doc.token_endpoint, issuer);
  if (!authorization || !token) throw new Error("identity provider discovery document is incomplete");
  return { authorization, token, endSession: httpsOrSame(doc.end_session_endpoint, issuer) };
}

// An endpoint is only used when it is http(s) and, if the issuer is https, https too: a discovery
// document must not downgrade the transport the tokens travel over.
function httpsOrSame(v: unknown, issuer: string): string | null {
  if (typeof v !== "string") return null;
  try {
    const u = new URL(v);
    const secure = new URL(issuer).protocol === "https:";
    if (u.protocol !== "https:" && (secure || u.protocol !== "http:")) return null;
    return u.toString();
  } catch {
    return null;
  }
}
