// Package remote runs commands on remote client systems over SSH, optionally
// forwarding an in-memory ssh-agent so that the client can reach the borg
// repository without having a repository key of its own.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
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
