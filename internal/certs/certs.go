// Package certs reads Caddy-managed certificates: the Go port of src/utils/certStorage.ts and
// the GetManagedCertificate RPC (src/services/config.service.ts:455-487, spec A §9.5, §11).
//
// The on-disk layout mirrors certmagic's and is shared with the orchestrator's generators and
// with ConfigureCaddy (group B), so it must not change:
//
//	<CaddyCertBase>/<safe>/<safe>.crt and .key, safe = SanitizeDomainForStorage(domain)
package certs

import (
	"context"
	"log/slog"
	"path"
	"strings"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// CaddyCertBase is certmagic's Let's Encrypt production directory under Caddy's data dir.
const CaddyCertBase = "/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory"

// SanitizeDomainForStorage applies, in this order: " "→"_", "+"→"_plus_", "*"→"wildcard_",
// ":"→"-", ".."→"" (single left-to-right pass), lower case. No trimming and no other stripping
// (spec A §9.5, R-10: this differs from certmagic's KeyBuilder.Safe, kept for parity).
func SanitizeDomainForStorage(domain string) string {
	s := strings.ReplaceAll(domain, " ", "_")
	s = strings.ReplaceAll(s, "+", "_plus_")
	s = strings.ReplaceAll(s, "*", "wildcard_")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "..", "")
	return strings.ToLower(s)
}

// Paths returns the cert and key paths for domain under base (CaddyCertBase when empty).
func Paths(domain, base string) (certPath, keyPath string) {
	if base == "" {
		base = CaddyCertBase
	}
	safe := SanitizeDomainForStorage(domain)
	return path.Join(base, safe, safe+".crt"), path.Join(base, safe, safe+".key")
}

// Service implements GetManagedCertificate.
type Service struct {
	Auth *auth.Verifier
	FS   fsx.FS
	Log  *slog.Logger
	// Base overrides CaddyCertBase (tests); empty means CaddyCertBase.
	Base string
}

// GetManagedCertificate: auth M|B; empty or blank domain → "Missing domain parameter."; read
// cert and key verbatim; any read error → success:false with the Node-style fs message. Status
// is OK on every path. The private key goes over the wire by design: the orchestrator relays the
// front's wildcard to its egress nodes.
func (s *Service) GetManagedCertificate(ctx context.Context, req *agentpb.CertificateRequest) (*agentpb.CertificateResponse, error) {
	if !s.Auth.MetadataOrBody(ctx, req.GetOrchestratorSecret()) {
		s.Log.Warn("Unauthorized GetManagedCertificate request blocked")
		return &agentpb.CertificateResponse{Success: false, Message: auth.InvalidSecretMessage}, nil
	}
	domain := req.GetDomain()
	if strings.TrimSpace(domain) == "" {
		return &agentpb.CertificateResponse{Success: false, Message: "Missing domain parameter."}, nil
	}
	// The untrimmed domain builds the path, as in TS.
	certPath, keyPath := Paths(domain, s.Base)
	// TS read both in parallel (Promise.all: whichever rejection came first). Reading the cert
	// first makes the reported error deterministic when both are missing.
	cert, err := s.FS.ReadFile(certPath)
	if err != nil {
		return s.readFailure(domain, certPath, err), nil
	}
	key, err := s.FS.ReadFile(keyPath)
	if err != nil {
		return s.readFailure(domain, keyPath, err), nil
	}
	return &agentpb.CertificateResponse{
		Success:       true,
		Message:       "OK",
		FullchainPem:  string(cert),
		PrivateKeyPem: string(key),
	}, nil
}

func (s *Service) readFailure(domain, p string, err error) *agentpb.CertificateResponse {
	msg := fsx.NodeError("open", p, err)
	s.Log.Warn("Managed certificate not found or unreadable (issuance may still be in progress)", "err", msg, "domain", domain)
	return &agentpb.CertificateResponse{Success: false, Message: msg}
}
