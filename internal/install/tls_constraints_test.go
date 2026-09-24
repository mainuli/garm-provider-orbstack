package install

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

func TestCertificateConstraintsAndPassphrase(t *testing.T) {
	files, err := certificateFiles(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := files["ca-key.pem"]; kept {
		t.Fatal("CA private key must not be persisted")
	}
	block, _ := pem.Decode(files["ca.pem"])
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"localhost": true, "host.orb.internal": true, "host.docker.internal": true}
	for _, d := range ca.PermittedDNSDomains {
		if !want[d] {
			t.Fatalf("unexpected permitted domain %q", d)
		}
	}
	if len(ca.PermittedDNSDomains) != 3 || len(ca.PermittedIPRanges) != 1 || !ca.PermittedIPRanges[0].IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("name constraints missing: %+v %+v", ca.PermittedDNSDomains, ca.PermittedIPRanges)
	}
	for _, ext := range ca.Extensions {
		if ext.Id.String() == "2.5.29.30" && ext.Critical {
			t.Fatal("name constraints must be non-critical")
		}
	}
}

// TestEnsureSecretsPassphraseLength asserts the GARM v0.2.1 database rule:
// the passphrase must be exactly 32 characters.
func TestEnsureSecretsPassphraseLength(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := pathsFor(home, "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := ensureSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.DatabasePassphrase) != 32 {
		t.Fatalf("database passphrase must be 32 chars (GARM v0.2.1 rule), got %d", len(creds.DatabasePassphrase))
	}
	if len(creds.JWTSecret) < 64 {
		t.Fatalf("jwt secret too short: %d", len(creds.JWTSecret))
	}
}
