# hop-license

The self-service half of hop's consent gate. `hop license accept` talks to
this; the manual path (`tools/hoplicense issue`) doesn't need it at all —
both produce tokens that verify against the same public key.

## What it does

1. `POST /accept` — records name + email + which terms version, emails a
   confirmation link.
2. `GET /verify?req=&code=` — the email link. Confirms email control and
   shows a one-time code plus a link to `/redeem`.
3. `GET /redeem` / `POST /redeem` — the webpage where that code is entered.
   The first (and only) successful redemption mints the signed token and
   shows it once. A second attempt with the same code is refused.
4. `GET /status?req=` — lets the CLI report pending / verified / redeemed.
   Never returns the token — that only ever happens through `/redeem`.

The `acceptances` D1 table is the durable, irrefutable record: who, what
they agreed to (exact terms hash), and when, written once at accept time
and never edited afterward.

## Deploy it

You need a Cloudflare account and a domain you can send email from.

```bash
npm install

# 1. D1 database
npx wrangler d1 create hop-license
# paste the printed database_id into wrangler.jsonc's d1_databases[0].database_id

npx wrangler d1 migrations apply hop-license --remote

# 2. Email sending — FROM_EMAIL in wrangler.jsonc must be on this domain
npx wrangler email sending enable yourdomain.com

# 3. Signing key — the seed from the SAME keypair internal/core/license.go
#    already has the public half of (see tools/hoplicense). Never let this
#    secret touch a file that gets committed.
go run ../../tools/hoplicense seed -key /path/to/hoplicense.key | npx wrangler secret put HOP_LICENSE_SEED

# 4. Deploy
npx wrangler deploy
```

After the first deploy, update `SERVICE_URL` in `wrangler.jsonc` to the real
`*.workers.dev` URL wrangler printed (or your custom domain/route), then
`wrangler deploy` again — every link this service emails or renders is
built from that value.

Finally, point hop's build at it: set `internal/core/license.go`'s
`licenseServiceURLDefault` to the same URL (or just tell people to export
`HOP_LICENSE_SERVICE_URL` themselves) before shipping a release that
advertises `hop license accept`.

## Local dev

```bash
npm install
npx wrangler d1 migrations apply hop-license --local
echo "HOP_LICENSE_SEED=$(go run ../../tools/hoplicense seed -key /path/to/hoplicense.key)" > .dev.vars
npx wrangler dev
```

Then point the CLI at it: `HOP_LICENSE_SERVICE_URL=http://localhost:8787 hop license accept`.
Emails aren't actually delivered in local dev — read the confirmation code
straight out of the local D1 database instead:

```bash
npx wrangler d1 execute hop-license --local \
  --command "SELECT id, redeem_code FROM acceptances ORDER BY created_at DESC LIMIT 1"
```
