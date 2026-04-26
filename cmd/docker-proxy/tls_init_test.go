package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errPEMNotFound = errors.New("no PEM block found")

func TestGenerateCerts(t *testing.T) {
	dir := t.TempDir()
	if err := generateCerts(dir, "acm"); err != nil {
		t.Fatalf("generateCerts: %v", err)
	}

	// All expected files present.
	for _, name := range []string{"ca.pem", "ca.key", "server.crt", "server.key", "cert.pem", "key.pem"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		// Keys should be 0600.
		if strings.HasSuffix(name, ".key") {
			if st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", name, st.Mode().Perm())
			}
		}
	}

	// Server cert verifies against CA.
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load ca.pem")
	}

	srvBytes, _ := os.ReadFile(filepath.Join(dir, "server.crt"))
	srvCert, err := parsePEMCert(srvBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srvCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server cert does not verify against CA: %v", err)
	}

	// Server cert SAN includes 127.0.0.1, localhost, host.docker.internal.
	wantDNS := map[string]bool{"localhost": false, "host.docker.internal": false}
	for _, d := range srvCert.DNSNames {
		if _, ok := wantDNS[d]; ok {
			wantDNS[d] = true
		}
	}
	for d, found := range wantDNS {
		if !found {
			t.Errorf("server cert missing SAN DNS:%s", d)
		}
	}
	wantIP := false
	for _, ip := range srvCert.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			wantIP = true
			break
		}
	}
	if !wantIP {
		t.Errorf("server cert missing SAN IP:127.0.0.1")
	}

	// Client cert verifies against CA with ClientAuth usage.
	cliBytes, _ := os.ReadFile(filepath.Join(dir, "cert.pem"))
	cliCert, err := parsePEMCert(cliBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client cert does not verify with ClientAuth usage: %v", err)
	}

	// CN strings include the user, useful for proxy log/audit.
	for _, name := range []struct {
		cert *x509.Certificate
		role string
	}{{srvCert, "server"}, {cliCert, "client"}} {
		if !strings.Contains(name.cert.Subject.CommonName, "acm") {
			t.Errorf("%s cert CN %q does not contain user 'acm'", name.role, name.cert.Subject.CommonName)
		}
	}

	// loadServerTLS accepts the generated tree.
	cfg, err := loadServerTLS(dir)
	if err != nil {
		t.Fatalf("loadServerTLS: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want >= TLS1.2", cfg.MinVersion)
	}
}

func TestGenerateCertsIdempotentDir(t *testing.T) {
	dir := t.TempDir()
	if err := generateCerts(dir, "acm"); err != nil {
		t.Fatal(err)
	}
	// Calling generateCerts again on the same dir overwrites. The CLI guard
	// in main() checks for ca.pem before calling — that's where we get
	// idempotency. Here we verify we don't crash on rewrite.
	if err := generateCerts(dir, "acm"); err != nil {
		t.Fatalf("second generateCerts: %v", err)
	}
}

func parsePEMCert(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errPEMNotFound
	}
	return x509.ParseCertificate(block.Bytes)
}
