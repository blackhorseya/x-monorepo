// Package oidc verifies bearer tokens from external OpenID Connect issuers
// It knows nothing about accounts: the output is the verified external
// identity (iss, sub, email, email_verified, iat), and mapping it to an account
// is the job of the server.ExternalIdentityResolver the application wires.
//
// Per issuer it fetches the discovery document and the JWKS lazily, caches the
// keys (lifetime from Cache-Control, clamped), refetches on an unknown kid at
// most once per 30 s, keeps serving the last keys for up to 24 h when a refetch
// fails, and only ever verifies RS256, ES256 and EdDSA against a JWK of the
// matching type.
package oidc
