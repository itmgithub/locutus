// Package mail sends the output of jobs by email through an SMTP server,
// over STARTTLS or SMTPS, optionally with a username and password.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Connection security.
const (
	StartTLS = "starttls" // plain connection that is upgraded with STARTTLS (usually port 587)
	SMTPS    = "smtps"    // TLS from the start (usually port 465)
)

// Securities lists the supported connection securities.
var Securities = []string{StartTLS, SMTPS}

// DefaultPort returns the usual port for a connection security.
func DefaultPort(security string) int {
	if security == SMTPS {
		return 465
	}
	return 587
}

// timeout bounds connecting to and talking with the mail server.
const timeout = 60 * time.Second

// Config describes the mail server.
type Config struct {
	Host     string
	Port     int
	Security string // StartTLS or SMTPS
	Username string // optional; without it no authentication is done
	Password string
	From     string // sender address
	// RootCAs are the trusted certificates; nil uses those of the system.
	RootCAs *x509.CertPool
}

// Configured reports whether a mail server has been set up.
func (c *Config) Configured() bool { return c != nil && c.Host != "" && c.From != "" }

// Validate checks the configuration.
func (c *Config) Validate() error {
	switch {
	case c.Host == "" || strings.ContainsAny(c.Host, " /\r\n"):
		return errors.New("invalid host")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("invalid port")
	case c.Security != StartTLS && c.Security != SMTPS:
		return errors.New("invalid connection security")
	case c.Password != "" && c.Username == "":
		return errors.New("a password needs a username")
	}
	if _, err := ParseAddress(c.From); err != nil {
		return fmt.Errorf("invalid sender address: %w", err)
	}
	return nil
}

// ParseAddress checks one email address and returns it without a display name.
func ParseAddress(s string) (string, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

// ParseAddresses checks a list of email addresses separated by commas or
// semicolons and returns them normalised; an empty text gives no addresses.
func ParseAddresses(s string) ([]string, error) {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		a, err := ParseAddress(part)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", part, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Message is a plain text email.
type Message struct {
	To      []string
	Subject string
	Body    string
}

// Send delivers the message through the mail server.
func Send(ctx context.Context, c *Config, m Message) error {
	if !c.Configured() {
		return errors.New("no mail server configured")
	}
	if len(m.To) == 0 {
		return errors.New("no recipients")
	}
	from, err := ParseAddress(c.From)
	if err != nil {
		return fmt.Errorf("sender address: %w", err)
	}
	data, err := m.bytes(from)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	tlsConfig := &tls.Config{ServerName: c.Host, RootCAs: c.RootCAs, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if c.Security == SMTPS {
		conn, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Hello(helloName()); err != nil {
		return err
	}
	if c.Security == StartTLS {
		// Never fall back to an unencrypted connection.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if c.Username != "" {
		if err := authenticate(client, c); err != nil {
			return fmt.Errorf("authentication: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, to := range m.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("recipient %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// authenticate logs in with AUTH PLAIN, or AUTH LOGIN when the server only
// offers that (as some Microsoft servers do). The connection is always
// encrypted at this point.
func authenticate(client *smtp.Client, c *Config) error {
	ok, mechs := client.Extension("AUTH")
	if !ok {
		return errors.New("the mail server does not offer authentication")
	}
	offered := strings.Fields(strings.ToUpper(mechs))
	has := func(m string) bool {
		for _, o := range offered {
			if o == m {
				return true
			}
		}
		return false
	}
	switch {
	case has("PLAIN"):
		return client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host))
	case has("LOGIN"):
		return client.Auth(&loginAuth{username: c.Username, password: c.Password})
	}
	return fmt.Errorf("no supported authentication method (server offers %s)", mechs)
}

// loginAuth implements AUTH LOGIN.
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("refusing to send the password over an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected server challenge %q", fromServer)
}

// helloName is the name Locutus introduces itself with.
func helloName() string {
	if h, err := os.Hostname(); err == nil && h != "" && !strings.ContainsAny(h, " \r\n") {
		return h
	}
	return "locutus"
}

// headerValue removes line breaks, so a value can never add a header.
func headerValue(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// bytes renders the message with its headers.
func (m Message) bytes(from string) ([]byte, error) {
	for _, to := range m.To {
		if strings.ContainsAny(to, "\r\n") {
			return nil, errors.New("invalid recipient")
		}
	}
	id := make([]byte, 12)
	rand.Read(id)
	domain := from[strings.LastIndex(from, "@")+1:]

	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Locutus <%s>\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(m.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", headerValue(m.Subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
