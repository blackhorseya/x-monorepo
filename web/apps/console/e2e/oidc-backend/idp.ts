import { createHash, createSign, generateKeyPairSync, randomUUID } from "node:crypto";
import type { KeyObject } from "node:crypto";
import { createServer } from "node:http";
import type { IncomingMessage, Server, ServerResponse } from "node:http";

// A loopback OIDC provider for the real-backend E2E: discovery, JWKS (ES256), authorization (no
// login page: it redirects straight back as the configured user), and a token endpoint with
// authorization_code + PKCE and rotating refresh tokens. Not Dex (that is #274); what matters is
// that the real rpc verifies its tokens against its real JWKS.

export type User = { sub: string; email: string; emailVerified: boolean };

export class TestIdp {
  readonly issuer: string;
  user: User = { sub: "sub-default", email: "default@example.com", emailVerified: true };
  private server: Server | undefined;
  private readonly key = generateKeyPairSync("ec", { namedCurve: "P-256" });
  private readonly kid = "e2e-es256";
  private codes = new Map<string, { challenge: string; nonce: string; user: User }>();
  private refresh = new Map<string, User>();

  readonly port: number;
  readonly clientId: string;

  constructor(port: number, clientId: string) {
    this.port = port;
    this.clientId = clientId;
    this.issuer = `http://127.0.0.1:${port}/idp`;
  }

  async start(): Promise<void> {
    this.server = createServer((req, res) => {
      this.handle(req, res).catch((err) => {
        console.error(err);
        res.writeHead(500);
        res.end();
      });
    });
    await new Promise<void>((r) => this.server!.listen(this.port, "127.0.0.1", r));
  }

  async stop(): Promise<void> {
    await new Promise<void>((r) => (this.server ? this.server.close(() => r()) : r()));
  }

  private sign(claims: Record<string, unknown>): string {
    const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString("base64url");
    const head = b64({ alg: "ES256", typ: "JWT", kid: this.kid });
    const body = b64(claims);
    const sig = createSign("SHA256").update(`${head}.${body}`).sign({ key: this.key.privateKey as KeyObject, dsaEncoding: "ieee-p1363" });
    return `${head}.${body}.${sig.toString("base64url")}`;
  }

  private tokens(user: User, nonce: string) {
    const now = Math.floor(Date.now() / 1000);
    const claims = { iss: this.issuer, aud: this.clientId, sub: user.sub, email: user.email, email_verified: user.emailVerified, iat: now, exp: now + 300 };
    const refresh = `rt-${randomUUID()}`;
    this.refresh.set(refresh, user);
    return {
      token_type: "Bearer",
      access_token: this.sign(claims),
      id_token: this.sign({ ...claims, ...(nonce ? { nonce } : {}) }),
      refresh_token: refresh,
      expires_in: 300,
    };
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? "/", this.issuer);
    const cors = { "access-control-allow-origin": "*", "access-control-allow-headers": "*" };
    const json = (status: number, body: unknown) => {
      res.writeHead(status, { ...cors, "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "OPTIONS") {
      res.writeHead(204, cors);
      return void res.end();
    }
    // Test control: who the next sign-in is (POST {sub,email,emailVerified}).
    if (url.pathname === "/control/user" && req.method === "POST") {
      const chunks: Buffer[] = [];
      for await (const c of req) chunks.push(c as Buffer);
      this.user = JSON.parse(Buffer.concat(chunks).toString()) as User;
      return json(200, this.user);
    }
    switch (url.pathname) {
      case "/idp/.well-known/openid-configuration":
        return json(200, {
          issuer: this.issuer,
          authorization_endpoint: `${this.issuer}/auth`,
          token_endpoint: `${this.issuer}/token`,
          jwks_uri: `${this.issuer}/jwks`,
          id_token_signing_alg_values_supported: ["ES256"],
        });
      case "/idp/jwks": {
        const jwk = this.key.publicKey.export({ format: "jwk" });
        return json(200, { keys: [{ ...jwk, kid: this.kid, alg: "ES256", use: "sig" }] });
      }
      case "/idp/auth": {
        const code = `code-${randomUUID()}`;
        this.codes.set(code, { challenge: url.searchParams.get("code_challenge") ?? "", nonce: url.searchParams.get("nonce") ?? "", user: this.user });
        const back = new URL(url.searchParams.get("redirect_uri") ?? "");
        back.searchParams.set("code", code);
        back.searchParams.set("state", url.searchParams.get("state") ?? "");
        res.writeHead(302, { location: back.toString() });
        return void res.end();
      }
      case "/idp/token": {
        const chunks: Buffer[] = [];
        for await (const c of req) chunks.push(c as Buffer);
        const form = new URLSearchParams(Buffer.concat(chunks).toString());
        if (form.get("grant_type") === "authorization_code") {
          const issued = this.codes.get(form.get("code") ?? "");
          this.codes.delete(form.get("code") ?? "");
          const verifier = form.get("code_verifier") ?? "";
          if (!issued || createHash("sha256").update(verifier).digest("base64url") !== issued.challenge) return json(400, { error: "invalid_grant" });
          return json(200, this.tokens(issued.user, issued.nonce));
        }
        if (form.get("grant_type") === "refresh_token") {
          const user = this.refresh.get(form.get("refresh_token") ?? "");
          this.refresh.delete(form.get("refresh_token") ?? "");
          return user ? json(200, this.tokens(user, "")) : json(400, { error: "invalid_grant" });
        }
        return json(400, { error: "unsupported_grant_type" });
      }
    }
    json(404, { error: "not_found" });
  }
}
