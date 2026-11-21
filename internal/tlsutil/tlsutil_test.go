package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeCert(t *testing.T, file string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func TestLoadPool(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, filepath.Join(dir, "a.pem"))
	writeCert(t, filepath.Join(dir, "b.crt"))
	os.WriteFile(filepath.Join(dir, "README"), []byte("ignored"), 0o644)
	single := filepath.Join(t.TempDir(), "c.pem")
	writeCert(t, single)

	_, n, err := LoadPool([]string{dir, single})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, _, err := LoadPool([]string{filepath.Join(dir, "README")}); err == nil {
		t.Error("expected error for a file without certificates")
	}
	if _, _, err := LoadPool([]string{"/does/not/exist"}); err == nil {
		t.Error("expected error for a missing path")
	}
}

func TestSplitPaths(t *testing.T) {
	got := SplitPaths(" /certs/ca:/etc/extra.pem, /x ")
	if want := []string{"/certs/ca", "/etc/extra.pem", "/x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
