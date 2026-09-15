// govDomainSuffixes is a heuristic, NOT an authoritative registry. It flags
// an email domain as a "registered government" domain only when it ends in
// one of these well-known official-government suffixes. This is honest
// about its own limits: many real government bodies (e.g. Germany's
// bund.de, plenty of municipal domains worldwide) don't use a suffix this
// list recognizes, and a determined bad actor could register a domain that
// happens to match by coincidence. It errs toward under-granting the
// exemption rather than over-granting it — anything not on this list still
// goes through the normal consent flow, it just doesn't get the "gov" flag.
export const govDomainSuffixes = [
  // United States — federal, state, tribal, military
  ".gov",
  ".mil",
  // Widely used national government suffixes
  ".gov.uk",
  ".gc.ca", // Canada
  ".gouv.fr", // France
  ".gov.au",
  ".govt.nz",
  ".gov.in",
  ".go.jp",
  ".gov.sg",
  ".gov.za",
  ".gov.br",
  ".gob.mx",
  ".gob.es",
  ".gov.ie",
  ".gov.il",
  ".gov.ae",
  ".gov.sa",
  ".gov.cn",
  ".gov.hk",
  ".gov.tw",
  ".gov.kr",
  ".gov.ph",
  ".gov.my",
  ".gov.pk",
  ".gov.bd",
  ".gov.eg",
  ".gov.ng",
  ".gov.gr",
  ".gov.pl",
  ".gov.pt",
  ".gov.it",
  ".admin.ch", // Switzerland
  ".overheid.nl", // Netherlands
  ".riik.ee", // Estonia
];

export function isGovDomain(email: string): boolean {
  const at = email.lastIndexOf("@");
  if (at < 0) return false;
  const domain = email.slice(at + 1).toLowerCase().trim();
  return govDomainSuffixes.some((suf) => domain === suf.slice(1) || domain.endsWith(suf));
}
