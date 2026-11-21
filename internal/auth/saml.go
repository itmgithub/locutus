package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"

	"itmon.nl/locutus/internal/models"
)

// GenerateSAMLKeyPair creates a self-signed certificate and RSA key that
// Locutus uses as SAML service provider.
func GenerateSAMLKeyPair(commonName string) (certPEM, keyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"Locutus"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(key)}))
	return certPEM, keyPEM, nil
}

func mustPKCS8(key *rsa.PrivateKey) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return b
}

// NewSAMLServiceProvider builds the service provider for a configured IdP.
// With withIDP the IdP metadata is loaded (from the URL or the stored XML);
// without it only the SP metadata can be produced.
func NewSAMLServiceProvider(ctx context.Context, cfg models.SAMLConfig, metadataURL, acsURL string, withIDP bool) (*saml.ServiceProvider, error) {
	pair, err := tls.X509KeyPair([]byte(cfg.SPCertificatePEM), []byte(cfg.SPKeyPEM))
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("SP key is not an RSA key")
	}
	mdURL, err := url.Parse(metadataURL)
	if err != nil {
		return nil, err
	}
	acs, err := url.Parse(acsURL)
	if err != nil {
		return nil, err
	}
	sp := &saml.ServiceProvider{
		EntityID:    cfg.EntityID,
		Key:         key,
		Certificate: cert,
		MetadataURL: *mdURL,
		AcsURL:      *acs,
	}
	if !withIDP {
		return sp, nil
	}
	switch {
	case strings.TrimSpace(cfg.IDPMetadataXML) != "":
		sp.IDPMetadata, err = samlsp.ParseMetadata([]byte(cfg.IDPMetadataXML))
	case cfg.IDPMetadataURL != "":
		var u *url.URL
		if u, err = url.Parse(cfg.IDPMetadataURL); err == nil {
			client := &http.Client{Timeout: 15 * time.Second}
			sp.IDPMetadata, err = samlsp.FetchMetadata(ctx, client, *u)
		}
	default:
		err = errors.New("no IdP metadata configured")
	}
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// SAMLAuthRequestURL creates an AuthnRequest using the HTTP-Redirect binding
// and returns the redirect URL and the request id.
func SAMLAuthRequestURL(sp *saml.ServiceProvider, relayState string) (string, string, error) {
	location := sp.GetSSOBindingLocation(saml.HTTPRedirectBinding)
	if location == "" {
		return "", "", errors.New("IdP does not support the HTTP-Redirect binding")
	}
	req, err := sp.MakeAuthenticationRequest(location, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", "", err
	}
	u, err := req.Redirect(relayState, sp)
	if err != nil {
		return "", "", err
	}
	return u.String(), req.ID, nil
}

// SAMLIdentity extracts the user information from a validated assertion.
func SAMLIdentity(a *saml.Assertion, usernameAttribute string) *Identity {
	attrs := map[string]string{}
	for _, st := range a.AttributeStatements {
		for _, attr := range st.Attributes {
			if len(attr.Values) == 0 {
				continue
			}
			attrs[attr.Name] = attr.Values[0].Value
			if attr.FriendlyName != "" {
				attrs[attr.FriendlyName] = attr.Values[0].Value
			}
		}
	}
	get := func(names ...string) string {
		for _, n := range names {
			if v := attrs[n]; v != "" {
				return v
			}
		}
		return ""
	}
	id := &Identity{
		Email: get("email", "mail", "emailaddress",
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", "urn:oid:0.9.2342.19200300.100.1.3"),
		DisplayName: get("displayName", "displayname", "name",
			"http://schemas.microsoft.com/identity/claims/displayname", "urn:oid:2.16.840.1.113730.3.1.241"),
	}
	transient := false
	if a.Subject != nil && a.Subject.NameID != nil {
		id.Subject = a.Subject.NameID.Value
		transient = a.Subject.NameID.Format == "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
	}
	if usernameAttribute != "" {
		id.Username = attrs[usernameAttribute]
	}
	if id.Username == "" {
		id.Username = firstNonEmpty(get("uid", "username", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name"), id.Subject)
	}
	// A transient NameID changes on every login, so it cannot identify the user.
	if id.Subject == "" || (transient && id.Username != id.Subject) {
		id.Subject = id.Username
	}
	return id
}
