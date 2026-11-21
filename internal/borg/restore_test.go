package borg

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidDestination(t *testing.T) {
	for dir, want := range map[string]bool{
		"/restore": true, "/": true, "/my files/x": true,
		"restore": false, "/a/../b": false, "/a/": false, "/it's": false, "/a\nb": false, "": false,
	} {
		if got := ValidDestination(dir); got != want {
			t.Errorf("ValidDestination(%q) = %v", dir, got)
		}
	}
}

func TestTreeBelow(t *testing.T) {
	tr := newTree()
	for _, p := range []string{"etc/hosts", "home/a/x", "home/a/y", "home/b/z"} {
		tr.add(Item{Path: p, Type: "-"})
	}
	tr.finish()
	var got []string
	for _, it := range tr.Below([]string{"home/a", "home/a/x", "etc/hosts", "missing"}) {
		got = append(got, it.Path)
	}
	want := "home/a home/a/x home/a/y etc/hosts"
	if strings.Join(got, " ") != want {
		t.Errorf("Below = %q, want %q", got, want)
	}
}

func TestLocalTargetRefused(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	os.Mkdir(data, 0o700)
	os.Symlink(data, filepath.Join(dir, "link"))
	s := &Service{opts: Options{Protected: []string{data}}}
	target := s.LocalTarget()
	for p, refused := range map[string]bool{
		filepath.Join(dir, "restore/x"):    false,
		filepath.Join(data, "locutus.db"):  true,
		data:                               true,
		filepath.Join(dir, "link/x"):       true,
		filepath.Join(dir, "link"):         false, // replacing the link itself is fine
		"/etc/passwd":                      true,
		filepath.Join(dir, "data2/x"):      false,
		filepath.Join(dir, "new/deeper/x"): false,
	} {
		if got := target.Refused(p) != ""; got != refused {
			t.Errorf("Refused(%s) = %v, want %v", p, got, refused)
		}
	}
}

// borgRepo creates a repository with one archive "a" holding src/... and
// returns the service, the repository and the source directory.
func borgRepo(t *testing.T) (*Service, *Repo, string, string) {
	t.Helper()
	if _, err := exec.LookPath("borg"); err != nil {
		t.Skip("borg is not installed")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not installed")
	}
	base := t.TempDir()
	s, err := NewService(Options{Binary: "borg", BaseDir: filepath.Join(base, "borg"), KnownHosts: filepath.Join(base, "known_hosts")})
	if err != nil {
		t.Fatal(err)
	}
	repo := &Repo{Location: filepath.Join(base, "repo"), Version: 1}
	ctx := context.Background()
	if _, err := s.Init(ctx, repo, "none"); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "src")
	files := map[string]string{"etc/hosts": "hosts from backup", "etc/app.conf": "conf from backup", "home/u/doc.txt": "doc",
		"weird/$(id); name": "weird"}
	for name, content := range files {
		p := filepath.Join(src, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("hosts", filepath.Join(src, "etc/hosts.link"))
	var out bytes.Buffer
	cmd := exec.Command("borg", "create", repo.Location+"::a", ".")
	cmd.Dir = src
	cmd.Env = s.env(repo, "")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("borg create: %v\n%s", err, out.String())
	}
	return s, repo, src, base
}

func TestRestoreLocal(t *testing.T) {
	s, repo, _, base := borgRepo(t)
	ctx := context.Background()
	tree, err := s.Tree(ctx, repo, "a")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "restore")
	os.MkdirAll(filepath.Join(dest, "etc"), 0o755)
	os.WriteFile(filepath.Join(dest, "etc/hosts"), []byte("existing"), 0o644)
	target := s.LocalTarget()
	paths := []string{"etc", "home/u/doc.txt", "weird"}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(dest, p))
		return string(b)
	}

	// Dry run: nothing is written, the existing file is reported.
	var out bytes.Buffer
	rc, _, err := s.Restore(ctx, repo, tree, RestoreOptions{Archive: "a", Paths: paths, Destination: dest, DryRun: true}, target, &out)
	if err != nil || rc != 0 {
		t.Fatalf("dry run rc=%d err=%v\n%s", rc, err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "etc/app.conf")); err == nil {
		t.Fatal("dry run wrote files")
	}
	if !strings.Contains(out.String(), "skip       "+filepath.Join(dest, "etc/hosts")+"  (exists)") ||
		!strings.Contains(out.String(), "new        "+filepath.Join(dest, "etc/app.conf")) {
		t.Errorf("dry run report:\n%s", out.String())
	}

	// Without overwrite the existing file stays.
	out.Reset()
	rc, n, err := s.Restore(ctx, repo, tree, RestoreOptions{Archive: "a", Paths: paths, Destination: dest}, target, &out)
	if err != nil || rc != 0 {
		t.Fatalf("restore rc=%d err=%v\n%s", rc, err, out.String())
	}
	if read("etc/hosts") != "existing" || read("etc/app.conf") != "conf from backup" || read("home/u/doc.txt") != "doc" ||
		read("weird/$(id); name") != "weird" {
		t.Errorf("unexpected result after restore:\n%s", out.String())
	}
	if l, _ := os.Readlink(filepath.Join(dest, "etc/hosts.link")); l != "hosts" {
		t.Errorf("symlink = %q", l)
	}
	if n != int64(len("conf from backup")+len("doc")+len("weird")) {
		t.Errorf("bytes = %d", n)
	}

	// With overwrite it is replaced.
	out.Reset()
	if rc, _, err = s.Restore(ctx, repo, tree, RestoreOptions{Archive: "a", Paths: []string{"etc/hosts"}, Destination: dest, Overwrite: true},
		target, &out); err != nil || rc != 0 {
		t.Fatalf("overwrite rc=%d err=%v\n%s", rc, err, out.String())
	}
	if read("etc/hosts") != "hosts from backup" {
		t.Errorf("not overwritten:\n%s", out.String())
	}

	// Protected paths are refused, the rest is restored.
	protected := filepath.Join(base, "prot")
	s.opts.Protected = []string{filepath.Join(protected, "etc")}
	out.Reset()
	rc, _, err = s.Restore(ctx, repo, tree, RestoreOptions{Archive: "a", Paths: []string{"etc", "home"}, Destination: protected},
		s.LocalTarget(), &out)
	if err != nil || rc != RCWarning {
		t.Fatalf("protected rc=%d err=%v\n%s", rc, err, out.String())
	}
	if _, err := os.Stat(filepath.Join(protected, "etc/hosts")); err == nil {
		t.Error("protected path was written")
	}
	if _, err := os.Stat(filepath.Join(protected, "home/u/doc.txt")); err != nil {
		t.Errorf("unprotected path missing:\n%s", out.String())
	}
}

// TestWrapperRestore runs the restore commands the way they run on a client:
// through the wrapper and in a plain shell, and uses the remote target's
// parsing of the stat output.
func TestWrapperRestore(t *testing.T) {
	wrapper, _ := filepath.Abs("../../contrib/locutus-borg-wrapper.sh")
	dir := t.TempDir()
	dest := filepath.Join(dir, "my restore")
	run := func(useWrapper bool, cmd string, stdin io.Reader, args ...string) (string, string, int) {
		var c *exec.Cmd
		if useWrapper {
			c = exec.Command("sh", append([]string{wrapper}, args...)...)
			c.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmd)
		} else {
			c = exec.Command("sh", "-c", cmd)
		}
		var stdout, stderr bytes.Buffer
		c.Stdin, c.Stdout, c.Stderr = stdin, &stdout, &stderr
		c.Run()
		return stdout.String(), stderr.String(), c.ProcessState.ExitCode()
	}
	statCmd, tarCmd := RestoreCommands(false)

	os.MkdirAll(filepath.Join(dest, "etc"), 0o755)
	os.WriteFile(filepath.Join(dest, "etc/$(id); x"), nil, 0o644)
	os.Symlink("/etc", filepath.Join(dest, "link"))
	var in bytes.Buffer
	for _, p := range []string{"etc", "etc/$(id); x", "etc/none", "link", "none/deeper"} {
		in.WriteString(filepath.Join(dest, p) + "\x00")
	}
	stdin := in.String()
	for _, useWrapper := range []bool{true, false} {
		out, errOut, rc := run(useWrapper, statCmd, strings.NewReader(stdin))
		want := "4" // directory mode starts with 4 in hex (0x41ed)
		if rc != 123 || !strings.HasPrefix(out, want) || strings.Count(out, "\n") != 3 {
			t.Fatalf("stat (wrapper %v): rc=%d out=%q err=%q", useWrapper, rc, out, errOut)
		}
		for _, line := range strings.Split(strings.TrimSpace(errOut), "\n") {
			if !missingFile(line) {
				t.Errorf("unexpected stat error %q", line)
			}
		}
	}

	// extract a tar stream with names relative to "/"
	rel := strings.TrimPrefix(dest, "/")
	tarball := func() io.Reader {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		tw.WriteHeader(&tar.Header{Name: rel + "/x/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: rel + "/x/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		tw.Write([]byte("data"))
		tw.Close()
		return &b
	}
	for _, useWrapper := range []bool{true, false} {
		os.RemoveAll(filepath.Join(dest, "x"))
		if _, errOut, rc := run(useWrapper, tarCmd, tarball()); rc != 0 {
			t.Fatalf("extract (wrapper %v): rc=%d %s", useWrapper, rc, errOut)
		}
		if b, _ := os.ReadFile(filepath.Join(dest, "x/file")); string(b) != "data" {
			t.Errorf("extract (wrapper %v) did not write the file", useWrapper)
		}
	}

	// refused by the wrapper
	sudoStat, sudoTar := RestoreCommands(true)
	for _, cmd := range []string{
		"tar -x -p -f - -C /etc",
		"sudo tar -x -f - -C /",
		"xargs -0 -r rm --",
		"env BORG_X=1 borg extract ssh://h/r::a",
		"env BORG_X=1 sudo borg create x",
		"sudo -n sh",
	} {
		if _, errOut, rc := run(true, cmd, strings.NewReader("")); rc != wrapperDenied {
			t.Errorf("%q: rc=%d %s", cmd, rc, errOut)
		}
	}
	for _, cmd := range []string{sudoStat, sudoTar, statCmd, tarCmd} {
		if _, _, rc := run(true, cmd, strings.NewReader(""), "--no-restore"); rc != wrapperDenied {
			t.Errorf("--no-restore %q: rc=%d", cmd, rc)
		}
	}
	// allowed with sudo (sudo itself may be missing in the test environment)
	for _, cmd := range []string{sudoStat, sudoTar, RemoteEnv + " sudo -n borg --version"} {
		if _, errOut, rc := run(true, cmd, strings.NewReader("")); rc == wrapperDenied && strings.Contains(errOut, "not allowed") {
			t.Errorf("%q refused by the wrapper", cmd)
		}
	}
}
