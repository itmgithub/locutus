package borg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestShellQuoteGlob(t *testing.T) {
	cases := map[string]string{
		"/data/sec*":      "/data/sec*",
		"/home/*/My Docs": "/home/*'/My Docs'",
		"/var/log/*.log":  "/var/log/*.log",
		"/srv/[ab]?":      "/srv/[ab]?",
		"/srv/[x":         "'/srv/[x'",
		"/srv/[a b]":      "'/srv/[a b]'",
		"/srv/[!0-9]x":    "/srv/[!0-9]x",
		"/plain":          "/plain",
		"/a b/c*":         "'/a b/c'*",
	}
	for in, want := range cases {
		if got := ShellQuoteGlob(in); got != want {
			t.Errorf("ShellQuoteGlob(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandPaths(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"secret.key", "second", "other", ".secret-hidden"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	got := ExpandPaths([]string{dir + "/sec*", dir + "/other", dir + "/nomatch*", dir + "/*hidden", dir + "/.sec*"})
	want := []string{dir + "/second", dir + "/secret.key", dir + "/other", dir + "/nomatch*", dir + "/*hidden", dir + "/.secret-hidden"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestRemoteCommandGlob(t *testing.T) {
	cmd := remoteCommand([]string{"create", "--", "ssh://b@h:22/r::a", "/data/sec*", "/etc"}, false, "/data/sec*")
	want := RemoteEnv + " borg create -- ssh://b@h:22/r::a /data/sec* /etc"
	if cmd != want {
		t.Errorf("got  %q\nwant %q", cmd, want)
	}
	cmd = remoteCommand([]string{"--version"}, true)
	want = RemoteEnv + " sudo -n borg --version"
	if cmd != want {
		t.Errorf("got  %q\nwant %q", cmd, want)
	}
}
