package borg

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"itmon.nl/locutus/internal/remote"
)

// Options configures the Service.
type Options struct {
	Binary     string // borg executable inside the container
	BaseDir    string // BORG_BASE_DIR: cache, config and security data
	KnownHosts string // known_hosts file for repositories and clients
	// Protected are paths in the Locutus container that a restore never
	// writes to (next to the system directories), such as the data directory.
	Protected []string
}

// Service runs borg commands for Locutus.
type Service struct {
	opts     Options
	hostKeys *remote.HostKeys
	trees    *treeCache

	versionOnce  sync.Once
	localVersion [2]int
}

// Exit codes of borg 1.x (legacy mode).
const (
	RCSuccess = 0
	RCWarning = 1
)

// NewService prepares the directories used by borg.
func NewService(opts Options) (*Service, error) {
	if err := os.MkdirAll(opts.BaseDir, 0o700); err != nil {
		return nil, err
	}
	hk, err := remote.NewHostKeys(opts.KnownHosts)
	if err != nil {
		return nil, err
	}
	return &Service{opts: opts, hostKeys: hk, trees: newTreeCache(4, 15*time.Minute)}, nil
}

// Error is returned when borg exits with an error code.
type Error struct {
	Code   int
	Output string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Output)
	if i := strings.LastIndex(msg, "\n"); i >= 0 && len(msg) > 2000 {
		msg = msg[i+1:]
	}
	return fmt.Sprintf("borg exit code %d: %s", e.Code, msg)
}

// env returns the environment for a local borg process.
func (s *Service) env(repo *Repo, agentSocket string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.opts.BaseDir,
		"LANG=C.UTF-8",
		"BORG_BASE_DIR=" + s.opts.BaseDir,
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_DISPLAY_PASSPHRASE=no",
		"BORG_CHECK_I_KNOW_WHAT_I_AM_DOING=NO",
		"BORG_DELETE_I_KNOW_WHAT_I_AM_DOING=NO",
		"BORG_RSH=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 -o UserKnownHostsFile=" +
			s.opts.KnownHosts,
	}
	if tz := os.Getenv("TZ"); tz != "" {
		env = append(env, "TZ="+tz)
	}
	if repo != nil {
		// Always set, so borg never waits for an interactive prompt.
		env = append(env, "BORG_PASSPHRASE="+repo.Passphrase)
	}
	if agentSocket != "" {
		env = append(env, "SSH_AUTH_SOCK="+agentSocket)
	}
	return env
}

// withAgent starts an in-memory ssh-agent for a remote repository.
func withAgent(repo *Repo) (socket string, cleanup func(), err error) {
	if repo == nil || !repo.Remote || repo.SSHKey == "" {
		return "", func() {}, nil
	}
	ring, err := newKeyring(repo.SSHKey)
	if err != nil {
		return "", nil, err
	}
	return serveAgent(ring)
}

// run executes borg locally. stdout receives the standard output; the
// standard error output is returned (and copied to stderr when set).
func (s *Service) run(ctx context.Context, repo *Repo, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, string, error) {
	socket, cleanup, err := withAgent(repo)
	if err != nil {
		return -1, "", err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, s.opts.Binary, args...)
	cmd.Env = s.env(repo, socket)
	cmd.Dir = s.opts.BaseDir
	cmd.Stdin = stdin
	var errBuf bytes.Buffer
	if stderr != nil && stderr == stdout {
		// Both streams go to one writer: serialise the writes.
		w := &syncWriter{w: stdout}
		cmd.Stdout, cmd.Stderr = w, io.MultiWriter(&errBuf, w)
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = &errBuf
		if stderr != nil {
			cmd.Stderr = io.MultiWriter(&errBuf, stderr)
		}
	}
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), errBuf.String(), nil
	}
	if err != nil {
		return -1, errBuf.String(), err
	}
	return 0, errBuf.String(), nil
}

// output runs a borg command and returns its standard output; any exit code
// other than success is an error.
func (s *Service) output(ctx context.Context, repo *Repo, args []string) ([]byte, error) {
	var out bytes.Buffer
	rc, errOut, err := s.run(ctx, repo, args, nil, &out, nil)
	if err != nil {
		return nil, err
	}
	if rc != RCSuccess && rc != RCWarning {
		return nil, &Error{Code: rc, Output: errOut}
	}
	return out.Bytes(), nil
}

func driver(repo *Repo) (Driver, error) { return DriverFor(repo.Version) }

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// LocalVersion returns the version string of the borg binary in the container.
func (s *Service) LocalVersion(ctx context.Context) (string, error) {
	out, err := s.output(ctx, nil, []string{"--version"})
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	s.versionOnce.Do(func() {
		if m := versionRe.FindStringSubmatch(v); m != nil {
			s.localVersion[0], _ = strconv.Atoi(m[1])
			s.localVersion[1], _ = strconv.Atoi(m[2])
		}
	})
	return v, nil
}

// supportsCompact reports whether the local borg has "borg compact" (1.2+).
func (s *Service) supportsCompact(ctx context.Context) bool {
	s.LocalVersion(ctx)
	v := s.localVersion
	return v[0] > 1 || (v[0] == 1 && v[1] >= 2)
}

// Init creates a new repository.
func (s *Service) Init(ctx context.Context, repo *Repo, encryption string) (string, error) {
	d, err := driver(repo)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	rc, errOut, err := s.run(ctx, repo, d.InitArgs(repo.Location, encryption), nil, &out, nil)
	if err != nil {
		return errOut, err
	}
	if rc != RCSuccess && rc != RCWarning {
		return out.String() + errOut, &Error{Code: rc, Output: errOut}
	}
	return out.String() + errOut, nil
}

// Info returns the human readable repository information.
func (s *Service) Info(ctx context.Context, repo *Repo) (string, error) {
	d, err := driver(repo)
	if err != nil {
		return "", err
	}
	out, err := s.output(ctx, repo, d.InfoArgs(repo.Location))
	return string(out), err
}

// Archives lists the archives of a repository, newest first. glob limits the
// list to the archives of one client.
func (s *Service) Archives(ctx context.Context, repo *Repo, glob string) ([]Archive, error) {
	d, err := driver(repo)
	if err != nil {
		return nil, err
	}
	out, err := s.output(ctx, repo, d.ListArchivesArgs(repo.Location, glob))
	if err != nil {
		return nil, err
	}
	return d.ParseArchives(out)
}

// Tree returns the (cached) file tree of an archive.
func (s *Service) Tree(ctx context.Context, repo *Repo, archive string) (*Tree, error) {
	key := repo.Location + "::" + archive
	return s.trees.get(key, func() (*Tree, error) {
		d, err := driver(repo)
		if err != nil {
			return nil, err
		}
		tree := newTree()
		pr, pw := io.Pipe()
		var parseErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
			for sc.Scan() {
				it, err := d.ParseItem(sc.Bytes())
				if err != nil {
					parseErr = err
					continue
				}
				tree.add(it)
			}
			io.Copy(io.Discard, pr)
		}()
		rc, errOut, err := s.run(ctx, repo, d.ListItemsArgs(repo.Location, archive), nil, pw, nil)
		pw.Close()
		<-done
		if err != nil {
			return nil, err
		}
		if rc != RCSuccess && rc != RCWarning {
			return nil, &Error{Code: rc, Output: errOut}
		}
		if parseErr != nil && tree.Len() == 0 {
			return nil, parseErr
		}
		tree.finish()
		return tree, nil
	})
}

// ExtractFile streams the contents of a single file from an archive.
func (s *Service) ExtractFile(ctx context.Context, repo *Repo, archive, path string, w io.Writer) error {
	d, err := driver(repo)
	if err != nil {
		return err
	}
	rc, errOut, err := s.run(ctx, repo, d.ExtractStdoutArgs(repo.Location, archive, path), nil, w, nil)
	if err != nil {
		return err
	}
	if rc != RCSuccess && rc != RCWarning {
		return &Error{Code: rc, Output: errOut}
	}
	return nil
}

// ExportTar streams the given paths (files and directories, recursively) of
// an archive as an uncompressed tar stream.
func (s *Service) ExportTar(ctx context.Context, repo *Repo, archive string, paths []string, w io.Writer) error {
	d, err := driver(repo)
	if err != nil {
		return err
	}
	rc, errOut, err := s.run(ctx, repo, d.ExportTarArgs(repo.Location, archive, paths), nil, w, nil)
	if err != nil {
		return err
	}
	if rc != RCSuccess && rc != RCWarning {
		return &Error{Code: rc, Output: errOut}
	}
	return nil
}

// RemoteEnv is put in front of every borg command that Locutus runs on a
// remote client. The passphrase is sent on standard input (file descriptor
// 0), so it never appears on a command line or in the environment.
const RemoteEnv = "env BORG_PASSPHRASE_FD=0 BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes BORG_RELOCATED_REPO_ACCESS_IS_OK=yes"

// sudoPrefix is put in front of the commands that run with sudo on a
// client. -n makes sudo fail at once instead of asking for a password.
const sudoPrefix = "sudo -n "

// remoteCommand quotes args for the remote shell; the arguments in globs
// keep their wildcards unquoted so the shell on the client expands them.
// With sudo, borg runs as root; the BORG_ variables are set by env before
// sudo and must be kept by sudo (env_keep in sudoers).
func remoteCommand(args []string, sudo bool, globs ...string) string {
	expand := map[string]bool{}
	for _, g := range globs {
		expand[g] = true
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		if expand[a] {
			quoted[i] = ShellQuoteGlob(a)
		} else {
			quoted[i] = ShellQuote(a)
		}
	}
	borg := "borg "
	if sudo {
		borg = sudoPrefix + borg
	}
	return RemoteEnv + " " + borg + strings.Join(quoted, " ")
}

// Create makes a backup. For a local client borg runs in the container; for
// a remote client Locutus logs in over SSH and forwards an agent holding the
// repository key, so that borg on the client can reach the repository.
// All output is written to out; the borg exit code is returned.
func (s *Service) Create(ctx context.Context, client Client, repo *Repo, archive string, opts CreateOptions, out io.Writer) (int, error) {
	d, err := driver(repo)
	if err != nil {
		return -1, err
	}
	if client.Local {
		expanded := ExpandPaths(opts.Paths)
		if strings.Join(expanded, "\n") != strings.Join(opts.Paths, "\n") {
			fmt.Fprintf(out, "Paths: %s\n", strings.Join(expanded, " "))
		}
		opts.Paths = expanded
		rc, _, err := s.run(ctx, repo, d.CreateArgs(repo.Location, archive, opts), nil, out, out)
		return rc, err
	}
	args := d.CreateArgs(repo.Location, archive, opts)
	if !repo.Remote {
		return -1, errors.New("a remote client can only back up to a remote repository")
	}
	var fwd agent.Agent
	if repo.SSHKey != "" {
		if fwd, err = newKeyring(repo.SSHKey); err != nil {
			return -1, err
		}
	}
	var globs []string
	for _, p := range opts.Paths {
		if hasGlob(p) {
			globs = append(globs, p)
		}
	}
	cmd := remoteCommand(args, client.Sudo, globs...)
	fmt.Fprintf(out, "$ ssh %s@%s %s\n", client.User, client.Address, cmd)
	return remote.Run(ctx, s.hostKeys, remote.Target{Address: client.Address, User: client.User, PrivateKey: client.SSHKey},
		cmd, strings.NewReader(repo.Passphrase), out, fwd)
}

// ClientVersion checks the connection to a remote client and returns the
// borg version installed there.
func (s *Service) ClientVersion(ctx context.Context, client Client) (string, error) {
	if client.Local {
		return s.LocalVersion(ctx)
	}
	var out bytes.Buffer
	rc, err := remote.Run(ctx, s.hostKeys, remote.Target{Address: client.Address, User: client.User, PrivateKey: client.SSHKey},
		remoteCommand([]string{"--version"}, client.Sudo), strings.NewReader(""), &out, nil)
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", &Error{Code: rc, Output: out.String()}
	}
	return strings.TrimSpace(out.String()), nil
}

// Prune removes old archives matching glob according to keep and then frees
// the space (borg compact). Output goes to out; the worst exit code is returned.
func (s *Service) Prune(ctx context.Context, repo *Repo, glob string, keep Retention, out io.Writer) (int, error) {
	d, err := driver(repo)
	if err != nil {
		return -1, err
	}
	if keep.Empty() {
		return -1, errors.New("no retention rules configured")
	}
	return s.removeAndCompact(ctx, d, repo, d.PruneArgs, glob, keep, out)
}

// Delete removes one archive and then frees the space (borg compact).
func (s *Service) Delete(ctx context.Context, repo *Repo, archive string, out io.Writer) (int, error) {
	d, err := driver(repo)
	if err != nil {
		return -1, err
	}
	if archive == "" || strings.HasPrefix(archive, "-") || strings.Contains(archive, "::") {
		return -1, fmt.Errorf("invalid archive name %q", archive)
	}
	args := func(location, _ string, _ Retention) []string { return d.DeleteArgs(location, archive) }
	return s.removeAndCompact(ctx, d, repo, args, "", Retention{}, out)
}

// removeAndCompact runs prune or delete with the prune key of the repository
// (when one is set) and then borg compact.
func (s *Service) removeAndCompact(ctx context.Context, d Driver, repo *Repo,
	args func(location, glob string, keep Retention) []string, glob string, keep Retention, out io.Writer) (int, error) {
	if repo.PruneSSHKey != "" {
		fmt.Fprintln(out, "Using the separate prune key of the repository.")
	}
	repo = repo.ForPrune()
	rc, _, err := s.run(ctx, repo, args(repo.Location, glob, keep), nil, out, out)
	if err != nil || rc > RCWarning {
		return rc, err
	}
	s.trees.dropRepo(repo.Location)
	if s.supportsCompact(ctx) {
		fmt.Fprintln(out, "--- borg compact ---")
		crc, _, err := s.run(ctx, repo, d.CompactArgs(repo.Location), nil, out, out)
		if err != nil {
			return crc, err
		}
		if crc > rc {
			rc = crc
		}
	}
	return rc, nil
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
