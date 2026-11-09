package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadTLSAcceptsNegativeSerial: the orchestrator issued agent certificates with a random 8-byte
// serial whose sign bit was not cleared (fixed 2026-11-09), so a large part of the fleet holds a
// certificate with a negative serial. Go 1.23+ rejects those unless x509negativeserial=1 (go.mod);
// without it the Go agent crash-looped on the first test node and the bridge guard rolled it back.
func TestLoadTLSAcceptsNegativeSerial(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialBytes := []byte{0x7f, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07}
	tmpl := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(serialBytes),
		Subject:      pkix.Name{CommonName: "node-uuid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the serial's first byte to 0x81: the same length, now a negative INTEGER. The signature no
	// longer matches, which neither parsing nor the key-pair check looks at.
	needle := append([]byte{0x02, byte(len(serialBytes))}, serialBytes...)
	at := bytes.Index(der, needle)
	if at < 0 {
		t.Fatal("serial not found in the DER")
	}
	negative := bytes.Clone(der)
	negative[at+2] = 0x81

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, kind string, b []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	paths := TLSPaths{
		CA:   write("ca.crt", "CERTIFICATE", der),
		Cert: write("agent.crt", "CERTIFICATE", negative),
		Key:  write("agent.key", "EC PRIVATE KEY", keyDER),
	}

	cfg, err := LoadTLS(paths)
	if err != nil {
		t.Fatalf("LoadTLS refused a certificate with a negative serial: %v", err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse served leaf: %v", err)
	}
	if leaf.SerialNumber.Sign() >= 0 {
		t.Fatalf("test setup: serial %s is not negative", leaf.SerialNumber)
	}
}
