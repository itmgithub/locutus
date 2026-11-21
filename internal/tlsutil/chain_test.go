package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type issued struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

func issue(t *testing.T, cn string, parent *issued, ca bool) *issued {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: ca, BasicConstraintsValid: true}
	if ca {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	} else {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &issued{cert: cert, der: der, key: key}
}

// TestIncompleteChain reproduces an IdP that only sends its leaf
// certificate: trusting the missing intermediate makes the request work.
func TestIncompleteChain(t *testing.T) {
	root := issue(t, "Test Root", nil, true)
	inter := issue(t, "Test Intermediate", root, true)
	leaf := issue(t, "idp", inter, false)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.der}, PrivateKey: leaf.key}}}
	srv.StartTLS()
	defer srv.Close()

	get := func() error {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(); err == nil {
		t.Fatal("expected a verification error without the intermediate")
	}
	file := filepath.Join(t.TempDir(), "intermediate.pem")
	os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: inter.der}), 0o644)
	pool, _, err := LoadPool([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallDefault(pool); err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	if err := get(); err != nil {
		t.Fatalf("with the intermediate trusted: %v", err)
	}
}
