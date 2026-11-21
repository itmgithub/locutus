package borg

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"itmon.nl/locutus/internal/remote"
)

// A restore runs in three steps:
//
//  1. the files to restore are taken from the (cached) archive tree;
//  2. the target reports which of them already exist in the destination;
//  3. borg export-tar streams the files to Locutus, which drops the entries
//     that may not be written (existing files when not overwriting, protected
//     paths, unsafe names), puts the destination in front of every name and
//     passes the rest on to "tar -x -C /" on the target.
//
// A dry run stops after step 2 and only reports what would happen. Because
// Locutus reads the repository itself, a client never needs (or gets) the
// repository key for a restore.
//
// On a remote client only two fixed commands are used, so that they work in
// a plain shell, through locutus-borg-wrapper and with sudo:
//
//	LC_ALL=C xargs -0 -r [sudo -n] stat -c '%f %n' --   (paths on stdin)
//	[sudo -n] tar -x -p -f - -C /                        (tar stream on stdin)
//
// File names never appear on a command line.

// RestoreOptions describes what to restore and where to.
type RestoreOptions struct {
	Archive     string
	Paths       []string // archive paths; directories include everything below them
	Destination string   // absolute directory on the target; files keep their path below it
	DryRun      bool
	Overwrite   bool
}

// RestoreTarget is the system the restored files are written to.
type RestoreTarget interface {
	// Existing reports which of paths (relative to dir) already exist; the
	// value is true for a directory.
	Existing(ctx context.Context, dir string, paths []string) (map[string]bool, error)
	// Refused returns why the absolute path dest may not be written, or "".
	Refused(dest string) string
	// Extract unpacks the tar stream, whose names are relative to "/", and
	// returns the exit code of tar.
	Extract(ctx context.Context, stream io.Reader, out io.Writer) (int, error)
}

// ValidDestination reports whether dir can be used as restore destination:
// a clean absolute path without quotes or line breaks.
func ValidDestination(dir string) bool {
	return strings.HasPrefix(dir, "/") && path.Clean(dir) == dir && !strings.ContainsAny(dir, "'\x00\n\r")
}

// systemDirs of the Locutus container are never written by a local restore.
var systemDirs = []string{"/bin", "/dev", "/etc", "/lib", "/proc", "/sbin", "/sys", "/usr"}

// localTarget writes into the Locutus container itself.
type localTarget struct {
	protected []string
	mu        sync.Mutex
	parents   map[string]string // resolved parent directories
}

// LocalTarget returns the target for restores into the Locutus container.
// The data directory, the program and the system directories are protected.
func (s *Service) LocalTarget() RestoreTarget {
	t := &localTarget{parents: map[string]string{}}
	for _, p := range append(append([]string{}, systemDirs...), s.opts.Protected...) {
		if p != "" {
			t.protected = append(t.protected, resolvePath(p))
		}
	}
	return t
}

// resolvePath resolves the symbolic links in p, also when the last parts of
// p do not exist (yet).
func resolvePath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func within(p, root string) bool {
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

func (t *localTarget) Existing(_ context.Context, dir string, paths []string) (map[string]bool, error) {
	m := map[string]bool{}
	for _, p := range paths {
		if fi, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			m[p] = fi.IsDir()
		}
	}
	return m, nil
}

func (t *localTarget) Refused(dest string) string {
	// tar replaces the entry itself, so only symbolic links in the parent
	// directories matter.
	dir := filepath.Dir(dest)
	t.mu.Lock()
	real, ok := t.parents[dir]
	if !ok {
		real = resolvePath(dir)
		t.parents[dir] = real
	}
	t.mu.Unlock()
	real = filepath.Join(real, filepath.Base(dest))
	for _, root := range t.protected {
		if within(real, root) {
			return "protected path of the Locutus container"
		}
	}
	return ""
}

func (t *localTarget) Extract(ctx context.Context, stream io.Reader, out io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "tar", "-x", "-p", "-f", "-", "-C", "/")
	cmd.Stdin = stream
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// remoteTarget writes to a remote client through locutus-borg-wrapper.
type remoteTarget struct {
	s      *Service
	client Client
}

// RemoteTarget returns the target for restores to a remote client.
func (s *Service) RemoteTarget(c Client) RestoreTarget { return &remoteTarget{s: s, client: c} }

// Commands for restores on a remote client. They are fixed strings, which
// locutus-borg-wrapper compares literally.
const (
	statCommand = "LC_ALL=C xargs -0 -r stat -c '%f %n' --"
	tarCommand  = "tar -x -p -f - -C /"
)

// RestoreCommands returns the commands a restore runs on a client.
func RestoreCommands(sudo bool) (stat, tar string) {
	if sudo {
		return strings.Replace(statCommand, "stat ", sudoPrefix+"stat ", 1), sudoPrefix + tarCommand
	}
	return statCommand, tarCommand
}

// wrapperDenied is the exit code of locutus-borg-wrapper for a refused command.
const wrapperDenied = 126

func (t *remoteTarget) login() remote.Target {
	return remote.Target{Address: t.client.Address, User: t.client.User, PrivateKey: t.client.SSHKey}
}

// remoteError explains a failed restore command on the client.
func (t *remoteTarget) remoteError(rc int, stderr string) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case rc == wrapperDenied && strings.Contains(msg, "locutus-borg-wrapper"):
		return errors.New("the client refused the restore: install the current locutus-borg-wrapper on the client " +
			"and make sure it is not started with --no-restore")
	case t.client.Sudo && strings.Contains(msg, "sudo:"):
		return fmt.Errorf("sudo on the client failed (check the sudoers rules for stat and tar): %s", msg)
	}
	return fmt.Errorf("exit code %d on the client: %s", rc, msg)
}

// missingFile reports whether a line of stat is the error for a path that
// does not exist, which simply means the file is new.
func missingFile(line string) bool {
	return strings.Contains(line, "No such file or directory") || strings.Contains(line, "Not a directory")
}

func (t *remoteTarget) Existing(ctx context.Context, dir string, paths []string) (map[string]bool, error) {
	var in bytes.Buffer
	for _, p := range paths {
		if !strings.Contains(p, "\n") {
			in.WriteString(path.Join(dir, p))
			in.WriteByte(0)
		}
	}
	statCmd, _ := RestoreCommands(t.client.Sudo)
	var stdout, stderr bytes.Buffer
	rc, err := remote.RunIO(ctx, t.s.hostKeys, t.login(), statCmd, &in, &stdout, &stderr, nil)
	if err != nil {
		return nil, err
	}
	// stat exits with an error for paths that do not exist; anything else on
	// the error output is a real problem.
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" && !missingFile(line) {
			return nil, t.remoteError(rc, stderr.String())
		}
	}
	if rc != 0 && rc != 123 { // 123: xargs, when stat failed for a missing path
		return nil, t.remoteError(rc, stderr.String())
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	m := map[string]bool{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		mode, name, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		bits, err := strconv.ParseUint(mode, 16, 32)
		if err != nil {
			continue
		}
		m[strings.TrimPrefix(name, prefix)] = bits&0o170000 == 0o040000
	}
	return m, nil
}

func (t *remoteTarget) Refused(dest string) string {
	if strings.Contains(dest, "\n") {
		return "names with a line break cannot be restored to a remote client"
	}
	return ""
}

func (t *remoteTarget) Extract(ctx context.Context, stream io.Reader, out io.Writer) (int, error) {
	_, tarCmd := RestoreCommands(t.client.Sudo)
	var stderr bytes.Buffer
	rc, err := remote.Run(ctx, t.s.hostKeys, t.login(), tarCmd, stream, io.MultiWriter(out, &stderr), nil)
	if err == nil && rc != 0 {
		err = t.remoteError(rc, stderr.String())
	}
	return rc, err
}

type restoreAction int

const (
	actNew restoreAction = iota
	actOverwrite
	actSkip
	actExistingDir
	actRefused
)

// maxListed limits the number of names listed per action in the output.
const maxListed = 1000

// restorePlan decides per entry what happens and keeps the report.
type restorePlan struct {
	opts     RestoreOptions
	target   RestoreTarget
	existing map[string]bool

	symlinks map[string]bool // restored symbolic links
	skipped  map[string]bool // skipped directories

	files   [actRefused + 1]int
	lines   [actRefused + 1][]string
	newDirs int
	oldDirs int
	bytes   int64
}

func newRestorePlan(opts RestoreOptions, target RestoreTarget, existing map[string]bool) *restorePlan {
	return &restorePlan{opts: opts, target: target, existing: existing, symlinks: map[string]bool{}, skipped: map[string]bool{}}
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// decide returns the action for the archive path name (cleaned, relative).
func (p *restorePlan) decide(name string, isDir, isSymlink bool, size int64, unsafe bool) restoreAction {
	dest := path.Join(p.opts.Destination, name)
	act, reason := actNew, ""
	for parent := parentOf(name); parent != "" && reason == ""; parent = parentOf(parent) {
		if p.symlinks[parent] {
			act, reason = actRefused, "inside a restored symbolic link"
		} else if p.skipped[parent] {
			act, reason = actSkip, "a file with the name of a parent directory exists"
		}
	}
	switch {
	case reason != "":
	case unsafe:
		act, reason = actRefused, "unsafe path"
	default:
		if r := p.target.Refused(dest); r != "" {
			act, reason = actRefused, r
		} else if existsDir, exists := p.existing[name]; exists {
			switch {
			case isDir && existsDir:
				act = actExistingDir
			case p.opts.Overwrite:
				act = actOverwrite
			default:
				act, reason = actSkip, "exists"
			}
		}
	}
	if isDir && act == actSkip {
		p.skipped[name] = true
	}
	if isSymlink && (act == actNew || act == actOverwrite) {
		p.symlinks[name] = true
	}

	switch {
	case isDir && act == actNew:
		p.newDirs++
		return act
	case act == actExistingDir:
		p.oldDirs++
		return act
	case act == actNew || act == actOverwrite:
		p.bytes += size
	}
	p.files[act]++
	if len(p.lines[act]) < maxListed {
		line := dest
		if isDir {
			line += "/"
		}
		switch {
		case reason != "":
			line += "  (" + reason + ")"
		case !isDir && !isSymlink:
			line += "  (" + humanBytes(size) + ")"
		}
		p.lines[act] = append(p.lines[act], line)
	}
	return act
}

// rebase returns the name of an archive path below the destination,
// relative to "/".
func (p *restorePlan) rebase(name string) string {
	return strings.TrimPrefix(path.Join(p.opts.Destination, name), "/")
}

// writes reports whether an entry with this action is passed on to tar.
func (p *restorePlan) writes(act restoreAction) bool {
	return act == actNew || act == actOverwrite || (act == actExistingDir && p.opts.Overwrite)
}

func (p *restorePlan) report(out io.Writer) {
	labels := [...]string{actNew: "new", actOverwrite: "overwrite", actSkip: "skip", actRefused: "refused"}
	for _, act := range []restoreAction{actNew, actOverwrite, actSkip, actRefused} {
		if p.files[act] == 0 {
			continue
		}
		fmt.Fprintln(out)
		for _, l := range p.lines[act] {
			fmt.Fprintf(out, "%-9s  %s\n", labels[act], l)
		}
		if more := p.files[act] - len(p.lines[act]); more > 0 {
			fmt.Fprintf(out, "%-9s  ... and %d more\n", labels[act], more)
		}
	}
	verb := "Restored"
	if p.opts.DryRun {
		verb = "Would restore"
	}
	fmt.Fprintf(out, "\n%s: %d new and %d overwritten file(s), %s; %d skipped because they exist, %d refused. "+
		"Directories: %d new, %d existing.\n", verb, p.files[actNew], p.files[actOverwrite], humanBytes(p.bytes),
		p.files[actSkip], p.files[actRefused], p.newDirs, p.oldDirs)
}

func (p *restorePlan) rc() int {
	if p.files[actRefused] > 0 {
		return RCWarning
	}
	return RCSuccess
}

// filter copies the tar stream r to w, leaving out what may not be written.
func (p *restorePlan) filter(r io.Reader, w io.Writer) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := cleanPath(hdr.Name)
		if name == "" {
			continue
		}
		unsafe := path.IsAbs(hdr.Name) || hasDotDot(hdr.Name) ||
			(hdr.Typeflag == tar.TypeLink && (path.IsAbs(hdr.Linkname) || hasDotDot(hdr.Linkname)))
		act := p.decide(name, hdr.Typeflag == tar.TypeDir, hdr.Typeflag == tar.TypeSymlink, hdr.Size, unsafe)
		if !p.writes(act) {
			continue
		}
		// tar extracts relative to "/": put the destination in front.
		hdr.Name = p.rebase(name)
		if hdr.Typeflag == tar.TypeDir {
			hdr.Name += "/"
		}
		if hdr.Typeflag == tar.TypeLink {
			hdr.Linkname = p.rebase(cleanPath(hdr.Linkname))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			// Let the writer choose a format that can hold this header.
			hdr.Format = tar.FormatUnknown
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	return tw.Close()
}

// errRestoreStopped ends the pipes once one step of a restore has stopped.
var errRestoreStopped = errors.New("restore stopped")

// Restore writes the selected paths of an archive to target, below
// opts.Destination. tree is the tree of the archive. It returns the borg
// style exit code (RCWarning when entries were refused) and the amount of
// file data written, or in a dry run the amount that would be written.
func (s *Service) Restore(ctx context.Context, repo *Repo, tree *Tree, opts RestoreOptions, target RestoreTarget, out io.Writer) (int, int64, error) {
	if !ValidDestination(opts.Destination) {
		return -1, 0, fmt.Errorf("invalid destination %q", opts.Destination)
	}
	d, err := driver(repo)
	if err != nil {
		return -1, 0, err
	}
	items := tree.Below(opts.Paths)
	if len(items) == 0 {
		return -1, 0, errors.New("nothing to restore")
	}
	rel := make([]string, len(items))
	for i, it := range items {
		rel[i] = it.Path
	}
	fmt.Fprintf(out, "Checking which of the %d entries already exist in %s\n", len(items), opts.Destination)
	existing, err := target.Existing(ctx, opts.Destination, rel)
	if err != nil {
		return -1, 0, fmt.Errorf("check destination: %w", err)
	}
	p := newRestorePlan(opts, target, existing)

	if opts.DryRun {
		for _, it := range items {
			p.decide(it.Path, it.IsDir(), it.Type == "l", it.Size, false)
		}
		fmt.Fprintln(out, "Dry run: nothing has been written.")
		p.report(out)
		return p.rc(), p.bytes, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The first failure is the one reported; it stops the other steps.
	var firstErr error
	var once sync.Once
	fail := func(err error) {
		once.Do(func() { firstErr = err })
		cancel()
	}
	sw := &syncWriter{w: out}
	fromBorg, borgW := io.Pipe()
	toTar, tarW := io.Pipe()
	var borgStderr string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rc, errOut, err := s.run(ctx, repo, d.ExportTarArgs(repo.Location, opts.Archive, opts.Paths), nil, borgW, nil)
		borgStderr = errOut
		if err == nil && rc > RCWarning {
			err = &Error{Code: rc, Output: errOut}
		}
		if err != nil {
			fail(err)
		}
		borgW.CloseWithError(err)
	}()
	go func() {
		defer wg.Done()
		err := p.filter(fromBorg, tarW)
		if err == nil {
			io.Copy(io.Discard, fromBorg) // the padding after the end of the archive
		} else if !errors.Is(err, errRestoreStopped) {
			fail(err)
		}
		tarW.CloseWithError(err)
		fromBorg.CloseWithError(errRestoreStopped)
	}()
	tarRC, err := target.Extract(ctx, toTar, sw)
	if err == nil && tarRC != 0 {
		err = fmt.Errorf("tar on the target exited with code %d", tarRC)
	}
	if err != nil {
		fail(err)
	}
	toTar.CloseWithError(errRestoreStopped)
	wg.Wait()

	if strings.TrimSpace(borgStderr) != "" {
		fmt.Fprint(out, borgStderr)
	}
	p.report(out)
	if firstErr != nil {
		return -1, p.bytes, firstErr
	}
	return p.rc(), p.bytes, nil
}

// humanBytes formats a size with binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
