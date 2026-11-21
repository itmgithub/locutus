// Package tlsutil configures which certificate authorities Locutus trusts for
// its outgoing HTTPS connections (OIDC discovery and token exchange, SAML
// metadata).
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// LoadPool returns the system certificate pool extended with the PEM
// certificates found in paths. Each path is a file or a directory; in a
// directory every *.pem, *.crt and *.cer file is read. It also returns the
// number of extra certificates.
func LoadPool(paths []string) (*x509.CertPool, int, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	added := 0
	for _, p := range paths {
		files, err := certFiles(p)
		if err != nil {
			return nil, 0, err
		}
		for _, f := range files {
			n, err := addPEM(pool, f)
			if err != nil {
				return nil, 0, err
			}
			added += n
		}
	}
	return pool, added, nil
}

func certFiles(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".pem", ".crt", ".cer":
			if !e.IsDir() {
				out = append(out, filepath.Join(p, e.Name()))
			}
		}
	}
	return out, nil
}

func addPEM(pool *x509.CertPool, file string) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", file, err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("%s: no PEM certificates found", file)
	}
	return n, nil
}

// SplitPaths splits a list of paths separated by ":" or ",".
func SplitPaths(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ',' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// InstallDefault makes pool the trust store of http.DefaultTransport, which
// the OIDC and SAML libraries use for their requests.
func InstallDefault(pool *x509.CertPool) error {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return errors.New("unexpected default HTTP transport")
	}
	cfg := t.TLSClientConfig
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		cfg = cfg.Clone()
	}
	cfg.RootCAs = pool
	t.TLSClientConfig = cfg
	return nil
}
