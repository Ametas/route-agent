import path from 'path';

// Mirrors certmagic's own KeyBuilder.Safe() storage-key sanitizer (github.com/caddyserver/
// certmagic/storage.go) — Caddy's real ACME-issued certs land under exactly this on-disk
// convention, so GetManagedCertificate (reading a Xeon front's genuinely Caddy-issued wildcard
// cert) must replicate it precisely to find the right file. Reused verbatim for the egress-side
// static cert write (ConfigureCaddy's extra_cert_pem/extra_key_pem) purely for consistency, not
// because egress needs to match Caddy's ACME layout for any functional reason.
export function sanitizeDomainForStorage(domain: string): string {
  return domain
    .replace(/ /g, '_')
    .replace(/\+/g, '_plus_')
    .replace(/\*/g, 'wildcard_')
    .replace(/:/g, '-')
    .replace(/\.\./g, '')
    .toLowerCase();
}

export const CADDY_CERT_BASE =
  '/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory';

export function getCaddyCertPaths(domain: string, certBase: string = CADDY_CERT_BASE): { certPath: string; keyPath: string } {
  const safeDomain = sanitizeDomainForStorage(domain);
  const dir = path.join(certBase, safeDomain);
  return {
    certPath: path.join(dir, `${safeDomain}.crt`),
    keyPath: path.join(dir, `${safeDomain}.key`),
  };
}
