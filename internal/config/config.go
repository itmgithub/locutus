// Package config reads the runtime configuration from environment variables.
// Every variable of Locutus starts with LOCUTUS_; only TZ is read without
// prefix, because it is the standard time zone variable.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"itmon.nl/locutus/internal/tlsutil"
)

// Config holds all runtime settings of the application.
type Config struct {
	ListenAddr      string
	DataDir         string
	DBPath          string
	BaseURL         string // external URL, e.g. https://locutus.example.com (used for OIDC/SAML callbacks)
	TLSCert         string
	TLSKey          string
	TLSCA           string // optional chain (intermediate certificates) appended to the server certificate
	DefaultLanguage string

	// ExtraCAs are files or directories with PEM certificates that are
	// trusted, next to the system CAs, for outgoing HTTPS (OIDC, SAML).
	ExtraCAs []string

	// SecretKey (base64, 32 bytes) encrypts SSH keys, passphrases and
	// identity provider secrets. Without it a key file in the data dir is used.
	SecretKey     string
	SecretKeyFile string

	BorgBinary     string
	BorgBaseDir    string // borg cache, config and security data
	KnownHostsFile string // SSH host keys of repositories and clients (trust on first use)
}

// Prefix is put in front of the name of every Locutus environment variable.
const Prefix = "LOCUTUS_"

// Load builds a Config from the environment, applying defaults.
func Load() *Config {
	dataDir := env("DATA_DIR", "/data")
	return &Config{
		ListenAddr:      env("LISTEN_ADDR", ":2224"),
		DataDir:         dataDir,
		DBPath:          env("DB_PATH", filepath.Join(dataDir, "locutus.db")),
		BaseURL:         strings.TrimRight(env("BASE_URL", ""), "/"),
		TLSCert:         env("TLS_CERT", ""),
		TLSKey:          env("TLS_KEY", ""),
		TLSCA:           env("TLS_CA", ""),
		DefaultLanguage: env("DEFAULT_LANGUAGE", "en"),
		ExtraCAs:        tlsutil.SplitPaths(env("EXTRA_CA_CERTS", "")),
		SecretKey:       env("SECRET_KEY", ""),
		SecretKeyFile:   env("SECRET_KEY_FILE", filepath.Join(dataDir, "secret.key")),
		BorgBinary:      env("BORG_BINARY", "borg"),
		BorgBaseDir:     env("BORG_BASE_DIR", filepath.Join(dataDir, "borg")),
		KnownHostsFile:  env("KNOWN_HOSTS_FILE", filepath.Join(dataDir, "ssh", "known_hosts")),
	}
}

// ProtectedPaths are the paths in the container that hold the data, the
// secrets and the program of Locutus. A restore into the container never
// writes there.
func (c *Config) ProtectedPaths() []string {
	paths := []string{c.DataDir, filepath.Dir(c.DBPath), c.BorgBaseDir, filepath.Dir(c.KnownHostsFile),
		filepath.Dir(c.SecretKeyFile)}
	if c.TLSCert != "" {
		paths = append(paths, filepath.Dir(c.TLSCert), filepath.Dir(c.TLSKey))
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Dir(exe))
	}
	return paths
}

// TLSEnabled reports whether a certificate and key have been configured.
func (c *Config) TLSEnabled() bool {
	return c.TLSCert != "" && c.TLSKey != ""
}

// TLSConfig returns the TLS configuration, or nil when TLS is not configured
// and plain HTTP should be used.
func (c *Config) TLSConfig() (*tls.Config, error) {
	if !c.TLSEnabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		return nil, err
	}
	if c.TLSCA != "" {
		chain, err := os.ReadFile(c.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("read CA chain: %w", err)
		}
		for {
			var block *pem.Block
			block, chain = pem.Decode(chain)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("parse CA chain: %w", err)
			}
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// env returns the value of LOCUTUS_<key>, or def when it is not set.
func env(key, def string) string {
	if v := os.Getenv(Prefix + key); v != "" {
		return v
	}
	return def
}
