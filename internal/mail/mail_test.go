package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseAddresses(t *testing.T) {
	got, err := ParseAddresses(" a@example.com, Bob <b@example.com>; ")
	if err != nil || strings.Join(got, ",") != "a@example.com,b@example.com" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseAddresses(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: got %v, %v", got, err)
	}
	for _, bad := range []string{"nope", "a@example.com, x", "a@example.com\r\nBcc: c@example.com"} {
		if _, err := ParseAddresses(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMessageHeaders(t *testing.T) {
	m := Message{To: []string{"a@example.com"}, Subject: "Backup of web1\r\nBcc: evil@example.com", Body: "line 1\nline 2"}
	b, err := m.bytes("locutus@example.com")
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(string(b), "\r\n\r\n")
	if strings.Contains(head, "\r\nBcc:") {
		t.Errorf("header injection:\n%s", head)
	}
	if !strings.Contains(head, "From: Locutus <locutus@example.com>") || body != "line 1\r\nline 2" {
		t.Errorf("unexpected message:\n%s", b)
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Host: "smtp.example.com", Port: 587, Security: StartTLS, From: "locutus@example.com"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{
		{Host: "", Port: 587, Security: StartTLS, From: "a@example.com"},
		{Host: "h", Port: 0, Security: StartTLS, From: "a@example.com"},
		{Host: "h", Port: 25, Security: "none", From: "a@example.com"},
		{Host: "h", Port: 25, Security: SMTPS, From: "nope"},
		{Host: "h", Port: 25, Security: SMTPS, From: "a@example.com", Password: "x"},
	} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// testCert returns a self-signed certificate for 127.0.0.1 and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// fakeServer accepts one SMTP session and reports what it received.
type fakeServer struct {
	auth     string // offered AUTH mechanisms
	startTLS bool   // offer STARTTLS on a plain connection
	got      chan string
}

func (f *fakeServer) serve(t *testing.T, ln net.Listener, cert tls.Certificate) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	var log strings.Builder
	defer func() { f.got <- log.String() }()
	_, encrypted := conn.(*tls.Conn)
	r, w := bufio.NewReader(conn), conn
	reply := func(s string) { w.Write([]byte(s + "\r\n")) }
	reply("220 test")
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				reply("250 queued")
			} else {
				log.WriteString("DATA " + line + "\n")
			}
			continue
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		log.WriteString(cmd + "\n")
		switch cmd {
		case "EHLO":
			lines := []string{"250-test"}
			if f.startTLS && !encrypted {
				lines = append(lines, "250-STARTTLS")
			}
			if encrypted && f.auth != "" {
				lines = append(lines, "250-AUTH "+f.auth)
			}
			lines = append(lines, "250 8BITMIME")
			w.Write([]byte(strings.Join(lines, "\r\n") + "\r\n"))
		case "STARTTLS":
			reply("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
			if err := tc.Handshake(); err != nil {
				t.Errorf("handshake: %v", err)
				return
			}
			conn, r, w, encrypted = tc, bufio.NewReader(tc), tc, true
		case "AUTH":
			fields := strings.Fields(line)
			switch fields[1] {
			case "PLAIN":
				raw, _ := base64.StdEncoding.DecodeString(fields[2])
				log.WriteString("CRED " + strings.ReplaceAll(string(raw), "\x00", "|") + "\n")
			case "LOGIN":
				reply("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := r.ReadString('\n')
				reply("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := r.ReadString('\n')
				du, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
				dp, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
				log.WriteString("CRED " + string(du) + "|" + string(dp) + "\n")
			}
			reply("235 ok")
		case "DATA":
			inData = true
			reply("354 go")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

func runFake(t *testing.T, f *fakeServer, smtps bool) (*Config, func() string) {
	cert, pool := testCert(t)
	var ln net.Listener
	var err error
	if smtps {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.got = make(chan string, 1)
	go f.serve(t, ln, cert)
	port := ln.Addr().(*net.TCPAddr).Port
	sec := StartTLS
	if smtps {
		sec = SMTPS
	}
	c := &Config{Host: "127.0.0.1", Port: port, Security: sec, From: "locutus@example.com", RootCAs: pool}
	return c, func() string {
		select {
		case s := <-f.got:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("server did not finish")
			return ""
		}
	}
}

func TestSendStartTLSWithLogin(t *testing.T) {
	c, result := runFake(t, &fakeServer{startTLS: true, auth: "LOGIN"}, false)
	c.Username, c.Password = "user", "secret"
	err := Send(context.Background(), c, Message{To: []string{"a@example.com"}, Subject: "Test", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	got := result()
	for _, want := range []string{"STARTTLS\n", "CRED user|secret\n", "RCPT\n", "DATA hello\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "STARTTLS") > strings.Index(got, "AUTH") {
		t.Errorf("authenticated before STARTTLS:\n%s", got)
	}
}

func TestSendSMTPSWithPlain(t *testing.T) {
	c, result := runFake(t, &fakeServer{auth: "PLAIN LOGIN"}, true)
	c.Username, c.Password = "user", "secret"
	if err := Send(context.Background(), c, Message{To: []string{"a@example.com"}, Subject: "Test", Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	if got := result(); !strings.Contains(got, "CRED |user|secret\n") {
		t.Errorf("no PLAIN authentication in:\n%s", got)
	}
}

func TestSendRefusesWithoutStartTLS(t *testing.T) {
	c, result := runFake(t, &fakeServer{}, false)
	c.Username, c.Password = "user", "secret"
	err := Send(context.Background(), c, Message{To: []string{"a@example.com"}, Subject: "Test", Body: "hello"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("got %v", err)
	}
	if got := result(); strings.Contains(got, "AUTH") || strings.Contains(got, "MAIL") {
		t.Errorf("continued without encryption:\n%s", got)
	}
}
