package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// System is the file system the configuration is applied to. Root is "/"
// in the container; tests use a temporary directory.
type System struct {
	Root     string
	Chown    func(path string, uid, gid int) error
	LookPath func(name string) (string, error) // full path of a command, as sudo finds it
}

// Container returns the System of the running container.
func Container() *System {
	return &System{Root: "/", Chown: os.Lchown, LookPath: sudoLookPath}
}

// sudoLookPath finds a command in the secure_path that the sudo rules set,
// so that the rules allow exactly the command sudo runs.
func sudoLookPath(name string) (string, error) {
	for _, dir := range strings.Split(secureSudoPath, ":") {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

// Result describes what Apply did, for the log.
type Result struct {
	HostKey            string // public host key, "ssh-ed25519 AAAA..."
	HostKeyFingerprint string
	NewHostKey         bool
}

// Apply writes the configuration of sshd, sudo and ssh, creates the users
// and directories and, the first time, the SSH host key. It is run at every
// start and for every configuration that Locutus pushes. sshd reads
// authorized_keys at every login, so a pushed configuration takes effect
// without restarting it.
func (s *System) Apply(c *Config) (*Result, error) {
	if err := s.ensureUsers(c); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{{"/var/empty", 0o755}, {"/run", 0o755}, {AuthorizedKeysDir, 0o755}, {c.ConfigDir + "/ssh", 0o755}} {
		if err := os.MkdirAll(s.path(d.path), d.mode); err != nil {
			return nil, err
		}
		if err := os.Chmod(s.path(d.path), d.mode); err != nil {
			return nil, err
		}
	}
	home := s.path("/home/" + sshUser)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	if err := s.Chown(home, c.UID, c.GID); err != nil {
		return nil, err
	}

	res := &Result{}
	pub, created, err := s.hostKey(c.ConfigDir + "/ssh/" + hostKeyName)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	res.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	res.HostKeyFingerprint, res.NewHostKey = ssh.FingerprintSHA256(pub), created

	var cmd Commands
	need := map[string]*string{"sudo": &cmd.Sudo}
	if c.Client != nil {
		need["borg"], need["stat"], need["tar"] = &cmd.Borg, &cmd.Stat, &cmd.Tar
	}
	for name, dst := range need {
		if *dst, err = s.LookPath(name); err != nil {
			return nil, err
		}
	}
	knownHosts := ""
	if c.Client != nil {
		knownHosts = c.Client.KnownHosts
	}
	files := map[string]string{
		sshdConfigPath:                    c.SSHDConfig(),
		AuthorizedKeysDir + "/" + sshUser: c.AuthorizedKeys(cmd),
		sudoersPath:                       c.Sudoers(cmd),
		sshConfigPath:                     c.SSHConfig(),
		knownHostsPath:                    knownHosts,
	}

	if len(c.Repositories) > 0 {
		// borg serve creates the repositories (Initialise in Locutus) below
		// this directory, so it must belong to borg. Only the directory
		// itself is changed, never the files in it.
		if err := os.MkdirAll(s.path(c.ReposDir), 0o700); err != nil {
			return nil, err
		}
		if err := s.Chown(s.path(c.ReposDir), c.UID, c.GID); err != nil {
			return nil, err
		}
		if err := os.Chmod(s.path(c.ReposDir), 0o700); err != nil {
			return nil, err
		}
	}

	if c.Client != nil {
		// borg runs as root: keep its cache and security data with the
		// other persistent data, so backups stay fast after recreating the
		// container.
		for _, l := range []struct{ link, target string }{
			{"/root/.cache/borg", c.ConfigDir + "/borg/cache"},
			{"/root/.config/borg", c.ConfigDir + "/borg/config"},
		} {
			if err := s.symlink(l.link, l.target); err != nil {
				return nil, err
			}
		}
	}

	for p, content := range files {
		mode := fs.FileMode(0o644)
		if p == sudoersPath {
			mode = 0o440
		}
		if err := writeFile(s.path(p), content, mode); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func writeFile(path, content string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *System) symlink(link, target string) error {
	if err := os.MkdirAll(s.path(target), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path(link)), 0o700); err != nil {
		return err
	}
	if cur, err := os.Readlink(s.path(link)); err == nil && cur == target {
		return nil
	}
	if err := os.RemoveAll(s.path(link)); err != nil {
		return err
	}
	return os.Symlink(target, s.path(link))
}

// hostKey returns the public SSH host key, creating the key the first time. It lives in the configuration volume: a new key on every
// container would make Locutus refuse the connection.
func (s *System) hostKey(p string) (ssh.PublicKey, bool, error) {
	path := s.path(p)
	if data, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", p, err)
		}
		return signer.PublicKey(), false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "locutus-client")
	if err != nil {
		return nil, false, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, false, err
	}
	if err := writeFile(path+".pub", string(ssh.MarshalAuthorizedKey(sshPub)), 0o644); err != nil {
		return nil, false, err
	}
	if err := writeFile(path, string(pem.EncodeToMemory(block)), 0o600); err != nil {
		return nil, false, err
	}
	return sshPub, true, nil
}

// ensureUsers adds the user borg (with the configured ids) and, when the
// image has none, the privilege separation user of sshd.
func (s *System) ensureUsers(c *Config) error {
	passwd, err := os.ReadFile(s.path("/etc/passwd"))
	if err != nil {
		return err
	}
	if !hasEntry(string(passwd), "sshd") {
		if err := s.setEntry("/etc/passwd", "sshd", "sshd:x:22:22:sshd privilege separation:/var/empty:/sbin/nologin", 0o644); err != nil {
			return err
		}
		if err := s.setEntry("/etc/group", "sshd", "sshd:x:22:", 0o644); err != nil {
			return err
		}
	}
	// The ids must not belong to another user or group of the image.
	for _, f := range []struct {
		file, setting string
		id            int
	}{{"/etc/passwd", envUID, c.UID}, {"/etc/group", envGID, c.GID}} {
		if other, err := s.idOwner(f.file, f.id); err != nil {
			return err
		} else if other != "" {
			return fmt.Errorf("id %d is already used by %q in %s; choose another with %s", f.id, other, f.file, f.setting)
		}
	}
	if err := s.setEntry("/etc/group", sshUser, fmt.Sprintf("%s:x:%d:", sshUser, c.GID), 0o644); err != nil {
		return err
	}
	if err := s.setEntry("/etc/passwd", sshUser, fmt.Sprintf("%s:x:%d:%d:Locutus:/home/%s:/bin/sh", sshUser, c.UID, c.GID, sshUser), 0o644); err != nil {
		return err
	}
	// "*": no password at all, but not locked ("!"), which would also
	// refuse logging in with a key.
	return s.setEntry("/etc/shadow", sshUser, sshUser+":*:0:0:99999:7:::", 0o640)
}

// idOwner returns the name other than borg with the given id (third field)
// in a passwd style file, or "".
func (s *System) idOwner(file string, id int) (string, error) {
	data, err := os.ReadFile(s.path(file))
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Split(l, ":"); len(f) > 2 && f[0] != sshUser && f[2] == fmt.Sprint(id) {
			return f[0], nil
		}
	}
	return "", nil
}

func hasEntry(data, name string) bool {
	for _, l := range strings.Split(data, "\n") {
		if strings.HasPrefix(l, name+":") {
			return true
		}
	}
	return false
}

// setEntry replaces (or adds) the line of name in a passwd style file.
func (s *System) setEntry(file, name, line string, mode fs.FileMode) error {
	data, err := os.ReadFile(s.path(file))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, name+":") {
			out = append(out, l)
		}
	}
	out = append(out, line)
	return writeFile(s.path(file), strings.Join(out, "\n")+"\n", mode)
}

// SSHDCommand is the command that runs the SSH server in the foreground,
// logging to standard error (the container log).
func SSHDCommand() (string, []string, error) {
	p, err := exec.LookPath("sshd")
	if err != nil {
		for _, c := range []string{"/usr/sbin/sshd", "/usr/sbin/sshd.pam"} {
			if _, err := os.Stat(c); err == nil {
				p = c
				break
			}
		}
		if p == "" {
			return "", nil, errors.New("sshd not found")
		}
	}
	return p, []string{p, "-D", "-e", "-f", sshdConfigPath}, nil
}
