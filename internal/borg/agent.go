package borg

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// newKeyring returns an in-memory ssh-agent holding one private key. The key
// therefore never has to be written to disk.
func newKeyring(privateKey string) (agent.Agent, error) {
	raw, err := ssh.ParseRawPrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("repository SSH key: %w", err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: raw, Comment: "locutus"}); err != nil {
		return nil, err
	}
	return ring, nil
}

// serveAgent exposes an agent on a private unix socket for a local ssh
// process (started by borg). cleanup stops the agent and removes the socket.
func serveAgent(ag agent.Agent) (socket string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "locutus-agent-")
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	socket = filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				agent.ServeAgent(ag, conn)
			}()
		}
	}()
	return socket, func() {
		ln.Close()
		os.RemoveAll(dir)
	}, nil
}
