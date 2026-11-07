// Package testcerts generates throwaway mTLS material shaped like the orchestrator's
// (orch:src/shared/utils/certs.ts, spec A §3.2): an ECDSA P-256 CA, a node certificate with
// CN=<node UUID>, no SAN and EKU serverAuth, and client certificates. For tests only.
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Pair is a PEM certificate and its PKCS#8 PEM key.
type Pair struct {
	CertPEM, KeyPEM []byte
	Cert            *x509.Certificate
	key             *ecdsa.PrivateKey
}

// Set is everything a server/client test needs.
type Set struct {
	CA        Pair
	Server    Pair // CN = NodeUUID, EKU serverAuth, no SAN
	Client    Pair // CN = Route Orchestrator Client, EKU clientAuth
	Untrusted Pair // clientAuth, signed by another CA
	ServerEKU Pair // signed by CA but EKU serverAuth only (must be refused as a client)
	NodeUUID  string
}

// New generates a Set.
func New() (*Set, error) {
	uuid := "6f1c2d3e-4b5a-4c6d-8e7f-001122334455"
	ca, err := newCA("Route Orchestrator Root CA")
	if err != nil {
		return nil, err
	}
	other, err := newCA("UntrustedCA")
	if err != nil {
		return nil, err
	}
	srv, err := leaf(ca, uuid, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, err
	}
	cli, err := leaf(ca, "Route Orchestrator Client", x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, err
	}
	untr, err := leaf(other, "Route Orchestrator Client", x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, err
	}
	srvEKU, err := leaf(ca, "another-node", x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, err
	}
	return &Set{CA: ca, Server: srv, Client: cli, Untrusted: untr, ServerEKU: srvEKU, NodeUUID: uuid}, nil
}

// WriteServerFiles writes ca.crt, agent.crt and agent.key (0600) into dir and returns the paths.
func (s *Set) WriteServerFiles(dir string) (ca, cert, key string, err error) {
	ca, cert, key = filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key")
	if err = os.WriteFile(ca, s.CA.CertPEM, 0o644); err != nil {
		return
	}
	if err = os.WriteFile(cert, s.Server.CertPEM, 0o644); err != nil {
		return
	}
	err = os.WriteFile(key, s.Server.KeyPEM, 0o600)
	return
}

// Fingerprint is the orchestrator's pin format: SHA-256 of the leaf DER, "AB:CD:…" upper case.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

var serial atomic.Int64

func nextSerial() *big.Int { return big.NewInt(serial.Add(1) + 1) }

func newCA(cn string) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Route Orchestrator"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return Pair{}, err
	}
	return encode(der, key)
}

func leaf(ca Pair, cn string, eku x509.ExtKeyUsage) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Route Orchestrator Node"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return Pair{}, err
	}
	return encode(der, key)
}

func encode(der []byte, key *ecdsa.PrivateKey) (Pair, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Pair{}, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Pair{}, err
	}
	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}),
		Cert:    cert,
		key:     key,
	}, nil
}
