package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// TLSPaths are the three mTLS files (CA_CERT_PATH, AGENT_CERT_PATH, AGENT_KEY_PATH).
type TLSPaths struct {
	CA, Cert, Key string
}

// LoadTLS is getGrpcServerCredentials (src/index.ts:75-116, spec A §3.2): all three files must
// exist; the client certificate is required and verified against the CA (ExtKeyUsageClientAuth,
// equivalent to OpenSSL's SSL-client purpose); no identity check beyond the chain; the leaf in
// agent.crt is served byte for byte (the orchestrator pins its SHA-256 and checks CN = node UUID).
// There is no insecure fallback, by design: the secret header must never travel in plaintext.
//
// Unlike grpc-js, which parsed lazily, invalid PEM content fails here, at startup — the TS agent
// failed at bindAsync for the same input, so the outcome (exit 1) is the same.
func LoadTLS(p TLSPaths) (*tls.Config, error) {
	caOK, certOK, keyOK := exists(p.CA), exists(p.Cert), exists(p.Key)
	if !caOK || !certOK || !keyOK {
		return nil, fmt.Errorf("mTLS certificates are required but unavailable "+
			"(CA: %s, cert: %s, key: %s). "+
			"Agent refuses to start in an unencrypted/unauthenticated transport mode instead of silently degrading "+
			"(x-orchestrator-secret must never travel in plaintext). Reissue node certificates via the orchestrator "+
			"install flow (re-run install.sh on this VPS with a valid --secret to re-enroll and receive fresh CA/"+
			"agent cert+key), or manually place them at the CA_CERT_PATH/AGENT_CERT_PATH/AGENT_KEY_PATH configured "+
			"in .env, then restart the route-agent service.",
			state(caOK, p.CA), state(certOK, p.Cert), state(keyOK, p.Key))
	}
	loadErr := func(err error) error {
		return fmt.Errorf("mTLS certificates were found but failed to load (CA: %s, cert: %s, "+
			"key: %s). Underlying error: %s. Agent refuses to start in an unencrypted/"+
			"unauthenticated transport mode. Verify the certificate/key files are valid PEM material issued for "+
			"this node, or reissue them via the orchestrator install flow (re-run install.sh with a valid "+
			"--secret), then restart the route-agent service.", p.CA, p.Cert, p.Key, err.Error())
	}
	caPEM, err := os.ReadFile(p.CA)
	if err != nil {
		return nil, loadErr(err)
	}
	certPEM, err := os.ReadFile(p.Cert)
	if err != nil {
		return nil, loadErr(err)
	}
	keyPEM, err := os.ReadFile(p.Key)
	if err != nil {
		return nil, loadErr(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, loadErr(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, loadErr(errors.New("no CA certificate found in " + p.CA))
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12, // Node's default range is TLS 1.2–1.3
	}, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func state(ok bool, path string) string {
	if ok {
		return "ok"
	}
	return "MISSING at " + path
}
