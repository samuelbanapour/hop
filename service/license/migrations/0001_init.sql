-- Acceptance records: one row per consent request. This table IS the
-- irrefutable record — it is written once at accept time, updated once at
-- verify time and once at redeem time, and never edited by the person it
-- describes.
--
-- The token itself is disclosed exactly once, through exactly one channel
-- (the /redeem webpage): verify_code proves control of the email address;
-- redeem_code, shown only after that, is a separate single-use code that
-- the /redeem page exchanges for the signed token. Once redeemed_at is
-- set, the redeem code — and the token it produced — can never be shown
-- through this flow again.
CREATE TABLE acceptances (
  id            TEXT PRIMARY KEY,   -- opaque request id
  name          TEXT NOT NULL,
  email         TEXT NOT NULL,
  terms_version TEXT NOT NULL,
  terms_hash    TEXT NOT NULL,      -- sha256 of the exact terms text shown
  is_gov        INTEGER NOT NULL DEFAULT 0,
  verify_code   TEXT NOT NULL,      -- required to complete /verify (proves email control)
  redeem_code   TEXT,               -- set at verify time; entered on /redeem
  created_at    TEXT NOT NULL,      -- ISO 8601, to the second, UTC
  verified_at   TEXT,               -- set once, never cleared
  redeemed_at   TEXT,               -- set once, never cleared — the one-time gate
  token         TEXT                -- the signed token, set at redeem time
);

CREATE INDEX idx_acceptances_email ON acceptances(email);
CREATE UNIQUE INDEX idx_acceptances_redeem_code ON acceptances(redeem_code);
