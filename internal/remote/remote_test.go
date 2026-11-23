package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKnownAndReplace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ssh", "known_hosts")
	hk, err := NewHostKeys(file)
	if err != nil {
		t.Fatal(err)
	}
	old, other, fresh := newKey(t), newKey(t), newKey(t)
	cb := hk.Callback()
	addr := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 2222}
	// Trust on first use.
	if err := cb("nas.example.com:2222", addr, old); err != nil {
		t.Fatal(err)
	}
	if err := cb("web.example.com:22", addr, other); err != nil {
		t.Fatal(err)
	}
	if keys, err := hk.Known("nas.example.com:2222"); err != nil || len(keys) != 1 || string(keys[0].Marshal()) != string(old.Marshal()) {
		t.Fatalf("known: %v %v", keys, err)
	}
	if keys, err := hk.Known("unknown.example.com:22"); err != nil || len(keys) != 0 {
		t.Fatalf("unknown: %v %v", keys, err)
	}
	// A changed key is refused until it is replaced.
	if err := cb("nas.example.com:2222", addr, fresh); err == nil {
		t.Fatal("changed host key accepted")
	}
	if err := hk.Replace("nas.example.com:2222", fresh); err != nil {
		t.Fatal(err)
	}
	if err := cb("nas.example.com:2222", addr, fresh); err != nil {
		t.Fatalf("replaced key refused: %v", err)
	}
	if err := cb("web.example.com:22", addr, other); err != nil {
		t.Fatalf("other host lost: %v", err)
	}
	data, _ := os.ReadFile(file)
	if strings.Count(string(data), "[nas.example.com]:2222") != 1 {
		t.Errorf("known_hosts:\n%s", data)
	}
	if l := KnownHostsLine("web.example.com:22", other); !strings.HasPrefix(l, "web.example.com ssh-ed25519 ") {
		t.Errorf("line for port 22: %q", l)
	}
	if l := KnownHostsLine("nas.example.com:2222", fresh); !strings.HasPrefix(l, "[nas.example.com]:2222 ssh-ed25519 ") {
		t.Errorf("line for port 2222: %q", l)
	}
}
