package install

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

type credentials struct {
	JWTSecret          string `json:"jwt_secret"`
	DatabasePassphrase string `json:"database_passphrase"`
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func certificateFiles(now time.Time) (map[string][]byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	caSerial, err := serial()
	if err != nil {
		return nil, err
	}
	serverSerial, err := serial()
	if err != nil {
		return nil, err
	}
	ca := &x509.Certificate{
		SerialNumber: caSerial, Subject: pkix.Name{CommonName: "GARM OrbStack installation CA"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// The CA is trusted by every runner clone and offered for explicit
		// browser trust; constrain it to the installation's own endpoints
		// so a stolen CA key could not mint certificates for other names.
		// NameConstraints are non-critical for compatibility.
		PermittedDNSDomains:         []string{"localhost", "host.orb.internal", "host.docker.internal"},
		PermittedDNSDomainsCritical: false,
		PermittedIPRanges:           []*net.IPNet{{IP: net.ParseIP("127.0.0.1").To4(), Mask: net.CIDRMask(32, 32)}},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	server := &x509.Certificate{
		SerialNumber: serverSerial, Subject: pkix.Name{CommonName: "GARM OrbStack controller"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(2, 0, 0),
		DNSNames:    []string{"localhost", "host.orb.internal", "host.docker.internal"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	encode := func(kind string, data []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}) }
	// The CA private key is deliberately NOT persisted: nothing needs it
	// again (the server certificate covers the installation's lifetime and
	// renewal means a fresh CA plus re-registered templates), and keeping
	// it would let anyone reading it mint constrained-but-attacker-chosen
	// certificates.
	return map[string][]byte{
		"ca.pem":         encode("CERTIFICATE", caDER),
		"server.pem":     encode("CERTIFICATE", serverDER),
		"server-key.pem": encode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey)),
	}, nil
}

func ensureSecrets(p Paths) (credentials, error) {
	if _, err := os.Lstat(p.Secrets); err == nil {
		return loadSecrets(p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return credentials{}, err
	}
	if err := privateDir(filepath.Dir(p.Secrets)); err != nil {
		return credentials{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(p.Secrets), ".garm-secrets-*")
	if err != nil {
		return credentials{}, err
	}
	defer os.RemoveAll(tmp)
	files, err := certificateFiles(time.Now())
	if err != nil {
		return credentials{}, err
	}
	jwt, err := randomHex(32)
	if err != nil {
		return credentials{}, err
	}
	// GARM v0.2.1 rejects database passphrases whose length is not exactly
	// 32 characters (aes-256 key material); randomHex(16) yields 32 hex
	// characters that also clear its zxcvbn strength floor.
	passphrase, err := randomHex(16)
	if err != nil {
		return credentials{}, err
	}
	creds := credentials{JWTSecret: jwt, DatabasePassphrase: passphrase}
	files["credentials.json"], err = json.Marshal(creds)
	if err != nil {
		return credentials{}, err
	}
	for name, contents := range files {
		if err := atomicFile(filepath.Join(tmp, name), contents, 0o600); err != nil {
			return credentials{}, err
		}
	}
	if err := os.Mkdir(filepath.Join(tmp, "cli-home"), 0o700); err != nil {
		return credentials{}, err
	}
	if err := os.Rename(tmp, p.Secrets); err != nil {
		return credentials{}, err
	}
	return creds, syncDir(filepath.Dir(p.Secrets))
}

func loadSecrets(p Paths) (credentials, error) {
	var creds credentials
	for _, name := range []string{"ca.pem", "server.pem", "server-key.pem", "credentials.json"} {
		info, err := os.Lstat(filepath.Join(p.Secrets, name))
		if err != nil {
			return creds, fmt.Errorf("incomplete secrets (never regenerated automatically): %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return creds, fmt.Errorf("unsafe secret file %s", name)
		}
	}
	data, err := os.ReadFile(filepath.Join(p.Secrets, "credentials.json"))
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, err
	}
	if len(creds.JWTSecret) != 64 || len(creds.DatabasePassphrase) != 32 {
		return creds, errors.New("invalid preserved encryption secrets")
	}
	pair, err := tls.LoadX509KeyPair(p.Certificate, p.Key)
	if err != nil {
		return creds, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return creds, err
	}
	roots, err := installationRoots(p.CA)
	if err != nil {
		return creds, err
	}
	for _, name := range []string{"localhost", "127.0.0.1", "host.orb.internal", "host.docker.internal"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			return creds, fmt.Errorf("installation certificate %s: %w", name, err)
		}
	}
	return creds, nil
}

func installationRoots(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("installation CA has no valid PEM certificate")
	}
	return pool, nil
}
