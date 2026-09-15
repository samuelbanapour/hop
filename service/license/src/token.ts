import * as ed from "@noble/ed25519";

// @noble/ed25519's etc.sha512Async already delegates to WebCrypto's
// crypto.subtle.digest("SHA-512", ...) by default (see its source) — no
// wiring needed. That matters here because it's what makes this produce
// byte-identical signatures to Go's crypto/ed25519 (both follow RFC 8032
// exactly), which is what lets a token minted here verify against the
// public key already embedded in the hop binary.

export interface TokenPayload {
  sub: string;
  iat: number;
  exp?: number;
  gov?: boolean;
}

// signToken mirrors internal/core/license.go's ParseLicenseToken exactly:
// "<base64url payload>.<base64url signature>", both unpadded, signature
// over the raw JSON payload bytes.
export async function signToken(payload: TokenPayload, seedB64: string): Promise<string> {
  const seed = Uint8Array.from(atob(seedB64), (c) => c.charCodeAt(0));
  if (seed.length !== 32) {
    throw new Error("HOP_LICENSE_SEED must decode to exactly 32 bytes");
  }

  const json = JSON.stringify(payload);
  const payloadBytes = new TextEncoder().encode(json);

  const sig = await ed.signAsync(payloadBytes, seed);

  return `${b64url(payloadBytes)}.${b64url(sig)}`;
}

function b64url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
