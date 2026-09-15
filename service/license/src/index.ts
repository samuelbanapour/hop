import { isGovDomain } from "./govdomains";
import { TERMS_TEXT, TERMS_VERSION, termsHash } from "./terms";
import { signToken } from "./token";

export interface Env {
  DB: D1Database;
  EMAIL: SendEmail;
  SERVICE_URL: string;
  FROM_EMAIL: string;
  FROM_NAME: string;
  HOP_LICENSE_SEED: string;
}

interface AcceptanceRow {
  id: string;
  name: string;
  email: string;
  terms_version: string;
  terms_hash: string;
  is_gov: number;
  verify_code: string;
  redeem_code: string | null;
  created_at: string;
  verified_at: string | null;
  redeemed_at: string | null;
  token: string | null;
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// TOKEN_TTL_SECONDS is how long a self-service token stays valid once
// issued. 0 disables expiry entirely.
const TOKEN_TTL_SECONDS = 0;

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);

    try {
      if (url.pathname === "/terms" && req.method === "GET") return await handleTerms();
      if (url.pathname === "/accept" && req.method === "POST") return await handleAccept(req, env);
      if (url.pathname === "/verify" && req.method === "GET") return await handleVerify(url, env);
      if (url.pathname === "/redeem" && req.method === "GET") return handleRedeemForm(url);
      if (url.pathname === "/redeem" && req.method === "POST") return await handleRedeemSubmit(req, env);
      if (url.pathname === "/status" && req.method === "GET") return await handleStatus(url, env);
    } catch (err) {
      return json({ error: String(err instanceof Error ? err.message : err) }, 500);
    }

    return json({ error: "not found" }, 404);
  },
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function html(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "content-type": "text/html; charset=utf-8" } });
}

function nowISO(): string {
  return new Date().toISOString().replace(/\.\d+Z$/, "Z"); // to the second
}

function randomCode(): string {
  // 10 chars from an unambiguous alphabet (no 0/O/1/I/l), grouped for
  // readability when typed by hand: XXXXX-XXXXX.
  const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789";
  const bytes = crypto.getRandomValues(new Uint8Array(10));
  const chars = [...bytes].map((b) => alphabet[b % alphabet.length]);
  return chars.slice(0, 5).join("") + "-" + chars.slice(5).join("");
}

async function handleTerms(): Promise<Response> {
  return json({ version: TERMS_VERSION, text: TERMS_TEXT, hash: await termsHash() });
}

async function handleAccept(req: Request, env: Env): Promise<Response> {
  let body: { name?: string; email?: string };
  try {
    body = await req.json();
  } catch {
    return json({ error: "expected a JSON body" }, 400);
  }

  const name = (body.name ?? "").trim();
  const email = (body.email ?? "").trim().toLowerCase();
  if (!name) return json({ error: "name is required" }, 400);
  if (!EMAIL_RE.test(email)) return json({ error: "a valid email is required" }, 400);

  const id = crypto.randomUUID();
  const verifyCode = crypto.randomUUID().replace(/-/g, "");
  const isGov = isGovDomain(email);

  await env.DB.prepare(
    `INSERT INTO acceptances (id, name, email, terms_version, terms_hash, is_gov, verify_code, created_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
  )
    .bind(id, name, email, TERMS_VERSION, await termsHash(), isGov ? 1 : 0, verifyCode, nowISO())
    .run();

  const verifyURL = `${env.SERVICE_URL}/verify?req=${id}&code=${verifyCode}`;
  await env.EMAIL.send({
    to: email,
    from: { email: env.FROM_EMAIL, name: env.FROM_NAME },
    subject: "Confirm your hop consent",
    text: `Hi ${name},\n\nConfirm you agreed to hop's terms (version ${TERMS_VERSION}) by opening this link:\n\n${verifyURL}\n\nIf you didn't request this, ignore this email.`,
    html: `<p>Hi ${escapeHtml(name)},</p><p>Confirm you agreed to hop's terms (version ${TERMS_VERSION}) by opening this link:</p><p><a href="${verifyURL}">${verifyURL}</a></p><p>If you didn't request this, ignore this email.</p>`,
  });

  return json({ request_id: id, is_gov: isGov });
}

async function handleVerify(url: URL, env: Env): Promise<Response> {
  const id = url.searchParams.get("req") ?? "";
  const code = url.searchParams.get("code") ?? "";

  const row = await env.DB.prepare(`SELECT * FROM acceptances WHERE id = ?`).bind(id).first<AcceptanceRow>();
  if (!row) return html(page("Link not valid", "<p>This confirmation link is not valid.</p>"), 404);
  if (row.verify_code !== code) return html(page("Link not valid", "<p>This confirmation link is not valid.</p>"), 403);

  if (row.redeemed_at) {
    return html(page("Already claimed", "<p>This consent has already been confirmed and its token already claimed. Each confirmation can only be redeemed once — contact the copyright holder if you need another.</p>"));
  }

  let redeemCode = row.redeem_code;
  if (!row.verified_at || !redeemCode) {
    redeemCode = randomCode();
    await env.DB.prepare(`UPDATE acceptances SET verified_at = ?, redeem_code = ? WHERE id = ?`)
      .bind(nowISO(), redeemCode, id)
      .run();
  }

  return html(
    page(
      "Email confirmed",
      `<p>Thanks, ${escapeHtml(row.name)} — your email is confirmed.</p>
       <p>Your one-time code:</p>
       <pre style="font-size:1.4rem;letter-spacing:.05em">${escapeHtml(redeemCode)}</pre>
       <p>Enter it at <a href="${env.SERVICE_URL}/redeem">${env.SERVICE_URL}/redeem</a> to get your token. It works once — after that, this code no longer works, so if you close the tab, come back to this same link to see it again.</p>
       <p><a href="/redeem?code=${encodeURIComponent(redeemCode)}">Go to /redeem now →</a></p>`
    )
  );
}

function handleRedeemForm(url: URL): Response {
  const prefill = url.searchParams.get("code") ?? "";
  return html(
    page(
      "Redeem your code",
      `<form method="POST" action="/redeem">
         <label style="display:block;margin-bottom:.5rem">Confirmation code</label>
         <input name="code" value="${escapeHtml(prefill)}" autocomplete="off" style="font-size:1.2rem;padding:.5rem;width:100%;max-width:20rem;box-sizing:border-box" placeholder="XXXXX-XXXXX">
         <button type="submit" style="display:block;margin-top:1rem;padding:.6rem 1.2rem;font-size:1rem">Redeem</button>
       </form>
       <p style="color:#666;font-size:.9rem;margin-top:1.5rem">This code came from the confirmation email hop sent you. Each code works exactly once.</p>`
    )
  );
}

async function handleRedeemSubmit(req: Request, env: Env): Promise<Response> {
  const form = await req.formData();
  const code = String(form.get("code") ?? "").trim().toUpperCase();
  if (!code) return html(page("Missing code", "<p>Enter a code.</p>"), 400);

  const row = await env.DB.prepare(`SELECT * FROM acceptances WHERE redeem_code = ?`).bind(code).first<AcceptanceRow>();
  if (!row) return html(page("Invalid code", "<p>That code isn't recognized. Check it against the confirmation email and try again.</p>"), 404);

  if (row.redeemed_at && row.token) {
    return html(page("Already used", "<p>This code has already been redeemed — each one works exactly once. If this wasn't you, the copyright holder can revoke and reissue.</p>"), 409);
  }

  const payload: Record<string, unknown> = { sub: row.name, iat: Math.floor(Date.now() / 1000) };
  if (TOKEN_TTL_SECONDS > 0) payload.exp = Math.floor(Date.now() / 1000) + TOKEN_TTL_SECONDS;
  if (row.is_gov) payload.gov = true;

  const token = await signToken(payload as any, env.HOP_LICENSE_SEED);

  // The UNIQUE index on redeem_code plus this being keyed off that same
  // code means a second concurrent redeem of the same code fails here
  // (SQLite serializes writes on a single D1 database), not after the
  // token has already been handed out twice.
  await env.DB.prepare(`UPDATE acceptances SET redeemed_at = ?, token = ? WHERE id = ? AND redeemed_at IS NULL`)
    .bind(nowISO(), token, row.id)
    .run();

  return html(
    page(
      "Token issued",
      `<p>Consent confirmed for <strong>${escapeHtml(row.name)}</strong>. This is the only time this token will be shown — copy it now.</p>
       <pre style="background:#f4f4f4;padding:1rem;border-radius:6px;overflow-x:auto">hop license install ${token}</pre>`
    )
  );
}

async function handleStatus(url: URL, env: Env): Promise<Response> {
  // Deliberately never returns the token — that only ever happens through
  // /redeem, exactly once. This exists so `hop license accept` can tell
  // someone "still waiting on your email" vs. "confirmed, go redeem it."
  const id = url.searchParams.get("req") ?? "";
  const row = await env.DB.prepare(`SELECT verified_at, redeemed_at, is_gov FROM acceptances WHERE id = ?`)
    .bind(id)
    .first<Pick<AcceptanceRow, "verified_at" | "redeemed_at" | "is_gov">>();
  if (!row) return json({ error: "unknown request_id" }, 404);

  const status = row.redeemed_at ? "redeemed" : row.verified_at ? "verified" : "pending";
  return json({ status, is_gov: !!row.is_gov });
}

function page(title: string, body: string): string {
  return `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>hop — ${escapeHtml(title)}</title>
<body style="font-family:-apple-system,system-ui,sans-serif;max-width:640px;margin:3rem auto;padding:0 1rem;line-height:1.5;color:#1a1a1a">
<h1 style="font-size:1.4rem">${escapeHtml(title)}</h1>
${body}
</body>`;
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}
