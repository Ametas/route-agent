package certs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/fsx"
	"github.com/Ametas/route-agent/internal/server/auth"
)

const secret = "s3cret-s3cret"

func TestSanitizeGolden(t *testing.T) {
	for in, want := range map[string]string{
		"A B": "a_b", "x+y": "x_plus_y", "*.Ex.com": "wildcard_.ex.com", "h:443": "h-443",
		"a..b": "ab", "....": "", "...": ".", " lead": "_lead",
	} {
		if got := SanitizeDomainForStorage(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaths(t *testing.T) {
	c, k := Paths("*.x.example.com", "")
	base := "/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory"
	if c != base+"/wildcard_.x.example.com/wildcard_.x.example.com.crt" || k != base+"/wildcard_.x.example.com/wildcard_.x.example.com.key" {
		t.Fatalf("%s %s", c, k)
	}
}

func newService(t *testing.T) (*Service, fsx.FS) {
	f := fsx.FS{Root: t.TempDir()}
	return &Service{Auth: auth.New(secret), FS: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, f
}

func md(v string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, v))
}

func TestGetManagedCertificate(t *testing.T) {
	s, f := newService(t)
	certPath, keyPath := Paths("*.x.example.com", "")
	fullchain := "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----\n"
	key := "-----BEGIN PRIVATE KEY-----\nKKK\n-----END PRIVATE KEY-----\n"
	_ = os.MkdirAll(filepath.Dir(f.Path(certPath)), 0o755)
	_ = os.WriteFile(f.Path(certPath), []byte(fullchain), 0o644)
	_ = os.WriteFile(f.Path(keyPath), []byte(key), 0o600)

	resp, err := s.GetManagedCertificate(md(secret), &agentpb.CertificateRequest{Domain: "*.x.example.com"})
	if err != nil || !resp.Success || resp.Message != "OK" || resp.FullchainPem != fullchain || resp.PrivateKeyPem != key {
		t.Fatalf("%v %+v", err, resp)
	}
	// Body secret alone is accepted (M|B).
	resp, _ = s.GetManagedCertificate(context.Background(), &agentpb.CertificateRequest{OrchestratorSecret: secret, Domain: "*.X.example.com"})
	if !resp.Success {
		t.Fatalf("body secret: %+v", resp)
	}
}

func TestGetManagedCertificateFailures(t *testing.T) {
	s, _ := newService(t)
	resp, err := s.GetManagedCertificate(md("wrong"), &agentpb.CertificateRequest{Domain: "a.com"})
	if err != nil || resp.Success || resp.Message != "Invalid orchestrator secret token." {
		t.Fatalf("auth: %v %+v", err, resp)
	}
	for _, d := range []string{"", "   ", "\t"} {
		resp, _ = s.GetManagedCertificate(md(secret), &agentpb.CertificateRequest{Domain: d})
		if resp.Success || resp.Message != "Missing domain parameter." {
			t.Fatalf("domain %q: %+v", d, resp)
		}
	}
	resp, err = s.GetManagedCertificate(md(secret), &agentpb.CertificateRequest{Domain: "*.missing.example.com"})
	certPath, _ := Paths("*.missing.example.com", "")
	if err != nil || resp.Success || resp.Message != "ENOENT: no such file or directory, open '"+certPath+"'" {
		t.Fatalf("missing: %v %+v", err, resp)
	}
}
