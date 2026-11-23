// Package remote runs commands on remote client systems over SSH, optionally
// forwarding an in-memory ssh-agent so that the client can reach the borg
// repository without having a repository key of its own.
package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Target is an SSH login on a remote system.
type Target struct {
	Address    string // host:port
	User       string
	PrivateKey string
}

// HostKeys verifies SSH host keys against a known_hosts file. Unknown hosts
// are trusted on first use and added to the file; changed keys are rejected.
type HostKeys struct {
	file string
	mu   sync.Mutex
}

// NewHostKeys uses (and creates) the given known_hosts file.
func NewHostKeys(file string) (*HostKeys, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &HostKeys{file: file}, nil
}

// File returns the path of the known_hosts file.
func (h *HostKeys) File() string { return h.file }

// Callback returns the host key callback for ssh.ClientConfig.
func (h *HostKeys) Callback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		check, err := knownhosts.New(h.file)
		if err != nil {
			return err
		}
		err = check(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			f, err := os.OpenFile(h.file, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			log.Printf("ssh: trusting new host key of %s (%s)", hostname, ssh.FingerprintSHA256(key))
			_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
			return err
		}
		if err != nil {
			return fmt.Errorf("host key verification failed for %s: %w", hostname, err)
		}
		return nil
	}
}

// Run executes cmd on the target. stdin is sent and then closed; stdout and
// stderr are both written to out. When forward is set the agent is forwarded
// to the remote side. It returns the remote exit code.
func Run(ctx context.Context, hk *HostKeys, t Target, cmd string, stdin io.Reader, out io.Writer, forward agent.Agent) (int, error) {
	w := &syncWriter{w: out}
	return RunIO(ctx, hk, t, cmd, stdin, w, w, forward)
}

// RunIO is Run with separate writers for the standard output and the
// standard error output.
func RunIO(ctx context.Context, hk *HostKeys, t Target, cmd string, stdin io.Reader, stdout, stderr io.Writer, forward agent.Agent) (int, error) {
	signer, err := ssh.ParsePrivateKey([]byte(t.PrivateKey))
	if err != nil {
		return -1, fmt.Errorf("client SSH key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hk.Callback(),
		Timeout:         20 * time.Second,
	}
	dialer := net.Dialer{Timeout: 20 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return -1, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Address, cfg)
	if err != nil {
		conn.Close()
		return -1, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()

	// Close the connection when the context ends, which aborts the command.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()

	if forward != nil {
		if err := agent.ForwardToAgent(client, forward); err != nil {
			return -1, err
		}
	}
	session, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer session.Close()
	if forward != nil {
		if err := agent.RequestAgentForwarding(session); err != nil {
			return -1, fmt.Errorf("agent forwarding refused by client: %w", err)
		}
	}
	session.Stdout, session.Stderr = stdout, stderr
	if stdin != nil {
		session.Stdin = stdin
	}
	err = session.Run(cmd)
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitStatus(), nil
	case ctx.Err() != nil:
		return -1, ctx.Err()
	default:
		return -1, err
	}
}

// syncWriter serialises writes from the stdout and stderr copiers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// errGotKey stops the handshake once FetchHostKey has the host key.
var errGotKey = errors.New("host key received")

// FetchHostKey returns the host key that the SSH server at address (host:port)
// presents now, without logging in.
func FetchHostKey(ctx context.Context, address string) (ssh.PublicKey, error) {
	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "locutus",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = key
			return errGotKey
		},
		Timeout: 20 * time.Second,
	}
	dialer := net.Dialer{Timeout: 20 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	_, _, _, err = ssh.NewClientConn(conn, address, cfg)
	if got != nil {
		return got, nil
	}
	if err == nil {
		err = errors.New("no host key received")
	}
	return nil, err
}

// Known returns the host keys recorded for address (host:port).
func (h *HostKeys) Known(address string) ([]ssh.PublicKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	check, err := knownhosts.New(h.file)
	if err != nil {
		return nil, err
	}
	// A key that is certainly not recorded: the error lists the known ones.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	err = check(address, &net.TCPAddr{IP: net.IPv4zero}, signer.PublicKey())
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return nil, err
	}
	var keys []ssh.PublicKey
	for _, k := range keyErr.Want {
		keys = append(keys, k.Key)
	}
	return keys, nil
}

// Replace records key as the only host key of address (host:port), for
// example after the admin has checked the new key of a recreated server.
func (h *HostKeys) Replace(address string, key ssh.PublicKey) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := os.ReadFile(h.file)
	if err != nil {
		return err
	}
	host := knownhosts.Normalize(address)
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l == "" {
			continue
		}
		if _, hosts, _, _, _, err := ssh.ParseKnownHosts([]byte(l)); err == nil && slices.Contains(hosts, host) {
			continue
		}
		out = append(out, l)
	}
	out = append(out, knownhosts.Line([]string{host}, key))
	tmp := h.file + ".new"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	log.Printf("ssh: host key of %s replaced by %s", host, ssh.FingerprintSHA256(key))
	return os.Rename(tmp, h.file)
}

// KnownHostsLine returns the known_hosts line of key for address
// (host:port), as OpenSSH expects it: "host key" for port 22, otherwise
// "[host]:port key".
func KnownHostsLine(address string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(address)}, key)
}
