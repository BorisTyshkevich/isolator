package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// generateCerts creates a per-user TLS cert tree at certDir:
//
//	ca.pem        — CA cert (also: client trusts this for server, server trusts for client)
//	ca.key        — CA private key (kept root-only, used to sign client/server)
//	server.crt    — server cert, SAN: 127.0.0.1, localhost, host.docker.internal
//	server.key    — server private key
//	cert.pem      — client cert (Docker CLI convention)
//	key.pem       — client private key (Docker CLI convention)
//
// The Docker daemon convention is: DOCKER_CERT_PATH points at a dir with
// ca.pem, cert.pem, key.pem. testcontainers reads from that path and
// bind-mounts it into Ryuk via DOCKER_CERT_PATH propagation.
//
// Files are written world-readable (0644) for cert files, 0600 for keys.
// The caller is responsible for chowning the directory to the target user
// after this returns — generateCerts itself does not chown (the proxy runs
// as root and the directory may not yet exist with the right ownership).
func generateCerts(certDir, user string) error {
	if certDir == "" {
		return fmt.Errorf("certDir is empty")
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", certDir, err)
	}

	notBefore := time.Now().Add(-1 * time.Hour) // small backdate to tolerate clock skew
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)

	// CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: fmt.Sprintf("isolator-docker-proxy-ca:%s", user)},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign CA: %w", err)
	}
	if err := writePEM(filepath.Join(certDir, "ca.pem"), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}
	if err := writeKeyPEM(filepath.Join(certDir, "ca.key"), caKey, 0o600); err != nil {
		return err
	}

	// Server cert
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("isolator-docker-proxy:%s", user)},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "host.docker.internal"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caTmpl, &srvKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign server cert: %w", err)
	}
	if err := writePEM(filepath.Join(certDir, "server.crt"), "CERTIFICATE", srvDER, 0o644); err != nil {
		return err
	}
	if err := writeKeyPEM(filepath.Join(certDir, "server.key"), srvKey, 0o600); err != nil {
		return err
	}

	// Client cert
	cliKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate client key: %w", err)
	}
	cliTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("isolator-docker-proxy-client:%s", user)},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caTmpl, &cliKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign client cert: %w", err)
	}
	if err := writePEM(filepath.Join(certDir, "cert.pem"), "CERTIFICATE", cliDER, 0o644); err != nil {
		return err
	}
	if err := writeKeyPEM(filepath.Join(certDir, "key.pem"), cliKey, 0o600); err != nil {
		return err
	}

	return nil
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: kind, Bytes: der})
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey, mode os.FileMode) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC key: %w", err)
	}
	return writePEM(path, "EC PRIVATE KEY", der, mode)
}
