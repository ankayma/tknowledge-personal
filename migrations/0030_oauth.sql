-- OAuth for Claude and standard MCP clients (owner decided 2/10/2026): sign in with
-- an email OTP, get an access token that is a tk-signed grant (1 hour) and a rotating
-- refresh token (30 days).
--
-- Numbered 0030, not 0020: the T–D stage 1 branch was using the 0020 range in
-- parallel, and these tables depend on nothing there. File name order is still right.
--
-- This is sign-in state, not knowledge: not append-only like the ledger.
-- Single-use codes are marked with `used_at` (UPDATE … WHERE used_at IS NULL, atomic),
-- and every row expires. No secrets are stored in the clear: OTPs, codes and refresh
-- tokens are kept only as SHA-256 hashes. No RLS: these tables belong to no store;
-- people arrive here before they have one.

-- Dynamic client registration (RFC 7591), for MCP clients that do not use CIMD.
-- Claude uses CIMD (client_id is a URL) and writes nothing here.
CREATE TABLE IF NOT EXISTS oauth_client (
  client_id     text PRIMARY KEY,
  client_name   text,
  redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) > 0),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz NOT NULL DEFAULT now()   -- not accepted after 90 days unused
);

-- OTP code sent by email: 6 digits, short-lived, limited attempts.
CREATE TABLE IF NOT EXISTS oauth_otp (
  id         bigserial PRIMARY KEY,
  email      text NOT NULL,
  code_hash  text NOT NULL,
  client_id  text NOT NULL,
  ip         text,
  attempts   integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at    timestamptz
);
CREATE INDEX IF NOT EXISTS oauth_otp_email_idx ON oauth_otp (email, id DESC);

-- Authorization code: single use, 60 seconds, bound to the client, redirect, PKCE,
-- scope, resource, the signed-in person and their store.
CREATE TABLE IF NOT EXISTS oauth_code (
  code_hash      text PRIMARY KEY,
  client_id      text NOT NULL,
  redirect_uri   text NOT NULL,
  code_challenge text NOT NULL,
  scope          text NOT NULL,
  resource       text NOT NULL,
  sub            text NOT NULL,
  tenant_id      text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz NOT NULL,
  used_at        timestamptz
);

-- Rotating refresh token: each use issues a new token with the same `family_id`.
-- Reusing a token that was already rotated signals a leak, so the whole family is revoked.
CREATE TABLE IF NOT EXISTS oauth_refresh (
  token_hash text PRIMARY KEY,
  family_id  text NOT NULL,
  client_id  text NOT NULL,
  sub        text NOT NULL,
  tenant_id  text NOT NULL,
  scope      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS oauth_refresh_family_idx ON oauth_refresh (family_id);

GRANT SELECT, INSERT, UPDATE ON oauth_client, oauth_otp, oauth_code, oauth_refresh TO tk_api;
GRANT USAGE ON oauth_otp_id_seq TO tk_api;
